package format

import (
	"sort"
	"time"

	"github.com/aquia-inc/frostfall/internal/runner"
)

// ReportMeta is the run context the json and junit formats carry. The time
// fields come in from the caller so the output is deterministic under test.
type ReportMeta struct {
	ToolVersion string
	AxeVersion  string
	// BaseURL is the base URL as a report may show it: already redacted when
	// the run has auth. Never pass the raw one.
	BaseURL  string
	Standard string
	Profile  string
	Mode     string
	Started  time.Time
	Duration time.Duration
	// ExitStatus is the exit code the process will return for this run.
	ExitStatus int
	// Failure is set when a test's environment error stopped the run. The run
	// then has no results, only this.
	Failure *RunFailure
}

// RunFailure names the test that stopped a run and the message the run
// printed for it.
type RunFailure struct {
	TestID  string
	Message string
}

// scanPoint is one test x scan point with everything found there.
type scanPoint struct {
	label      string
	pageURL    string
	results    []runner.Result // violations, baselined included, sorted
	transcript []string
	hasScript  bool
}

type testGroup struct {
	id        string
	points    []*scanPoint
	checks    []string
	unchecked []string
}

// groupRun is the one place a run is arranged into tests and scan points, so
// json and junit can never disagree about it. Tests and points keep the order
// they ran in; a point that only has a transcript, or a violation from a run
// that recorded no points, is added after the ones that did. Violations are
// sorted by impact (worst first), rule, target, fingerprint.
func groupRun(run *runner.Run) []*testGroup {
	var tests []*testGroup
	byTest := map[string]*testGroup{}
	type key struct{ test, label string }
	byPoint := map[key]*scanPoint{}

	point := func(testID, label, url string) *scanPoint {
		g := byTest[testID]
		if g == nil {
			g = &testGroup{id: testID, checks: run.Checks(testID), unchecked: run.Uncovered(testID)}
			byTest[testID] = g
			tests = append(tests, g)
		}
		k := key{testID, label}
		p := byPoint[k]
		if p == nil {
			p = &scanPoint{label: label, pageURL: url}
			byPoint[k] = p
			g.points = append(g.points, p)
		}
		if p.pageURL == "" {
			p.pageURL = url
		}
		return p
	}

	for _, t := range run.Tests {
		point(t.ID, "", "") // a test with no scan points still appears
		for _, sp := range t.Points {
			point(t.ID, sp.Label, sp.PageURL)
		}
	}
	for _, res := range run.Results {
		p := point(res.TestID, res.ScanLabel, res.PageURL)
		p.results = append(p.results, res)
	}
	for _, tr := range run.Transcripts {
		p := point(tr.TestID, tr.Label, tr.PageURL)
		p.transcript, p.hasScript = tr.Lines, true
	}
	// The empty-label placeholder only exists to register a test.
	for _, g := range tests {
		kept := g.points[:0]
		for _, p := range g.points {
			if p.label != "" || len(p.results) > 0 || p.hasScript {
				kept = append(kept, p)
			}
		}
		g.points = kept
	}
	for _, g := range tests {
		for _, p := range g.points {
			sort.SliceStable(p.results, func(i, j int) bool {
				a, b := p.results[i], p.results[j]
				switch {
				case a.Impact != b.Impact:
					return a.Impact > b.Impact
				case a.RuleID != b.RuleID:
					return a.RuleID < b.RuleID
				case a.StableTarget != b.StableTarget:
					return a.StableTarget < b.StableTarget
				}
				return a.Fingerprint < b.Fingerprint
			})
		}
	}
	return tests
}

// newViolations counts the results at p that are not baselined.
func (p *scanPoint) newViolations() []runner.Result {
	var out []runner.Result
	for _, r := range p.results {
		if !r.Baselined {
			out = append(out, r)
		}
	}
	return out
}
