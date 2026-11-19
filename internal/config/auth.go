package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"gopkg.in/yaml.v3"

	"github.com/aquia-inc/frostfall/internal/secrets"
)

// Auth is the optional login block (DESIGN.md §7.5). Exactly one of Setup,
// StorageState and Attach supplies the session every test starts with.
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
	// Attach takes the session from a Chrome the user already has open and
	// signed in, reached with --browser-url: the cookies and localStorage of
	// the app's origin and of AllowedOrigins are read from that browser once
	// and restored into every test's own context. Local runs only; the flag
	// and this key must be set together.
	Attach bool `yaml:"attach"`
	// SignedIn is a CSS selector that exists on every page a signed-in
	// session shows and on no login page. Required: a URL comparison alone
	// cannot tell a signed-out page from a signed-in one (a login rendered in
	// place, an IdP page, a renamed route), a marker in the page can. It is
	// the guard's main signal.
	SignedIn string `yaml:"signedIn"`
	// LoginPath is the path, relative to the base URL, that a signed-out
	// visitor is sent to. Optional, with no default: a login route exists
	// only when this is set. It is a second signal beside SignedIn and the
	// source of clearer messages, and the crawl never follows it. (setup.path
	// is no default: it is often a protected page that bounces through SSO
	// and back, which is not a login page.)
	LoginPath string `yaml:"loginPath"`
	// AllowedOrigins are the origins, besides the base URL's, that a setup
	// flow may pass through on the way back to the app (an SSO identity
	// provider): their storage is captured. They never excuse a test landing
	// off the app's origin.
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

// LoginRoute is the configured login path, "" when there is none.
func (a *Auth) LoginRoute() string { return a.LoginPath }

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
			if errors.Is(err, ErrInvalidIDN) {
				// The reason without the value, as for every error here.
				return nil, fmt.Errorf("origins[%d]: %w", i, ErrInvalidIDN)
			}
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
	var sources []string
	if a.Setup != nil {
		sources = append(sources, "setup")
	}
	if a.StorageState != "" {
		sources = append(sources, "storageState")
	}
	if a.Attach {
		sources = append(sources, "attach")
	}
	switch len(sources) {
	case 0:
		return errors.New("auth: set exactly one of setup, storageState and attach")
	case 1:
	default:
		return fmt.Errorf("auth: set exactly one of setup, storageState and attach, not %s", strings.Join(sources, " and "))
	}
	if a.SignedIn == "" {
		return errors.New("auth.signedIn: required; a CSS selector that exists on every signed-in page and on no login page " +
			"(Frostfall fails a test where it is missing, since the URL alone cannot tell a login page from the app)")
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

	if a.Attach {
		if a.Reuse != nil {
			return errors.New("auth.reuse: only applies to setup; an attached browser's session is read once and restored into every test")
		}
		return nil
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

// NormalizeOrigin returns the scheme://host[:port] a browser reports as
// location.origin for raw: scheme and host lower-cased, a scheme's default
// port dropped. raw must be an absolute http(s) URL; anything past the origin
// (a path, query, credentials) is an error, since an origin that silently
// dropped it would mean something other than what was written.
func NormalizeOrigin(raw string) (string, error) {
	// Errors print the value redacted: it can be a base URL with userinfo, and a
	// url.Error would carry all of it.
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid URL", secrets.RedactURL(raw))
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an absolute http(s) origin", secrets.RedactURL(raw))
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q has more than an origin (what follows the origin is not shown here): drop the path, query and credentials", secrets.RedactURL(raw))
	}
	if _, err := CanonicalHost(u.Hostname()); err != nil {
		return "", fmt.Errorf("%q: %w", secrets.RedactURL(raw), err)
	}
	return originOfURL(u), nil
}

// OriginOfURL is the normalized origin of u, as NormalizeOrigin writes it,
// without requiring u to be a bare origin.
func OriginOfURL(u *url.URL) string { return originOfURL(u) }

// ErrInvalidIDN is CanonicalHost's error for a non-ASCII host that does not
// pass the checks CanonicalHost makes on what a browser would load.
var ErrInvalidIDN = errors.New("the host is not a valid internationalized domain name (the browser refuses it, or reads it as an IP address)")

// browserIDNA converts a non-ASCII host as the WHATWG URL standard's "domain
// to ASCII" does: UTS #46 mapping with non-transitional processing (ß stays
// ß, xn--zca, instead of becoming ss), the bidi and joiner rules, and no STD3
// or hyphen checks, which browsers leave off (an underscore or a leading
// hyphen in a label loads in Chrome).
var browserIDNA = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.Transitional(false),
	idna.StrictDomainName(false), idna.CheckHyphens(false))

// CanonicalHost returns host, as url.URL.Hostname gives it (no port, an IPv6
// literal without brackets), the way a browser writes it in location.origin
// and in the URLs it reports: lower-cased, an IPv6 literal in brackets, and a
// non-ASCII (IDN) host in punycode (café.example is xn--caf-dma.example).
//
// An all-ASCII host is only lower-cased and bracketed, exactly as before IDN
// support; none of the checks below apply to it.
//
// A non-ASCII host is refused with ErrInvalidIDN when:
//   - a label starts with xn-- once mapped but is not all ASCII (see
//     nonASCIIPunycodeLabel);
//   - browserIDNA refuses it (a disallowed code point, the bidi or joiner
//     rules);
//   - the bidi rule fails on the mapped labels. x/net/idna decides whether to
//     run that rule from the runes as written, so a code point that maps into
//     a right-to-left script (ℵ, U+2135, maps to Hebrew alef) skips it.
//     Converting the result a second time decodes its punycode labels and
//     applies the rule to what the browser sees; that pass must also give
//     back the same string;
//   - the result fails the URL standard's checks after domain to ASCII: it is
//     empty, has a forbidden domain code point (fullwidth ／, ＠ and ： map to
//     /, @ and :), or its last label is a number. The browser then parses the
//     host as IPv4 and refuses it (é.1.2.3) or rewrites it to an address
//     (１２７.0.0.1 is 127.0.0.1); both are refused here, the second on
//     purpose, since a config that means an IP address should say so;
//   - an xn-- label decodes to nothing (é.xn--.example). A label that is
//     empty as written is kept, as the browser keeps it (é..example).
//
// These are the checks made, not a promise to refuse everything a browser
// refuses. A host the browser refuses for a reason not listed here is still
// converted, and nothing here proves the result is a host no page reports;
// the one known way a refused host converts to a real one (a malformed xn--
// label) is checked explicitly, and the Chrome comparison test in
// internal/browser covers the rest of what is known.
//
// A refused host comes back exactly as written. It is non-ASCII, so it can
// never equal an origin a browser reports, and a comparison against it fails
// closed. (Lower-casing it could not promise that: strings.ToLower turns the
// Kelvin sign U+212A into "k" and İ U+0130 into "i", so a refused host could
// come out ASCII and equal a real one.)
func CanonicalHost(host string) (string, error) {
	if isASCII(host) {
		host = strings.ToLower(host)
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return host, nil
	}
	if nonASCIIPunycodeLabel(host) {
		return host, ErrInvalidIDN
	}
	ascii, err := browserIDNA.ToASCII(host)
	if err != nil || !browserDomainOK(ascii) || emptyLabels(ascii) > emptyLabels(dotsReplacer.Replace(host)) {
		return host, ErrInvalidIDN
	}
	// The second pass decodes the result's punycode labels, checks them, and
	// encodes them again: anything but the same string back means the first
	// pass decoded something that does not stand for itself.
	if again, err := browserIDNA.ToASCII(ascii); err != nil || again != ascii {
		return host, ErrInvalidIDN
	}
	return strings.ToLower(ascii), nil
}

// nonASCIIPunycodeLabel reports whether a label of host, once mapped as UTS
// #46 maps it (case, fullwidth forms, ignored code points), starts with xn--
// and is not all ASCII: xn--café-, XN--café-, ｘｎ--café-. The browser
// refuses such a label. x/net/idna does not: its punycode decoder takes the
// last hyphen for the delimiter and decodes café-, so the host would come out
// as the real xn--caf-dma.example.
//
// The mapping is x/net/idna's own, the one its decoder sees. There is no
// mapping-only profile, and any label that starts with xn-- after mapping is
// decoded, so each label is mapped with an "a" in front, which keeps it from
// starting with xn-- and maps to itself.
func nonASCIIPunycodeLabel(host string) bool {
	for _, l := range strings.Split(dotsReplacer.Replace(host), ".") {
		m, _ := browserIDNA.ToUnicode("a" + l)
		m, ok := strings.CutPrefix(m, "a")
		if !ok {
			// The label began with a combining mark, which composed with the
			// "a": it cannot start with xn--, and ToASCII refuses it anyway.
			continue
		}
		for _, s := range strings.Split(m, ".") {
			if strings.HasPrefix(s, "xn--") && !isASCII(s) {
				return true
			}
		}
	}
	return false
}

// browserDomainOK is the URL standard's checks on the result of domain to
// ASCII: not empty, no forbidden domain code point, and a last label (one
// trailing dot aside) that is not a number.
func browserDomainOK(ascii string) bool {
	if ascii == "" || !isASCII(ascii) {
		return false
	}
	for i := range len(ascii) {
		if c := ascii[i]; c <= ' ' || c == 0x7f || strings.IndexByte(`#%/:<>?@[\]^|`, c) >= 0 {
			return false
		}
	}
	labels := strings.Split(ascii, ".")
	if len(labels) > 1 && labels[len(labels)-1] == "" {
		labels = labels[:len(labels)-1]
	}
	return !isNumberLabel(labels[len(labels)-1])
}

// isNumberLabel is the URL standard's "ends in a number" test for the last
// label: all decimal digits, or 0x or 0X followed by hex digits (or none).
func isNumberLabel(l string) bool {
	if strings.HasPrefix(l, "0x") || strings.HasPrefix(l, "0X") {
		return strings.Trim(l[2:], "0123456789abcdefABCDEF") == ""
	}
	return l != "" && strings.Trim(l, "0123456789") == ""
}

// dotsReplacer writes the label separators UTS #46 maps to "." as "." (。
// U+3002, ． U+FF0E, ｡ U+FF61), so the labels of a host as written can be
// counted.
var dotsReplacer = strings.NewReplacer("。", ".", "．", ".", "｡", ".")

// emptyLabels counts the empty labels of a host. The browser keeps a label
// that is empty as written (é..example loads as xn--9ca..example) but
// refuses an xn-- label that decodes to nothing, which x/net/idna turns into
// a new empty label instead: one more empty label in the result than in the
// input is that case.
func emptyLabels(host string) int {
	n := 0
	for _, l := range strings.Split(host, ".") {
		if l == "" {
			n++
		}
	}
	return n
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func originOfURL(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	// An invalid IDN keeps the host as written: see CanonicalHost.
	host, _ := CanonicalHost(u.Hostname())
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
// the real decode reports that. Every profile's auth block is linted too,
// selected or not: the literal is in the file either way.
func lintSecrets(raw string) []string {
	masked := envRe.ReplaceAllString(raw, envMarker)
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(masked), &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	out := lintSetupSteps("", nodeAt(root, "auth", "setup", "steps"))
	if profiles := nodeAt(root, "profiles"); profiles != nil && profiles.Kind == yaml.MappingNode {
		byName := map[string]*yaml.Node{}
		for i := 0; i+1 < len(profiles.Content); i += 2 {
			byName[profiles.Content[i].Value] = profiles.Content[i+1]
		}
		for _, name := range slices.Sorted(maps.Keys(byName)) {
			out = append(out, lintSetupSteps("profiles."+name+".", nodeAt(byName[name], "auth", "setup", "steps"))...)
		}
	}
	return out
}

// lintSetupSteps is lintSecrets for one auth.setup.steps sequence; prefix
// says whose auth block it is.
func lintSetupSteps(prefix string, steps *yaml.Node) []string {
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
				out = append(out, fmt.Sprintf("%sauth.setup.steps[%d].fill %q: a password field is filled with a literal value; "+
					"write it as ${ENV_VAR} so the secret stays out of the config file", prefix, i, sel))
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

// yamlPair is one key/value of a mapping as the decoder sees it. key is the
// decoded name; keyNode is the alias-resolved key, for its tag and line.
type yamlPair struct {
	key     string
	keyNode *yaml.Node
	val     *yaml.Node
}

// isMergeKey matches a merge key (<<) the way yaml.v3 does: a plain or
// !!merge-tagged scalar, not a quoted "<<" string or an alias to one.
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" && (k.Tag == "" || k.Tag == "!" || k.ShortTag() == "!!merge")
}

// hasMergeKey reports whether n, alias-resolved, is a mapping with a merge
// key (<<) of its own.
func hasMergeKey(n *yaml.Node) bool {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if isMergeKey(n.Content[i]) {
			return true
		}
	}
	return false
}

// keyName decodes a mapping key to the field or map key the decoder would
// see: an alias is followed and the scalar decoded as a string, so
// `!!binary YXV0aA==` is "auth". A key that does not decode keeps its text.
func keyName(k *yaml.Node) (string, *yaml.Node) {
	k = resolveAlias(k)
	var s string
	if err := k.Decode(&s); err != nil {
		return k.Value, k
	}
	return s, k
}

// effectivePairs returns the key/value pairs a mapping decodes to: aliases
// followed, keys decoded (keyName), and merge keys (<<, given a mapping, an
// alias to one, or a sequence of them) expanded in the decoder's order, so an
// explicit key shadows a merged one wholesale and an earlier merged mapping
// shadows a later one. Values are alias-resolved; nil unless n is a mapping.
//
// Each mapping is expanded at most once per call. Keys are first-wins, so a
// second visit could only offer keys already taken; skipping it keeps alias
// diamonds (&aN {<<: [*aN-1, *aN-1]}) linear rather than exponential, since
// this runs before the decoder's own excessive-aliasing check, and it ends
// cycles (a mapping that merges itself), which the real decode reports.
func effectivePairs(n *yaml.Node) []yamlPair {
	var out []yamlPair
	seen := map[string]bool{}
	visited := map[*yaml.Node]bool{}
	var walk func(m *yaml.Node)
	walk = func(m *yaml.Node) {
		m = resolveAlias(m)
		if m == nil || m.Kind != yaml.MappingNode || visited[m] {
			return
		}
		visited[m] = true
		var merge *yaml.Node
		for i := 0; i+1 < len(m.Content); i += 2 {
			if isMergeKey(m.Content[i]) {
				merge = resolveAlias(m.Content[i+1]) // the last << wins, as in the decoder
				continue
			}
			name, k := keyName(m.Content[i])
			if !seen[name] {
				seen[name] = true
				out = append(out, yamlPair{name, k, resolveAlias(m.Content[i+1])})
			}
		}
		if merge != nil && merge.Kind == yaml.SequenceNode {
			for _, item := range merge.Content {
				walk(item)
			}
		} else if merge != nil {
			walk(merge)
		}
	}
	walk(n)
	return out
}

// effectiveAt is nodeAt over effectivePairs: it follows merge keys at every
// level as well as aliases. nil when any key is absent.
func effectiveAt(n *yaml.Node, keys ...string) *yaml.Node {
	n = resolveAlias(n)
	for _, k := range keys {
		var next *yaml.Node
		for _, p := range effectivePairs(n) {
			if p.key == k {
				next = p.val
				break
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// CheckTestOrigins rejects, for a config with auth, a test whose url: is not
// on the base URL's origin unless it sets auth: false. The session's cookies
// are injected into the whole browser context, so such a test would send them
// to a host the guard cannot judge (localhost versus 127.0.0.1 is enough). It
// takes the base URL the run resolved, which --base-url and serve: decide.
func (c *Config) CheckTestOrigins(baseURL string, tests []Test) error {
	if c.Auth == nil {
		return nil
	}
	bu, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("base url %s is not a valid URL", secrets.RedactURL(baseURL))
	}
	base := originOfURL(bu)
	for _, t := range tests {
		if t.URL == "" || !t.UsesAuth() {
			continue
		}
		u, err := url.Parse(t.URL)
		if err != nil || u.Host == "" {
			continue // not an absolute URL: validate and the run report that
		}
		// A host the browser refuses is not compared at all: it is on no
		// origin, the app's least of all.
		if _, err := CanonicalHost(u.Hostname()); err != nil {
			return fmt.Errorf("test %q: url %s: %w; set auth: false on this test or fix the host", t.ID, secrets.RedactURL(t.URL), err)
		}
		if o := originOfURL(u); o != base {
			return fmt.Errorf("test %q: url is on %s, not on the app's origin %s: the session's cookies go to every host a test loads "+
				"and the login guard cannot judge another origin; set auth: false on this test", t.ID, o, base)
		}
	}
	return nil
}

// JoinURL appends a test's path to the base URL, producing what plain
// concatenation did for every path that worked before:
//
//   - "/x": the base's trailing slash is dropped, so there is no "//" ("/" under
//     "http://h/sub/" is http://h/sub/);
//   - "?a=1" and "#/route": appended to the base as it is, so the base's own
//     slash stays ("http://h/sub/" gives http://h/sub/?a=1, "http://h/sub" gives
//     http://h/sub?a=1);
//   - anything else ("index.html"): joined with a "/" unless the base already
//     ends in one, so "@evil.example/" stays a path on the same host instead of
//     becoming userinfo.
func JoinURL(base, path string) string {
	switch {
	case path == "":
		return base
	case path[0] == '/':
		return strings.TrimRight(base, "/") + path
	case path[0] == '?' || path[0] == '#':
		return base + path
	case strings.HasSuffix(base, "/"):
		return base + path
	}
	return base + "/" + path
}
