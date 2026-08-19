package appname

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSlugFromRemote(t *testing.T) {
	cases := map[string]string{
		"https://github.com/aquia-inc/ztmf-ui.git":    "aquia-inc/ztmf-ui",
		"https://github.com/aquia-inc/ztmf-ui":        "aquia-inc/ztmf-ui",
		"git@github.com:aquia-inc/ztmf-ui.git":        "aquia-inc/ztmf-ui",
		"ssh://git@github.com/aquia-inc/ztmf-ui.git":  "aquia-inc/ztmf-ui",
		"https://gitlab.com/group/sub/repo.git":       "sub/repo",
		"nonsense":                                    "",
	}
	for in, want := range cases {
		if got := SlugFromRemote(in); got != want {
			t.Errorf("SlugFromRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectPrecedence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"pkg-name"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCfg := "[remote \"origin\"]\n\turl = git@github.com:org/repo.git\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(gitCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GITHUB_REPOSITORY", "")
	if got := Detect(dir, "explicit"); got != "explicit" {
		t.Errorf("config name should win, got %q", got)
	}
	t.Setenv("GITHUB_REPOSITORY", "ci-org/ci-repo")
	if got := Detect(dir, ""); got != "ci-org/ci-repo" {
		t.Errorf("CI env should beat git remote, got %q", got)
	}
	t.Setenv("GITHUB_REPOSITORY", "")
	if got := Detect(dir, ""); got != "org/repo" {
		t.Errorf("git remote should beat package.json, got %q", got)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := Detect(dir, ""); got != "pkg-name" {
		t.Errorf("package.json fallback, got %q", got)
	}
	if got := Detect(t.TempDir(), ""); got != "" {
		t.Errorf("empty when nothing found, got %q", got)
	}
}
