package config

import (
	"strings"
	"testing"
)

const keyboardConfig = `version: 1
server:
  baseUrl: http://localhost:5174
defaults:
  rules:
    frostfall/focus-not-visible: off
tests:
  - id: on
    path: /
    keyboard: true
  - id: off
    path: /
    keyboard: false
  - id: full
    path: /
    keyboard: { maxTabs: 50, scope: main }
  - id: step
    path: /
    steps:
      - click: "#open"
      - keyboard:
      - keyboard: { label: dialog, scope: "[role=dialog]" }
`

func TestKeyboardForms(t *testing.T) {
	cfg, err := Load(writeConfig(t, keyboardConfig), "")
	if err != nil {
		t.Fatal(err)
	}
	on, off, full, step := cfg.Tests[0], cfg.Tests[1], cfg.Tests[2], cfg.Tests[3]
	if on.Keyboard == nil || on.Keyboard.Off {
		t.Errorf("keyboard: true decoded as %+v", on.Keyboard)
	}
	if off.Keyboard == nil || !off.Keyboard.Off {
		t.Errorf("keyboard: false decoded as %+v", off.Keyboard)
	}
	if full.Keyboard.MaxTabs != 50 || full.Keyboard.Scope != "main" {
		t.Errorf("full form decoded as %+v", full.Keyboard)
	}
	if step.Steps[1].Keyboard == nil {
		t.Error("bare - keyboard: step decoded as no action")
	}
	if step.Steps[2].Keyboard.Label != "dialog" {
		t.Errorf("keyboard step decoded as %+v", step.Steps[2].Keyboard)
	}
}

func TestKeyboardValidation(t *testing.T) {
	cases := map[string]struct {
		yaml, wantErr string
	}{
		"unknown frostfall rule": {`
defaults:
  rules:
    frostfall/focus-visible: off
tests: [{id: a, path: /}]`, `unknown rule "frostfall/focus-visible"`},
		"unknown rule on a keyboard step": {`
tests:
  - id: a
    path: /
    steps:
      - keyboard: { rules: { frostfall/trap: off } }`, `unknown rule "frostfall/trap"`},
		"label on the test-level audit": {`
tests: [{id: a, path: /, keyboard: {label: x}}]`, "always labelled initial"},
		"keyboard false as a step": {`
tests:
  - id: a
    path: /
    steps: [{keyboard: false}]`, "only meaningful on a test"},
		"unknown key inside a keyboard step": {`
tests:
  - id: a
    path: /
    steps:
      - keyboard: { scop: main }`, `field scop not found`},
		"negative maxTabs": {`
tests: [{id: a, path: /, keyboard: {maxTabs: -1}}]`, "must not be negative"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "version: 1\nserver:\n  baseUrl: http://x\n"+strings.TrimPrefix(tc.yaml, "\n")), "")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
