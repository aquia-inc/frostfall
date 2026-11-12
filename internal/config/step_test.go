package config

import (
	"strings"
	"testing"
)

const stepPrefix = "version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\n    steps:\n"

// A bare `- scan:` means all defaults (DESIGN.md §4); yaml.v3 decodes the
// null to a nil pointer, which used to fail as a step with no action.
func TestBareScanStep(t *testing.T) {
	cfg, err := Load(writeConfig(t, stepPrefix+"      - scan:\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tests[0].Steps[0].Scan == nil {
		t.Error("bare - scan: decoded as no action")
	}
}

// Unknown keys must fail inside custom-decoded blocks too, not only at the
// top level: node.Decode does not inherit KnownFields.
func TestUnknownKeysInSteps(t *testing.T) {
	cases := map[string]struct{ step, want string }{
		"inside scan":       {"      - scan: { lable: modal }\n", "field lable not found"},
		"misspelled action": {"      - clik: '#open'\n", "field clik not found"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, stepPrefix+tc.step), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
