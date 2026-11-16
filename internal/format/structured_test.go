package format

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aquia-inc/frostfall/internal/baseline"
	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/runner"
)

func structuredMeta() ReportMeta {
	return ReportMeta{
		ToolVersion: "1.2.3",
		AxeVersion:  "4.10.3",
		BaseURL:     "http://localhost:5173",
		Standard:    "wcag21aa",
		Mode:        "enforcing (serious+)",
		Started:     time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC),
		Duration:    2500 * time.Millisecond,
		Enforcing:   true,
		ExitStatus:  1,
		GateFailed:  true,
		Counts:      structuredFlagged,
	}
}

// structuredRun has a flagged critical, a baselined violation, a minor one,
// a clean scan point, and a transcript.
func structuredRun() *runner.Run {
	return &runner.Run{
		TestsRun: 2,
		Tests: []runner.TestRecord{
			{ID: "home", Points: []runner.ScanPoint{
				{Label: "initial", PageURL: "http://localhost:5173/"},
				{Label: "modal", PageURL: "http://localhost:5173/"},
			}},
			{ID: "about", Points: []runner.ScanPoint{{Label: "initial", PageURL: "http://localhost:5173/about"}}},
		},
		Results: []runner.Result{
			{
				Violation:    engine.Violation{RuleID: "color-contrast", Impact: engine.Minor, Summary: "Contrast too low", HelpURL: "https://example.com/contrast", Tags: []string{"wcag143"}},
				TestID:       "home",
				ScanLabel:    "initial",
				Fingerprint:  "cccc",
				StableTarget: ".chip",
				PageURL:      "http://localhost:5173/",
			},
			{
				Violation:    engine.Violation{RuleID: "image-alt", Impact: engine.Critical, Summary: "Images need alt text", Help: "Images must have alternative text", Tags: []string{"wcag111", "wcag2a"}},
				TestID:       "home",
				ScanLabel:    "initial",
				Fingerprint:  "aaaa",
				StableTarget: "#hero > img",
				PageURL:      "http://localhost:5173/",
			},
			{
				Violation:    engine.Violation{RuleID: "image-alt", Impact: engine.Critical, Summary: "Images need alt text"},
				TestID:       "about",
				ScanLabel:    "initial",
				Fingerprint:  "bbbb",
				StableTarget: "#footer > img",
				PageURL:      "http://localhost:5173/about",
				Baselined:    true,
			},
		},
		Transcripts: []runner.Transcript{
			{TestID: "home", Label: "modal", PageURL: "http://localhost:5173/", Lines: []string{"dialog, Settings", "button, Close"}},
		},
		Stale: []baseline.Violation{{Fingerprint: "dead", TestID: "home", ScanLabel: "initial", RuleID: "label", StableTarget: "#gone"}},
	}
}

func structuredFlagged(r runner.Result) bool {
	return !r.Baselined && (r.Impact >= engine.Serious || r.RuleID == "never-matches")
}

// The shape tests below decode into their own structs, not the formatter's:
// the schema is a contract and the test is the second reading of it.
type jsonDoc struct {
	SchemaVersion int `json:"schemaVersion"`
	Run           struct {
		ToolVersion string `json:"toolVersion"`
		AxeVersion  string `json:"axeVersion"`
		BaseURL     string `json:"baseUrl"`
		Standard    string `json:"standard"`
		Mode        string `json:"mode"`
		Enforcing   bool   `json:"enforcing"`
		StartedAt   string `json:"startedAt"`
		DurationMs  int64  `json:"durationMs"`
		ExitStatus  int    `json:"exitStatus"`
		TestsRun    int    `json:"testsRun"`
		Error       *struct {
			TestID  string `json:"testId"`
			Message string `json:"message"`
		} `json:"error"`
		Totals struct {
			Violations, New, Flagged, Baselined, Stale int
		} `json:"totals"`
	} `json:"run"`
	Tests []struct {
		ID             string   `json:"id"`
		Checks         []string `json:"checks"`
		UncheckedRules []string `json:"uncheckedRules"`
		ScanPoints     []struct {
			Label      string   `json:"label"`
			PageURL    string   `json:"pageUrl"`
			Transcript []string `json:"transcript"`
			Violations []struct {
				RuleID      string   `json:"ruleId"`
				Impact      string   `json:"impact"`
				Tags        []string `json:"tags"`
				Target      string   `json:"target"`
				Summary     string   `json:"summary"`
				Help        string   `json:"help"`
				HelpURL     string   `json:"helpUrl"`
				Fingerprint string   `json:"fingerprint"`
				Baselined   bool     `json:"baselined"`
				Flagged     bool     `json:"flagged"`
			} `json:"violations"`
		} `json:"scanPoints"`
	} `json:"tests"`
	Stale []struct {
		Fingerprint string `json:"fingerprint"`
		RuleID      string `json:"ruleId"`
	} `json:"stale"`
}

func TestJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, structuredRun(), structuredMeta(), structuredFlagged); err != nil {
		t.Fatal(err)
	}
	var doc jsonDoc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", doc.SchemaVersion)
	}
	r := doc.Run
	if r.ToolVersion != "1.2.3" || r.AxeVersion != "4.10.3" || r.BaseURL != "http://localhost:5173" ||
		r.Standard != "wcag21aa" || r.Mode != "enforcing (serious+)" || !r.Enforcing || r.ExitStatus != 1 || r.TestsRun != 2 {
		t.Errorf("run metadata wrong: %+v", r)
	}
	if r.StartedAt != "2026-11-16T09:30:00Z" || r.DurationMs != 2500 {
		t.Errorf("start/duration = %s / %d", r.StartedAt, r.DurationMs)
	}
	if r.Error != nil {
		t.Errorf("a finished run has no error: %+v", r.Error)
	}
	want := struct{ Violations, New, Flagged, Baselined, Stale int }{3, 2, 1, 1, 1}
	if r.Totals != want {
		t.Errorf("totals = %+v, want %+v", r.Totals, want)
	}

	if len(doc.Tests) != 2 || doc.Tests[0].ID != "home" || doc.Tests[1].ID != "about" {
		t.Fatalf("tests not in run order: %+v", doc.Tests)
	}
	home := doc.Tests[0]
	if len(home.ScanPoints) != 2 || home.ScanPoints[0].Label != "initial" || home.ScanPoints[1].Label != "modal" {
		t.Fatalf("scan points = %+v", home.ScanPoints)
	}
	// Worst impact first, whatever order the engine reported in.
	vs := home.ScanPoints[0].Violations
	if len(vs) != 2 || vs[0].RuleID != "image-alt" || vs[1].RuleID != "color-contrast" {
		t.Fatalf("violations not impact-sorted: %+v", vs)
	}
	v := vs[0]
	if v.Impact != "critical" || v.Target != "#hero > img" || v.Fingerprint != "aaaa" || !v.Flagged || v.Baselined ||
		v.Help == "" || strings.Join(v.Tags, ",") != "wcag111,wcag2a" || v.Summary == "" {
		t.Errorf("violation fields wrong: %+v", v)
	}
	if vs[1].Flagged {
		t.Error("a minor violation below the floor must not be flagged")
	}
	// A clean scan point is present, with an empty (not null) list, and the
	// transcript rides on the point it belongs to.
	modal := home.ScanPoints[1]
	if modal.Violations == nil || len(modal.Violations) != 0 {
		t.Errorf("clean point violations = %#v, want empty list", modal.Violations)
	}
	if len(modal.Transcript) != 2 {
		t.Errorf("transcript = %v", modal.Transcript)
	}
	about := doc.Tests[1].ScanPoints[0].Violations
	if len(about) != 1 || !about[0].Baselined || about[0].Flagged {
		t.Errorf("baselined violation wrong: %+v", about)
	}
	if len(doc.Stale) != 1 || doc.Stale[0].Fingerprint != "dead" {
		t.Errorf("stale = %+v", doc.Stale)
	}
}

// Tests with no recorded checks and runs with nothing in them still encode
// every list as [] so a consumer never has to nil-check.
func TestJSONEmptyListsAreNotNull(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, &runner.Run{}, ReportMeta{}, func(runner.Result) bool { return false }); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tests", "stale"} {
		if string(raw[k]) != "[]" {
			t.Errorf("%s = %s, want []", k, raw[k])
		}
	}
	buf.Reset()
	run := &runner.Run{Tests: []runner.TestRecord{{ID: "t", Points: []runner.ScanPoint{{Label: "initial"}}}}}
	if err := JSON(&buf, run, ReportMeta{}, func(runner.Result) bool { return false }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"checks": []`, `"uncheckedRules": []`, `"violations": []`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %s:\n%s", want, buf.String())
		}
	}
}

// The same results in a different arrival order produce the same bytes.
func TestJSONViolationOrderIsDeterministic(t *testing.T) {
	a := structuredRun()
	b := structuredRun()
	b.Results[0], b.Results[1] = b.Results[1], b.Results[0]
	var x, y bytes.Buffer
	if err := JSON(&x, a, structuredMeta(), structuredFlagged); err != nil {
		t.Fatal(err)
	}
	if err := JSON(&y, b, structuredMeta(), structuredFlagged); err != nil {
		t.Fatal(err)
	}
	if x.String() != y.String() {
		t.Errorf("order of arrival changed the output:\n%s\n---\n%s", x.String(), y.String())
	}
}

func TestJSONFailureCarriesTheError(t *testing.T) {
	meta := structuredMeta()
	meta.ExitStatus = 3
	meta.Failure = &RunFailure{TestID: "home", Message: `test "home": landed on the login page`}
	var buf bytes.Buffer
	if err := JSON(&buf, &runner.Run{}, meta, func(runner.Result) bool { return false }); err != nil {
		t.Fatal(err)
	}
	var doc jsonDoc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Run.ExitStatus != 3 || doc.Run.Error == nil || doc.Run.Error.TestID != "home" || !strings.Contains(doc.Run.Error.Message, "login page") {
		t.Errorf("failure not reported: %+v", doc.Run)
	}
}

type junitDoc struct {
	XMLName  xml.Name `xml:"testsuites"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	Errors   int      `xml:"errors,attr"`
	Time     string   `xml:"time,attr"`
	Suites   []struct {
		Name     string `xml:"name,attr"`
		Tests    int    `xml:"tests,attr"`
		Failures int    `xml:"failures,attr"`
		Errors   int    `xml:"errors,attr"`
		Cases    []struct {
			Name      string `xml:"name,attr"`
			Classname string `xml:"classname,attr"`
			Failure   *struct {
				Message string `xml:"message,attr"`
				Body    string `xml:",chardata"`
			} `xml:"failure"`
			Error *struct {
				Message string `xml:"message,attr"`
			} `xml:"error"`
			SystemOut string `xml:"system-out"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

// wellFormed parses every token, so malformed XML fails even where the
// struct decode would shrug.
func wellFormed(t *testing.T, data []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("XML is not well-formed: %v\n%s", err, data)
		}
	}
}

func TestJUnitStructure(t *testing.T) {
	var buf bytes.Buffer
	if err := JUnit(&buf, structuredRun(), structuredMeta()); err != nil {
		t.Fatal(err)
	}
	wellFormed(t, buf.Bytes())
	if !strings.HasPrefix(buf.String(), "<?xml version=\"1.0\" encoding=\"UTF-8\"?>") {
		t.Errorf("missing XML declaration:\n%s", buf.String())
	}
	var doc junitDoc
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	// 3 scan points: home/initial (fails), home/modal (clean), about/initial
	// (only a baselined violation, so it passes).
	if doc.Tests != 3 || doc.Failures != 1 || doc.Errors != 0 || doc.Time != "2.500" {
		t.Errorf("totals: tests=%d failures=%d errors=%d time=%s", doc.Tests, doc.Failures, doc.Errors, doc.Time)
	}
	if len(doc.Suites) != 2 || doc.Suites[0].Name != "home" || doc.Suites[1].Name != "about" {
		t.Fatalf("suites = %+v", doc.Suites)
	}
	home := doc.Suites[0]
	if home.Tests != 2 || home.Failures != 1 || len(home.Cases) != 2 {
		t.Fatalf("home suite = %+v", home)
	}
	c := home.Cases[0]
	if c.Name != "initial" || c.Classname != "home" || c.Failure == nil {
		t.Fatalf("home/initial should fail: %+v", c)
	}
	if c.Failure.Message != "1 violation(s) counted against the gate" {
		t.Errorf("failure message = %q", c.Failure.Message)
	}
	for _, want := range []string{"image-alt (critical)", "target: #hero > img", "summary: Images need alt text"} {
		if !strings.Contains(c.Failure.Body, want) {
			t.Errorf("failure body lacks %q:\n%s", want, c.Failure.Body)
		}
	}
	// The minor violation is under the enforced severity: information, not a
	// failure.
	if strings.Contains(c.Failure.Body, "color-contrast") {
		t.Errorf("an uncounted violation is in the failure body:\n%s", c.Failure.Body)
	}
	for _, want := range []string{"1 new violation(s) not failing the run", "color-contrast (minor)", "target: .chip"} {
		if !strings.Contains(c.SystemOut, want) {
			t.Errorf("system-out lacks %q:\n%s", want, c.SystemOut)
		}
	}
	modal := home.Cases[1]
	if modal.Failure != nil || !strings.Contains(modal.SystemOut, "button, Close") {
		t.Errorf("clean point should pass and carry its transcript: %+v", modal)
	}
	about := doc.Suites[1].Cases[0]
	if about.Failure != nil {
		t.Error("a baselined violation failed its testcase")
	}
	if !strings.Contains(about.SystemOut, "1 baselined violation(s)") || !strings.Contains(about.SystemOut, "page: http://localhost:5173/about") {
		t.Errorf("system-out = %q", about.SystemOut)
	}
}

// Selectors and summaries are page content: markup characters, quotes and a
// CDATA terminator must come out escaped and parse back to the original.
func TestJUnitEscapesPageContent(t *testing.T) {
	target := `a[href="x?a=1&b=2"] > <b>]]>'q'`
	summary := `Needs <alt> & "text" ]]> done`
	run := &runner.Run{
		TestsRun: 1,
		Tests:    []runner.TestRecord{{ID: `t<1>&"x"`, Points: []runner.ScanPoint{{Label: `l"&<`, PageURL: "http://h/?a=1&b=2"}}}},
		Results: []runner.Result{{
			Violation:    engine.Violation{RuleID: "r", Impact: engine.Serious, Summary: summary},
			TestID:       `t<1>&"x"`,
			ScanLabel:    `l"&<`,
			StableTarget: target,
			PageURL:      "http://h/?a=1&b=2",
		}},
	}
	var buf bytes.Buffer
	if err := JUnit(&buf, run, structuredMeta()); err != nil {
		t.Fatal(err)
	}
	wellFormed(t, buf.Bytes())
	if strings.Contains(buf.String(), "<b>") || strings.Contains(buf.String(), "]]>") {
		t.Errorf("page content reached the XML unescaped:\n%s", buf.String())
	}
	var doc junitDoc
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	c := doc.Suites[0].Cases[0]
	if doc.Suites[0].Name != `t<1>&"x"` || c.Name != `l"&<` {
		t.Errorf("names did not round-trip: %q / %q", doc.Suites[0].Name, c.Name)
	}
	if !strings.Contains(c.Failure.Body, "target: "+target) || !strings.Contains(c.Failure.Body, "summary: "+summary) {
		t.Errorf("body did not round-trip:\n%s", c.Failure.Body)
	}
	if !strings.Contains(c.SystemOut, "page: http://h/?a=1&b=2") {
		t.Errorf("system-out = %q", c.SystemOut)
	}
}

func TestJUnitEnvironmentErrorIsAnError(t *testing.T) {
	meta := structuredMeta()
	meta.ExitStatus = 3
	meta.Failure = &RunFailure{TestID: "home", Message: `test "home": scan "initial": browser crashed <boom>`}
	var buf bytes.Buffer
	if err := JUnit(&buf, &runner.Run{}, meta); err != nil {
		t.Fatal(err)
	}
	wellFormed(t, buf.Bytes())
	var doc junitDoc
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Errors != 1 || doc.Failures != 0 || doc.Tests != 1 || len(doc.Suites) != 1 {
		t.Fatalf("totals: %+v", doc)
	}
	c := doc.Suites[0].Cases[0]
	if doc.Suites[0].Name != "home" || c.Error == nil || !strings.Contains(c.Error.Message, "<boom>") {
		t.Errorf("error case wrong: %+v", c)
	}
}

// The formatters print what the run carries and nothing else: they have no
// raw URL to leak. A run already redacted by the runner stays redacted, and
// the raw token that is not in the Run is not in either output. (The wiring
// from the real runner is covered end to end in cmd/frostfall.)
func TestStructuredFormatsCarryRedactedValuesVerbatim(t *testing.T) {
	run := &runner.Run{
		TestsRun: 1,
		Tests:    []runner.TestRecord{{ID: "acct", Points: []runner.ScanPoint{{Label: "initial", PageURL: "http://localhost/account"}}}},
		Results: []runner.Result{{
			Violation:    engine.Violation{RuleID: "image-alt", Impact: engine.Critical, Summary: "alt", Target: "img[src=\"RAWSELECTORSECRET\"]"},
			TestID:       "acct",
			ScanLabel:    "initial",
			Fingerprint:  "f",
			StableTarget: "img",
			PageURL:      "http://localhost/account",
		}},
	}
	meta := structuredMeta()
	meta.BaseURL = "http://localhost"
	var j, x bytes.Buffer
	if err := JSON(&j, run, meta, structuredFlagged); err != nil {
		t.Fatal(err)
	}
	if err := JUnit(&x, run, meta); err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"json": j.String(), "junit": x.String()} {
		if strings.Contains(out, "RAWSELECTORSECRET") {
			t.Errorf("%s printed the raw selector, not the scrubbed StableTarget", name)
		}
		if !strings.Contains(out, "http://localhost/account") {
			t.Errorf("%s lost the page URL", name)
		}
	}
}

// gateRun has two scan points that each hold a counted (critical) violation
// and one that holds only a moderate one, plus a baselined critical.
func gateRun() *runner.Run {
	viol := func(test, label, fp string, impact engine.Impact, baselined bool) runner.Result {
		return runner.Result{
			Violation:    engine.Violation{RuleID: "rule-" + fp, Impact: impact, Summary: "s " + fp},
			TestID:       test,
			ScanLabel:    label,
			Fingerprint:  fp,
			StableTarget: "#" + fp,
			Baselined:    baselined,
		}
	}
	return &runner.Run{
		TestsRun: 1,
		Tests:    []runner.TestRecord{{ID: "t", Points: []runner.ScanPoint{{Label: "a"}, {Label: "b"}, {Label: "c"}, {Label: "d"}}}},
		Results: []runner.Result{
			viol("t", "a", "a1", engine.Critical, false),
			viol("t", "b", "b1", engine.Critical, false),
			viol("t", "b", "b2", engine.Moderate, false),
			viol("t", "c", "c1", engine.Moderate, false),
			viol("t", "d", "d1", engine.Critical, true),
		},
	}
}

func junitFailures(t *testing.T, run *runner.Run, meta ReportMeta) (doc junitDoc, failedCases []string) {
	t.Helper()
	var buf bytes.Buffer
	if err := JUnit(&buf, run, meta); err != nil {
		t.Fatal(err)
	}
	wellFormed(t, buf.Bytes())
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Suites {
		for _, c := range s.Cases {
			if c.Failure != nil {
				failedCases = append(failedCases, c.Name)
			}
		}
	}
	// The invariant: the file has a failure if and only if the run exits 1
	// because of violations.
	if got, want := doc.Failures > 0, meta.GateFailed && meta.ExitStatus == 1; got != want || (len(failedCases) > 0) != want {
		t.Errorf("failures=%d (cases %v) but the gate failed = %v", doc.Failures, failedCases, want)
	}
	return doc, failedCases
}

// A testcase fails only for violations that count toward the gate, and only
// when the gate fails. Everything else is information in system-out.
func TestJUnitFailsOnlyWhenTheGateFails(t *testing.T) {
	critical := func(r runner.Result) bool { return !r.Baselined && r.Impact >= engine.Critical }
	cases := []struct {
		name         string
		meta         func(*ReportMeta)
		moderateOnly bool     // the run has no new violation at the enforced severity
		want         []string // testcases that fail
	}{
		{"report only exits 0", func(m *ReportMeta) { m.Enforcing, m.ExitStatus, m.GateFailed, m.Counts = false, 0, false, nil }, false, nil},
		{"under maxViolations exits 0", func(m *ReportMeta) { m.ExitStatus, m.GateFailed, m.Counts = 0, false, critical }, false, nil},
		{"only violations below the enforced severity exits 0", func(m *ReportMeta) { m.ExitStatus, m.GateFailed, m.Counts = 0, false, critical }, true, nil},
		{"over maxViolations fails every point holding a counted violation", func(m *ReportMeta) { m.ExitStatus, m.GateFailed, m.Counts = 1, true, critical }, false, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := structuredMeta()
			tc.meta(&meta)
			run := gateRun()
			if tc.moderateOnly {
				kept := run.Results[:0]
				for _, r := range run.Results {
					if r.Impact == engine.Moderate || r.Baselined {
						kept = append(kept, r)
					}
				}
				run.Results = kept
			}
			doc, failed := junitFailures(t, run, meta)
			if !slices.Equal(failed, tc.want) {
				t.Errorf("failing testcases = %v, want %v", failed, tc.want)
			}
			// Nothing is lost: a violation that does not fail its testcase is
			// still listed in its system-out.
			out := doc.Suites[0].Cases
			if !strings.Contains(out[2].SystemOut, "rule-c1 (moderate)") {
				t.Errorf("an uncounted violation is missing from system-out:\n%s", out[2].SystemOut)
			}
			if !strings.Contains(out[3].SystemOut, "1 baselined violation(s)") {
				t.Errorf("baselined count missing from system-out:\n%s", out[3].SystemOut)
			}
			if !tc.moderateOnly && tc.want != nil && !strings.Contains(out[1].SystemOut, "rule-b2 (moderate)") {
				t.Errorf("the moderate violation beside a counted one should be information:\n%s", out[1].SystemOut)
			}
		})
	}
}

// A failure with no test id (the browser would not start) must not make a
// suite with an empty name.
func TestJUnitFailureWithNoTestIDUsesRun(t *testing.T) {
	meta := structuredMeta()
	meta.ExitStatus, meta.GateFailed = 3, false
	meta.Failure = &RunFailure{Message: "browser error: no chrome"}
	doc, _ := junitFailures(t, &runner.Run{}, meta)
	if len(doc.Suites) != 1 || doc.Suites[0].Name != "run" {
		t.Fatalf("suites = %+v", doc.Suites)
	}
	c := doc.Suites[0].Cases[0]
	if c.Classname != "run" || c.Error == nil || !strings.Contains(c.Error.Message, "no chrome") || doc.Errors != 1 {
		t.Errorf("errored testcase wrong: %+v (errors=%d)", c, doc.Errors)
	}
}

// The stale list is read from a baseline file, not from this run's scan, so
// with auth configured its targets are scrubbed like every other selector.
func TestJSONStaleTargetsAreScrubbedUnderAuth(t *testing.T) {
	run := &runner.Run{Stale: []baseline.Violation{{
		Fingerprint: "dead", TestID: "home", ScanLabel: "initial", RuleID: "label",
		StableTarget: `a[href="https://x.test/cb?token=SECRETTOK123"] > input[data-key='TOKENVAL']`,
	}}}
	for _, redact := range []bool{true, false} {
		meta := structuredMeta()
		meta.Redact = redact
		var buf bytes.Buffer
		if err := JSON(&buf, run, meta, structuredFlagged); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Stale []struct {
				StableTarget string `json:"stableTarget"`
			} `json:"stale"`
		}
		if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		leaked := strings.Contains(buf.String(), "SECRETTOK123") || strings.Contains(buf.String(), "TOKENVAL")
		if redact {
			if leaked || len(doc.Stale) != 1 || doc.Stale[0].StableTarget != "a[href] > input[data-key]" {
				t.Errorf("redacted stale entry = %+v\n%s", doc.Stale, buf.String())
			}
		} else if !leaked {
			t.Errorf("without auth the entry should be untouched:\n%s", buf.String())
		}
	}
}

func TestJSONEnforcingFlag(t *testing.T) {
	for _, enforcing := range []bool{true, false} {
		meta := structuredMeta()
		meta.Enforcing = enforcing
		var buf bytes.Buffer
		if err := JSON(&buf, &runner.Run{}, meta, structuredFlagged); err != nil {
			t.Fatal(err)
		}
		var doc jsonDoc
		if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Run.Enforcing != enforcing || !strings.Contains(buf.String(), `"enforcing":`) {
			t.Errorf("enforcing = %v, want %v:\n%s", doc.Run.Enforcing, enforcing, buf.String())
		}
	}
}
