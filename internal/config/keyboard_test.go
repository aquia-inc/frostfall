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
    keyboard: on
  - id: off
    path: /
    keyboard: false
    transcript:
  - id: full
    path: /
    keyboard: { maxTabs: 50, scope: main }
  - id: step
    path: /
    steps:
      - click: "#open"
      - keyboard:
      - keyboard: { label: dialog, scope: "[role=dialog]" }
      - transcript:
      - expectFocus: { within: "[role=dialog]", name: Close }
`

func TestKeyboardForms(t *testing.T) {
	cfg, err := Load(writeConfig(t, keyboardConfig), "")
	if err != nil {
		t.Fatal(err)
	}
	on, off, full, step := cfg.Tests[0], cfg.Tests[1], cfg.Tests[2], cfg.Tests[3]
	if on.Keyboard == nil || on.Keyboard.Off {
		t.Errorf("keyboard: on decoded as %+v", on.Keyboard)
	}
	if off.Keyboard == nil || !off.Keyboard.Off {
		t.Errorf("keyboard: false decoded as %+v", off.Keyboard)
	}
	// A bare key on a test means on, as it does on a step.
	if off.Transcript == nil || off.Transcript.Off {
		t.Errorf("bare transcript: on a test decoded as %+v", off.Transcript)
	}
	if full.Keyboard.MaxTabs != 50 || full.Keyboard.Scope != "main" {
		t.Errorf("full form decoded as %+v", full.Keyboard)
	}
	if step.Steps[1].Keyboard == nil {
		t.Error("bare - keyboard: step decoded as no action")
	}
	if step.Steps[3].Transcript == nil {
		t.Error("bare - transcript: step decoded as no action")
	}
	if e := step.Steps[4].ExpectFocus; e == nil || e.Within != "[role=dialog]" || e.Name != "Close" {
		t.Errorf("expectFocus decoded as %+v", e)
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
		"expectFocus with nothing to check": {`
tests:
  - id: a
    path: /
    steps:
      - expectFocus: { label: x }`, "at least one of within, role, name"},
		"unknown key in expectFocus": {`
tests:
  - id: a
    path: /
    steps:
      - expectFocus: { whithin: main }`, "field whithin not found"},
		"transcript false as a step": {`
tests:
  - id: a
    path: /
    steps: [{transcript: false}]`, "only meaningful on a test"},
		"frostfall rule in a scan step": {`
tests:
  - id: a
    path: /
    steps:
      - scan: { rules: { frostfall/focus-not-visible: off } }`, "is a frostfall/ rule"},
		"axe rule in a keyboard step": {`
tests:
  - id: a
    path: /
    steps:
      - keyboard: { rules: { color-contrast: off } }`, "is an axe rule"},
		"focus-expectation in a keyboard step": {`
tests:
  - id: a
    path: /
    steps:
      - keyboard: { rules: { frostfall/focus-expectation: off } }`, "applies to expectFocus steps"},
		"keyboard toggle that is not a boolean": {`
tests: [{id: a, path: /, keyboard: sometimes}]`, "want true, false, or a mapping"},
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

func TestKeyboardDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, `version: 1
server:
  baseUrl: http://x
defaults:
  keyboard:
    maxTabs: 400
    rules: { frostfall/focus-hidden: off, frostfall/dialog-focus: off }
profiles:
  quick:
    defaults:
      keyboard: false
tests:
  - id: inherits
    path: /
  - id: opts-out
    path: /
    keyboard: false
  - id: own
    path: /
    keyboard: { maxTabs: 50, rules: { frostfall/dialog-focus: on } }
  - id: bare
    path: /
    keyboard: true
`), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeyboardFor(cfg.Tests[0]) == nil {
		t.Error("a test without its own setting did not inherit defaults.keyboard")
	}
	// Discovered tests are synthesized with only an id and path.
	if cfg.KeyboardFor(Test{ID: "discovered:/x", Path: "/x"}) == nil {
		t.Error("a discovered test did not inherit defaults.keyboard")
	}
	if cfg.KeyboardFor(cfg.Tests[1]) != nil {
		t.Error("keyboard: false did not opt the test out")
	}
	// A test's own setting layers over the default rather than replacing it.
	if k := cfg.KeyboardFor(cfg.Tests[2]); k == nil || k.MaxTabs != 50 ||
		k.Rules["frostfall/focus-hidden"] || !k.Rules["frostfall/dialog-focus"] {
		t.Errorf("own setting over the default: %+v", k)
	}
	if k := cfg.KeyboardFor(cfg.Tests[3]); k == nil || k.MaxTabs != 400 || k.Rules["frostfall/focus-hidden"] {
		t.Errorf("keyboard: true lost the default's maxTabs and rules: %+v", k)
	}
	if cfg.Defaults.Keyboard.Rules["frostfall/dialog-focus"] {
		t.Error("layering a test's rules mutated the default's")
	}

	quick, err := Load(writeConfig(t, `version: 1
server:
  baseUrl: http://x
defaults:
  keyboard: true
profiles:
  quick:
    defaults:
      keyboard: false
tests: [{id: a, path: /}]
`), "quick")
	if err != nil {
		t.Fatal(err)
	}
	if quick.KeyboardFor(quick.Tests[0]) != nil {
		t.Error("a profile's keyboard: false did not override the default")
	}

	none, err := Load(writeConfig(t, "version: 1\nserver:\n  baseUrl: http://x\ntests: [{id: a, path: /}]\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if none.KeyboardFor(none.Tests[0]) != nil {
		t.Error("keyboard audits ran with nothing configured; they must stay opt-in")
	}

	if _, err := Load(writeConfig(t, "version: 1\nserver:\n  baseUrl: http://x\ndefaults:\n  keyboard: { scope: main }\ntests: [{id: a, path: /}]\n"), ""); err == nil ||
		!strings.Contains(err.Error(), "label and scope are per test") {
		t.Errorf("scope on defaults.keyboard: err = %v", err)
	}
}
