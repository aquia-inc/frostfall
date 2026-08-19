// Package appname best-effort identifies the project under test, for report
// headers. Every source is optional; an empty result means the report simply
// omits the name.
package appname

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Detect resolves the app name. Precedence: explicit config name, the CI
// repository slug, the git remote origin, package.json.
func Detect(dir, configName string) string {
	if configName != "" {
		return configName
	}
	if repo := os.Getenv("GITHUB_REPOSITORY"); repo != "" {
		return repo
	}
	if repo := gitRemote(dir); repo != "" {
		return repo
	}
	return packageName(dir)
}

var remoteURLRe = regexp.MustCompile(`(?m)^\s*url\s*=\s*(.+)$`)

// gitRemote walks up from dir looking for .git/config and parses the first
// remote url into an org/repo slug. Best-effort: any parse failure returns "".
func gitRemote(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for d := abs; ; d = filepath.Dir(d) {
		raw, err := os.ReadFile(filepath.Join(d, ".git", "config"))
		if err == nil {
			if m := remoteURLRe.FindSubmatch(raw); m != nil {
				return SlugFromRemote(strings.TrimSpace(string(m[1])))
			}
			return ""
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// SlugFromRemote reduces a git remote URL to "org/repo".
// Handles https://host/org/repo(.git), git@host:org/repo(.git), ssh://git@host/org/repo.
func SlugFromRemote(url string) string {
	url = strings.TrimSuffix(strings.TrimSpace(url), "/")
	url = strings.TrimSuffix(url, ".git")
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	}
	// scp-like syntax: git@host:org/repo
	if at := strings.Index(url, "@"); at >= 0 {
		url = url[at+1:]
	}
	url = strings.Replace(url, ":", "/", 1)
	parts := strings.Split(url, "/")
	if len(parts) < 3 { // host, org, repo
		return ""
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

func packageName(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return ""
	}
	return pkg.Name
}
