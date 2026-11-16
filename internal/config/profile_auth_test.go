package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// profileAuthBase logs in with a setup flow; tests append a profiles block.
const profileAuthBase = "version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\nauth:\n" + validSetup + signed

// stateDir is a config directory holding state.json.
func stateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(goodState), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A profile's auth replaces the top-level block as a unit: setup locally,
// storageState in CI, with no "not both" error and nothing of the base block
// left over. The relative path resolves against the config file, as it does
// at the top level.
func TestProfileAuthReplacesTopLevel(t *testing.T) {
	dir := stateDir(t)
	t.Chdir(t.TempDir())
	path := writeConfigIn(t, dir, profileAuthBase+"profiles:\n  ci:\n    auth:\n      storageState: state.json\n      signedIn: \"#ci-shell\"\n")

	cfg, err := Load(path, "ci")
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if a == nil || a.Setup != nil || a.StorageState != "state.json" || a.SignedIn != "#ci-shell" {
		t.Fatalf("profile auth not applied wholesale: %+v", a)
	}
	if a.State == nil || len(a.State.Cookies) != 1 || a.State.Cookies[0].Name != "sid" {
		t.Errorf("profile storageState not loaded from the config's directory: %v", a.State)
	}

	base, err := Load(path, ProfileNone)
	if err != nil {
		t.Fatal(err)
	}
	if base.Auth == nil || base.Auth.Setup == nil || base.Auth.StorageState != "" || base.Auth.SignedIn != "#shell" {
		t.Errorf("base auth changed by an unselected profile: %+v", base.Auth)
	}
}

// A profile without auth inherits the top-level block exactly, warnings
// included, so configs that never set profile auth load as before.
func TestProfileWithoutAuthInheritsTopLevel(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigIn(t, dir, profileAuthBase+"  allowedOrigins: [\"https://IDP.example.com:443\"]\nprofiles:\n  ci:\n    defaults:\n      standard: section508\n")
	withProfile, err := Load(path, "ci")
	if err != nil {
		t.Fatal(err)
	}
	without, err := Load(path, ProfileNone)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withProfile.Auth, without.Auth) || !reflect.DeepEqual(withProfile.Warnings, without.Warnings) {
		t.Errorf("profile without auth changed it:\n%+v\n%+v", withProfile.Auth, without.Auth)
	}
	if withProfile.authProfile != "" {
		t.Errorf("authProfile = %q for a profile without auth", withProfile.authProfile)
	}
}

// A profile can add auth to a base config without any.
func TestProfileAuthAddsAuth(t *testing.T) {
	path := writeConfigIn(t, stateDir(t), "version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\n    auth: true\n"+
		"profiles:\n  staging:\n    auth:\n      storageState: state.json\n      signedIn: \"#in\"\n")
	if cfg, err := Load(path, "staging"); err != nil || cfg.Auth == nil {
		t.Fatalf("profile auth not added: %v", err)
	}
	// Without the profile, auth: true has nothing to refer to.
	if _, err := Load(path, ProfileNone); err == nil || !strings.Contains(err.Error(), "auth: true needs an auth block") {
		t.Errorf("err = %v, want auth: true rejected without the profile", err)
	}
}

// Every auth check runs on the selected profile's block, and the error says
// which profile it came from. A bad block is a config error (exit 2).
func TestProfileAuthValidation(t *testing.T) {
	dir := stateDir(t)
	cases := []struct {
		name string
		auth string // the profile's auth block, indented under it
		want string
	}{
		{"missing signedIn", "      storageState: state.json\n", `profile "ci": auth.signedIn: required`},
		{"both setup and storageState", "      storageState: state.json\n      signedIn: \"#in\"\n      setup:\n        path: /login\n        steps:\n          - click: a\n", `profile "ci": auth: set exactly one of setup and storageState, not both`},
		{"empty block", "      {}\n", `profile "ci": auth: set exactly one of setup and storageState`},
		{"missing storageState file", "      storageState: nope.json\n      signedIn: \"#in\"\n", `profile "ci": auth.storageState nope.json`},
		{"bad allowedOrigins", "      storageState: state.json\n      signedIn: \"#in\"\n      allowedOrigins: [\"/sso\"]\n", `profile "ci": auth.allowedOrigins[0]`},
		{"scan in setup", "      signedIn: \"#in\"\n      setup:\n        path: /login\n        steps:\n          - scan: {}\n", `profile "ci": auth.setup.steps[0]: scan is not allowed`},
		{"reuse with storageState", "      storageState: state.json\n      signedIn: \"#in\"\n      reuse: true\n", `profile "ci": auth.reuse`},
		{"unknown key", "      storageState: state.json\n      signedIn: \"#in\"\n      logInPath: /x\n", "field logInPath not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigIn(t, dir, profileAuthBase+"profiles:\n  ci:\n    auth:\n"+tc.auth), "ci")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// An unselected profile gets what every other profile field gets today:
// strict decoding, but no semantic checks. Its storageState file is not read,
// so the CI profile's file need not exist on a laptop.
func TestUnselectedProfileAuthIsOnlyDecoded(t *testing.T) {
	dir := t.TempDir()
	semantic := profileAuthBase + "profiles:\n  ci:\n    auth:\n      storageState: .auth/ci-only.json\n"
	if _, err := Load(writeConfigIn(t, dir, semantic), ProfileNone); err != nil {
		t.Errorf("unselected profile auth was validated: %v", err)
	}
	if _, err := Load(writeConfigIn(t, dir, semantic), "ci"); err == nil {
		t.Error("selected profile auth was not validated")
	}
	typo := profileAuthBase + "profiles:\n  ci:\n    auth:\n      storageStat: x.json\n"
	if _, err := Load(writeConfigIn(t, dir, typo), ProfileNone); err == nil || !strings.Contains(err.Error(), "field storageStat not found") {
		t.Errorf("err = %v, want the unknown key in an unselected profile rejected", err)
	}
}

// A profile cannot remove auth: a null or scalar auth would otherwise drop the
// signed-in guard and the redaction for a whole environment (silently, for
// null, which decodes like an absent key). It is an error in every profile,
// selected or not, and names the profile.
func TestProfileCannotRemoveAuth(t *testing.T) {
	for name, block := range map[string]string{
		"bare":          "    auth:\n",
		"null":          "    auth: null\n",
		"tilde":         "    auth: ~\n",
		"false":         "    auth: false\n",
		"string":        "    auth: none\n",
		"merged null":   "    <<: { auth: null }\n",
		"alias to null": "    auth: *nothing\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := profileAuthBase + "profiles:\n  other:\n    discover: &nothing null\n  ci:\n" + block
			for _, req := range []string{"ci", ProfileNone} {
				_, err := Load(writeConfigIn(t, t.TempDir(), body), req)
				if err == nil || !strings.Contains(err.Error(), `profile "ci": auth: a profile can replace the auth block but not remove it`) {
					t.Errorf("profile %q: err = %v", req, err)
				}
			}
		})
	}
}

// The literal-password lint covers every profile's setup flow, whichever
// profile runs, and says which profile the field is in.
func TestSecretLintCoversProfileAuth(t *testing.T) {
	const literal = "hunter2-literal-value"
	body := profileAuthBase + "profiles:\n" +
		"  zeta:\n    auth:\n      signedIn: \"#in\"\n      setup:\n        path: /login\n        steps:\n          - fill: { \"#password\": \"" + literal + "\" }\n" +
		"  alpha:\n    auth:\n      signedIn: \"#in\"\n      setup:\n        path: /login\n        steps:\n          - click: a\n          - fill: { \"#passwd\": \"" + literal + "\" }\n" +
		"  env:\n    auth:\n      signedIn: \"#in\"\n      setup:\n        path: /login\n        steps:\n          - fill: { \"#password\": \"${FF_PROFILE_PASS:-x}\" }\n"
	for _, req := range []string{ProfileNone, "env", "zeta"} {
		cfg, err := Load(writeConfigIn(t, t.TempDir(), body), req)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			`profiles.alpha.auth.setup.steps[1].fill "#passwd"`,
			`profiles.zeta.auth.setup.steps[0].fill "#password"`,
		}
		if len(cfg.Warnings) != len(want) {
			t.Fatalf("profile %q: warnings = %v, want %d", req, cfg.Warnings, len(want))
		}
		for i, w := range cfg.Warnings {
			if !strings.HasPrefix(w, want[i]) || strings.Contains(w, literal) {
				t.Errorf("profile %q: warning %d = %q, want prefix %q and no value", req, i, w, want[i])
			}
		}
	}
}

// An explicit auth block wins over a merged null, as it does in the decoder.
func TestProfileExplicitAuthWinsOverMergedNull(t *testing.T) {
	path := writeConfigIn(t, stateDir(t), profileAuthBase+"profiles:\n  ci:\n    <<: { auth: null }\n    auth:\n      storageState: state.json\n      signedIn: \"#in\"\n")
	if cfg, err := Load(path, "ci"); err != nil || cfg.Auth == nil || cfg.Auth.StorageState != "state.json" {
		t.Fatalf("explicit profile auth lost to a merged null: %v", err)
	}
}
