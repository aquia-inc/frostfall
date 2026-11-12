package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const authPrefix = "version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\nauth:\n"

const validSetup = `  setup:
    path: /login
    steps:
      - fill: { "#email": "me@example.com" }
      - fill: { "#password": "${AUTH_TEST_PASS:-x}" }
      - click: "button[type=submit]"
`

const goodState = `{"cookies":[{"name":"sid","value":"s3cret","domain":"x","path":"/","expires":-1,
"httpOnly":true,"secure":false,"sameSite":"Lax"}],"origins":[{"origin":"http://x","localStorage":[{"name":"token","value":"t0k"}]}]}`

// writeAt writes content next to a config in dir and returns the config path.
func writeConfigIn(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "frostfall.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(goodState), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"cookies": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		auth string
		want string // empty means it must load
	}{
		{"setup", validSetup, ""},
		{"storageState", "  storageState: state.json\n  loginPath: /login\n", ""},
		{"setup with explicit reuse", validSetup + "  reuse: false\n", ""},
		{"both setup and storageState", validSetup + "  storageState: state.json\n", "not both"},
		{"neither", "  loginPath: /login\n", "exactly one of setup and storageState"},
		{"missing path", "  setup:\n    steps:\n      - click: a\n", "auth.setup.path: required"},
		{"relative path without slash", "  setup:\n    path: login\n    steps:\n      - click: a\n", "auth.setup.path: required"},
		{"empty steps", "  setup:\n    path: /login\n    steps: []\n", "at least one step"},
		{"no steps key", "  setup:\n    path: /login\n", "at least one step"},
		{"scan in setup", "  setup:\n    path: /login\n    steps:\n      - scan: {}\n", "steps[0]: scan is not allowed in setup"},
		{"keyboard in setup", "  setup:\n    path: /login\n    steps:\n      - click: a\n      - keyboard:\n", "steps[1]: keyboard is not allowed"},
		{"transcript in setup", "  setup:\n    path: /login\n    steps:\n      - transcript:\n", "transcript is not allowed"},
		{"expectFocus in setup", "  setup:\n    path: /login\n    steps:\n      - expectFocus: { role: button }\n", "expectFocus is not allowed"},
		{"invalid step", "  setup:\n    path: /login\n    steps:\n      - click: a\n        press: Enter\n", "exactly one action"},
		{"reuse with storageState", "  storageState: state.json\n  reuse: true\n", "auth.reuse: only applies to setup"},
		{"missing storageState file", "  storageState: nope.json\n", "auth.storageState nope.json"},
		{"malformed JSON", "  storageState: bad.json\n", "not a storage-state JSON file"},
		{"loginPath needs a slash", validSetup + "  loginPath: login\n", "auth.loginPath"},
		{"unknown auth key", validSetup + "  logInPath: /x\n", "field logInPath not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigIn(t, dir, authPrefix+tc.auth), "")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestStorageStateRelativeToConfigDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".auth", "state.json"), []byte(goodState), 0o600); err != nil {
		t.Fatal(err)
	}
	// The working directory is somewhere else entirely: resolution must
	// follow the config file, not the process.
	t.Chdir(t.TempDir())
	cfg, err := Load(writeConfigIn(t, dir, authPrefix+"  storageState: .auth/state.json\n  loginPath: /login\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	st := cfg.Auth.State
	if st == nil || len(st.Cookies) != 1 || st.Cookies[0].Name != "sid" || st.Origins[0].LocalStorage[0].Name != "token" {
		t.Errorf("state not loaded from the config's directory: %v", st)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings with loginPath set: %v", cfg.Warnings)
	}
}

func TestStorageStateContentErrors(t *testing.T) {
	cases := map[string]string{
		"cookie without name":   `{"cookies":[{"value":"v","domain":"x"}]}`,
		"cookie without domain": `{"cookies":[{"name":"n","value":"v"}]}`,
		"bad sameSite":          `{"cookies":[{"name":"n","value":"v","domain":"x","sameSite":"Sideways"}]}`,
		"origin not a URL":      `{"origins":[{"origin":"example.com","localStorage":[]}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "s.json"), []byte(content), 0o600)
			_, err := Load(writeConfigIn(t, dir, authPrefix+"  storageState: s.json\n"), "")
			if err == nil {
				t.Fatal("want a config error")
			}
			if strings.Contains(err.Error(), `"v"`) {
				t.Errorf("error echoes a cookie value: %v", err)
			}
		})
	}
}

func TestStorageStateWithoutLoginPathWarns(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s.json"), []byte(goodState), 0o600)
	cfg, err := Load(writeConfigIn(t, dir, authPrefix+"  storageState: s.json\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "loginPath") {
		t.Errorf("warnings = %v, want one about loginPath", cfg.Warnings)
	}
}

func TestAuthDefaults(t *testing.T) {
	cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup), "")
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if !a.ReuseSetup() {
		t.Error("reuse should default to true")
	}
	if a.LoginRoute() != "/login" {
		t.Errorf("login route = %q, want setup.path", a.LoginRoute())
	}
	cfg, err = Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+"  reuse: false\n  loginPath: /signin\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.ReuseSetup() || cfg.Auth.LoginRoute() != "/signin" {
		t.Errorf("explicit reuse/loginPath not honored: %+v", cfg.Auth)
	}
}

func TestSecretLint(t *testing.T) {
	const literal = "hunter2-literal-value"
	t.Setenv("FF_LINT_PASS", "from-env")
	cases := []struct {
		name  string
		fill  string
		warns int
	}{
		{"literal password", `{ "#password": "` + literal + `" }`, 1},
		{"env reference", `{ "#password": "${FF_LINT_PASS}" }`, 0},
		{"env reference with default", `{ "#password": "${FF_LINT_PASS:-fallback}" }`, 0},
		{"unquoted env reference in flow mapping", `{ "#password": ${FF_LINT_PASS} }`, 0},
		{"type=password selector", `{ "input[type=password]": "` + literal + `" }`, 1},
		{"passwd selector, any case", `{ "#UserPasswd": "` + literal + `" }`, 1},
		{"literal in a username field is fine", `{ "#email": "` + literal + `" }`, 0},
		{"one of two fields", `{ "#email": "a@b.c", "#password": "` + literal + `" }`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+"  setup:\n    path: /login\n    steps:\n      - fill: "+tc.fill+"\n  loginPath: /login\n"), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Warnings) != tc.warns {
				t.Fatalf("warnings = %v, want %d", cfg.Warnings, tc.warns)
			}
			for _, w := range cfg.Warnings {
				if strings.Contains(w, literal) {
					t.Errorf("warning echoes the value: %q", w)
				}
				if !strings.Contains(w, "#password") && !strings.Contains(w, "type=password") && !strings.Contains(w, "#UserPasswd") {
					t.Errorf("warning does not name the field: %q", w)
				}
			}
		})
	}
}
