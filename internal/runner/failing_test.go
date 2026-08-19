package runner

import (
	"testing"

	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/engine"
)

func res(test, rule string, impact engine.Impact, baselined bool) Result {
	return Result{
		Violation: engine.Violation{RuleID: rule, Impact: impact},
		TestID:    test,
		Baselined: baselined,
	}
}

func TestFailingSeverityFloor(t *testing.T) {
	def := config.Expect{Severity: "serious"}
	if !Failing(res("t", "label", engine.Critical, false), def, nil) {
		t.Errorf("critical above serious floor should fail")
	}
	if Failing(res("t", "label", engine.Moderate, false), def, nil) {
		t.Errorf("moderate below serious floor should not fail")
	}
	if Failing(res("t", "label", engine.Critical, true), def, nil) {
		t.Errorf("baselined violations never fail")
	}
}

func TestFailingEnforcedRuleBelowFloor(t *testing.T) {
	// Issue #6: a rules-only match below the severity floor must be counted
	// by reports exactly as the exit code counts it.
	def := config.Expect{Severity: "critical", Rules: []string{"image-alt"}}
	if !Failing(res("t", "image-alt", engine.Minor, false), def, nil) {
		t.Errorf("enforced rule at minor impact should fail")
	}
	if Failing(res("t", "color-contrast", engine.Serious, false), def, nil) {
		t.Errorf("serious below critical floor, not an enforced rule: should pass")
	}
}

func TestFailingPerTestOverride(t *testing.T) {
	def := config.Expect{Severity: "critical"}
	perTest := map[string]config.Expect{"strict": {Severity: "minor"}}
	if !Failing(res("strict", "label", engine.Minor, false), def, perTest) {
		t.Errorf("per-test floor should apply to its test")
	}
	if Failing(res("other", "label", engine.Minor, false), def, perTest) {
		t.Errorf("other tests keep the default floor")
	}
}
