package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/config"
)

// ErrLoginRedirect marks a test or a setup that ended somewhere other than
// the signed-in app: the login page, or an origin nobody allowed. It is an
// environment failure, never a finding, because scanning a login page and
// passing is the one outcome this tool must not produce quietly. Test with
// errors.Is; the error's own text says where the page landed.
var ErrLoginRedirect = errors.New("login redirect")

type redirectError struct{ msg string }

func (e *redirectError) Error() string        { return e.msg }
func (e *redirectError) Is(target error) bool { return target == ErrLoginRedirect }

// signedInWaitMax caps how long a signedIn selector is waited for after a
// page is ready. The page has already loaded and settled, so the selector is
// either there or the session is missing; a quarter of the test budget is
// the ceiling for a short budget.
const signedInWaitMax = 5 * time.Second

// logoutRe matches paths that end a session when visited. A crawler that
// follows one signs itself out and every later page is scanned as the login
// page.
var logoutRe = regexp.MustCompile(`(?i)/(log|sign)[-_]?(out|off)\b`)

var pathParamRe = regexp.MustCompile(`;[^/]*`)

// RedactURL reduces a URL to where it points, safe to print: userinfo, the
// query, ;params on path segments (/login;jsessionid=...) and the fragment
// are dropped. A hash route (#/users) is the page's identity in a hash-routed
// app and is kept, without its own query. Setup paths and navigation targets
// are printed in errors and logs, and any of those parts can carry a token or
// credential. A URL that does not parse prints as a placeholder.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable url>"
	}
	out := pathParamRe.ReplaceAllString(u.Path, "")
	if u.Host != "" {
		out = u.Scheme + "://" + u.Host + out
	}
	if frag := u.Fragment; strings.HasPrefix(frag, "/") {
		if i := strings.IndexByte(frag, '?'); i >= 0 {
			frag = frag[:i]
		}
		out += "#" + pathParamRe.ReplaceAllString(frag, "")
	}
	return out
}

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

// AuthOptions are where NewAuth reports. Both may be nil.
type AuthOptions struct {
	// Log receives one line per setup step naming the action and selector,
	// never a value. The caller decides whether that is shown (--verbose).
	Log func(format string, args ...any)
	// Warn receives findings that do not stop the run but should always be
	// seen, such as a setup that left no session behind.
	Warn func(format string, args ...any)
}

// Auth carries everything a new test context needs to start signed in.
type Auth struct {
	baseURL  string
	basePath string
	origin   string
	allowed  map[string]bool
	signedIn string
	defaults config.Defaults
	opts     AuthOptions

	// state is restored into every context: the file's, or what the one
	// setup run captured. Nil when setup runs per context.
	state *State
	// setup is the login flow each context runs for itself (reuse: false).
	setup *config.AuthSetup

	// setupCfg is the configured flow, kept even when its state is captured
	// once, so discovery can capture its own.
	setupCfg *config.AuthSetup

	// loginDisplay is the configured login path redacted for printing;
	// loginKey is its page identity under the base path. Both empty when no
	// login route is known (storageState without loginPath).
	loginDisplay string
	loginKey     string
}

// NewAuth prepares the config's auth block: it loads a storage-state file's
// contents or, with setup and reuse, runs the login flow once now, before any
// test, in a context of its own. It returns nil for a config without auth. A
// setup failure is an environment error.
func NewAuth(b *Browser, cfg *config.Config, baseURL string, opts AuthOptions) (*Auth, error) {
	if cfg.Auth == nil {
		return nil, nil
	}
	bu, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("auth: base url: %w", err)
	}
	origin, err := originOf(baseURL)
	if err != nil {
		return nil, fmt.Errorf("auth: base url: %w", err)
	}
	a := &Auth{
		baseURL:  strings.TrimRight(baseURL, "/"),
		basePath: strings.TrimRight(bu.Path, "/"),
		origin:   origin,
		allowed:  map[string]bool{},
		signedIn: cfg.Auth.SignedIn,
		defaults: cfg.Defaults,
		opts:     opts,
	}
	for _, o := range cfg.Auth.AllowedOrigins {
		a.allowed[o] = true
	}
	if route := cfg.Auth.LoginRoute(); route != "" {
		lu, err := url.Parse(route)
		if err != nil {
			return nil, fmt.Errorf("auth: login path: %w", err)
		}
		a.loginDisplay = RedactURL(route)
		// The login path is relative to the base URL, so an app served under a
		// prefix (http://host/app) has its login page at /app/login.
		lu.Path = a.basePath + lu.Path
		a.loginKey = guardKey(lu)
	}
	a.setupCfg = cfg.Auth.Setup

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

// ForCrawl is the auth discovery crawls with. With reuse: false the flow would
// otherwise run once per crawled page, so it runs once here and its state is
// restored into every page instead. Otherwise it returns a unchanged.
func (a *Auth) ForCrawl(b *Browser) (*Auth, error) {
	if a == nil || a.state != nil || a.setupCfg == nil {
		return a, nil
	}
	crawl := *a
	crawl.setup = nil
	st, err := crawl.captureOnce(b, a.setupCfg)
	if err != nil {
		return nil, err
	}
	crawl.state = st
	return &crawl, nil
}

func (a *Auth) logf(format string, args ...any) {
	if a.opts.Log != nil {
		a.opts.Log(format, args...)
	}
}

func (a *Auth) warnf(format string, args ...any) {
	if a.opts.Warn != nil {
		a.opts.Warn(format, args...)
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
	s.redact = true
	track := s.trackOrigins()
	defer track.stop()
	if err := a.runSetup(s, setup); err != nil {
		return nil, err
	}
	st, err := a.capture(s, track.origins())
	if err != nil {
		return nil, fmt.Errorf("auth setup: capturing session: %w", err)
	}
	if len(st.cookies) == 0 && len(st.origins) == 0 {
		a.warnf("auth setup: the flow left no cookies or storage behind, so no session can be restored; every test will start signed out")
	}
	return st, nil
}

// runSetup drives the login flow in s through the ordinary step machinery,
// then checks that it actually ended signed in. Errors name the step index
// and the action's selector, and never a filled value or a URL query, so a
// failed login cannot leak a credential into CI logs.
func (a *Auth) runSetup(s *Session, setup *config.AuthSetup) error {
	settle := a.defaults.SettleTime.Std()
	a.logf("auth setup: loading %s", RedactURL(setup.Path))
	if err := s.navigate(a.baseURL+setup.Path, a.defaults.WaitFor, settle); err != nil {
		return fmt.Errorf("auth setup: %w", err)
	}
	for i, st := range setup.Steps {
		a.logf("auth setup: step %d: %s", i, describeStep(st))
		if err := s.Step(st, a.baseURL, a.defaults.WaitFor, settle); err != nil {
			return fmt.Errorf("auth setup: steps[%d]: %w", i, err)
		}
	}
	return a.checkSetupEnd(s)
}

// checkSetupEnd fails a setup that did not end signed in on the app. A flow
// that finishes on the login page, on another origin, or without the
// signedIn marker has produced no session, and capturing it anyway would
// start every test signed out. An SSO flow passes through its provider
// (allowedOrigins) but must finish back on the app.
func (a *Auth) checkSetupEnd(s *Session) error {
	landed, err := s.landedAt()
	if err != nil {
		return fmt.Errorf("auth setup: %w", err)
	}
	switch o := originOf2(landed); {
	case o != a.origin:
		return &redirectError{fmt.Sprintf("auth setup: the flow ended on %s, not on the app's origin %s: an SSO flow must finish back on the app "+
			"(end the steps with a waitFor for something only the signed-in app shows), and a base URL that redirects needs baseUrl set to the app's real origin; "+
			"auth.allowedOrigins only lists providers the flow passes through", o, a.origin)}
	case a.onLogin(landed):
		return &redirectError{fmt.Sprintf("auth setup: still on the login page %s after the steps: the credentials were rejected, "+
			"or the steps need a final waitFor for something only the signed-in app shows", a.loginDisplay)}
	case !s.has(a.signedIn):
		return &redirectError{fmt.Sprintf("auth setup: signedIn selector %q not found after the steps (waited up to %s): the login did not complete",
			a.signedIn, s.signedInWait())}
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
		return fmt.Sprintf("goto %q", RedactURL(st.Goto))
	}
	return "step"
}

// NewAuthSession is NewSession for an authenticated run: the context is
// seeded with a's state before its first navigation, or runs a's setup flow
// first (reuse: false), and the landing page is checked. A nil a is plain
// NewSession.
func (b *Browser) NewAuthSession(a *Auth, url string, vp config.Viewport, waitFor string, settle, timeout time.Duration) (*Session, error) {
	var state *State
	if a != nil {
		state = a.state
	}
	s, err := b.open(vp, timeout, state)
	if err != nil {
		return nil, err
	}
	s.auth, s.testURL, s.redact = a, url, a != nil
	if a != nil && a.setup != nil {
		// A login is not part of the test: it runs under the budget a login
		// gets anywhere else, and the test's own budget starts after it.
		s.setTimeout(a.defaults.Timeout.Std())
		if err := a.runSetup(s, a.setup); err != nil {
			s.Close()
			return nil, err
		}
		s.setTimeout(timeout)
	}
	if err := s.navigate(url, waitFor, settle); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.CheckLanding(); err != nil {
		s.Close()
		return nil, err
	}
	// The storage seed is for the first load only. Left registered it would
	// run again on every later navigation and put back a token the page had
	// removed (a logout step), hiding the very behavior under test.
	s.dropSeed()
	return s, nil
}

// CheckLanding fails with ErrLoginRedirect when the session's page is not
// the signed-in app. On the app's origin the signedIn marker must be present;
// being on the login route fails too, with a clearer message. A test on the
// app's origin that lands on any other origin fails, allowedOrigins
// included: those are for a setup flow's hops, and a test is not one. A test
// that is itself about the login route may land on it. A test whose own URL
// is on another origin is not judged by the app's session, except that
// landing on the app's login page still fails. A nil error for an
// unauthenticated session. It runs after the first load and again around
// every check that reports on the page, because a goto step, or the check
// itself, can walk the page into the login page after the first load passed.
func (s *Session) CheckLanding() error {
	a := s.auth
	if a == nil {
		return nil
	}
	landed, err := s.landedAt()
	if err != nil {
		return err
	}
	var want *url.URL
	if u, err := url.Parse(s.testURL); err == nil && u.Host != "" {
		want = u
	}
	landedOrigin := originOf2(landed)
	testOrigin := ""
	if want != nil {
		testOrigin = originOf2(want)
	}
	switch {
	case landedOrigin == a.origin:
	case testOrigin == a.origin:
		return &redirectError{fmt.Sprintf("landed on %s, not on the app's origin %s: the auth session was not applied or has expired "+
			"(auth.allowedOrigins does not apply to tests)", landedOrigin, a.origin)}
	default:
		return nil // a cross-origin test sent elsewhere is not judged by the app's session
	}
	onLogin := a.onLogin(landed)
	if onLogin && want != nil && a.onLogin(want) {
		return nil // the test is about the login page itself
	}
	if onLogin {
		return &redirectError{fmt.Sprintf("landed on the login page %s: the auth session was not applied or has expired", a.loginDisplay)}
	}
	if !s.has(a.signedIn) {
		return &redirectError{fmt.Sprintf("landed on %s without the signedIn selector %q (waited up to %s): "+
			"the auth session was not applied or has expired", RedactURL(landed.String()), a.signedIn, s.signedInWait())}
	}
	return nil
}

func (s *Session) landedAt() (*url.URL, error) {
	res, err := s.page.Eval(`() => location.href`)
	if err != nil {
		return nil, fmt.Errorf("reading landing url: %w", err)
	}
	return url.Parse(res.Value.Str())
}

func (s *Session) signedInWait() time.Duration { return min(signedInWaitMax, s.timeout/4) }

// has reports whether selector matches within the short signedIn wait.
func (s *Session) has(selector string) bool {
	p := s.page.Timeout(s.signedInWait())
	defer p.CancelTimeout()
	_, err := p.Element(selector)
	return err == nil
}

// onLogin reports whether u is the app's login route: same origin as the
// base URL and the same path (under the base path) plus hash route.
func (a *Auth) onLogin(u *url.URL) bool {
	return a.loginKey != "" && originOf2(u) == a.origin && guardKey(u) == a.loginKey
}

// guardKey is RouteKey with the query of a hash route removed, so
// /#/login?redirect=/x is the login route. RouteKey itself keeps that query
// (discovery treats two such routes as two pages); only the guard, which asks
// "is this the login page", ignores it.
func guardKey(u *url.URL) string {
	c := *u
	if i := strings.IndexByte(c.Fragment, '?'); i >= 0 {
		c.Fragment = c.Fragment[:i]
	}
	return RouteKey(&c)
}

// DiscoverExcludes are the paths a crawl must not follow when signed in:
// anything that looks like a logout (visiting one ends the session and the
// rest of the crawl reads the login page), and the login page itself.
func (a *Auth) DiscoverExcludes() []*regexp.Regexp {
	out := []*regexp.Regexp{logoutRe}
	if a.loginKey != "" {
		out = append(out, regexp.MustCompile(`^`+regexp.QuoteMeta(a.loginKey)+`(\?.*)?$`))
	}
	return out
}

// RouteKey is a URL's page identity; see config.RouteKey.
func RouteKey(u *url.URL) string { return config.RouteKey(u) }

// originOf is the scheme://host[:port] a browser reports as location.origin,
// lower-cased, with a scheme's default port dropped.
func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an http(s) URL", RedactURL(raw))
	}
	return config.OriginOfURL(u), nil
}

func originOf2(u *url.URL) string { return config.OriginOfURL(u) }

// originTracker records the origin of each main-frame navigation, so storage
// can be captured for every identity provider a flow passed through and not
// only the page it ended on.
type originTracker struct {
	mu   sync.Mutex
	seen map[string]bool
	stop func()
}

func (s *Session) trackOrigins() *originTracker {
	t := &originTracker{seen: map[string]bool{}}
	p, cancel := s.page.WithCancel()
	t.stop = cancel
	go p.EachEvent(func(e *proto.PageFrameNavigated) {
		if e.Frame == nil || e.Frame.ParentID != "" {
			return
		}
		if o, err := originOf(e.Frame.URL); err == nil {
			t.mu.Lock()
			t.seen[o] = true
			t.mu.Unlock()
		}
	})()
	return t
}

func (t *originTracker) origins() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.seen))
	for o := range t.seen {
		out = append(out, o)
	}
	return out
}

// stateFromFile converts a Playwright storage-state file into browser state.
// Cookies already expired are left out: Chrome silently drops an expired
// cookie from Storage.setCookies without failing the batch, so this changes
// no outcome, it only makes the intent plain. The expired session then shows
// up where it should, as the login-redirect guard.
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

// storageSnapshot is the local and session storage of the page s is on.
type storageSnapshot struct {
	Origin  string      `json:"origin"`
	Local   [][2]string `json:"local"`
	Session [][2]string `json:"session"`
}

func (s *Session) snapshotStorage() (*storageSnapshot, error) {
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
	var snap storageSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// capture reads everything the login left in s's browser context: all of
// the context's cookies, plus local and session storage of the app origin
// and of every allowed origin the flow passed through (storage is per origin
// and an identity provider's may be part of the session). The page the flow
// ended on is read in place; a visited origin it left is revisited at its
// root to read it, and skipped, never guessed at, if that fails.
func (a *Auth) capture(s *Session, visited []string) (*State, error) {
	st := &State{}
	add := func(snap *storageSnapshot) {
		if len(snap.Local) > 0 || len(snap.Session) > 0 {
			st.origins = append(st.origins, originStorage{origin: snap.Origin, local: snap.Local, session: snap.Session})
		}
	}
	final, err := s.snapshotStorage()
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	if final.Origin == a.origin || a.allowed[final.Origin] {
		add(final)
		done[final.Origin] = true
	}
	for _, o := range visited {
		if done[o] || (o != a.origin && !a.allowed[o]) {
			continue
		}
		done[o] = true
		if err := s.navigate(o+"/", "load", 0); err != nil {
			a.warnf("auth setup: could not revisit %s to capture its storage (%v); the session may be missing what that origin holds", o, err)
			continue
		}
		snap, err := s.snapshotStorage()
		if err != nil || snap.Origin != o {
			a.warnf("auth setup: could not read the storage of %s; the session may be missing what that origin holds", o)
			continue
		}
		add(snap)
	}

	// Cookies last: the revisits above can set or rotate them, and the state
	// must describe one moment, not storage from after and cookies from before.
	cookies, err := s.ctx.GetCookies()
	if err != nil {
		return nil, err
	}
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
