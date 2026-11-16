package format

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aquia-inc/frostfall/internal/runner"
	"github.com/aquia-inc/frostfall/internal/secrets"
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
	// Enforcing is whether the run's contract can fail the build at all: some
	// expect block (the default or a test's) sets a severity, rules or a
	// maxViolations. False is report-only.
	Enforcing bool
	// ExitStatus is the exit code the process will return for this run. It is
	// the pass/fail signal: 0 passed, 1 the gate failed on violations, 2 a
	// config error, 3 an environment error.
	ExitStatus int
	// GateFailed is true when the run exits 1 because the violations that
	// count toward the gate outnumber what maxViolations tolerates. junit
	// fails a testcase only when this is set.
	GateFailed bool
	// Counts is the predicate the exit code counts with: a new violation at or
	// above the enforced severity, or on an enforced rule, under an enforcing
	// contract. It is not the informational "flagged" marker, which also
	// covers report-only runs. Nil means nothing counts.
	Counts func(runner.Result) bool
	// Limit is the maxViolations the gate tolerated (0 when the contract sets
	// none). Only the run-level junit failure reports it.
	Limit int
	// Redact is set when the run has auth configured, so anything the report
	// repeats from outside this run's own scan (a baseline's stale entries) is
	// scrubbed the way the scan's own selectors are.
	Redact bool
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

// split sorts the results at p by what they mean for the gate: failing are
// the ones the run exits 1 for (only when the gate failed), info are new
// violations that do not fail the run (report-only, under the enforced
// severity, or tolerated by maxViolations), and baselined is the count of
// accepted ones.
func (p *scanPoint) split(meta ReportMeta) (failing, info []runner.Result, baselined int) {
	for _, r := range p.results {
		switch {
		case r.Baselined:
			baselined++
		case meta.GateFailed && meta.Counts != nil && meta.Counts(r):
			failing = append(failing, r)
		default:
			info = append(info, r)
		}
	}
	return failing, info, baselined
}

// tokenRe is one whitespace-separated token of a run error. Replacing tokens
// leaves the whitespace between them exactly as it was.
var tokenRe = regexp.MustCompile(`\S+`)

// urlInText is a URL inside one token. It may contain brackets (an IPv6 host,
// a path like /a[0]); trimURL takes trailing punctuation back off.
var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// failureText is the failure message as a report may show it. A run error is
// free text from the browser, the auth flow or the server probe, and it can
// quote a URL (with or without a scheme) or a selector. With auth configured
// the report's copy is cleaned fail-closed, one token at a time: see
// redactRunError. stderr keeps the original.
func (m ReportMeta) failureText() string {
	msg := m.Failure.Message
	if !m.Redact {
		return msg
	}
	return redactRunError(msg)
}

// redactRunError cleans a run error in two passes over its tokens. The first
// fails closed without trying to recognize anything: every token is cut at its
// first '?' or at its first '#' after the start (a query or fragment, which is
// where tokens and codes travel; see queryCut), and a token that still holds
// '=' or '@' (an attribute value, a key=value pair, userinfo) or whose
// brackets do not nest is replaced whole by [redacted]. Only then are
// attribute values scrubbed from what looks like a selector and URLs reduced
// to where they point. That order means the later, pattern-based pass only
// ever sees what the first one let through, so a URL or selector it fails to
// parse cannot carry a secret past it. Text without those characters comes
// through unchanged.
func redactRunError(msg string) string {
	return scrubTokens(failClosedTokens(msg))
}

// failClosedTokens is the first pass of redactRunError.
func failClosedTokens(msg string) string {
	return tokenRe.ReplaceAllStringFunc(msg, func(tok string) string {
		tok = tok[:queryCut(tok)]
		if strings.ContainsAny(tok, "=@") || !bracketsNest(tok) {
			return "[redacted]"
		}
		return tok
	})
}

// queryCut is where failClosedTokens cuts a token: at the first '?', or at
// the first '#' after the token's first byte. A token that starts with '#' is
// a CSS id selector (#login), not a fragment, so that '#' is kept; the token
// still goes through the '=', '@' and bracket rules, so #access_token=abc is
// redacted whole. A URL's fragment always follows something, so it is cut.
func queryCut(tok string) int {
	cut := len(tok)
	if i := strings.IndexByte(tok, '?'); i >= 0 {
		cut = i
	}
	if len(tok) > 1 {
		if i := strings.IndexByte(tok[1:], '#'); i >= 0 && i+1 < cut {
			cut = i + 1
		}
	}
	return cut
}

// scrubTokens is the second pass of redactRunError: the selector scrub and URL
// redaction, applied per token so neither can reach across a token boundary.
func scrubTokens(msg string) string {
	return tokenRe.ReplaceAllStringFunc(msg, func(tok string) string {
		tok = secrets.ScrubSelector(tok)
		return urlInText.ReplaceAllStringFunc(tok, func(u string) string {
			trimmed := trimURL(u)
			return secrets.RedactURL(trimmed) + u[len(trimmed):]
		})
	})
}

// bracketsNest reports whether every '[' in s is closed by a later ']' and no
// ']' comes before its '['. Equal counts are not enough: "]x[" would still
// hand the selector scrub an unterminated bracket.
func bracketsNest(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// trimURL takes sentence punctuation off the end of a URL found in text, and
// a closing ')' or ']' that has no opening partner inside it.
func trimURL(u string) string {
	for u != "" {
		switch c := u[len(u)-1]; {
		case strings.IndexByte(".,;:!?", c) >= 0:
		case c == ')' && strings.Count(u, "(") < strings.Count(u, ")"):
		case c == ']' && strings.Count(u, "[") < strings.Count(u, "]"):
		default:
			return u
		}
		u = u[:len(u)-1]
	}
	return u
}
