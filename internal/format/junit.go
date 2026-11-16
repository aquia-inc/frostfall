package format

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aquia-inc/frostfall/internal/runner"
)

// JUnit XML for CI dashboards: one testsuite per test, one testcase per scan
// point. The file contains a <failure> if and only if the run exits 1 because
// of violations, so a dashboard is red exactly when the gate is. A testcase
// fails only for violations the gate counts (enforced severity or rule, not
// baselined, under an enforcing contract), and only when the run's counted
// total is over maxViolations; once the run is over, every testcase holding a
// counted violation fails. Everything else (report-only runs, violations under
// the enforced severity, a tolerated total, baselined ones) goes in system-out
// as information. An environment error that stops the run is an <error>.
// Screen reader transcripts are evidence, not results: they go in system-out.
// All text goes through encoding/xml, so selectors with <, & or quotes are
// escaped.

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Errors    int         `xml:"errors,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Timestamp string      `xml:"timestamp,attr,omitempty"`
	Cases     []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitProblem `xml:"failure,omitempty"`
	Error     *junitProblem `xml:"error,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

// JUnit writes the run as JUnit XML. meta.GateFailed and meta.Counts decide
// which testcases fail; they come from the same logic as the exit code.
func JUnit(w io.Writer, run *runner.Run, meta ReportMeta) error {
	out := junitSuites{
		Name: "frostfall",
		Time: strconv.FormatFloat(meta.Duration.Seconds(), 'f', 3, 64),
	}
	stamp := meta.Started.UTC().Format("2006-01-02T15:04:05")

	for _, g := range groupRun(run) {
		s := junitSuite{Name: g.id, Timestamp: stamp}
		for _, p := range g.points {
			c := junitCase{Name: p.label, Classname: g.id}
			failing, info, baselined := p.split(meta)
			if len(failing) > 0 {
				var body strings.Builder
				for _, res := range failing {
					fmt.Fprintf(&body, "%s (%s)\n  target: %s\n  summary: %s\n", res.RuleID, res.Impact, res.StableTarget, res.Summary)
				}
				c.Failure = &junitProblem{
					Message: fmt.Sprintf("%d violation(s) counted against the gate", len(failing)),
					Type:    "accessibility",
					Body:    body.String(),
				}
				s.Failures++
			}
			c.SystemOut = junitOutput(p, info, baselined)
			s.Tests++
			s.Cases = append(s.Cases, c)
		}
		out.Suites = append(out.Suites, s)
	}

	if f := meta.Failure; f != nil {
		// The run stopped at this test, so nothing after it ran and what ran
		// before it was not kept. The error stands in for the scan points the
		// test never reached.
		// A failure before any test ran (browser, auth, config) names no test:
		// it goes in a suite called "run", never a suite with an empty name.
		id := f.TestID
		if id == "" {
			id = "run"
		}
		var s *junitSuite
		for i := range out.Suites {
			if out.Suites[i].Name == id {
				s = &out.Suites[i]
			}
		}
		if s == nil {
			out.Suites = append(out.Suites, junitSuite{Name: id, Timestamp: stamp})
			s = &out.Suites[len(out.Suites)-1]
		}
		s.Cases = append(s.Cases, junitCase{
			Name:      "run",
			Classname: id,
			Error:     &junitProblem{Message: f.Message, Type: "environment", Body: f.Message},
		})
		s.Tests++
		s.Errors++
	}

	for _, s := range out.Suites {
		out.Tests += s.Tests
		out.Failures += s.Failures
		out.Errors += s.Errors
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// junitOutput is a scan point's system-out: where it ran, the new violations
// that did not fail the run, how many are baselined, and any screen reader
// transcript.
func junitOutput(p *scanPoint, info []runner.Result, baselined int) string {
	var b strings.Builder
	if p.pageURL != "" {
		fmt.Fprintf(&b, "page: %s\n", p.pageURL)
	}
	if len(info) > 0 {
		fmt.Fprintf(&b, "%d new violation(s) not failing the run:\n", len(info))
		for _, res := range info {
			fmt.Fprintf(&b, "  %s (%s)\n    target: %s\n    summary: %s\n", res.RuleID, res.Impact, res.StableTarget, res.Summary)
		}
	}
	if baselined > 0 {
		fmt.Fprintf(&b, "%d baselined violation(s)\n", baselined)
	}
	if p.hasScript {
		b.WriteString("screen reader transcript:\n")
		for _, line := range p.transcript {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
}
