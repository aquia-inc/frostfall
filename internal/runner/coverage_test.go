package runner

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aquia-inc/frostfall/internal/baseline"
	"github.com/aquia-inc/frostfall/internal/keyboard"
)

// A test that ran only a keyboard audit looked for keyboard rules, not axe
// ones: its baselined axe entry is unchecked, not fixed.
func TestStaleOnlyWhereTheRulesCheckRan(t *testing.T) {
	r := &Runner{Baseline: &baseline.File{Violations: []baseline.Violation{
		{Fingerprint: "axe", TestID: "page", RuleID: "color-contrast"},
		{Fingerprint: "kb", TestID: "page", RuleID: keyboard.RuleNotVisible},
		{Fingerprint: "focus", TestID: "page", RuleID: keyboard.RuleFocusExpectation},
	}}}
	cov := newCoverage()
	cov.record(checkKeyboard, nil)
	run := &Run{ran: map[string]*coverage{"page": cov}}
	r.classify(run)
	if len(run.Stale) != 1 || run.Stale[0].Fingerprint != "kb" {
		t.Errorf("stale = %+v, want only the keyboard entry", run.Stale)
	}
}

// A keyboard audit cut short by a trap evaluated only the trap: baselined
// focus-not-visible and focus-unreachable entries on that page are unchecked,
// not fixed, and must not go stale (or have their issues closed).
func TestTrapLeavesRulesUnchecked(t *testing.T) {
	r := &Runner{Baseline: &baseline.File{Violations: []baseline.Violation{
		{Fingerprint: "ring", TestID: "page", RuleID: keyboard.RuleNotVisible},
		{Fingerprint: "unreached", TestID: "page", RuleID: keyboard.RuleUnreachable},
		{Fingerprint: "trap", TestID: "page", RuleID: keyboard.RuleTrap},
	}}}
	cov := newCoverage()
	cov.record(checkKeyboard, withUnchecked(nil, []string{keyboard.RuleNotVisible, keyboard.RuleUnreachable}))
	run := &Run{ran: map[string]*coverage{"page": cov}}
	r.classify(run)
	if len(run.Stale) != 1 || run.Stale[0].Fingerprint != "trap" {
		t.Errorf("stale = %+v, want only the trap entry", run.Stale)
	}
}

// A rule switched off was not looked for: its entries are not fixed. One
// step switching it off does not hide another that checked it.
func TestRuleSwitchedOffIsNotCovered(t *testing.T) {
	off := map[string]bool{"scrollable-region-focusable": false}
	cov := newCoverage()
	cov.record(checkAxe, off)
	run := &Run{ran: map[string]*coverage{"page": cov}}
	if run.Covered("page", "scrollable-region-focusable") {
		t.Error("a rule off in the only scan counted as covered")
	}
	if !run.Covered("page", "color-contrast") {
		t.Error("a rule left on was not covered")
	}
	cov.record(checkAxe, nil)
	if !run.Covered("page", "scrollable-region-focusable") {
		t.Error("a rule checked by a second scan was not covered")
	}
}

// Checks and Uncovered expose what the run tracks to the reports: sorted, and
// empty (never nil) for a test that was not seen.
func TestChecksAndUncoveredAccessors(t *testing.T) {
	cov := newCoverage()
	cov.record(checkAxe, map[string]bool{"scrollable-region-focusable": false, "color-contrast": true})
	cov.record(checkKeyboard, withUnchecked(nil, []string{keyboard.RuleUnreachable, keyboard.RuleNotVisible}))
	run := &Run{ran: map[string]*coverage{"page": cov}}

	if got := run.Checks("page"); len(got) != 2 || got[0] != "axe" || got[1] != "keyboard" {
		t.Errorf("Checks = %v", got)
	}
	got := run.Uncovered("page")
	want := []string{keyboard.RuleNotVisible, keyboard.RuleUnreachable, "scrollable-region-focusable"}
	if len(got) != len(want) {
		t.Fatalf("Uncovered = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Uncovered = %v, want %v", got, want)
		}
	}
	if run.Checks("other") == nil || run.Uncovered("other") == nil {
		t.Error("an unseen test must give empty lists, not nil")
	}
	cov.record(checkAxe, nil)
	if u := run.Uncovered("page"); len(u) != 2 {
		t.Errorf("a second scan that checked the rule should clear it: %v", u)
	}
}

// A scan, a keyboard audit and a focus assertion that share a label are one
// scan point, in the order they first ran.
func TestScanPointsAreDeduplicatedByLabel(t *testing.T) {
	cov := newCoverage()
	cov.point("initial", "http://x/")
	cov.point("dialog", "http://x/")
	cov.point("initial", "http://x/")
	cov.point("dialog", "http://x/")
	if len(cov.points) != 2 || cov.points[0].Label != "initial" || cov.points[1].Label != "dialog" {
		t.Errorf("points = %+v", cov.points)
	}
}

// The run's error text is unchanged by the typed error, and the test is
// recoverable from it.
func TestTestErrorKeepsTheMessage(t *testing.T) {
	cause := errors.New("boom")
	var err error = &TestError{TestID: "home", Err: cause}
	if err.Error() != `test "home": boom` {
		t.Errorf("Error() = %q", err.Error())
	}
	var te *TestError
	if !errors.As(fmt.Errorf("wrapped: %w", err), &te) || te.TestID != "home" || !errors.Is(err, cause) {
		t.Error("TestError does not unwrap")
	}
}
