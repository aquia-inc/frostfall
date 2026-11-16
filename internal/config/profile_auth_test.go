package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// The removal check walks the document, profiles and each profile through
// merge keys and aliases the way the decoder does, so a null or scalar auth
// that arrives through << is an error like a written one, whichever profile
// is selected. The root merge used to slip past it.
func TestProfileRemovalFollowsMergeKeys(t *testing.T) {
	for name, tail := range map[string]string{
		"root merge":          "<<: { profiles: { ci: { auth: null } } }\n",
		"root merge sequence": "<<: [ { tests: [] }, { profiles: { ci: { auth: null } } } ]\n",
		"merged profile":      "profiles:\n  <<: { ci: { auth: null } }\n",
		"tagged null":         "profiles:\n  ci:\n    auth: !!null\n",
		"merge sequence":      "profiles:\n  ci:\n    <<: [ { defaults: { standard: wcag21aa } }, { auth: null } ]\n",
		"nested merge":        "profiles:\n  ci:\n    <<: { <<: { auth: false } }\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfigIn(t, t.TempDir(), profileAuthBase+tail)
			for _, req := range []string{"ci", ProfileNone} {
				_, err := Load(path, req)
				if err == nil || !strings.Contains(err.Error(), `profile "ci": auth: a profile can replace the auth block but not remove it`) {
					t.Errorf("profile %q: err = %v", req, err)
				}
			}
		})
	}
}

// A merge sequence decodes its first auth: a block first means the profile
// sets that block (it loads), a null first means it removes auth (rejected).
func TestProfileMergeSequenceAuthOrder(t *testing.T) {
	dir := stateDir(t)
	block := "{ auth: { storageState: state.json, signedIn: \"#in\" } }"
	cfg, err := Load(writeConfigIn(t, dir, profileAuthBase+"profiles:\n  ci:\n    <<: [ "+block+", { auth: null } ]\n"), "ci")
	if err != nil || cfg.Auth == nil || cfg.Auth.StorageState != "state.json" || cfg.Auth.Setup != nil {
		t.Fatalf("block-first merge sequence: err = %v, auth = %+v", err, cfg)
	}
	_, err = Load(writeConfigIn(t, dir, profileAuthBase+"profiles:\n  ci:\n    <<: [ { auth: null }, "+block+" ]\n"), "ci")
	if err == nil || !strings.Contains(err.Error(), `profile "ci": auth: a profile can replace the auth block but not remove it`) {
		t.Errorf("null-first merge sequence: err = %v", err)
	}
}

// The walk itself, for shapes whose anchor sits under a top-level key the
// decoder rejects (Load runs this check first, so it would still give the
// removal error, but the config could not load either way), and for the
// cases it must leave alone because the decoder never sees a null auth.
func TestProfileRemovingAuthWalk(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"profiles alias", "x: &p\n  ci:\n    auth: null\nprofiles: *p\n", "ci"},
		{"profile alias", "x: &c\n  auth: ~\nprofiles:\n  ci: *c\n", "ci"},
		{"merge alias", "x: &m\n  auth: false\nprofiles:\n  ci:\n    <<: *m\n", "ci"},
		{"alias to null", "x: &n null\nprofiles:\n  ci:\n    auth: *n\n", "ci"},
		{"lowest name wins", "profiles:\n  zz: { auth: null }\n  <<: { ab: { auth: null } }\n", "ab"},
		{"explicit profiles shadow a root merge", "profiles:\n  ci: {}\n<<: { profiles: { ci: { auth: null } } }\n", ""},
		{"explicit auth shadows a merged null", "profiles:\n  ci:\n    <<: { auth: null }\n    auth: { signedIn: x }\n", ""},
		{"first merged auth wins", "profiles:\n  ci:\n    <<: [ { auth: { signedIn: x } }, { auth: null } ]\n", ""},
		{"quoted << is not a merge", "profiles:\n  ci:\n    \"<<\": { auth: null }\n", ""},
		{"no profiles", "auth: { signedIn: x }\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := profileRemovingAuth(tc.text); got != tc.want {
				t.Errorf("profileRemovingAuth = %q, want %q", got, tc.want)
			}
		})
	}
}

// A mapping that merges itself cannot come from yaml.v3's parser (it reports
// the anchor), but the walk must still end if one is built.
func TestEffectivePairsStopsOnACycle(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("a: 1\n<<: { b: 2 }\n"), &doc); err != nil {
		t.Fatal(err)
	}
	m := doc.Content[0]
	m.Content[3] = &yaml.Node{Kind: yaml.AliasNode, Alias: m}
	got := effectivePairs(m)
	if len(got) != 1 || got[0].key != "a" {
		t.Errorf("effectivePairs = %+v, want just a", got)
	}
}

// The README's "setup locally, storageState in CI" example, as written, must
// load in CI with the login variables unset: interpolation runs over the
// whole file before the profile applies, so the setup block the ci profile
// replaces still has to interpolate, which the :- defaults allow. Locally,
// with the variables set, the setup flow carries their values as before.
func TestReadmeProfileAuthExampleLoads(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	i := strings.Index(text, "#### Setup locally, storageState in CI")
	if i < 0 {
		t.Fatal("README has no setup-locally, storageState-in-CI section")
	}
	start := strings.Index(text[i:], "```yaml\n")
	if start < 0 {
		t.Fatal("the section has no yaml example")
	}
	end := strings.Index(text[i+start+8:], "```")
	if end < 0 {
		t.Fatal("the section's yaml example is not closed")
	}
	example := text[i+start+8 : i+start+8+end]

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".auth", "state.json"), []byte(goodState), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfigIn(t, dir, "version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\n"+example)

	// Unset, not empty: interpolate tells the two apart.
	for _, k := range []string{"A11Y_USER", "A11Y_PASS"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	ci, err := Load(path, "ci")
	if err != nil {
		t.Fatalf("the README example does not load under the ci profile without the login variables: %v\n%s", err, example)
	}
	if a := ci.Auth; a == nil || a.Setup != nil || a.StorageState != ".auth/state.json" || a.State == nil {
		t.Errorf("ci profile did not swap in the storageState block: %+v", a)
	}

	t.Setenv("A11Y_USER", "user@example.com")
	t.Setenv("A11Y_PASS", "pw-from-env")
	base, err := Load(path, ProfileNone)
	if err != nil {
		t.Fatalf("the README example does not load without a profile: %v\n%s", err, example)
	}
	a := base.Auth
	if a == nil || a.Setup == nil || a.StorageState != "" || len(a.Setup.Steps) < 2 {
		t.Fatalf("base config lost its setup flow: %+v", a)
	}
	if got := a.Setup.Steps[0].Fill; len(got) != 1 || got[0].Value != "user@example.com" {
		t.Errorf("email fill = %+v, want the A11Y_USER value", got)
	}
	if got := a.Setup.Steps[1].Fill; len(got) != 1 || got[0].Value != "pw-from-env" {
		t.Errorf("password fill = %+v, want the A11Y_PASS value", got)
	}
	if len(base.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for env-referenced secrets", base.Warnings)
	}
}

// Merge keys inside a profile's auth block resolve as anywhere else in the
// file: merged keys count, an explicit key overrides a merged one, and the
// result gets the full checks with the profile named in errors.
func TestProfileAuthMergeKeys(t *testing.T) {
	dir := stateDir(t)
	shared := "profiles:\n  staging:\n    auth: &shared\n      storageState: state.json\n      signedIn: \"#in\"\n      loginPath: /login\n"

	cfg, err := Load(writeConfigIn(t, dir, profileAuthBase+shared+"  ci:\n    auth:\n      <<: *shared\n      signedIn: \"#ci-shell\"\n"), "ci")
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if a == nil || a.Setup != nil || a.StorageState != "state.json" || a.LoginPath != "/login" || a.SignedIn != "#ci-shell" {
		t.Fatalf("merged profile auth = %+v, want storageState and loginPath merged, signedIn overridden", a)
	}
	if a.State == nil || len(a.State.Cookies) != 1 {
		t.Errorf("merged storageState was not loaded: %v", a.State)
	}

	cases := []struct {
		name, body, want string
	}{
		{
			"merged setup under a storageState is both",
			"version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\nauth: &base\n" + validSetup + signed +
				"profiles:\n  ci:\n    auth:\n      <<: *base\n      storageState: state.json\n",
			`profile "ci": auth: set exactly one of setup and storageState, not both`,
		},
		{
			"merged block missing signedIn",
			profileAuthBase + "profiles:\n  staging:\n    auth: &s\n      storageState: state.json\n  ci:\n    auth:\n      <<: *s\n      loginPath: /login\n",
			`profile "ci": auth.signedIn: required`,
		},
		{
			"merged origin is validated",
			profileAuthBase + shared + "  ci:\n    auth:\n      <<: [ { allowedOrigins: [\"/sso\"] }, *shared ]\n",
			`profile "ci": auth.allowedOrigins[0]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigIn(t, dir, tc.body), "ci")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
