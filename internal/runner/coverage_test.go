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
	run := &Run{ran: map[string]map[string]bool{"page": {checkKeyboard: true}}}
	r.classify(run)
	if len(run.Stale) != 1 || run.Stale[0].Fingerprint != "kb" {
		t.Errorf("stale = %+v, want only the keyboard entry", run.Stale)
	}
}
