package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Auth is the optional login block (DESIGN.md §7.5). Exactly one of Setup
// and StorageState supplies the session every test starts with.
type Auth struct {
	// Setup is a login flow run through the normal step machinery.
	Setup *AuthSetup `yaml:"setup"`
	// Reuse runs Setup once and restores what it captured into every test's
	// context. Nil means true. Meaningless with StorageState, which is
	// always restored per test.
	Reuse *bool `yaml:"reuse"`
	// StorageState is a Playwright-compatible storage-state file captured
	// elsewhere. A relative path resolves against the config file.
	StorageState string `yaml:"storageState"`
	// LoginPath is the path, relative to the base URL, that a signed-out
	// visitor is sent to. A test that lands there without having asked for
	// it fails instead of being scanned. Defaults to Setup.Path.
	LoginPath string `yaml:"loginPath"`

	// State is the parsed StorageState file, held in memory only.
	State *StorageState `yaml:"-"`
}

type AuthSetup struct {
	Path  string `yaml:"path"`
	Steps []Step `yaml:"steps"`
}

// ReuseSetup reports whether the setup flow runs once for the whole run
// (the default) rather than inside every test's own context.
func (a *Auth) ReuseSetup() bool { return a.Reuse == nil || *a.Reuse }

// LoginRoute is the path whose appearance after a test's navigation means
// the session was not applied: loginPath, else the setup flow's path.
func (a *Auth) LoginRoute() string {
	if a.LoginPath != "" {
		return a.LoginPath
	}
	if a.Setup != nil {
		return a.Setup.Path
	}
	return ""
}

// StorageState is the Playwright storage-state shape. Fields Playwright adds
// beyond these (indexedDB, newer versions' extras) are ignored on purpose, so
// a file from a newer Playwright still loads.
type StorageState struct {
	Cookies []StateCookie `json:"cookies"`
	Origins []StateOrigin `json:"origins"`
}

type StateCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
	// Expires is seconds since the epoch; -1 marks a session cookie.
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	// SameSite is Strict, Lax, or None; empty leaves the browser default.
	SameSite string `json:"sameSite"`
}

type StateOrigin struct {
	Origin       string      `json:"origin"`
	LocalStorage []StateItem `json:"localStorage"`
}

type StateItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// String redacts: a stray %v of the state must not put session values in a
// log.
func (s *StorageState) String() string { return "StorageState{redacted}" }

// GoString redacts for %#v, as String does for %v.
func (s *StorageState) GoString() string { return s.String() }

// LoadStorageState reads and validates a storage-state file. Errors name the
// file and the position of the problem, never a cookie or storage value.
func LoadStorageState(path string) (*StorageState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st StorageState
	if err := json.Unmarshal(raw, &st); err != nil {
		// A syntax error's text is a position and one character, a type
		// error's is a field name and a Go type; neither carries a value.
		return nil, fmt.Errorf("not a storage-state JSON file: %w", err)
	}
	for i, c := range st.Cookies {
		switch {
		case c.Name == "":
			return nil, fmt.Errorf("cookies[%d]: name is required", i)
		case c.Domain == "":
			return nil, fmt.Errorf("cookies[%d]: domain is required", i)
		}
		switch c.SameSite {
		case "", "Strict", "Lax", "None":
		default:
			return nil, fmt.Errorf("cookies[%d]: sameSite must be Strict, Lax, or None", i)
		}
	}
	for i, o := range st.Origins {
		if u, err := url.Parse(o.Origin); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("origins[%d]: origin must be an http(s) origin", i)
		}
	}
	return &st, nil
}

// validateAuth checks the block's shape and loads the storage-state file.
// Silently ignoring a bad auth block would mean scanning a login redirect
// and passing, the worst failure mode for this tool, so every mistake here
// is a config error.
func (c *Config) validateAuth() error {
	a := c.Auth
	switch {
	case a.Setup != nil && a.StorageState != "":
		return errors.New("auth: set exactly one of setup and storageState, not both")
	case a.Setup == nil && a.StorageState == "":
		return errors.New("auth: set exactly one of setup and storageState")
	}
	if a.LoginPath != "" && !strings.HasPrefix(a.LoginPath, "/") {
		return errors.New("auth.loginPath: must be a path starting with /")
	}

	if a.StorageState != "" {
		if a.Reuse != nil {
			return errors.New("auth.reuse: only applies to setup; a storageState file is restored into every test")
		}
		path := a.StorageState
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.dir, path)
		}
		st, err := LoadStorageState(path)
		if err != nil {
			return fmt.Errorf("auth.storageState %s: %w", a.StorageState, err)
		}
		a.State = st
		if a.LoginPath == "" {
			c.Warnings = append(c.Warnings, "auth.storageState without auth.loginPath: an expired session in the file cannot be recognised, "+
				"so a test redirected to your login page would be scanned as if it were the page; set loginPath")
		}
		return nil
	}

	s := a.Setup
	if s.Path == "" || !strings.HasPrefix(s.Path, "/") {
		return errors.New("auth.setup.path: required, a path starting with /")
	}
	if len(s.Steps) == 0 {
		return errors.New("auth.setup.steps: at least one step is required")
	}
	for i, st := range s.Steps {
		// Setup runs before any test and its page is discarded, so a check
		// there would have nowhere to report to.
		for _, check := range []struct {
			name string
			set  bool
		}{
			{"scan", st.Scan != nil}, {"keyboard", st.Keyboard != nil},
			{"transcript", st.Transcript != nil}, {"expectFocus", st.ExpectFocus != nil},
		} {
			if check.set {
				return fmt.Errorf("auth.setup.steps[%d]: %s is not allowed in setup; setup steps only drive the login flow", i, check.name)
			}
		}
		if err := st.validate(); err != nil {
			return fmt.Errorf("auth.setup.steps[%d]: %w", i, err)
		}
	}
	return nil
}

var (
	passwordSelRe = regexp.MustCompile(`(?i)passw(or)?d`)
	// envMarker stands in for ${...} references while the raw text is
	// parsed: they are not valid plain YAML scalars in every position (a
	// flow mapping), and the lint only needs to know a reference was there.
	envMarker = "frostfallenvref"
)

// lintSecrets warns about auth.setup fills that put a literal value into a
// password-looking field. It reads the text before interpolation, which is
// the only place a literal and a ${VAR} reference differ. The findings name
// the field, never the value. A file that does not parse yields nothing:
// the real decode reports that.
func lintSecrets(raw string) []string {
	masked := envRe.ReplaceAllString(raw, envMarker)
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(masked), &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	steps := nodeAt(doc.Content[0], "auth", "setup", "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for i, step := range steps.Content {
		fill := nodeAt(step, "fill")
		if fill == nil || fill.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(fill.Content); j += 2 {
			sel := fill.Content[j].Value
			val := resolveAlias(fill.Content[j+1])
			if !passwordSelRe.MatchString(sel) || val.Kind != yaml.ScalarNode || val.Value == "" {
				continue
			}
			if !strings.Contains(val.Value, envMarker) {
				out = append(out, fmt.Sprintf("auth.setup.steps[%d].fill %q: a password field is filled with a literal value; "+
					"write it as ${ENV_VAR} so the secret stays out of the config file", i, sel))
			}
		}
	}
	return out
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// nodeAt walks mapping keys from n, following aliases; nil when any is absent.
func nodeAt(n *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		n = resolveAlias(n)
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				next = n.Content[i+1]
			}
		}
		n = next
	}
	return resolveAlias(n)
}
