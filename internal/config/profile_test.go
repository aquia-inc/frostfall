package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const profileConfig = `version: 1
server:
  baseUrl: http://localhost:5174
defaults:
  standard: section508
baseline: .frostfall-baseline.json
profiles:
  ci:
    server:
      serve: ./dist
    defaults:
      expect:
        severity: serious
tests:
  - id: home
    path: /
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frostfall.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProfileOverridesServerAndExpect(t *testing.T) {
	cfg, err := Load(writeConfig(t, profileConfig), "ci")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Serve != "./dist" || cfg.Server.BaseURL != "" {
		t.Errorf("server not replaced by profile: %+v", cfg.Server)
	}
	if !cfg.Defaults.Expect.Enforcing() || cfg.Defaults.Expect.Severity != "serious" {
		t.Errorf("expect not overlaid: %+v", cfg.Defaults.Expect)
	}
	// Unset profile fields keep base values.
	if cfg.Defaults.Standard != "section508" {
		t.Errorf("standard lost in merge: %q", cfg.Defaults.Standard)
	}
	if cfg.Baseline != ".frostfall-baseline.json" {
		t.Errorf("baseline lost in merge: %q", cfg.Baseline)
	}
}

func TestNoProfileKeepsBase(t *testing.T) {
	for _, req := range []string{"", ProfileNone} {
		cfg, err := Load(writeConfig(t, profileConfig), req)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.BaseURL != "http://localhost:5174" || cfg.Defaults.Expect.Enforcing() {
			t.Errorf("profile %q leaked into base config", req)
		}
	}
}

func TestProfileAutoAppliesCIWhenDefined(t *testing.T) {
	cfg, err := Load(writeConfig(t, profileConfig), ProfileAuto)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Serve != "./dist" {
		t.Errorf("auto did not apply the ci profile")
	}
}

func TestProfileAutoNoopWithoutCIProfile(t *testing.T) {
	plain := strings.Replace(profileConfig, "  ci:", "  staging:", 1)
	cfg, err := Load(writeConfig(t, plain), ProfileAuto)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.BaseURL != "http://localhost:5174" {
		t.Errorf("auto applied something without a ci profile")
	}
}

func TestUnknownProfileErrors(t *testing.T) {
	if _, err := Load(writeConfig(t, profileConfig), "staging"); err == nil {
		t.Errorf("unknown profile name did not error")
	}
}

func TestProfileCannotCarryTests(t *testing.T) {
	bad := strings.Replace(profileConfig, "    server:\n      serve: ./dist",
		"    tests:\n      - id: sneaky\n        path: /x", 1)
	if _, err := Load(writeConfig(t, bad), "ci"); err == nil {
		t.Errorf("profile with tests key did not fail strict decoding")
	}
}
