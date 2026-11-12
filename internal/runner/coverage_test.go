package runner

import (
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
