package github

import (
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
	return Plan(groups, existing, testsRun)
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
