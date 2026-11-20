package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/runner"
)

func result(rule, test, scan, target string) runner.Result {
	return runner.Result{
		Violation:    engine.Violation{RuleID: rule, Impact: engine.Critical},
		TestID:       test,
		ScanLabel:    scan,
		StableTarget: target,
	}
}

func TestBuildGroupsCollapsesElements(t *testing.T) {
	groups := BuildGroups([]runner.Result{
		result("aria-required-children", "users", "initial", `div[data-rowindex="0"]`),
		result("aria-required-children", "users", "initial", `div[data-rowindex="1"]`),
		result("aria-required-children", "dashboard", "initial", ".grid"),
		result("color-contrast", "users", "initial", ".chip"),
	})
	if len(groups) != 3 {
		t.Fatalf("want 3 groups (rule+page), got %d", len(groups))
	}
	for _, g := range groups {
		if g.Rule == "aria-required-children" && g.Page == "users" && len(g.Elements) != 2 {
			t.Errorf("users group should list 2 elements, got %v", g.Elements)
		}
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	g := Group{Rule: "color-contrast", Page: "pillar-scores-modal"}
	rule, page, ok := parseMarker(g.Body(""))
	if !ok || rule != g.Rule || page != g.Page {
		t.Errorf("marker did not round-trip: %v %q %q", ok, rule, page)
	}
	if _, _, ok := parseMarker("a human-written issue body"); ok {
		t.Errorf("non-frostfall body parsed as marker")
	}
}

func plan(t *testing.T, groups []Group, existing []Issue, ran ...string) []Action {
	t.Helper()
	testsRun := map[string]bool{}
	for _, id := range ran {
		testsRun[id] = true
	}
	return Plan(groups, existing, func(page, _ string) bool { return testsRun[page] }, nil)
}

func TestPlanCreatesOnlyMissing(t *testing.T) {
	g1 := Group{Rule: "label", Page: "home"}
	g2 := Group{Rule: "image-alt", Page: "home"}
	existing := []Issue{{Number: 7, State: "open", Body: g1.Body("")}}
	actions := plan(t, []Group{g1, g2}, existing, "home")
	if len(actions) != 1 || actions[0].Kind != "create" {
		t.Fatalf("want 1 create, got %v", actions)
	}
}

func TestPlanIdempotentSecondRun(t *testing.T) {
	g := Group{Rule: "label", Page: "home"}
	existing := []Issue{{Number: 7, State: "open", Body: g.Body("")}}
	if actions := plan(t, []Group{g}, existing, "home"); len(actions) != 0 {
		t.Errorf("second run should be a no-op, got %v", actions)
	}
}

func TestPlanReopensClosedRecurrence(t *testing.T) {
	g := Group{Rule: "label", Page: "home"}
	existing := []Issue{{Number: 7, State: "closed", Body: g.Body("")}}
	actions := plan(t, []Group{g}, existing, "home")
	if len(actions) != 1 || actions[0].Kind != "reopen" || actions[0].Number != 7 {
		t.Fatalf("want reopen #7, got %v", actions)
	}
}

func TestPlanClosesFixedOnlyWhenPageWasScanned(t *testing.T) {
	g := Group{Rule: "label", Page: "home"}
	existing := []Issue{{Number: 7, State: "open", Body: g.Body("")}}
	// Page scanned, violation gone: close.
	actions := plan(t, nil, existing, "home")
	if len(actions) != 1 || actions[0].Kind != "close" {
		t.Fatalf("want close, got %v", actions)
	}
	// Filtered run that skipped the page: leave it alone.
	if actions := plan(t, nil, existing, "other-page"); len(actions) != 0 {
		t.Errorf("filtered run must not close unscanned pages, got %v", actions)
	}
}

func TestPlanIgnoresHumanIssues(t *testing.T) {
	existing := []Issue{{Number: 3, State: "open", Body: "manually filed a11y note"}}
	if actions := plan(t, nil, existing, "home"); len(actions) != 0 {
		t.Errorf("human issues must never be touched, got %v", actions)
	}
}

// call is one write the fake GitHub API received.
type call struct {
	Method string
	Path   string
	Body   map[string]string
}

// fakeAPI configures the fake GitHub server.
type fakeAPI struct {
	failBody bool             // a PATCH that carries a body answers 422
	failPath string           // a PATCH to this path answers 500
	issues   []map[string]any // served by GET /repos/org/repo/issues
}

// newFake serves the issue API and records every write (not reads), in order.
func newFake(t *testing.T, f fakeAPI) (*Client, func() []call) {
	t.Helper()
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(f.issues)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		c := call{Method: r.Method, Path: r.URL.Path, Body: map[string]string{}}
		_ = json.Unmarshal(raw, &c.Body)
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		_, hasBody := c.Body["body"]
		switch {
		case r.Method == "PATCH" && f.failBody && hasBody:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"body too long"}`))
		case r.Method == "PATCH" && f.failPath != "" && r.URL.Path == f.failPath:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient("tok", "org/repo")
	c.APIBase = srv.URL
	return c, func() []call {
		mu.Lock()
		defer mu.Unlock()
		return append([]call(nil), calls...)
	}
}

func fakeGitHub(t *testing.T, failPath string) (*Client, func() []call) {
	t.Helper()
	return newFake(t, fakeAPI{failPath: failPath})
}

func ipath(n int) string { return fmt.Sprintf("/repos/org/repo/issues/%d", n) }

func bodyPatch(n int, body string) call {
	return call{"PATCH", ipath(n), map[string]string{"body": body}}
}
func commentPost(n int, body string) call {
	return call{"POST", ipath(n) + "/comments", map[string]string{"body": body}}
}
func statePatch(n int, state string) call {
	return call{"PATCH", ipath(n), map[string]string{"state": state}}
}

func els(e ...string) []string { return e }

func grp(elements ...string) Group {
	return Group{Rule: "label", Page: "home", Impact: "critical", Summary: "s", Elements: elements}
}

func syncOnce(t *testing.T, c *Client, g Group, existing []Issue) ([]Action, error) {
	t.Helper()
	return Sync(c, []Group{g}, existing, func(string, string) bool { return true }, nil, "ctx.")
}

func wantCalls(t *testing.T, got, want []call) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls differ\n got: %+v\nwant: %+v", got, want)
	}
}

const issuePath = "/repos/org/repo/issues/7"

func TestSyncGrownSetUpdatesBodyThenCommentsNewOnly(t *testing.T) {
	old := grp("#a", "#b")
	cur := grp("#a", "#b", "#c", "#d")
	existing := []Issue{{Number: 7, State: "open", Body: old.Body("old run.")}}

	actions := plan(t, []Group{cur}, existing, "home")
	if len(actions) != 1 || actions[0].Kind != "update" || actions[0].Number != 7 ||
		!reflect.DeepEqual(actions[0].Added, els("#c", "#d")) || actions[0].Removed != 0 {
		t.Fatalf("want update #7 adding #c,#d, got %+v", actions)
	}
	if got := actions[0].String(); got != "update #7 Accessibility: label on home (+2 -0 elements)" {
		t.Errorf("String() = %q", got)
	}

	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, cur, existing); err != nil {
		t.Fatal(err)
	}
	// Body first, then the comment naming only the new elements.
	wantCalls(t, calls(), []call{
		bodyPatch(7, cur.Body("ctx.")),
		commentPost(7, "2 new failing elements:\n\n- `#c`\n- `#d`\n\nctx."),
	})
}

func TestSyncSingleNewElementIsSingular(t *testing.T) {
	existing := []Issue{{Number: 7, State: "open", Body: grp("#a").Body("")}}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, grp("#a", "#b"), existing); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || !strings.HasPrefix(got[1].Body["body"], "1 new failing element:\n") {
		t.Errorf("calls = %+v", got)
	}
}

func TestSyncShrunkSetUpdatesBodySilently(t *testing.T) {
	existing := []Issue{{Number: 7, State: "open", Body: grp("#a", "#b", "#c").Body("")}}
	cur := grp("#a")
	actions := plan(t, []Group{cur}, existing, "home")
	if len(actions) != 1 || actions[0].Kind != "update" || len(actions[0].Added) != 0 || actions[0].Removed != 2 {
		t.Fatalf("want silent update removing 2, got %+v", actions)
	}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, cur, existing); err != nil {
		t.Fatal(err)
	}
	// Only the body PATCH: no comment POST, no state change.
	wantCalls(t, calls(), []call{bodyPatch(7, cur.Body("ctx."))})
}

func TestSyncIdenticalSetDoesNothing(t *testing.T) {
	g := grp("#a", "#b")
	// A different order and an older run link must not count as a change.
	existing := []Issue{{Number: 7, State: "open", Body: grp("#b", "#a").Body("older run.")}}
	if actions := plan(t, []Group{g}, existing, "home"); len(actions) != 0 {
		t.Fatalf("identical set must plan nothing, got %v", actions)
	}
	c, calls := fakeGitHub(t, "")
	actions, err := syncOnce(t, c, g, existing)
	if err != nil || len(actions) != 0 {
		t.Errorf("identical set: actions=%v err=%v", actions, err)
	}
	wantCalls(t, calls(), nil)
}

func TestSyncReopenWithNewElementsIsOneComment(t *testing.T) {
	existing := []Issue{{Number: 7, State: "closed", Body: grp("#a").Body("")}}
	cur := grp("#a", "#b")
	actions := plan(t, []Group{cur}, existing, "home")
	if len(actions) != 1 || actions[0].Kind != "reopen" || !actions[0].Rewrite ||
		!reflect.DeepEqual(actions[0].Added, els("#b")) {
		t.Fatalf("want reopen with rewrite, got %+v", actions)
	}
	if got := actions[0].String(); got != "reopen #7 Accessibility: label on home (+1 -0 elements)" {
		t.Errorf("String() = %q", got)
	}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, cur, existing); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, calls(), []call{
		bodyPatch(7, cur.Body("ctx.")),
		commentPost(7, "Recurred.\n\n1 new failing element:\n\n- `#b`\n\nctx."),
		statePatch(7, "open"),
	})
}

func TestSyncReopenSameElementsKeepsOldBehavior(t *testing.T) {
	g := grp("#a")
	existing := []Issue{{Number: 7, State: "closed", Body: g.Body("")}}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, g, existing); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, calls(), []call{commentPost(7, "Recurred. ctx."), statePatch(7, "open")})
}

func TestSyncReopenWithOnlyRemovalsIsSilentBodyUpdate(t *testing.T) {
	existing := []Issue{{Number: 7, State: "closed", Body: grp("#a", "#b").Body("")}}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, grp("#a"), existing); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, calls(), []call{
		bodyPatch(7, grp("#a").Body("ctx.")),
		commentPost(7, "Recurred. ctx."),
		statePatch(7, "open"),
	})
}

// A body edit GitHub always rejects (too long, locked issue, 403, rate limit)
// must not make every run repeat the same "new failing elements" comment, and
// must not keep a recurrence closed.
func TestSyncRejectedBodyEditNeverAnnouncesAndStillReopens(t *testing.T) {
	openOld := Group{Rule: "label", Page: "home", Impact: "critical", Summary: "s", Elements: els("#a")}
	openNew := openOld
	openNew.Elements = els("#a", "#b")
	closedOld := openOld
	closedOld.Page = "other"
	closedNew := openNew
	closedNew.Page = "other"
	existing := []Issue{
		{Number: 7, State: "open", Body: openOld.Body("")},
		{Number: 8, State: "closed", Body: closedOld.Body("")},
	}
	c, calls := newFake(t, fakeAPI{failBody: true})
	cov := func(string, string) bool { return true }

	for run := 1; run <= 3; run++ {
		before := len(calls())
		_, err := Sync(c, []Group{openNew, closedNew}, existing, cov, nil, "ctx.")
		if err == nil || !strings.Contains(err.Error(), "#7") || !strings.Contains(err.Error(), "#8") ||
			!strings.Contains(err.Error(), "edit body: github api: 422") {
			t.Fatalf("run %d: want the body errors collected, got %v", run, err)
		}
		// Open #7: the rejected body edit and nothing else. Closed #8: the
		// rejected edit, the plain Recurred comment, then the reopen.
		wantCalls(t, calls()[before:], []call{
			bodyPatch(7, openNew.Body("ctx.")),
			bodyPatch(8, closedNew.Body("ctx.")),
			commentPost(8, "Recurred. ctx."),
			statePatch(8, "open"),
		})
	}
	for _, cl := range calls() {
		if strings.Contains(cl.Body["body"], "new failing") {
			t.Fatalf("announced elements the body does not list: %+v", cl)
		}
	}
}

func TestParseElementsOldFormatBody(t *testing.T) {
	// The shape of a body filed before this change: plain single-backtick
	// bullets and nothing else recording the elements.
	body := "Frostfall detected **label** (critical) on **home**.\n\n> s\n\n" +
		"Affected element(s):\n- `#a`\n- `input[name=\"q\"] (after-open)`\n\n" +
		"[How to fix label](https://example.test)\n\nFiled by Frostfall.\n\n" +
		"<!-- frostfall:v1:rule=label:page=home -->\n"
	got := parseElements(body)
	if !got.ok || got.truncated || !reflect.DeepEqual(got.els, els("#a", `input[name="q"] (after-open)`)) {
		t.Fatalf("parseElements = %+v", got)
	}
	existing := []Issue{{Number: 7, State: "open", Body: body}}
	same := grp("#a", `input[name="q"] (after-open)`)
	if actions := plan(t, []Group{same}, existing, "home"); len(actions) != 0 {
		t.Errorf("old-format body with the same set must be a no-op, got %v", actions)
	}
	grown := grp("#a", `input[name="q"] (after-open)`, "#z")
	actions := plan(t, []Group{grown}, existing, "home")
	if len(actions) != 1 || !reflect.DeepEqual(actions[0].Added, els("#z")) {
		t.Errorf("old-format body should diff against its visible list, got %+v", actions)
	}
}

func TestSyncUnreadableListRewritesBodyWithoutClaimingNewElements(t *testing.T) {
	body := "Frostfall detected something.\n\nSee notes below.\n\n<!-- frostfall:v1:rule=label:page=home -->\n"
	existing := []Issue{{Number: 7, State: "open", Body: body}}
	c, calls := fakeGitHub(t, "")
	actions, err := syncOnce(t, c, grp("#a"), existing)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls(t, calls(), []call{bodyPatch(7, grp("#a").Body("ctx."))})
	if len(actions) != 1 || actions[0].Kind != "update" {
		t.Fatalf("actions = %v", actions)
	}
	if s := actions[0].String(); s != "update #7 Accessibility: label on home (rewrite body: element list unreadable)" {
		t.Errorf("String() = %q", s)
	}
}

func TestSyncFailuresAreCollectedNotFatal(t *testing.T) {
	g1 := Group{Rule: "label", Page: "home", Elements: els("#a", "#b")}
	g2 := Group{Rule: "image-alt", Page: "home", Elements: els("#x", "#y")}
	existing := []Issue{
		{Number: 7, State: "open", Body: Group{Rule: "label", Page: "home", Elements: els("#a")}.Body("")},
		{Number: 8, State: "open", Body: Group{Rule: "image-alt", Page: "home", Elements: els("#x")}.Body("")},
	}
	c, calls := fakeGitHub(t, issuePath)
	actions, err := Sync(c, []Group{g1, g2}, existing, func(string, string) bool { return true }, nil, "ctx.")
	want := `update #7 Accessibility: label on home (+1 -0 elements): edit body: github api: 500: {"message":"boom"}`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want exactly %q", err, want)
	}
	if len(actions) != 2 {
		t.Fatalf("want both actions planned, got %v", actions)
	}
	// #7 failed on its body edit and posted nothing; #8 was unaffected.
	wantCalls(t, calls(), []call{
		bodyPatch(7, g1.Body("ctx.")),
		bodyPatch(8, g2.Body("ctx.")),
		commentPost(8, "1 new failing element:\n\n- `#y`\n\nctx."),
	})
}

func TestCodeSpanRoundTripAndContainment(t *testing.T) {
	for _, el := range []string{
		"#plain",
		"a[href=\"x`y\"]",
		"a``b",
		"`leading",
		"trailing`",
		"``",
		" padded ",
		" ",
		"   ",
		"",
		"@octocat and `code` @team/name",
	} {
		span := codeSpan(el)
		got, ok := parseCodeSpan(span)
		if !ok || got != el {
			t.Errorf("round trip %q via %q = %q, %v", el, span, got, ok)
		}
		// The fence must be longer than any backtick run in the content, so
		// the content can never close the span early.
		longest := 0
		for _, run := range strings.FieldsFunc(el, func(r rune) bool { return r != '`' }) {
			longest = max(longest, len(run))
		}
		fence := span[:len(span)-len(strings.TrimLeft(span, "`"))]
		if len(fence) <= longest {
			t.Errorf("fence %q does not exceed longest inner run %d for %q", fence, longest, el)
		}
	}
	if got := codeSpan("#plain"); got != "`#plain`" {
		t.Errorf("common case must stay unchanged, got %q", got)
	}
}

// CommonMark strips one space from each end of a span only when it is not
// all spaces, and only U+0020 counts. Padding or stripping all-space content
// (or a tab or NBSP) would corrupt it.
func TestCodeSpanWhitespaceMatchesCommonMark(t *testing.T) {
	for _, s := range []string{" ", "  ", "   "} {
		if got := codeSpan(s); got != "`"+s+"`" {
			t.Errorf("codeSpan(%q) = %q: all-space content must not be padded", s, got)
		}
	}
	if got := codeSpan(" a "); got != "`  a  `" {
		t.Errorf("codeSpan(\" a \") = %q", got)
	}
	// Not all spaces, so a reader strips one space from each end.
	if got := codeSpan(" \t "); got != "`  \t  `" {
		t.Errorf("codeSpan(space tab space) = %q", got)
	}
	for _, s := range []string{"\t", "\ta\t", " x ", " \t "} {
		got, ok := parseCodeSpan(codeSpan(s))
		if !ok || got != s {
			t.Errorf("%q round trip = %q, %v", s, got, ok)
		}
	}
	if got, _ := parseCodeSpan("` \t`"); got != " \t" {
		t.Errorf("a tab is not a space to strip: got %q", got)
	}
	if got, _ := parseCodeSpan("`   `"); got != "   " {
		t.Errorf("all-space content must not be stripped: got %q", got)
	}
}

func TestBodyKeepsBacktickSelectorInsideCodeSpan(t *testing.T) {
	g := grp("a[title=\"`@everyone`\"]")
	body := g.Body("")
	if !strings.Contains(body, "- ``a[title=\"`@everyone`\"]``\n") {
		t.Errorf("selector not fenced safely:\n%s", body)
	}
	got := parseElements(body)
	if !got.ok || !reflect.DeepEqual(got.els, g.Elements) {
		t.Errorf("fenced element did not round-trip: %+v", got)
	}
}

func TestBuildGroupsFlattensLineBreaksInSelectorsAndLabels(t *testing.T) {
	groups := BuildGroups([]runner.Result{
		result("label", "home", "initial", "a[x=\"1\n2\"]"),
		result("label", "home", "after\nopen", "#b"),
	})
	want := els("a[x=\"1 2\"]", "#b (after open)")
	if len(groups) != 1 || !reflect.DeepEqual(groups[0].Elements, want) {
		t.Fatalf("got %+v, want elements %q", groups, want)
	}
	if got := parseElements(groups[0].Body("")); !got.ok || !reflect.DeepEqual(got.els, groups[0].Elements) {
		t.Errorf("round trip failed: %+v", got)
	}
}

func TestActionStringStyle(t *testing.T) {
	cases := []struct {
		a    Action
		want string
	}{
		{Action{Kind: "create", Title: "T"}, "create T"},
		{Action{Kind: "close", Title: "T", Number: 3}, "close #3 T"},
		{Action{Kind: "reopen", Title: "T", Number: 3}, "reopen #3 T"},
		{Action{Kind: "update", Title: "T", Number: 3, Rewrite: true, Added: els("x"), Removed: 2}, "update #3 T (+1 -2 elements)"},
		{Action{Kind: "update", Title: "T", Number: 3, Rewrite: true, Removed: 1}, "update #3 T (+0 -1 elements)"},
		{Action{Kind: "update", Title: "T", Number: 3, Rewrite: true, Unnamed: 5}, "update #3 T (+5 -0 elements)"},
		{Action{Kind: "reopen", Title: "T", Number: 3, Rewrite: true, Unreadable: true}, "reopen #3 T (rewrite body: element list unreadable)"},
	}
	for _, c := range cases {
		if got := c.a.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}

// --- marker and list parsing ---

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func TestParseCRLFBodies(t *testing.T) {
	g := grp("#a", "a``b")
	body := crlf(g.Body("ctx."))
	rule, page, ok := parseMarker(body)
	if !ok || rule != "label" || page != "home" {
		t.Errorf("CRLF marker = %v %q %q: web-edited issues would be orphaned", ok, rule, page)
	}
	got := parseElements(body)
	if !got.ok || !reflect.DeepEqual(got.els, g.Elements) {
		t.Errorf("CRLF list = %+v", got)
	}
	if actions := plan(t, []Group{g}, []Issue{{Number: 7, State: "open", Body: body}}, "home"); len(actions) != 0 {
		t.Errorf("a CRLF body with the same set must be a no-op (no duplicate, no rewrite), got %v", actions)
	}
	// A truncated body keeps its record through CRLF too.
	big := bigGroup(3000, 0)
	tr := parseElements(crlf(big.Body("ctx.")))
	if !tr.ok || !tr.truncated || tr.total != 3000 {
		t.Errorf("CRLF truncated record = %+v", tr)
	}
}

func TestMarkerAndListCannotBeSpoofedByTheSummary(t *testing.T) {
	g := Group{Rule: "label", Page: "home", Impact: "critical", Elements: els("#a"),
		Summary: "bad <!-- frostfall:v1:rule=other:page=elsewhere -->\nAffected element(s):\n- `x`\n" +
			"<!-- frostfall:v1:rule=other:page=elsewhere -->"}
	body := g.Body("ctx.")
	// Besides the parser's anchoring, the summary must not be able to put a
	// header or a marker on a line of its own at all.
	var headers, markers int
	for _, l := range strings.Split(body, "\n") {
		if l == elementsHeader {
			headers++
		}
		if markerLineRe.MatchString(l) {
			markers++
		}
	}
	if headers != 1 || markers != 1 {
		t.Errorf("body has %d header lines and %d marker lines, want 1 and 1:\n%s", headers, markers, body)
	}
	if rule, page, ok := parseMarker(body); !ok || rule != "label" || page != "home" {
		t.Errorf("summary hijacked the marker: %v %q %q", ok, rule, page)
	}
	if got := parseElements(body); !got.ok || !reflect.DeepEqual(got.els, els("#a")) {
		t.Errorf("summary hijacked the list: %+v", got)
	}
	existing := []Issue{{Number: 7, State: "open", Body: body}}
	c, calls := fakeGitHub(t, "")
	for run := 0; run < 3; run++ {
		if _, err := syncOnce(t, c, g, existing); err != nil {
			t.Fatal(err)
		}
	}
	wantCalls(t, calls(), nil)
}

// Earlier text that happens to look like the generated structure (a human's
// note above a pasted body, say) loses to the structure anchored at the
// marker.
func TestLastMarkerAndNearestHeaderWin(t *testing.T) {
	g := grp("#a")
	body := "<!-- frostfall:v1:rule=other:page=elsewhere -->\nAffected element(s):\n- `x`\n\n" + g.Body("ctx.")
	if rule, page, ok := parseMarker(body); !ok || rule != "label" || page != "home" {
		t.Errorf("marker = %v %q %q, want the last", ok, rule, page)
	}
	if got := parseElements(body); !got.ok || !reflect.DeepEqual(got.els, els("#a")) {
		t.Errorf("list = %+v, want the one above the marker", got)
	}
}

func TestMarkerMustBeOnItsOwnLine(t *testing.T) {
	if _, _, ok := parseMarker("quoted: <!-- frostfall:v1:rule=a:page=b --> in prose"); ok {
		t.Errorf("an inline marker must not match")
	}
	if _, _, ok := parseMarker("> <!-- frostfall:v1:rule=a:page=b -->\n"); ok {
		t.Errorf("a quoted marker must not match")
	}
}

// --- partial scans ---

func partialPage(page string) func(string, string) bool {
	return func(p, _ string) bool { return p == page }
}

func TestPartialScanGrowsListButNeverShrinksIt(t *testing.T) {
	existing := []Issue{{Number: 7, State: "open", Body: grp("#a", "#b", "#c").Body("")}}
	cur := grp("#a", "#d")
	cov := func(string, string) bool { return true }

	// Complete scan: #b and #c are fixed.
	full := Plan([]Group{cur}, existing, cov, nil)
	if len(full) != 1 || full[0].Removed != 2 || !reflect.DeepEqual(full[0].Elements, els("#a", "#d")) {
		t.Fatalf("complete scan: %+v", full)
	}

	// A frame was missed: #b and #c may just be unchecked.
	part := Plan([]Group{cur}, existing, cov, partialPage("home"))
	if len(part) != 1 || part[0].Removed != 0 || !reflect.DeepEqual(part[0].Added, els("#d")) ||
		!reflect.DeepEqual(part[0].Elements, els("#a", "#b", "#c", "#d")) {
		t.Fatalf("partial scan must write existing plus new: %+v", part)
	}
	c, calls := fakeGitHub(t, "")
	if _, err := Sync(c, []Group{cur}, existing, cov, partialPage("home"), "ctx."); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, calls(), []call{
		bodyPatch(7, grp("#a", "#b", "#c", "#d").Body("ctx.")),
		commentPost(7, "1 new failing element:\n\n- `#d`\n\nctx."),
	})

	// Only losses on a partial page: nothing to do.
	if got := Plan([]Group{grp("#a")}, existing, cov, partialPage("home")); len(got) != 0 {
		t.Errorf("partial scan with only missing elements must plan nothing, got %v", got)
	}
	// Partial elsewhere does not protect this page.
	if got := Plan([]Group{grp("#a")}, existing, cov, partialPage("other")); len(got) != 1 {
		t.Errorf("partial on another page must not block removals, got %v", got)
	}
}

func TestPartialScanLeavesTruncatedListAlone(t *testing.T) {
	existing := []Issue{{Number: 7, State: "open", Body: bigGroup(3000, 0).Body("")}}
	got := Plan([]Group{bigGroup(100, 0)}, existing, func(string, string) bool { return true }, partialPage("home"))
	if len(got) != 0 {
		t.Errorf("the hidden part of a truncated list is unknown, so a partial scan must not rewrite it: %v", got)
	}
}

// An unreadable list rewritten from a partial scan would leave a
// known-incomplete list that the next complete scan reads cleanly and
// announces as new elements.
func TestPartialScanLeavesUnreadableListAlone(t *testing.T) {
	body := "Frostfall detected something.\n\n<!-- frostfall:v1:rule=label:page=home -->\n"
	existing := []Issue{{Number: 7, State: "open", Body: body}}
	cov := func(string, string) bool { return true }
	if got := Plan([]Group{grp("#a")}, existing, cov, partialPage("home")); len(got) != 0 {
		t.Errorf("partial scan must not rewrite an unreadable list, got %v", got)
	}
	if got := Plan([]Group{grp("#a")}, existing, cov, nil); len(got) != 1 {
		t.Errorf("a complete scan still rewrites it, got %v", got)
	}
}

// A partial scan can lose the very elements that keep an issue open, so it
// never closes one; a complete scan of the same page still does.
func TestPartialScanNeverClosesAnIssue(t *testing.T) {
	existing := []Issue{{Number: 7, State: "open", Body: grp("#a").Body("")}}
	cov := func(string, string) bool { return true }
	if got := Plan(nil, existing, cov, partialPage("home")); len(got) != 0 {
		t.Errorf("partial scan must not close, got %v", got)
	}
	if got := Plan(nil, existing, cov, partialPage("other")); len(got) != 1 || got[0].Kind != "close" {
		t.Errorf("partial on another page must not block the close, got %v", got)
	}
	if got := Plan(nil, existing, cov, nil); len(got) != 1 || got[0].Kind != "close" {
		t.Errorf("a complete scan closes, got %v", got)
	}
}

// --- fences and hand-edited bodies ---

// maxRun is the longest run of backticks in s.
func maxRun(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// cmark-gfm (GitHub's renderer) will not open a code span whose opening
// backtick run is over 80 long: the fence prints as text and the rest of the
// selector becomes live markdown. parseCodeSpan has no such limit, so only a
// check on the rendered text sees it.
func TestHugeBacktickRunStaysInsideAShortFence(t *testing.T) {
	huge := strings.Repeat("`", 200)
	sel := `a[x="` + huge + `@everyone` + huge + `"]`
	norm := normElement(sel)
	if maxRun(norm) != maxBackticks {
		t.Fatalf("normElement kept a run of %d, want it capped at %d", maxRun(norm), maxBackticks)
	}
	fence := strings.Repeat("`", maxBackticks+1)
	cases := map[string]Group{
		"through BuildGroups": BuildGroups([]runner.Result{result("label", "home", "initial", sel)})[0],
		"literal group":       grp(sel),
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			body := g.Body("ctx.")
			if n := maxRun(body); n > 80 {
				t.Fatalf("body has a backtick run of %d, over cmark's limit of 80", n)
			}
			var line string
			for _, l := range strings.Split(body, "\n") {
				if strings.HasPrefix(l, "- ") {
					line = l
				}
			}
			if !strings.HasPrefix(line, "- "+fence+`a[x="`) || !strings.HasSuffix(line, `"]`+fence) || !strings.Contains(line, "@everyone") {
				t.Errorf("the selector is not inside one short code span: %.60q...", line)
			}
			got := parseElements(body)
			if !got.ok || got.stale || !reflect.DeepEqual(got.els, els(norm)) {
				t.Errorf("round trip = %+v", got)
			}
			// Stable: three runs against the body it wrote make no calls.
			c, calls := fakeGitHub(t, "")
			existing := []Issue{{Number: 7, State: "open", Body: body}}
			for run := 0; run < 3; run++ {
				if _, err := syncOnce(t, c, g, existing); err != nil {
					t.Fatal(err)
				}
			}
			wantCalls(t, calls(), nil)
		})
	}

	// The comment names the element with the same short fence.
	cur := grp("#a", sel)
	existing := []Issue{{Number: 7, State: "open", Body: grp("#a").Body("")}}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, cur, existing); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("calls = %+v", got)
	}
	if n := maxRun(got[1].Body["body"]); n > 80 {
		t.Errorf("comment has a backtick run of %d, over cmark's limit of 80", n)
	}
	if want := "1 new failing element:\n\n- " + codeSpan(norm) + "\n\nctx."; got[1].Body["body"] != want {
		t.Errorf("comment = %.80q...", got[1].Body["body"])
	}
}

func TestMarkerToleratesHandEditedWhitespace(t *testing.T) {
	const m = "<!-- frostfall:v1:rule=label:page=home -->"
	g := grp("#a")
	for _, edited := range []string{"  " + m, m + "  ", "\t" + m + "\t", "  " + m + " "} {
		for _, eol := range []string{"\n", "\r\n"} {
			body := strings.Replace(g.Body("ctx."), m, edited, 1)
			body = strings.ReplaceAll(body, "\n", eol)
			rule, page, ok := parseMarker(body)
			if !ok || rule != "label" || page != "home" {
				t.Errorf("marker %q with %q eol: %v %q %q (the issue would be orphaned and a duplicate filed)", edited, eol, ok, rule, page)
			}
			if got := parseElements(body); !got.ok || !reflect.DeepEqual(got.els, els("#a")) {
				t.Errorf("marker %q: list = %+v", edited, got)
			}
			if actions := plan(t, []Group{g}, []Issue{{Number: 7, State: "open", Body: body}}, "home"); len(actions) != 0 {
				t.Errorf("marker %q: a hand-edited marker with the same set must plan nothing, got %v", edited, actions)
			}
		}
	}
}

// Text a human appends after a hand-edited (indented) marker, including a
// look-alike list, does not replace the list above it.
func TestListIsAnchoredAtAHandEditedMarker(t *testing.T) {
	g := grp("#a")
	for name, edit := range map[string][2]string{
		"indented": {"<!-- frostfall:v1", "   <!-- frostfall:v1"},
		"trailing": {"page=home -->", "page=home -->  "},
	} {
		body := strings.Replace(g.Body("ctx."), edit[0], edit[1], 1) +
			"\nnotes\nAffected element(s):\n- `junk`\n"
		if got := parseElements(body); !got.ok || !reflect.DeepEqual(got.els, els("#a")) {
			t.Errorf("%s: list = %+v, want #a from above the marker", name, got)
		}
		if actions := plan(t, []Group{g}, []Issue{{Number: 7, State: "open", Body: body}}, "home"); len(actions) != 0 {
			t.Errorf("%s: want no actions, got %v", name, actions)
		}
	}
}

// A body from an older version wrapped every selector in a single-backtick
// span, which a selector holding a backtick breaks out of. It reads back as
// the right element, so only the rendering shows it is unsafe: rewrite it
// once, quietly, and then leave it alone.
func TestOldUnsafeBulletsAreRewrittenOnceQuietly(t *testing.T) {
	old := func(bullets string) string {
		return "Frostfall detected **label** (critical) on **home**.\n\n> s\n\nAffected element(s):\n" + bullets +
			"\nFiled by Frostfall.\n\n<!-- frostfall:v1:rule=label:page=home -->\n"
	}
	cases := map[string]struct {
		bullets string
		want    Group
		unread  bool
	}{
		"backtick inside": {"- `a[x=\"`@everyone\"]`\n- `#b`\n", grp("a[x=\"`@everyone\"]", "#b"), false},
		// Old rendering of a selector ending in a backtick: "`a``".
		"backtick at the end": {"- `a``\n- `#b`\n", grp("a`", "#b"), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			existing := []Issue{{Number: 7, State: "open", Body: old(tc.bullets)}}
			actions := plan(t, []Group{tc.want}, existing, "home")
			if len(actions) != 1 || actions[0].Kind != "update" || !actions[0].Rewrite ||
				len(actions[0].Added) != 0 || actions[0].Removed != 0 || actions[0].Unnamed != 0 || actions[0].Unreadable != tc.unread {
				t.Fatalf("want one quiet rewrite, got %+v", actions)
			}
			c, calls := fakeGitHub(t, "")
			if _, err := syncOnce(t, c, tc.want, existing); err != nil {
				t.Fatal(err)
			}
			got := calls()
			wantCalls(t, got, []call{bodyPatch(7, tc.want.Body("ctx."))})
			// What it wrote is stable.
			again := []Issue{{Number: 7, State: "open", Body: got[0].Body["body"]}}
			for run := 0; run < 2; run++ {
				if _, err := syncOnce(t, c, tc.want, again); err != nil {
					t.Fatal(err)
				}
			}
			if len(calls()) != 1 {
				t.Errorf("rewrite did not settle: %+v", calls()[1:])
			}
		})
	}
	// A bullet filed with a fence over cmark's limit reads back as the capped
	// element, not as one element gone and another new.
	huge := `a[x="` + strings.Repeat("`", 200) + `@everyone"]`
	overlong := []Issue{{Number: 7, State: "open", Body: old("- " + codeSpan(huge) + "\n")}}
	if got := plan(t, []Group{grp(huge)}, overlong, "home"); len(got) != 1 || !got[0].Rewrite ||
		len(got[0].Added) != 0 || got[0].Removed != 0 {
		t.Errorf("an over-long fence must be rewritten quietly, got %+v", got)
	}

	// The same quiet rewrite for a truncated list whose set is unchanged.
	big := bigGroup(3000, 0)
	big.Elements[0] = "a`b" + big.Elements[0][3:]
	tbody := strings.Replace(big.Body(""), codeSpan(big.Elements[0]), "`"+big.Elements[0]+"`", 1)
	if tbody == big.Body("") {
		t.Fatalf("setup: the unsafe bullet was not substituted")
	}
	got := plan(t, []Group{big}, []Issue{{Number: 7, State: "open", Body: tbody}}, "home")
	if len(got) != 1 || !got[0].Rewrite || len(got[0].Added)+got[0].Unnamed+got[0].Removed != 0 {
		t.Errorf("truncated body with an old bullet and the same set: want a quiet rewrite, got %+v", got)
	}

	// On a partial page the rewrite keeps every element already listed.
	partialExisting := []Issue{{Number: 7, State: "open", Body: old("- `a[x=\"`@everyone\"]`\n- `#b`\n")}}
	got = Plan([]Group{grp("a[x=\"`@everyone\"]")}, partialExisting, func(string, string) bool { return true }, partialPage("home"))
	if len(got) != 1 || !got[0].Rewrite || len(got[0].Added) != 0 ||
		!reflect.DeepEqual(got[0].Elements, els("a[x=\"`@everyone\"]", "#b")) {
		t.Errorf("partial page: want a quiet rewrite keeping #b, got %+v", got)
	}

	// A reopen takes the quiet rewrite too, without a second comment.
	existing := []Issue{{Number: 7, State: "closed", Body: old("- `a[x=\"`@everyone\"]`\n")}}
	g := grp("a[x=\"`@everyone\"]")
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, g, existing); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, calls(), []call{bodyPatch(7, g.Body("ctx.")), commentPost(7, "Recurred. ctx."), statePatch(7, "open")})
}

// --- long lists ---

const maxBody = 65536

// bigGroup has n 120-character elements numbered from first.
func bigGroup(n, first int) Group {
	g := grp()
	for i := first; i < first+n; i++ {
		el := fmt.Sprintf("#el-%05d-", i)
		g.Elements = append(g.Elements, el+strings.Repeat("x", 120-len(el)))
	}
	return g
}

func TestOversizedListIsCappedAndReadBackAsTruncated(t *testing.T) {
	g := bigGroup(3000, 0)
	body := g.Body("ctx.")
	if len(body) > maxBody {
		t.Fatalf("body is %d bytes, GitHub rejects over %d", len(body), maxBody)
	}
	got := parseElements(body)
	if !got.ok || !got.truncated || got.total != 3000 || got.sha != digest(g.Elements) || len(got.els) >= 3000 {
		t.Fatalf("parse = total %d sha %q visible %d ok=%v trunc=%v", got.total, got.sha, len(got.els), got.ok, got.truncated)
	}
	if !strings.Contains(body, fmt.Sprintf("and %d more not shown.", 3000-len(got.els))) {
		t.Errorf("body does not say how many are not shown")
	}
	// The record appears only when there is something hidden.
	if small := grp("#a").Body(""); strings.Contains(small, "frostfall:elements") || strings.Contains(small, "more not shown") {
		t.Errorf("an untruncated body must not carry the record:\n%s", small)
	}
}

func TestUnchangedOversizedSetMakesNoCalls(t *testing.T) {
	g := bigGroup(3000, 0)
	c, calls := fakeGitHub(t, "")
	// Run 1 files it.
	cov := func(string, string) bool { return true }
	if _, err := Sync(c, []Group{g}, nil, cov, nil, "run 1."); err != nil {
		t.Fatal(err)
	}
	created := calls()
	if len(created) != 1 || created[0].Method != "POST" || len(created[0].Body["body"]) > maxBody {
		t.Fatalf("create = %+v", created)
	}
	existing := []Issue{{Number: 7, State: "open", Body: created[0].Body["body"]}}
	// Runs 2 and 3 find the same set: nothing to do, though the visible list
	// is only a prefix of it.
	for run := 2; run <= 3; run++ {
		if _, err := Sync(c, []Group{g}, existing, cov, nil, fmt.Sprintf("run %d.", run)); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls()) != 1 {
		t.Errorf("runs 2 and 3 made calls: %+v", calls()[1:])
	}
}

func TestOversizedGrowthCommentsOnceWithoutNames(t *testing.T) {
	old := bigGroup(3000, 0)
	cur := bigGroup(3005, 0)
	cov := func(string, string) bool { return true }
	existing := []Issue{{Number: 7, State: "open", Body: old.Body("run 1.")}}
	c, calls := fakeGitHub(t, "")
	if _, err := Sync(c, []Group{cur}, existing, cov, nil, "run 2."); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || got[0].Method != "PATCH" || got[1].Path != ipath(7)+"/comments" {
		t.Fatalf("want one body edit then one comment, got %+v", got)
	}
	if len(got[0].Body["body"]) > maxBody {
		t.Errorf("body is %d bytes", len(got[0].Body["body"]))
	}
	if want := "5 more failing elements (list truncated; see the issue body).\n\nrun 2."; got[1].Body["body"] != want {
		t.Errorf("comment = %q, want %q", got[1].Body["body"], want)
	}
	// Fed back, the new body is stable.
	again := []Issue{{Number: 7, State: "open", Body: got[0].Body["body"]}}
	if _, err := Sync(c, []Group{cur}, again, cov, nil, "run 3."); err != nil {
		t.Fatal(err)
	}
	if len(calls()) != 2 {
		t.Errorf("a settled oversized issue must not be touched again: %+v", calls()[2:])
	}
}

// The digest is over the sorted set, so the order results arrive in does not
// make an unchanged oversized set look changed.
func TestOversizedSetInADifferentOrderIsUnchanged(t *testing.T) {
	g := bigGroup(3000, 0)
	reordered := g
	reordered.Elements = nil
	for i := len(g.Elements) - 1; i >= 0; i-- {
		reordered.Elements = append(reordered.Elements, g.Elements[i])
	}
	existing := []Issue{{Number: 7, State: "open", Body: g.Body("")}}
	if got := plan(t, []Group{reordered}, existing, "home"); len(got) != 0 {
		t.Errorf("reordered but identical set must plan nothing, got %v", got)
	}
}

func TestOversizedShrinkAndSwapAreSilent(t *testing.T) {
	cov := func(string, string) bool { return true }
	old := bigGroup(3000, 0)
	existing := []Issue{{Number: 7, State: "open", Body: old.Body("")}}

	// Same count, different elements: a rewrite and no comment.
	c, calls := fakeGitHub(t, "")
	swapped := bigGroup(3000, 10)
	if _, err := Sync(c, []Group{swapped}, existing, cov, nil, "ctx."); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) != 1 || got[0].Method != "PATCH" {
		t.Errorf("swap: want a body edit only, got %+v", got)
	}

	// Shrinks to a set that fits: back to the plain visible form, silently,
	// and stable afterwards.
	c, calls = fakeGitHub(t, "")
	small := bigGroup(10, 0)
	if _, err := Sync(c, []Group{small}, existing, cov, nil, "ctx."); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 || got[0].Method != "PATCH" || strings.Contains(got[0].Body["body"], "frostfall:elements") {
		t.Fatalf("shrink: want a plain body edit only, got %+v", got)
	}
	again := []Issue{{Number: 7, State: "open", Body: got[0].Body["body"]}}
	if _, err := Sync(c, []Group{small}, again, cov, nil, "ctx."); err != nil {
		t.Fatal(err)
	}
	if len(calls()) != 1 {
		t.Errorf("settled: %+v", calls()[1:])
	}
}

func TestListThatOutgrowsTheBudgetNamesWhatFitsAndSettles(t *testing.T) {
	cov := func(string, string) bool { return true }
	old := bigGroup(10, 0)
	cur := bigGroup(3000, 0)
	existing := []Issue{{Number: 7, State: "open", Body: old.Body("")}}
	c, calls := fakeGitHub(t, "")
	if _, err := Sync(c, []Group{cur}, existing, cov, nil, "ctx."); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || got[0].Method != "PATCH" || got[1].Path != ipath(7)+"/comments" {
		t.Fatalf("calls = %+v", got)
	}
	comment := got[1].Body["body"]
	if len(got[0].Body["body"]) > maxBody || len(comment) > maxBody {
		t.Errorf("body %d, comment %d bytes: GitHub rejects over %d", len(got[0].Body["body"]), len(comment), maxBody)
	}
	if !strings.HasPrefix(comment, "2990 new failing elements:\n") || !strings.Contains(comment, "more not shown.") {
		t.Errorf("comment = %.120q...", comment)
	}
	again := []Issue{{Number: 7, State: "open", Body: got[0].Body["body"]}}
	if _, err := Sync(c, []Group{cur}, again, cov, nil, "ctx."); err != nil {
		t.Fatal(err)
	}
	if len(calls()) != 2 {
		t.Errorf("settled: %+v", calls()[2:])
	}
}

// --- list -> plan -> sync ---

func TestListPlanSyncEndToEnd(t *testing.T) {
	old := grp("#a")
	stale := Group{Rule: "image-alt", Page: "home", Impact: "critical", Summary: "s", Elements: els("#z")}
	issues := []map[string]any{
		// A web-edited (CRLF) body: must still match and gain #b.
		{"number": 7, "state": "open", "title": old.Title(), "body": crlf(old.Body("old."))},
		// A pull request carrying the label is not ours: it is neither
		// closed nor updated, though its marker names a group that is gone.
		{"number": 9, "state": "open", "title": stale.Title(), "body": stale.Body("old."), "pull_request": map[string]any{}},
		// A human's issue.
		{"number": 3, "state": "open", "title": "hand filed", "body": "keyboard nav is slow"},
	}
	c, calls := newFake(t, fakeAPI{issues: issues})
	listed, err := c.ListLabeled(Label)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Number != 7 || listed[1].Number != 3 {
		t.Fatalf("listed = %+v, want #7 and #3 (the pull request skipped)", listed)
	}
	cur := grp("#a", "#b")
	if _, err := Sync(c, []Group{cur}, listed, func(string, string) bool { return true }, nil, "ctx."); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, calls(), []call{
		bodyPatch(7, cur.Body("ctx.")),
		commentPost(7, "1 new failing element:\n\n- `#b`\n\nctx."),
	})
}
