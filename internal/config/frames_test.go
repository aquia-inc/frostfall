package config

import (
	"strings"
	"testing"
)

func TestFramesSetting(t *testing.T) {
	base := "version: 1\nserver:\n  baseUrl: http://localhost:5174\n"
	tests := "tests:\n  - id: absent\n    path: /\n  - id: off\n    path: /\n    frames: false\n  - id: on\n    path: /\n    frames: true\n  - id: bare\n    path: /\n    frames:\n"
	cases := []struct {
		name     string
		defaults string
		want     map[string]bool
	}{
		// Absent everywhere means on; a test's false opts it out.
		{"no default", "", map[string]bool{"absent": true, "off": false, "on": true, "bare": true}},
		// defaults.frames: false reaches tests with no setting of their own,
		// discovered ones included; a test's own value wins.
		{"default off", "defaults:\n  frames: false\n", map[string]bool{"absent": false, "off": false, "on": true}},
		{"default on", "defaults:\n  frames: on\n", map[string]bool{"absent": true, "off": false, "on": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, base+c.defaults+tests), "")
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range cfg.Tests {
				want, ok := c.want[tc.ID]
				if !ok {
					continue
				}
				if got := cfg.FramesFor(tc); got != want {
					t.Errorf("FramesFor(%s) = %v, want %v", tc.ID, got, want)
				}
			}
			// A discovered test is a bare Test: it follows the default.
			if got, want := cfg.FramesFor(Test{ID: "discovered", Path: "/x"}), c.want["absent"]; got != want {
				t.Errorf("FramesFor(discovered) = %v, want %v", got, want)
			}
		})
	}
}

func TestFramesProfileOverride(t *testing.T) {
	body := "version: 1\nserver:\n  baseUrl: http://localhost:5174\ndefaults:\n  frames: true\n" +
		"profiles:\n  quiet:\n    defaults:\n      frames: false\n" +
		"tests:\n  - id: home\n    path: /\n"
	path := writeConfig(t, body)
	for profile, want := range map[string]bool{"": true, "quiet": false} {
		cfg, err := Load(path, profile)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.FramesFor(cfg.Tests[0]); got != want {
			t.Errorf("profile %q: FramesFor = %v, want %v", profile, got, want)
		}
	}
}

// frames is a boolean like scan and auth, and a typo of the key is the
// usual unknown-key error.
func TestFramesRejectsNonBool(t *testing.T) {
	for body, wantIn := range map[string]string{
		"tests:\n  - id: home\n    path: /\n    frames: sometimes\n":    "sometimes",
		"defaults:\n  frames: [a]\ntests:\n  - id: home\n    path: /\n": "bool",
		"tests:\n  - id: home\n    path: /\n    frame: false\n":         "frame",
	} {
		_, err := Load(writeConfig(t, "version: 1\nserver:\n  baseUrl: http://localhost:5174\n"+body), "")
		if err == nil {
			t.Errorf("accepted:\n%s", body)
			continue
		}
		if !strings.Contains(err.Error(), wantIn) {
			t.Errorf("error lacks %q: %v", wantIn, err)
		}
	}
}
