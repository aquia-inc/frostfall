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
	// it fails instead of being scanned. Defaults to Setup.Path; required
	// with StorageState, which has no path of its own to fall back on.
	LoginPath string `yaml:"loginPath"`
	// SignedIn is an optional CSS selector that exists on every page a
	// signed-in session shows. Where it is absent the test failed to sign
	// in. It is the only guard for a login rendered in place at the base
	// root, which no path comparison can see.
	SignedIn string `yaml:"signedIn"`
	// AllowedOrigins are the origins, besides the base URL's, a flow or a
	// test may legitimately end on (an SSO identity provider). A landing on
	// any other origin is treated as a login redirect.
	AllowedOrigins []string `yaml:"allowedOrigins"`

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
		if _, err := NormalizeOrigin(o.Origin); err != nil {
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
	for i, o := range a.AllowedOrigins {
		norm, err := NormalizeOrigin(o)
		if err != nil {
			return fmt.Errorf("auth.allowedOrigins[%d]: %w", i, err)
		}
		a.AllowedOrigins[i] = norm
	}

	if a.StorageState != "" {
		// Without a login route the guard has nothing to compare against, and
		// an expired session in the file would be scanned as the login page.
		if a.LoginPath == "" {
			return errors.New("auth.loginPath: required with storageState; without it a session that has expired cannot be told from a valid one")
		}
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
		return a.requireSignedInAtRoot()
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
	return a.requireSignedInAtRoot()
}

// requireSignedInAtRoot rejects a login route that is the base root: a login
// rendered at / (or in place, with no URL change) leaves no path to compare,
// so only a selector for the signed-in app can tell the two apart.
func (a *Auth) requireSignedInAtRoot() error {
	u, err := url.Parse(a.LoginRoute())
	if err != nil {
		return fmt.Errorf("auth.loginPath: %w", err)
	}
	if RouteKey(u) == "/" && a.SignedIn == "" {
		return errors.New("auth.signedIn: required when the login route is the base root (/): the login page and the app share a URL, " +
			"so only a selector that exists on every signed-in page can tell them apart")
	}
	return nil
}

// NormalizeOrigin returns the scheme://host[:port] a browser reports as
// location.origin for raw: scheme and host lower-cased, a scheme's default
// port dropped. raw must be an absolute http(s) URL; anything past the origin
// (a path, query, credentials) is an error, since an origin that silently
// dropped it would mean something other than what was written.
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an absolute http(s) origin", raw)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q has more than an origin: drop the path, query and credentials", raw)
	}
	return originOfURL(u), nil
}

// OriginOfURL is the normalized origin of u, as NormalizeOrigin writes it,
// without requiring u to be a bare origin.
func OriginOfURL(u *url.URL) string { return originOfURL(u) }

func originOfURL(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + port
}

// RouteKey is a URL's page identity: the path with a trailing slash trimmed
// (root stays "/"), plus a hash-router fragment (#/users) kept as part of it,
// because dropping that would collapse a whole HashRouter app into "/".
// Queries and plain fragments are not identity. Discovery and the login
// guard share it so they cannot disagree about what counts as one page.
func RouteKey(u *url.URL) string {
	p := u.Path
	if p == "" {
		p = "/"
	}
	if p != "/" {
		p = strings.TrimSuffix(p, "/")
	}
	if frag := u.Fragment; strings.HasPrefix(frag, "/") && frag != "/" {
		p += "#" + strings.TrimSuffix(frag, "/")
	}
	return p
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
