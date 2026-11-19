package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attach is a third auth source beside setup and storageState: exactly one of
// the three, signedIn still required, loginPath and allowedOrigins as usual,
// and reuse (a setup option) refused.
func TestAttachValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(goodState), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, auth, want string // want empty: it must load
	}{
		{"attach alone", "  attach: true\n" + signed, ""},
		{"attach with loginPath and allowedOrigins", "  attach: true\n  loginPath: /login\n  allowedOrigins: [\"https://login.example.com\"]\n" + signed, ""},
		{"attach with setup", "  attach: true\n" + validSetup + signed, "auth: set exactly one of setup, storageState and attach, not setup and attach"},
		{"attach with storageState", "  attach: true\n  storageState: state.json\n" + signed, "not storageState and attach"},
		{"all three", "  attach: true\n  storageState: state.json\n" + validSetup + signed, "not setup and storageState and attach"},
		{"attach without signedIn", "  attach: true\n", "auth.signedIn: required"},
		{"attach with reuse", "  attach: true\n  reuse: true\n" + signed, "auth.reuse: only applies to setup"},
		{"attach false alone", "  attach: false\n" + signed, "auth: set exactly one of setup, storageState and attach"},
		{"attach false with setup", "  attach: false\n" + validSetup + signed, ""},
		{"attach not a bool", "  attach: yes please\n" + signed, "cannot unmarshal"},
		{"bad allowedOrigins", "  attach: true\n  allowedOrigins: [\"/sso\"]\n" + signed, "auth.allowedOrigins[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigIn(t, dir, authPrefix+tc.auth), "")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if err == nil && tc.name == "attach alone" && (!cfg.Auth.Attach || cfg.Auth.State != nil || cfg.Auth.Setup != nil) {
				t.Errorf("attach block loaded as %+v", cfg.Auth)
			}
		})
	}
}

// The local and CI split for attach: the base block attaches to the
// developer's own Chrome, and a ci profile replaces the whole block with a
// storageState file. Each loads on its own, with nothing of the other left.
func TestAttachLocallyStorageStateInCI(t *testing.T) {
	dir := stateDir(t)
	body := authPrefix + "  attach: true\n  loginPath: /login\n" + signed +
		"profiles:\n  ci:\n    auth:\n      storageState: state.json\n      signedIn: \"#ci-shell\"\n"
	path := writeConfigIn(t, dir, body)

	local, err := Load(path, ProfileNone)
	if err != nil {
		t.Fatal(err)
	}
	if a := local.Auth; !a.Attach || a.StorageState != "" || a.LoginPath != "/login" || a.SignedIn != "#shell" {
		t.Errorf("base auth = %+v, want the attach block", a)
	}

	ci, err := Load(path, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if a := ci.Auth; a.Attach || a.StorageState != "state.json" || a.State == nil || a.LoginPath != "" || a.SignedIn != "#ci-shell" {
		t.Errorf("ci auth = %+v, want the storageState block alone", a)
	}

	// And the reverse: a profile can switch to attach.
	rev := writeConfigIn(t, dir, authPrefix+"  storageState: state.json\n"+signed+
		"profiles:\n  local:\n    auth:\n      attach: true\n      signedIn: \"#shell\"\n")
	cfg, err := Load(rev, "local")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.Attach || cfg.Auth.StorageState != "" {
		t.Errorf("local profile auth = %+v, want attach", cfg.Auth)
	}

	// A profile that sets attach next to another source names the profile.
	bad := writeConfigIn(t, dir, authPrefix+"  storageState: state.json\n"+signed+
		"profiles:\n  local:\n    auth:\n      attach: true\n      storageState: state.json\n      signedIn: \"#shell\"\n")
	if _, err := Load(bad, "local"); err == nil || !strings.Contains(err.Error(), `profile "local": auth: set exactly one of setup, storageState and attach`) {
		t.Errorf("err = %v", err)
	}
}
