package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/secrets"
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

// RedactURL reduces a URL to where it points, safe to print; see
// secrets.RedactURL.
func RedactURL(raw string) string { return secrets.RedactURL(raw) }

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
	// FirstTest is the path (or absolute URL) of the first explicit test that
	// uses auth. The startup probe loads it signed out too, since that is the
	// page the guard will be judging first. Empty to probe the rest only.
	FirstTest string
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

	// anon marks the auth an auth: false test gets: no state, no setup, no
	// guard. It exists to carry the run's redaction setting alone.
	anon bool

	// loginPath is the configured login path, loginDisplay it redacted for
	// printing, loginKey its page identity under the base path. All empty
	// when the config names no login path.
	loginPath    string
	loginDisplay string
	loginKey     string
	// setupPath is the configured flow's start page, for the probe.
	setupPath string
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
		return nil, fmt.Errorf("auth: base url %s is not a valid URL", RedactURL(baseURL))
	}
	origin, err := originOf(baseURL)
	if err != nil {
		return nil, fmt.Errorf("auth: base url: %w", err)
	}
	a := &Auth{
		baseURL:  baseURL,
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
	if cfg.Auth.Setup != nil {
		a.setupPath = cfg.Auth.Setup.Path
	}
	if route := cfg.Auth.LoginRoute(); route != "" {
		a.loginPath = route
		lu, err := url.Parse(route)
		if err != nil {
			return nil, fmt.Errorf("auth: login path %s is not a valid URL path", RedactURL(route))
		}
		a.loginDisplay = RedactURL(route)
		// The login path is relative to the base URL, so an app served under a
		// prefix (http://host/app) has its login page at /app/login.
		lu.Path = a.basePath + lu.Path
		a.loginKey = guardKey(lu)
	}

	// The marker must not match a page with no session, or the guard would
	// pass the login page it is there to catch. Checked before anything
	// runs, so a bad marker costs one page load, not a scan of the wrong page.
	if err := a.probeSignedOut(b); err != nil {
		return nil, err
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

// Anonymous is the auth an auth: false test gets: no session, no setup, no
// guard. It keeps only the run's redaction, which follows the config, not the
// test. Nil stays nil.
func (a *Auth) Anonymous() *Auth {
	if a == nil {
		return nil
	}
	c := *a
	c.state, c.setup, c.anon = nil, nil, true
	return &c
}

// readiness is the readiness the startup probe and the setup flow's first
// load use: the configured one, except that a selector waitFor is replaced by
// load. A selector is typically something only signed-in pages render, so
// waiting for it signed out would time out on every run, for no reason the
// error could name.
func readiness(waitFor string) string {
	switch waitFor {
	case "", "load", "networkIdle":
		return waitFor
	}
	return "load"
}

// RedactingAuth is the auth of a run whose config has an auth block but where
// nothing uses it (every test says auth: false and there is no discovery): no
// session, no guard, only the redaction of printed URLs.
func RedactingAuth() *Auth { return &Auth{anon: true} }

type probeResult struct {
	err     error
	landed  string // origin the page ended on, when it is not the app's
	checked bool   // the marker was looked for, on the app's origin
	present bool
}

// probeSignedOut loads pages in fresh contexts with no session and no storage
// seed, and fails if signedIn matches on any of them: a marker that also
// matches a signed-out page (main, nav, a layout shell that renders before
// auth resolves) cannot tell the two apart, and the run would scan a login
// form and pass. The pages are the base URL, the login path, the setup page,
// and the first explicit test that uses auth. The marker is waited for the
// way the guard waits for it (signedInWait), so a marker drawn a second after
// load is seen here as it would be there; the pages load in parallel so that
// costs one wait, not one per page. A page that bounces off the app's origin
// (to an identity provider) cannot be checked, which is a warning, not a
// failure.
func (a *Auth) probeSignedOut(b *Browser) error {
	type page struct {
		what, url string
		root      bool
	}
	pages := []page{{"the base URL", config.JoinURL(a.baseURL, "/"), true}}
	add := func(what, u string) {
		for _, p := range pages {
			if p.url == u {
				return
			}
		}
		pages = append(pages, page{what: what, url: u})
	}
	if a.loginPath != "" {
		add("the login path "+a.loginDisplay, config.JoinURL(a.baseURL, a.loginPath))
	}
	if a.setupPath != "" {
		add("the setup page "+RedactURL(a.setupPath), config.JoinURL(a.baseURL, a.setupPath))
	}
	if t := a.opts.FirstTest; t != "" {
		u := t
		if !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") {
			u = config.JoinURL(a.baseURL, t)
		}
		add("the first test that uses auth ("+RedactURL(t)+")", u)
	}

	results := make([]probeResult, len(pages))
	var wg sync.WaitGroup
	for i, p := range pages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = a.probeOne(b, p.url)
		}()
	}
	wg.Wait()

	// Warnings already collected are always said, even when a later page ends
	// the probe with an error.
	var warns []string
	defer func() {
		for _, w := range warns {
			a.warnf("%s", w)
		}
	}()
	checked := 0
	var probeErr error
	var bounced []string // pages that ended off the app's origin
	var bouncedTo []string
scan:
	for i, r := range results {
		p := pages[i]
		switch {
		case r.landed != "":
			// Off the app's origin, whatever happened after: the marker cannot be
			// checked there, and an identity provider's page that never settles
			// is not a failure of the app.
			bounced = append(bounced, p.what)
			if !slices.Contains(bouncedTo, r.landed) {
				bouncedTo = append(bouncedTo, r.landed)
			}
		case r.err != nil && p.root:
			// An unreachable root alone is not fatal: a run that never loads it
			// does not need it, and one that does fails on its own.
			warns = append(warns, fmt.Sprintf("auth: could not load %s signed out (%v): the signedIn marker was not checked there", p.what, r.err))
		case r.err != nil:
			probeErr = fmt.Errorf("auth: probing %s signed out: %w", p.what, r.err)
			break scan
		case r.checked && r.present:
			probeErr = &redirectError{fmt.Sprintf("auth: the signedIn selector %q also matches %s with no session: pick a marker that only signed-in pages "+
				"render, such as a test id on content that needs the user (not main, nav, header or a layout shell, which render before auth resolves)",
				a.signedIn, p.what)}
			break scan
		}
		if r.checked {
			checked++
		}
	}
	switch len(bounced) {
	case 0:
	case 1:
		warns = append(warns, fmt.Sprintf("auth: probing %s signed out landed on %s, not the app's origin %s: the signedIn marker was not checked on that page",
			bounced[0], strings.Join(bouncedTo, ", "), a.origin))
	default:
		// A fully SSO-gated app bounces every page: one line, not one per page.
		warns = append(warns, fmt.Sprintf("auth: %d probed pages (%s) landed on %s, not the app's origin %s: the signedIn marker was not checked on them",
			len(bounced), strings.Join(bounced, "; "), strings.Join(bouncedTo, ", "), a.origin))
	}
	if probeErr != nil {
		return probeErr
	}
	if checked == 0 {
		warns = append(warns, "auth: no page loaded signed out on the app's origin, so the signedIn marker could not be verified as absent without a session")
	}
	return nil
}

func (a *Auth) probeOne(b *Browser, url string) probeResult {
	s, err := b.open(a.defaults.Viewport, a.defaults.Timeout.Std(), nil)
	if err != nil {
		return probeResult{err: err}
	}
	defer s.Close()
	s.redact = true
	// load, whatever the configured readiness: networkIdle waits out every open
	// request, and an identity provider's page that keeps one open would run the
	// page's whole timeout for nothing the probe needs.
	navErr := s.navigate(url, "load", a.defaults.SettleTime.Std())
	// Where the page ended is read before anything else can go wrong, and a
	// page that is off the app's origin is a warning whatever the error was.
	landed, lerr := s.landedLoosely()
	if lerr != nil && navErr == nil {
		// The page loaded, but its main thread is busy: the quick read gave up,
		// and the page is a valid one of the app's. Read again with the full budget.
		landed, lerr = s.landedAt()
	}
	if lerr == nil && (landed.Scheme == "http" || landed.Scheme == "https") {
		// Only a real page counts: a failed navigation leaves chrome-error://
		// behind, and that is the failure, not a bounce.
		if o := originOf2(landed); o != a.origin {
			return probeResult{landed: o}
		}
	}
	if navErr != nil {
		return probeResult{err: navErr}
	}
	if lerr != nil {
		return probeResult{err: lerr}
	}
	return probeResult{checked: true, present: s.has(a.signedIn)}
}

// landedLoosely is landedAt that still works after the page's own time budget
// ran out, which is when the answer matters most.
func (s *Session) landedLoosely() (*url.URL, error) {
	p := s.raw.Timeout(3 * time.Second)
	defer p.CancelTimeout()
	res, err := p.Eval(`() => location.href`)
	if err != nil {
		return nil, errors.New("could not read the landing URL")
	}
	return parseLanding(res.Value.Str())
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
	if err := s.navigate(config.JoinURL(a.baseURL, setup.Path), readiness(a.defaults.WaitFor), settle); err != nil {
		return fmt.Errorf("auth setup: %w", err)
	}
	for i, st := range setup.Steps {
		a.logf("auth setup: step %d: %s", i, describeStep(st))
		if err := s.Step(st, a.baseURL, readiness(a.defaults.WaitFor), settle); err != nil {
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
	guarded := a != nil && !a.anon
	var state *State
	if guarded {
		state = a.state
	}
	s, err := b.open(vp, timeout, state)
	if err != nil {
		return nil, err
	}
	// Redaction follows the run's auth config, an auth: false test included;
	// the guard and the session follow the test's.
	s.redact = a != nil
	if guarded {
		s.auth = a
	}
	if guarded && a.setup != nil {
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
// the signed-in app: it is on another origin (allowedOrigins is for a setup
// flow's hops, not for a test), on the configured login path, or without the
// signedIn marker, which is the main signal and must be present within a
// short wait. There is no exemption: a test of the login page itself, or of a
// signed-out state, sets auth: false and has no session and no guard. A nil
// error for a session with no guard. It runs after the first load and again
// around every check that reports on the page, because a goto step, or the
// check itself, can walk the page into the login page after the first load
// passed.
func (s *Session) CheckLanding() error {
	a := s.auth
	if a == nil {
		return nil
	}
	landed, err := s.landedAt()
	if err != nil {
		return err
	}
	if o := originOf2(landed); o != a.origin {
		return &redirectError{fmt.Sprintf("landed on %s, not on the app's origin %s: the auth session was not applied or has expired "+
			"(auth.allowedOrigins is for a setup flow's hops, not for tests)", o, a.origin)}
	}
	if a.onLogin(landed) {
		return &redirectError{fmt.Sprintf("landed on the login page %s: the auth session was not applied or has expired "+
			"(to test the login page itself, set auth: false on that test)", a.loginDisplay)}
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
	return parseLanding(res.Value.Str())
}

// parseLanding parses a page's location. A url.Error would carry the whole URL
// into the message, token and all, so the error says nothing about it.
func parseLanding(href string) (*url.URL, error) {
	u, err := url.Parse(href)
	if err != nil {
		return nil, errors.New("could not parse the landing URL")
	}
	return u, nil
}

func (s *Session) signedInWait() time.Duration { return min(signedInWaitMax, s.timeout/4) }

// has reports whether selector matches within the short signedIn wait.
func (s *Session) has(selector string) bool {
	p := s.page.Timeout(s.signedInWait())
	defer p.CancelTimeout()
	_, err := p.Element(selector)
	return err == nil
}

// onLogin reports whether u is the configured login path (under the base
// path), by path plus hash route. Callers have compared the origin.
func (a *Auth) onLogin(u *url.URL) bool {
	return a.loginKey != "" && guardKey(u) == a.loginKey
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
		// A url.Error carries the whole URL, query and userinfo included.
		return "", fmt.Errorf("%s is not a valid URL", RedactURL(raw))
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
