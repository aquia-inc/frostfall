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
// point. A testcase fails when its scan point has a violation that is not
// baselined, so a report-only run can show red while it exits 0; the failure
// body marks the violations the exit code would count as flagged. Baselined
// violations never fail a testcase. An environment error that stops the run is
// an <error>. Screen reader transcripts are evidence, not results: they go in
// system-out. All text goes through encoding/xml, so selectors with <, & or
// quotes are escaped.

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

// JUnit writes the run as JUnit XML. flagged is the shared enforcement
// predicate.
func JUnit(w io.Writer, run *runner.Run, meta ReportMeta, flagged func(runner.Result) bool) error {
	out := junitSuites{
		Name: "frostfall",
		Time: strconv.FormatFloat(meta.Duration.Seconds(), 'f', 3, 64),
	}
	stamp := meta.Started.UTC().Format("2006-01-02T15:04:05")

	for _, g := range groupRun(run) {
		s := junitSuite{Name: g.id, Timestamp: stamp}
		for _, p := range g.points {
			c := junitCase{Name: p.label, Classname: g.id}
			fresh := p.newViolations()
			if len(fresh) > 0 {
				nFlagged := 0
				var body strings.Builder
				for _, res := range fresh {
					mark := ""
					if flagged(res) {
						nFlagged++
						mark = ", flagged"
					}
					fmt.Fprintf(&body, "%s (%s%s)\n  target: %s\n  summary: %s\n", res.RuleID, res.Impact, mark, res.StableTarget, res.Summary)
				}
				msg := fmt.Sprintf("%d new violation(s)", len(fresh))
				if nFlagged > 0 {
					msg += fmt.Sprintf(", %d flagged", nFlagged)
				}
				c.Failure = &junitProblem{Message: msg, Type: "accessibility", Body: body.String()}
				s.Failures++
			}
			c.SystemOut = junitOutput(p)
			s.Tests++
			s.Cases = append(s.Cases, c)
		}
		out.Suites = append(out.Suites, s)
	}

	if f := meta.Failure; f != nil {
		// The run stopped at this test, so nothing after it ran and what ran
		// before it was not kept. The error stands in for the scan points the
		// test never reached.
		var s *junitSuite
		for i := range out.Suites {
			if out.Suites[i].Name == f.TestID {
				s = &out.Suites[i]
			}
		}
		if s == nil {
			out.Suites = append(out.Suites, junitSuite{Name: f.TestID, Timestamp: stamp})
			s = &out.Suites[len(out.Suites)-1]
		}
		s.Cases = append(s.Cases, junitCase{
			Name:      "run",
			Classname: f.TestID,
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

// junitOutput is a scan point's system-out: where it ran, what is baselined,
// and any screen reader transcript.
func junitOutput(p *scanPoint) string {
	var b strings.Builder
	if p.pageURL != "" {
		fmt.Fprintf(&b, "page: %s\n", p.pageURL)
	}
	if n := len(p.results) - len(p.newViolations()); n > 0 {
		fmt.Fprintf(&b, "%d baselined violation(s)\n", n)
	}
	if p.hasScript {
		b.WriteString("screen reader transcript:\n")
		for _, line := range p.transcript {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
}
