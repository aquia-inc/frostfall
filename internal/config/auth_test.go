package config

import (
	"os"
	"path/filepath"
	"slices"
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

// signed is the signedIn line every auth block needs.
const signed = "  signedIn: \"#shell\"\n"

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
	validSetup := validSetup + signed
	cases := []struct {
		name string
		auth string
		want string // empty means it must load
	}{
		{"setup", validSetup, ""},
		{"storageState", "  storageState: state.json\n" + signed, ""},
		{"storageState with loginPath", "  storageState: state.json\n  loginPath: /login\n" + signed, ""},
		{"setup without signedIn", validSetup[:len(validSetup)-len(signed)], "auth.signedIn: required"},
		{"storageState without signedIn", "  storageState: state.json\n  loginPath: /login\n", "auth.signedIn: required"},
		{"setup with explicit reuse", validSetup + "  reuse: false\n", ""},
		{"both setup and storageState", validSetup + "  storageState: state.json\n", "not both"},
		{"neither", "  loginPath: /login\n" + signed, "exactly one of setup and storageState"},
		{"missing path", "  setup:\n    steps:\n      - click: a\n" + signed, "auth.setup.path: required"},
		{"relative path without slash", "  setup:\n    path: login\n    steps:\n      - click: a\n" + signed, "auth.setup.path: required"},
		{"empty steps", "  setup:\n    path: /login\n    steps: []\n" + signed, "at least one step"},
		{"no steps key", "  setup:\n    path: /login\n" + signed, "at least one step"},
		{"scan in setup", "  setup:\n    path: /login\n    steps:\n      - scan: {}\n" + signed, "steps[0]: scan is not allowed in setup"},
		{"keyboard in setup", "  setup:\n    path: /login\n    steps:\n      - click: a\n      - keyboard:\n" + signed, "steps[1]: keyboard is not allowed"},
		{"transcript in setup", "  setup:\n    path: /login\n    steps:\n      - transcript:\n" + signed, "transcript is not allowed"},
		{"expectFocus in setup", "  setup:\n    path: /login\n    steps:\n      - expectFocus: { role: button }\n" + signed, "expectFocus is not allowed"},
		{"invalid step", "  setup:\n    path: /login\n    steps:\n      - click: a\n        press: Enter\n" + signed, "exactly one action"},
		{"reuse with storageState", "  storageState: state.json\n  loginPath: /login\n  reuse: true\n" + signed, "auth.reuse: only applies to setup"},
		{"missing storageState file", "  storageState: nope.json\n" + signed, "auth.storageState nope.json"},
		{"malformed JSON", "  storageState: bad.json\n" + signed, "not a storage-state JSON file"},
		{"allowedOrigins", validSetup + "  allowedOrigins: [\"https://login.example.com\", \"http://localhost:9000\"]\n", ""},
		{"allowedOrigins relative", validSetup + "  allowedOrigins: [\"/sso\"]\n", "auth.allowedOrigins[0]"},
		{"allowedOrigins with a path", validSetup + "  allowedOrigins: [\"https://idp.example.com/realm\"]\n", "auth.allowedOrigins[0]"},
		{"allowedOrigins wrong scheme", validSetup + "  allowedOrigins: [\"ftp://idp.example.com\"]\n", "auth.allowedOrigins[0]"},
		{"allowedOrigins bare host", validSetup + "  allowedOrigins: [\"idp.example.com\"]\n", "auth.allowedOrigins[0]"},
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
	cfg, err := Load(writeConfigIn(t, dir, authPrefix+"  storageState: .auth/state.json\n  loginPath: /login\n"+signed), "")
	if err != nil {
		t.Fatal(err)
	}
	st := cfg.Auth.State
	if st == nil || len(st.Cookies) != 1 || st.Cookies[0].Name != "sid" || st.Origins[0].LocalStorage[0].Name != "token" {
		t.Errorf("state not loaded from the config's directory: %v", st)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
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
			_, err := Load(writeConfigIn(t, dir, authPrefix+"  storageState: s.json\n  loginPath: /login\n"+signed), "")
			if err == nil {
				t.Fatal("want a config error")
			}
			if strings.Contains(err.Error(), `"v"`) {
				t.Errorf("error echoes a cookie value: %v", err)
			}
		})
	}
}

func TestAllowedOriginsAreNormalized(t *testing.T) {
	cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+signed+
		"  allowedOrigins: [\"HTTPS://Login.Example.com:443\", \"http://localhost:80/\", \"https://idp.example.com:8443\"]\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://login.example.com", "http://localhost", "https://idp.example.com:8443"}
	if !slices.Equal(cfg.Auth.AllowedOrigins, want) {
		t.Errorf("allowedOrigins = %v, want %v", cfg.Auth.AllowedOrigins, want)
	}
}

// loginPath is a second signal, not a requirement: signedIn carries the guard.
func TestLoginPathIsOptionalWithStorageState(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s.json"), []byte(goodState), 0o600)
	cfg, err := Load(writeConfigIn(t, dir, authPrefix+"  storageState: s.json\n"+signed), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.LoginRoute() != "" {
		t.Errorf("login route = %q, want none known", cfg.Auth.LoginRoute())
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}
}

// A test's auth must be a real boolean: a string or mapping there is a typo
// that would otherwise silently leave the session on.
func TestTestAuthIsAStrictBool(t *testing.T) {
	cases := map[string]struct {
		line string
		ok   bool
		uses bool
	}{
		"absent": {"", true, true},
		"false":  {"    auth: false\n", true, false},
		"true":   {"    auth: true\n", true, true},
		"string": {"    auth: \"false\"\n", false, false},
		"map":    {"    auth: { x: 1 }\n", false, false},
		"list":   {"    auth: [false]\n", false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// The tests: block comes before auth: in authPrefix, so append a second test.
			yml := "version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\n" + tc.line + "auth:\n" + validSetup + signed
			cfg, err := Load(writeConfigIn(t, t.TempDir(), yml), "")
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if err == nil && cfg.Tests[0].UsesAuth() != tc.uses {
				t.Errorf("UsesAuth = %v, want %v", cfg.Tests[0].UsesAuth(), tc.uses)
			}
		})
	}
}

func TestEmptyLoginPathMeansNoLoginRoute(t *testing.T) {
	cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+signed+"  loginPath: \"\"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Auth.LoginRoute(); got != "" {
		t.Errorf("login route = %q, want none: an explicit empty loginPath overrides the setup path", got)
	}
}

func TestAuthDefaults(t *testing.T) {
	cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+signed), "")
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
	cfg, err = Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+signed+"  reuse: false\n  loginPath: /signin\n"), "")
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
			cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+"  setup:\n    path: /login\n    steps:\n      - fill: "+tc.fill+"\n  loginPath: /login\n"+signed), "")
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
