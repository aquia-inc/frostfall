// Package runner orchestrates tests: navigate, execute steps, scan, then
// fingerprint and classify results against the baseline.
package runner

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/aquia-inc/frostfall/internal/baseline"
	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/fingerprint"
)

// Result is one classified violation.
type Result struct {
	engine.Violation
	TestID       string
	ScanLabel    string
	Fingerprint  string
	StableTarget string
	Baselined    bool
	// ScreenshotPath is set when an element screenshot was captured for this
	// violation ("" when capture is off or the element couldn't be shot).
	ScreenshotPath string
}

// Run holds a full run's outcome.
type Run struct {
	Results  []Result
	Stale    []baseline.Violation // baselined entries that no longer occur
	TestsRun int
	// Scanned records test ids that actually executed at least one scan —
	// distinct from configured tests: issue lifecycle must only treat a page
	// as "looked at" when a scan genuinely ran there.
	Scanned map[string]bool
}

// NewViolations counts non-baselined results at or above the severity floor.
func (r *Run) NewViolations(minImpact engine.Impact) int {
	n := 0
	for _, res := range r.Results {
		if !res.Baselined && res.Impact >= minImpact {
			n++
		}
	}
	return n
}

// Failing reports whether a single result breaks the expect contract, judged
// against its test's effective expect: a per-test override that enforces
// anything replaces the default wholesale; otherwise the default applies. A
// violation fails when it is at or above the severity floor, or matches an
// enforced rule id at any impact. This is THE enforcement predicate — the
// exit code and every report must share it so a run can never exit 1 while
// its summary reads clean.
func Failing(res Result, def config.Expect, perTest map[string]config.Expect) bool {
	if res.Baselined {
		return false
	}
	exp := def
	if o, ok := perTest[res.TestID]; ok && o.Enforcing() {
		exp = o
	}
	if minImpact, hasFloor := engine.ParseImpact(exp.Severity); hasFloor && res.Impact >= minImpact {
		return true
	}
	return slices.Contains(exp.Rules, res.RuleID)
}

// EnforcedFailures counts new violations that break the expect contract.
func (r *Run) EnforcedFailures(def config.Expect, perTest map[string]config.Expect) int {
	n := 0
	for _, res := range r.Results {
		if Failing(res, def, perTest) {
			n++
		}
	}
	return n
}

type Runner struct {
	Browser  *browser.Browser
	Engine   engine.Engine
	Config   *config.Config
	BaseURL  string
	Baseline *baseline.File
	// ScreenshotDir enables element screenshots for new (non-baselined)
	// violations when non-empty. Captures happen at scan time, while the
	// page session is still alive.
	ScreenshotDir string
	Verbose       bool
	Log           func(format string, args ...any)

	baselineIdx map[string]baseline.Violation
}

func (r *Runner) logf(format string, args ...any) {
	if r.Verbose && r.Log != nil {
		r.Log(format, args...)
	}
}

// Execute runs every test and classifies the aggregate results.
func (r *Runner) Execute(ctx context.Context, tests []config.Test) (*Run, error) {
	if r.Baseline != nil {
		r.baselineIdx = r.Baseline.Index()
	}
	if r.ScreenshotDir != "" {
		if err := os.MkdirAll(r.ScreenshotDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating screenshot dir: %w", err)
		}
	}
	run := &Run{Scanned: map[string]bool{}}
	for _, t := range tests {
		results, scanned, err := r.runTest(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("test %q: %w", t.ID, err)
		}
		if scanned {
			run.Scanned[t.ID] = true
		}
		run.Results = append(run.Results, results...)
		run.TestsRun++
	}
	r.classify(run)
	return run, nil
}

func (r *Runner) runTest(ctx context.Context, t config.Test) (results []Result, scanned bool, err error) {
	d := r.Config.Defaults
	url := t.URL
	if url == "" {
		url = r.BaseURL + t.Path
	}
	waitFor := t.WaitFor
	if waitFor == "" {
		waitFor = d.WaitFor
	}

	r.logf("test %s: loading %s", t.ID, url)
	session, err := r.Browser.NewSession(url, d.Viewport, waitFor, d.SettleTime.Std(), d.Timeout.Std())
	if err != nil {
		return nil, false, err
	}
	defer session.Close()

	scanInitial := t.Scan == nil || *t.Scan
	if scanInitial {
		res, err := r.scan(ctx, session, t, "initial", config.ScanStep{})
		if err != nil {
			return nil, false, err
		}
		scanned = true
		results = append(results, res...)
	}

	for i, step := range t.Steps {
		if step.Scan != nil {
			label := step.Scan.Label
			if label == "" {
				label = "step-" + strconv.Itoa(i)
			}
			res, err := r.scan(ctx, session, t, label, *step.Scan)
			if err != nil {
				return nil, false, err
			}
			scanned = true
			results = append(results, res...)
			continue
		}
		r.logf("test %s: step %d", t.ID, i)
		if err := session.Step(step, r.BaseURL, d.WaitFor, d.SettleTime.Std()); err != nil {
			return nil, false, fmt.Errorf("steps[%d]: %w", i, err)
		}
	}
	return results, scanned, nil
}

func (r *Runner) scan(ctx context.Context, session *browser.Session, t config.Test, label string, s config.ScanStep) ([]Result, error) {
	rules := map[string]bool{}
	maps.Copy(rules, r.Config.Defaults.Rules)
	maps.Copy(rules, t.Rules)
	maps.Copy(rules, s.Rules)

	r.logf("test %s: scanning (%s)", t.ID, label)
	violations, err := r.Engine.Audit(ctx, session, engine.ScanOptions{
		Standard: r.Config.Defaults.Standard,
		Scope:    s.Scope,
		Rules:    rules,
	})
	if err != nil {
		return nil, fmt.Errorf("scan %q: %w", label, err)
	}

	results := make([]Result, 0, len(violations))
	for _, v := range violations {
		res := Result{
			Violation:    v,
			TestID:       t.ID,
			ScanLabel:    label,
			Fingerprint:  fingerprint.Fingerprint(t.ID, label, v.RuleID, v.Target),
			StableTarget: fingerprint.StableTarget(v.Target),
		}
		// Screenshot new violations only: baselined ones are known debt, and
		// skipping them keeps CI artifact size proportional to what changed.
		if r.ScreenshotDir != "" {
			if _, known := r.baselineIdx[res.Fingerprint]; !known {
				res.ScreenshotPath = r.captureScreenshot(session, res)
			}
		}
		results = append(results, res)
	}
	return results, nil
}

// captureScreenshot best-effort shoots the violating element. Failure is
// logged, never fatal: a violation without a picture still counts.
func (r *Runner) captureScreenshot(session *browser.Session, res Result) string {
	png, err := session.CaptureElement(res.Target)
	if err != nil {
		r.logf("screenshot %s: %v", res.Fingerprint, err)
		return ""
	}
	path := filepath.Join(r.ScreenshotDir, res.Fingerprint+".png")
	if err := os.WriteFile(path, png, 0o644); err != nil {
		r.logf("screenshot %s: %v", res.Fingerprint, err)
		return ""
	}
	return path
}

// classify marks baselined results and collects stale baseline entries.
func (r *Runner) classify(run *Run) {
	if r.Baseline == nil {
		return
	}
	idx := r.Baseline.Index()
	seen := map[string]bool{}
	for i := range run.Results {
		if _, ok := idx[run.Results[i].Fingerprint]; ok {
			run.Results[i].Baselined = true
			seen[run.Results[i].Fingerprint] = true
		}
	}
	for _, entry := range r.Baseline.Violations {
		// Stale means "scanned its page and the violation is gone" — an entry
		// whose test didn't run this time (filtered run) is unknown, not stale.
		if !seen[entry.Fingerprint] && run.Scanned[entry.TestID] {
			run.Stale = append(run.Stale, entry)
		}
	}
}

// ToBaseline builds the baseline file for --update-baseline, preserving notes
// from surviving entries.
func (r *Runner) ToBaseline(run *Run) *baseline.File {
	notes := map[string]string{}
	if r.Baseline != nil {
		for _, v := range r.Baseline.Violations {
			notes[v.Fingerprint] = v.Note
		}
	}
	f := &baseline.File{Version: 1, AxeVersion: r.Engine.Version()}
	dedup := map[string]bool{}
	for _, res := range run.Results {
		if dedup[res.Fingerprint] {
			continue
		}
		dedup[res.Fingerprint] = true
		f.Violations = append(f.Violations, baseline.Violation{
			Fingerprint:  res.Fingerprint,
			TestID:       res.TestID,
			ScanLabel:    res.ScanLabel,
			RuleID:       res.RuleID,
			StableTarget: res.StableTarget,
			Note:         notes[res.Fingerprint],
		})
	}
	return f
}
