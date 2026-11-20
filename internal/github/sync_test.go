package github

import (
	"encoding/json"
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
	return Plan(groups, existing, func(page, _ string) bool { return testsRun[page] })
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

// call is one request the fake GitHub API received.
type call struct {
	Method string
	Path   string
	Body   map[string]string
}

// fakeGitHub records every write Sync makes. A PATCH to failPath answers 500.
func fakeGitHub(t *testing.T, failPath string) (*Client, func() []call) {
	t.Helper()
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := call{Method: r.Method, Path: r.URL.Path, Body: map[string]string{}}
		_ = json.Unmarshal(raw, &c.Body)
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		if failPath != "" && r.URL.Path == failPath && r.Method == "PATCH" {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{}`))
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

func els(e ...string) []string { return e }

func grp(elements ...string) Group {
	return Group{Rule: "label", Page: "home", Impact: "critical", Summary: "s", Elements: elements}
}

func syncOnce(t *testing.T, c *Client, g Group, existing []Issue) ([]Action, error) {
	t.Helper()
	return Sync(c, []Group{g}, existing, func(string, string) bool { return true }, "ctx.")
}

const issuePath = "/repos/org/repo/issues/7"

func TestSyncGrownSetUpdatesBodyAndCommentsNewOnly(t *testing.T) {
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
	got := calls()
	if len(got) != 2 {
		t.Fatalf("want comment + body patch, got %+v", got)
	}
	wantComment := "2 new failing elements:\n\n- `#c`\n- `#d`\n\nctx."
	if got[0].Path != issuePath+"/comments" || got[0].Body["body"] != wantComment {
		t.Errorf("comment = %+v, want body %q", got[0], wantComment)
	}
	if strings.Contains(got[0].Body["body"], "#a") {
		t.Errorf("comment must list only new elements: %q", got[0].Body["body"])
	}
	if got[1].Method != "PATCH" || got[1].Path != issuePath || got[1].Body["body"] != cur.Body("ctx.") {
		t.Errorf("body patch = %+v", got[1])
	}
	if _, has := got[1].Body["state"]; has {
		t.Errorf("update must not touch state: %+v", got[1])
	}
}

func TestSyncSingleNewElementIsSingular(t *testing.T) {
	existing := []Issue{{Number: 7, State: "open", Body: grp("#a").Body("")}}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, grp("#a", "#b"), existing); err != nil {
		t.Fatal(err)
	}
	if b := calls()[0].Body["body"]; !strings.HasPrefix(b, "1 new failing element:\n") {
		t.Errorf("comment = %q", b)
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
	got := calls()
	if len(got) != 1 || got[0].Method != "PATCH" || got[0].Path != issuePath {
		t.Fatalf("want one body patch and no comment, got %+v", got)
	}
	if strings.Contains(got[0].Body["body"], "#b") {
		t.Errorf("removed element still in body: %q", got[0].Body["body"])
	}
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
	if err != nil || len(actions) != 0 || len(calls()) != 0 {
		t.Errorf("identical set: actions=%v err=%v calls=%v", actions, err, calls())
	}
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
	got := calls()
	if len(got) != 3 {
		t.Fatalf("want comment, body patch, state patch, got %+v", got)
	}
	if got[0].Path != issuePath+"/comments" ||
		got[0].Body["body"] != "Recurred.\n\n1 new failing element:\n\n- `#b`\n\nctx." {
		t.Errorf("comment = %+v", got[0])
	}
	if got[1].Body["body"] != cur.Body("ctx.") {
		t.Errorf("body patch = %+v", got[1])
	}
	if got[2].Body["state"] != "open" {
		t.Errorf("state patch = %+v", got[2])
	}
}

func TestSyncReopenSameElementsKeepsOldBehavior(t *testing.T) {
	g := grp("#a")
	existing := []Issue{{Number: 7, State: "closed", Body: g.Body("")}}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, g, existing); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || got[0].Body["body"] != "Recurred. ctx." || got[1].Body["state"] != "open" {
		t.Errorf("want Recurred comment then reopen, got %+v", got)
	}
}

func TestSyncReopenWithOnlyRemovalsIsSilentBodyUpdate(t *testing.T) {
	existing := []Issue{{Number: 7, State: "closed", Body: grp("#a", "#b").Body("")}}
	c, calls := fakeGitHub(t, "")
	if _, err := syncOnce(t, c, grp("#a"), existing); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 3 || got[0].Body["body"] != "Recurred. ctx." || got[1].Body["body"] != grp("#a").Body("ctx.") ||
		got[2].Body["state"] != "open" {
		t.Errorf("got %+v", got)
	}
}

func TestParseElementsOldFormatBody(t *testing.T) {
	// The shape of a body filed before this change: plain single-backtick
	// bullets and nothing else recording the elements.
	body := "Frostfall detected **label** (critical) on **home**.\n\n> s\n\n" +
		"Affected element(s):\n- `#a`\n- `input[name=\"q\"] (after-open)`\n\n" +
		"[How to fix label](https://example.test)\n\nFiled by Frostfall.\n\n" +
		"<!-- frostfall:v1:rule=label:page=home -->\n"
	got, ok := parseElements(body)
	if !ok || !reflect.DeepEqual(got, els("#a", `input[name="q"] (after-open)`)) {
		t.Fatalf("parseElements = %v, %v", got, ok)
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

func TestSyncUnreadableListRefreshesBodyWithoutClaimingNewElements(t *testing.T) {
	body := "Frostfall detected something.\n\nSee notes below.\n\n<!-- frostfall:v1:rule=label:page=home -->\n"
	existing := []Issue{{Number: 7, State: "open", Body: body}}
	c, calls := fakeGitHub(t, "")
	actions, err := syncOnce(t, c, grp("#a"), existing)
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(actions) != 1 || actions[0].Kind != "update" || len(got) != 1 || got[0].Method != "PATCH" {
		t.Fatalf("want a silent body refresh, got actions=%v calls=%+v", actions, got)
	}
	if s := actions[0].String(); s != "update #7 Accessibility: label on home (refresh element list)" {
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
	actions, err := Sync(c, []Group{g1, g2}, existing, func(string, string) bool { return true }, "ctx.")
	if err == nil || !strings.Contains(err.Error(), "#7") || strings.Contains(err.Error(), "#8 ") {
		t.Errorf("want error naming only #7, got %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("want both actions planned, got %v", actions)
	}
	var patched8 bool
	for _, cl := range calls() {
		if cl.Method == "PATCH" && cl.Path == "/repos/org/repo/issues/8" {
			patched8 = true
		}
	}
	if !patched8 {
		t.Errorf("a failure on #7 must not stop #8: %+v", calls())
	}
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

func TestBodyKeepsBacktickSelectorInsideCodeSpan(t *testing.T) {
	g := grp("a[title=\"`@everyone`\"]")
	body := g.Body("")
	if !strings.Contains(body, "- ``a[title=\"`@everyone`\"]``\n") {
		t.Errorf("selector not fenced safely:\n%s", body)
	}
	got, ok := parseElements(body)
	if !ok || !reflect.DeepEqual(got, g.Elements) {
		t.Errorf("fenced element did not round-trip: %v %v", got, ok)
	}
}

func TestBuildGroupsFlattensLineBreaksInSelectors(t *testing.T) {
	groups := BuildGroups([]runner.Result{result("label", "home", "initial", "a[x=\"1\n2\"]")})
	if len(groups) != 1 || groups[0].Elements[0] != "a[x=\"1 2\"]" {
		t.Fatalf("got %+v", groups)
	}
	if got, ok := parseElements(groups[0].Body("")); !ok || !reflect.DeepEqual(got, groups[0].Elements) {
		t.Errorf("round trip failed: %v %v", got, ok)
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
	}
	for _, c := range cases {
		if got := c.a.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}
