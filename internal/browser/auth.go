package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/config"
)

// ErrLoginRedirect marks a test that was sent to the login page: its session
// was not applied or has expired. It is an environment failure, never a
// finding, because scanning the login page and passing is the one outcome
// this tool must not produce quietly.
var ErrLoginRedirect = errors.New("landed on the login page")

// State is the session a test's context starts with: cookies, plus the
// local and session storage of the origins that have any. It exists only in
// memory. Nothing here is ever written to disk, a report, or a log line, and
// the formatters redact it in case a %v reaches it.
type State struct {
	cookies []*proto.NetworkCookieParam
	origins []originStorage
}

type originStorage struct {
	origin  string
	local   [][2]string
	session [][2]string
}

func (s *State) String() string   { return "State{redacted}" }
func (s *State) GoString() string { return s.String() }

// Auth carries everything a new test context needs to start signed in.
type Auth struct {
	baseURL  string
	origin   string
	defaults config.Defaults
	log      func(format string, args ...any)

	// state is restored into every context: the file's, or what the one
	// setup run captured. Nil when setup runs per context.
	state *State
	// setup is the login flow each context runs for itself (reuse: false).
	setup *config.AuthSetup

	// loginRoute is the configured login path, loginKey its RouteKey; both
	// empty when nothing says where the login page is.
	loginRoute string
	loginKey   string
}

// NewAuth prepares the config's auth block: it loads a storage-state file's
// contents or, with setup and reuse, runs the login flow once now, before any
// test, in a context of its own. It returns nil for a config without auth. A
// setup failure is an environment error. log receives one line per setup
// step naming the action and selector, never a value.
func NewAuth(b *Browser, cfg *config.Config, baseURL string, log func(format string, args ...any)) (*Auth, error) {
	if cfg.Auth == nil {
		return nil, nil
	}
	origin, err := originOf(baseURL)
	if err != nil {
		return nil, fmt.Errorf("auth: base url: %w", err)
	}
	a := &Auth{baseURL: baseURL, origin: origin, defaults: cfg.Defaults, log: log, loginRoute: cfg.Auth.LoginRoute()}
	if a.loginRoute != "" {
		u, err := url.Parse(a.loginRoute)
		if err != nil {
			return nil, fmt.Errorf("auth: login path: %w", err)
		}
		a.loginKey = RouteKey(u)
	}

	switch {
	case cfg.Auth.State != nil:
		if a.state, err = stateFromFile(cfg.Auth.State); err != nil {
			return nil, fmt.Errorf("auth storageState: %w", err)
		}
	case cfg.Auth.ReuseSetup():
		if a.state, err = a.captureOnce(b, cfg.Auth.Setup); err != nil {
			return nil, err
		}
	default:
		a.setup = cfg.Auth.Setup
	}
	return a, nil
}

func (a *Auth) logf(format string, args ...any) {
	if a.log != nil {
		a.log(format, args...)
	}
}

// captureOnce runs the setup flow in a throwaway context and keeps what it
// left behind. The context is discarded afterwards: the captured state, not
// that browser, is what every test starts from.
func (a *Auth) captureOnce(b *Browser, setup *config.AuthSetup) (*State, error) {
	a.logf("auth setup: running once, state reused for every test")
	// The setup flow gets the default test budget; it is one flow, not a test.
	s, err := b.open(a.defaults.Viewport, a.defaults.Timeout.Std(), nil)
	if err != nil {
		return nil, fmt.Errorf("auth setup: %w", err)
	}
	defer s.Close()
	if err := a.runSetup(s, setup); err != nil {
		return nil, err
	}
	st, err := s.captureState(a.origin)
	if err != nil {
		return nil, fmt.Errorf("auth setup: capturing session: %w", err)
	}
	if len(st.cookies) == 0 && len(st.origins) == 0 {
		a.logf("auth setup: the flow left no cookies or storage behind")
	}
	return st, nil
}

// runSetup drives the login flow in s through the ordinary step machinery.
// Errors name the step index and the action's selector, and never a filled
// value, so a failed login cannot leak a credential into CI logs.
func (a *Auth) runSetup(s *Session, setup *config.AuthSetup) error {
	settle := a.defaults.SettleTime.Std()
	a.logf("auth setup: loading %s", setup.Path)
	if err := s.navigate(a.baseURL+setup.Path, a.defaults.WaitFor, settle); err != nil {
		return fmt.Errorf("auth setup: %w", err)
	}
	for i, st := range setup.Steps {
		a.logf("auth setup: step %d: %s", i, describeStep(st))
		if err := s.Step(st, a.baseURL, a.defaults.WaitFor, settle); err != nil {
			return fmt.Errorf("auth setup: steps[%d]: %w", i, err)
		}
	}
	return nil
}

// describeStep names a step's action and selectors for the log. Fill and
// select values are credentials or near enough; they are never included.
func describeStep(st config.Step) string {
	quote := func(fs config.Fields) string {
		sels := make([]string, len(fs))
		for i, f := range fs {
			sels[i] = fmt.Sprintf("%q", f.Selector)
		}
		return strings.Join(sels, ", ")
	}
	switch {
	case st.Click != "":
		return fmt.Sprintf("click %q", st.Click)
	case len(st.Fill) > 0:
		return "fill " + quote(st.Fill)
	case st.Press != "":
		return "press " + st.Press
	case st.Hover != "":
		return fmt.Sprintf("hover %q", st.Hover)
	case len(st.Select) > 0:
		return "select " + quote(st.Select)
	case st.WaitFor != "":
		return fmt.Sprintf("waitFor %q", st.WaitFor)
	case st.Wait != 0:
		return "wait " + st.Wait.Std().String()
	case st.Goto != "":
		return fmt.Sprintf("goto %q", st.Goto)
	}
	return "step"
}

// NewAuthSession is NewSession for an authenticated run: the context is
// seeded with a's state before its first navigation, or runs a's setup flow
// first (reuse: false), and the landing page is checked against the login
// route. A nil a is plain NewSession.
func (b *Browser) NewAuthSession(a *Auth, url string, vp config.Viewport, waitFor string, settle, timeout time.Duration) (*Session, error) {
	var state *State
	if a != nil {
		state = a.state
	}
	s, err := b.open(vp, timeout, state)
	if err != nil {
		return nil, err
	}
	if a != nil && a.setup != nil {
		if err := a.runSetup(s, a.setup); err != nil {
			s.Close()
			return nil, err
		}
	}
	if err := s.navigate(url, waitFor, settle); err != nil {
		s.Close()
		return nil, err
	}
	if a != nil {
		if err := a.checkLanding(s, url); err != nil {
			s.Close()
			return nil, err
		}
	}
	// The storage seed is for the first load only. Left registered it would
	// run again on every later navigation and put back a token the page had
	// removed (a logout step), hiding the very behavior under test.
	s.dropSeed()
	return s, nil
}

// checkLanding fails with ErrLoginRedirect when s ended up on the login
// route of the app without the test having asked for it. A cross-origin page
// is never the app's login page, and a test that targets the login page
// itself is allowed to be there.
func (a *Auth) checkLanding(s *Session, testURL string) error {
	if a.loginKey == "" {
		return nil
	}
	res, err := s.page.Eval(`() => location.href`)
	if err != nil {
		return fmt.Errorf("reading landing url: %w", err)
	}
	landed, err := url.Parse(res.Value.Str())
	if err != nil || !a.onLogin(landed) {
		return nil
	}
	if want, err := url.Parse(testURL); err == nil && a.onLogin(want) {
		return nil
	}
	return fmt.Errorf("%w %s: the auth session was not applied or has expired", ErrLoginRedirect, a.loginRoute)
}

// onLogin reports whether u is the app's login route: same origin as the
// base URL and the same path plus hash route.
func (a *Auth) onLogin(u *url.URL) bool {
	if o, err := originOf(u.String()); err != nil || o != a.origin {
		return false
	}
	return RouteKey(u) == a.loginKey
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

// originOf is the scheme://host[:port] a browser reports as location.origin,
// with a scheme's default port dropped.
func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an http(s) URL", raw)
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port == "" || (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		return u.Scheme + "://" + host, nil
	}
	return u.Scheme + "://" + host + ":" + port, nil
}

// stateFromFile converts a Playwright storage-state file into browser state.
// Cookies already expired are dropped rather than sent: Chrome rejects a
// batch containing one, and the expired session then shows up where it
// should, as the login-redirect guard.
func stateFromFile(f *config.StorageState) (*State, error) {
	st := &State{}
	now := float64(time.Now().Unix())
	for _, c := range f.Cookies {
		if c.Expires > 0 && c.Expires < now {
			continue
		}
		p := &proto.NetworkCookieParam{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HTTPOnly,
		}
		if p.Path == "" {
			p.Path = "/"
		}
		// expires -1 (or absent) is a session cookie: leave Expires unset.
		if c.Expires > 0 {
			p.Expires = proto.TimeSinceEpoch(c.Expires)
		}
		switch c.SameSite {
		case "Strict":
			p.SameSite = proto.NetworkCookieSameSiteStrict
		case "Lax":
			p.SameSite = proto.NetworkCookieSameSiteLax
		case "None":
			p.SameSite = proto.NetworkCookieSameSiteNone
		}
		st.cookies = append(st.cookies, p)
	}
	for _, o := range f.Origins {
		if len(o.LocalStorage) == 0 {
			continue
		}
		origin, err := originOf(o.Origin)
		if err != nil {
			return nil, err
		}
		store := originStorage{origin: origin}
		for _, it := range o.LocalStorage {
			store.local = append(store.local, [2]string{it.Name, it.Value})
		}
		st.origins = append(st.origins, store)
	}
	return st, nil
}

// captureState reads everything the login left in s's browser context: all
// of the context's cookies, plus local and session storage of the app origin
// (storage is per origin, and only the app's is the session). If the flow
// ended on another origin its storage is not captured, never guessed at.
func (s *Session) captureState(appOrigin string) (*State, error) {
	cookies, err := s.ctx.GetCookies()
	if err != nil {
		return nil, err
	}
	st := &State{}
	for _, c := range cookies {
		p := &proto.NetworkCookieParam{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite,
			Priority: c.Priority, PartitionKey: c.PartitionKey,
		}
		if !c.Session {
			p.Expires = c.Expires
		}
		st.cookies = append(st.cookies, p)
	}

	raw, err := s.Eval(`() => {
		const dump = (name) => {
			try { const s = window[name]; return Object.keys(s).map(k => [k, s.getItem(k)]) }
			catch (e) { return [] }
		};
		return { origin: location.origin, local: dump('localStorage'), session: dump('sessionStorage') };
	}`)
	if err != nil {
		return nil, err
	}
	var snap struct {
		Origin  string      `json:"origin"`
		Local   [][2]string `json:"local"`
		Session [][2]string `json:"session"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	if snap.Origin == appOrigin && (len(snap.Local) > 0 || len(snap.Session) > 0) {
		st.origins = append(st.origins, originStorage{origin: appOrigin, local: snap.Local, session: snap.Session})
	}
	return st, nil
}

// seedScript is JavaScript that restores the state's storage into a new
// document. Every block is guarded by location.origin so a value never lands
// on another origin, and a key the page already holds is left alone. The
// state is embedded as JSON (which is valid JavaScript), never concatenated
// as text, and the script goes to Chrome over CDP only.
func (s *State) seedScript() (string, error) {
	var b strings.Builder
	for _, o := range s.origins {
		origin, err := json.Marshal(o.origin)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "if (location.origin === %s) {\n", origin)
		for _, area := range []struct {
			name  string
			items [][2]string
		}{{"localStorage", o.local}, {"sessionStorage", o.session}} {
			if len(area.items) == 0 {
				continue
			}
			items, err := json.Marshal(area.items)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "  try { for (const [k, v] of %s) { if (%s.getItem(k) === null) %s.setItem(k, v) } } catch (e) {}\n",
				items, area.name, area.name)
		}
		b.WriteString("}\n")
	}
	return b.String(), nil
}
