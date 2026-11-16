package format

import (
	"encoding/json"
	"io"
	"time"

	"github.com/aquia-inc/frostfall/internal/baseline"
	"github.com/aquia-inc/frostfall/internal/runner"
	"github.com/aquia-inc/frostfall/internal/secrets"
)

// JSONSchemaVersion is the version of the json format's schema. It changes
// only when an existing field is renamed, removed or changes meaning; new
// fields are additive and do not bump it.
const JSONSchemaVersion = 1

// The json format is the complete structured result of a run, and what
// integration tests should assert against. Keys are camelCase, like the
// baseline file and the SARIF property bags. Arrays are never null.
//
//	schemaVersion      int, JSONSchemaVersion
//	run                run metadata and totals (jsonRun). Scripts gate on
//	                   run.exitStatus (0 passed, 1 violations, 2 config error,
//	                   3 environment error), the same code the process exits
//	                   with. run.enforcing says whether the contract can fail
//	                   the build at all; false is report-only. run.error is
//	                   set when the run stopped before it could scan.
//	tests[]            one per test, in run order
//	  id               the test id (tests have no separate name)
//	  checks[]         checks that ran: axe, keyboard, expectFocus
//	  uncheckedRules[] rules switched off, or not fully evaluated, every time
//	                   their check ran: their absence is not a pass
//	  scanPoints[]     one per scan point, in the order the checks ran
//	    label, pageUrl
//	    violations[]   worst impact first, then rule, target, fingerprint
//	    transcript[]   screen reader lines, when the point has one
//	stale[]            baseline entries that no longer occur (targets are
//	                   scrubbed when auth is configured)
//
// Everything that names a page or an element is the value the other formats
// print: the page URL is redacted and the target scrubbed when auth is
// configured. This formatter never sees a raw URL.

type jsonReport struct {
	SchemaVersion int         `json:"schemaVersion"`
	Run           jsonRun     `json:"run"`
	Tests         []jsonTest  `json:"tests"`
	Stale         []jsonStale `json:"stale"`
}

type jsonRun struct {
	ToolVersion string       `json:"toolVersion"`
	AxeVersion  string       `json:"axeVersion"`
	BaseURL     string       `json:"baseUrl"`
	Standard    string       `json:"standard"`
	Profile     string       `json:"profile,omitempty"`
	Mode        string       `json:"mode"`
	Enforcing   bool         `json:"enforcing"`
	StartedAt   string       `json:"startedAt"`
	DurationMs  int64        `json:"durationMs"`
	ExitStatus  int          `json:"exitStatus"`
	Error       *jsonFailure `json:"error,omitempty"`
	TestsRun    int          `json:"testsRun"`
	Totals      jsonTotals   `json:"totals"`
}

type jsonFailure struct {
	TestID  string `json:"testId,omitempty"`
	Message string `json:"message"`
}

type jsonTotals struct {
	Violations int `json:"violations"`
	New        int `json:"new"`
	Flagged    int `json:"flagged"`
	Baselined  int `json:"baselined"`
	Stale      int `json:"stale"`
}

type jsonTest struct {
	ID             string          `json:"id"`
	Checks         []string        `json:"checks"`
	UncheckedRules []string        `json:"uncheckedRules"`
	ScanPoints     []jsonScanPoint `json:"scanPoints"`
}

type jsonScanPoint struct {
	Label      string          `json:"label"`
	PageURL    string          `json:"pageUrl"`
	Violations []jsonViolation `json:"violations"`
	Transcript []string        `json:"transcript,omitempty"`
}

type jsonViolation struct {
	RuleID      string   `json:"ruleId"`
	Impact      string   `json:"impact"`
	Tags        []string `json:"tags"`
	Target      string   `json:"target"`
	Summary     string   `json:"summary"`
	Help        string   `json:"help,omitempty"`
	HelpURL     string   `json:"helpUrl,omitempty"`
	Fingerprint string   `json:"fingerprint"`
	Baselined   bool     `json:"baselined"`
	// Flagged is the informational marker the text, html and sarif formats
	// share: severity floor plus enforced rules, never for a baselined
	// violation. A report-only run still marks its serious or worse
	// violations, and a run can exit 0 with flagged violations (report-only,
	// or within maxViolations). It is not the pass/fail signal: that is
	// run.exitStatus.
	Flagged        bool   `json:"flagged"`
	ScreenshotPath string `json:"screenshotPath,omitempty"`
}

type jsonStale struct {
	Fingerprint  string `json:"fingerprint"`
	TestID       string `json:"testId"`
	ScanLabel    string `json:"scanLabel"`
	RuleID       string `json:"ruleId"`
	StableTarget string `json:"stableTarget"`
}

// JSON writes the run as the structured json format. flagged is the shared
// informational marker; the pass/fail signal is meta.ExitStatus.
func JSON(w io.Writer, run *runner.Run, meta ReportMeta, flagged func(runner.Result) bool) error {
	rep := jsonReport{
		SchemaVersion: JSONSchemaVersion,
		Run: jsonRun{
			ToolVersion: meta.ToolVersion,
			AxeVersion:  meta.AxeVersion,
			BaseURL:     meta.BaseURL,
			Standard:    meta.Standard,
			Profile:     meta.Profile,
			Mode:        meta.Mode,
			Enforcing:   meta.Enforcing,
			StartedAt:   meta.Started.UTC().Format(time.RFC3339),
			DurationMs:  meta.Duration.Milliseconds(),
			ExitStatus:  meta.ExitStatus,
			TestsRun:    run.TestsRun,
		},
		Tests: []jsonTest{},
		Stale: []jsonStale{},
	}
	if meta.Failure != nil {
		rep.Run.Error = &jsonFailure{TestID: meta.Failure.TestID, Message: meta.failureText()}
	}
	tot := &rep.Run.Totals

	for _, g := range groupRun(run) {
		jt := jsonTest{ID: g.id, Checks: g.checks, UncheckedRules: g.unchecked, ScanPoints: []jsonScanPoint{}}
		for _, p := range g.points {
			jp := jsonScanPoint{Label: p.label, PageURL: p.pageURL, Violations: []jsonViolation{}, Transcript: p.transcript}
			for _, res := range p.results {
				f := flagged(res) && !res.Baselined
				tags := res.Tags
				if tags == nil {
					tags = []string{}
				}
				jp.Violations = append(jp.Violations, jsonViolation{
					RuleID:         res.RuleID,
					Impact:         res.Impact.String(),
					Tags:           tags,
					Target:         res.StableTarget,
					Summary:        res.Summary,
					Help:           res.Help,
					HelpURL:        res.HelpURL,
					Fingerprint:    res.Fingerprint,
					Baselined:      res.Baselined,
					Flagged:        f,
					ScreenshotPath: res.ScreenshotPath,
				})
				tot.Violations++
				switch {
				case res.Baselined:
					tot.Baselined++
				default:
					tot.New++
				}
				if f {
					tot.Flagged++
				}
			}
			jt.ScanPoints = append(jt.ScanPoints, jp)
		}
		rep.Tests = append(rep.Tests, jt)
	}
	for _, s := range run.Stale {
		rep.Stale = append(rep.Stale, staleEntry(s, meta.Redact))
	}
	tot.Stale = len(rep.Stale)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// staleEntry is one baseline entry that no longer occurs. A baseline is read
// from disk, not produced by this run's scan, so its target may predate auth
// or have been written by hand: with auth configured it is scrubbed like the
// selectors the scan reports.
func staleEntry(s baseline.Violation, redact bool) jsonStale {
	target := s.StableTarget
	if redact {
		target = secrets.ScrubSelector(target)
	}
	return jsonStale{
		Fingerprint:  s.Fingerprint,
		TestID:       s.TestID,
		ScanLabel:    s.ScanLabel,
		RuleID:       s.RuleID,
		StableTarget: target,
	}
}
