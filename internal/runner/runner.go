// Package runner orchestrates tests: navigate, execute steps, scan, then
// fingerprint and classify results against the baseline.
package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aquia-inc/frostfall/internal/baseline"
	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/fingerprint"
	"github.com/aquia-inc/frostfall/internal/keyboard"
	"github.com/aquia-inc/frostfall/internal/screenreader"
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
	// PageURL is the URL the scan ran against, for report locations.
	PageURL string
}

// Transcript is what a screen reader would announce at one point in a test.
// It is evidence for a reviewer, never a pass/fail result.
type Transcript struct {
	TestID  string
	Label   string
	PageURL string
	Lines   []string
}

// Run holds a full run's outcome.
type Run struct {
	Results     []Result
	Transcripts []Transcript
	Stale       []baseline.Violation // baselined entries that no longer occur
	TestsRun    int
	// ran records, per test id, which checks actually executed and which
	// rules they ran with switched off: a violation may only be treated as
	// fixed (a stale baseline entry, a closed issue) where its rule was
	// genuinely looked for.
	ran map[string]*coverage
}

// coverage counts how often each check ran on a test's page, and how often
// each rule was switched off when its check ran. A rule was looked for if
// its check ran more often than the rule was off - so one step toggling it
// off does not hide another step that checked it.
type coverage struct {
	runs map[string]int
	off  map[string]int
}

func newCoverage() *coverage {
	return &coverage{runs: map[string]int{}, off: map[string]int{}}
}

func (c *coverage) record(check string, rules map[string]bool) {
	c.runs[check]++
	for id, on := range rules {
		if !on && checkFor(id) == check {
			c.off[id]++
		}
	}
}

// The checks that produce findings. axe owns every rule id outside the
// frostfall/ namespace.
const (
	checkAxe         = "axe"
	checkKeyboard    = "keyboard"
	checkExpectFocus = "expectFocus"
)

func checkFor(ruleID string) string {
	switch {
	case ruleID == keyboard.RuleFocusExpectation:
		return checkExpectFocus
	case keyboard.IsRule(ruleID):
		return checkKeyboard
	}
	return checkAxe
}

// Covered reports whether this run looked for ruleID on test's page, so its
// absence means fixed rather than unchecked.
func (r *Run) Covered(testID, ruleID string) bool {
	c := r.ran[testID]
	return c != nil && c.runs[checkFor(ruleID)] > c.off[ruleID]
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
	Browser *browser.Browser
	Engine  engine.Engine
	Config  *config.Config
	// Auth seeds every test's context with the configured session; nil runs
	// signed out.
	Auth     *browser.Auth
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
	run := &Run{ran: map[string]*coverage{}}
	for _, t := range tests {
		results, transcripts, cov, err := r.runTest(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("test %q: %w", t.ID, err)
		}
		run.Transcripts = append(run.Transcripts, transcripts...)
		run.ran[t.ID] = cov
		run.Results = append(run.Results, results...)
		run.TestsRun++
	}
	r.classify(run)
	return run, nil
}

func (r *Runner) runTest(ctx context.Context, t config.Test) (results []Result, transcripts []Transcript, cov *coverage, err error) {
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
	session, err := r.Browser.NewAuthSession(r.Auth, url, d.Viewport, waitFor, d.SettleTime.Std(), d.Timeout.Std())
	if err != nil {
		return nil, nil, nil, err
	}
	defer session.Close()
	cov = newCoverage()

	scanInitial := t.Scan == nil || *t.Scan
	if scanInitial {
		res, err := r.scan(ctx, session, t, url, "initial", config.ScanStep{}, cov)
		if err != nil {
			return nil, nil, nil, err
		}
		results = append(results, res...)
	}
	if tr := t.Transcript; tr != nil && !tr.Off {
		lines, err := screenreader.Transcript(session, tr.Scope)
		if err != nil {
			return nil, nil, nil, err
		}
		transcripts = append(transcripts, Transcript{TestID: t.ID, Label: "initial", PageURL: url, Lines: lines})
	}
	// Last of the initial-state checks: the audit leaves focus where it
	// found it, but its Tab presses can fire focus handlers that change the
	// DOM, which the scan and transcript above must not see.
	if k := r.Config.KeyboardFor(t); k != nil {
		res, err := r.keyboard(ctx, session, t, url, "initial", *k, cov)
		if err != nil {
			return nil, nil, nil, err
		}
		results = append(results, res...)
	}

	for i, step := range t.Steps {
		label := "step-" + strconv.Itoa(i)
		if tr := step.Transcript; tr != nil {
			if tr.Label != "" {
				label = tr.Label
			}
			r.logf("test %s: transcript (%s)", t.ID, label)
			lines, err := screenreader.Transcript(session, tr.Scope)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("steps[%d]: %w", i, err)
			}
			transcripts = append(transcripts, Transcript{TestID: t.ID, Label: label, PageURL: url, Lines: lines})
			continue
		}
		if e := step.ExpectFocus; e != nil {
			if e.Label != "" {
				label = e.Label
			}
			res, err := r.expectFocus(session, t, url, label, *e, cov)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("steps[%d]: %w", i, err)
			}
			results = append(results, res...)
			continue
		}
		if k := step.Keyboard; k != nil {
			if k.Label != "" {
				label = k.Label
			}
			res, err := r.keyboard(ctx, session, t, url, label, *k, cov)
			if err != nil {
				return nil, nil, nil, err
			}
			results = append(results, res...)
			continue
		}
		if step.Scan != nil {
			if step.Scan.Label != "" {
				label = step.Scan.Label
			}
			res, err := r.scan(ctx, session, t, url, label, *step.Scan, cov)
			if err != nil {
				return nil, nil, nil, err
			}
			results = append(results, res...)
			continue
		}
		r.logf("test %s: step %d", t.ID, i)
		if err := session.Step(step, r.BaseURL, d.WaitFor, d.SettleTime.Std()); err != nil {
			return nil, nil, nil, fmt.Errorf("steps[%d]: %w", i, err)
		}
	}
	return results, transcripts, cov, nil
}

// mergeRules layers rule toggles (defaults, then test, then step) and keeps
// only the keyboard rules or only the engine's: axe rejects ids it does not
// know, so frostfall/ toggles must never reach it.
func mergeRules(kb bool, layers ...map[string]bool) map[string]bool {
	rules := map[string]bool{}
	for _, l := range layers {
		maps.Copy(rules, l)
	}
	maps.DeleteFunc(rules, func(id string, _ bool) bool { return keyboard.IsRule(id) != kb })
	return rules
}

func (r *Runner) keyboard(ctx context.Context, session *browser.Session, t config.Test, url, label string, k config.KeyboardStep, cov *coverage) ([]Result, error) {
	r.logf("test %s: keyboard audit (%s)", t.ID, label)
	rules := mergeRules(true, r.Config.Defaults.Rules, t.Rules, k.Rules)
	audit, err := keyboard.Audit(ctx, session, keyboard.Options{
		Scope:   k.Scope,
		MaxTabs: k.MaxTabs,
		Rules:   rules,
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("keyboard audit %q: %w (an audit presses Tab through every stop on the page, ~150ms each; raise defaults.timeout, or set it in a profile to raise it for CI alone)", label, err)
	}
	if err != nil {
		return nil, fmt.Errorf("keyboard audit %q: %w", label, err)
	}
	cov.record(checkKeyboard, withUnchecked(rules, audit.Unchecked))
	return r.results(session, t, url, label, audit.Violations), nil
}

// withUnchecked marks the rules an audit could not fully evaluate as off,
// for coverage: their absence is not a fix (a page regressing into a trap
// must not close its focus-not-visible issues).
func withUnchecked(rules map[string]bool, unchecked []string) map[string]bool {
	out := maps.Clone(rules)
	if out == nil {
		out = map[string]bool{}
	}
	for _, id := range unchecked {
		out[id] = false
	}
	return out
}

// expectFocus turns a failed focus assertion into a finding, so it gates and
// baselines like any violation. Its target is the assertion, not the element
// that wrongly has focus: that element varies with the bug, and the finding's
// identity must not.
func (r *Runner) expectFocus(session *browser.Session, t config.Test, url, label string, e config.ExpectFocus, cov *coverage) ([]Result, error) {
	if on, set := mergeRules(true, r.Config.Defaults.Rules, t.Rules)[keyboard.RuleFocusExpectation]; set && !on {
		return nil, nil
	}
	r.logf("test %s: expectFocus (%s)", t.ID, label)
	ok, actual, err := screenreader.CheckFocus(session, screenreader.Expectation{Within: e.Within, Role: e.Role, Name: e.Name})
	if err != nil {
		return nil, err
	}
	cov.record(checkExpectFocus, nil)
	if ok {
		return nil, nil
	}
	var want []string
	if e.Role != "" {
		want = append(want, "role "+e.Role)
	}
	if e.Name != "" {
		want = append(want, fmt.Sprintf("named %q", e.Name))
	}
	if e.Within != "" {
		want = append(want, "within "+e.Within)
	}
	target := e.Within
	if target == "" {
		target = "document"
	}
	v := keyboard.NewViolation(keyboard.RuleFocusExpectation, target, "",
		"Expected focus "+strings.Join(want, ", ")+"; "+actual)
	return r.results(session, t, url, label, []engine.Violation{v}), nil
}

func (r *Runner) scan(ctx context.Context, session *browser.Session, t config.Test, url, label string, s config.ScanStep, cov *coverage) ([]Result, error) {
	rules := mergeRules(false, r.Config.Defaults.Rules, t.Rules, s.Rules)

	r.logf("test %s: scanning (%s)", t.ID, label)
	violations, err := r.Engine.Audit(ctx, session, engine.ScanOptions{
		Standard: r.Config.Defaults.Standard,
		Scope:    s.Scope,
		Rules:    rules,
	})
	if err != nil {
		return nil, fmt.Errorf("scan %q: %w", label, err)
	}
	cov.record(checkAxe, rules)
	return r.results(session, t, url, label, violations), nil
}

// results fingerprints one scan point's violations and captures evidence.
func (r *Runner) results(session *browser.Session, t config.Test, url, label string, violations []engine.Violation) []Result {
	results := make([]Result, 0, len(violations))
	for _, v := range violations {
		res := Result{
			Violation:    v,
			TestID:       t.ID,
			ScanLabel:    label,
			PageURL:      url,
			Fingerprint:  fingerprint.Fingerprint(t.ID, label, v.RuleID, v.Target),
			StableTarget: fingerprint.StableTarget(v.Target),
		}
		// Screenshot new violations only: baselined ones are known debt, and
		// skipping them keeps CI artifact size proportional to what changed.
		// A focus expectation names no element (its target is the
		// assertion), so there is nothing to photograph.
		if r.ScreenshotDir != "" && v.RuleID != keyboard.RuleFocusExpectation {
			if _, known := r.baselineIdx[res.Fingerprint]; !known {
				res.ScreenshotPath = r.captureScreenshot(session, res)
			}
		}
		results = append(results, res)
	}
	return results
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
		if !seen[entry.Fingerprint] && run.Covered(entry.TestID, entry.RuleID) {
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
