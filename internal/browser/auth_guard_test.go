package browser

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/config"
)

// allowing returns the auth YAML line that allows origins.
func allowing(origins ...string) string {
	q := make([]string, len(origins))
	for i, o := range origins {
		q[i] = fmt.Sprintf("%q", o)
	}
	return "  allowedOrigins: [" + strings.Join(q, ", ") + "]\n"
}

// openUnguarded opens url with a's state seeded and no landing check, to
// observe what the seed does on an origin the guard would refuse.
func openUnguarded(t *testing.T, b *Browser, a *Auth, url string) *Session {
	t.Helper()
	s, err := b.open(a.defaults.Viewport, 8*time.Second, a.state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.navigate(url, "load", 0); err != nil {
		t.Fatal(err)
	}
	return s
}

func authWith(t *testing.T, b *Browser, app *authApp, authYAML string) (*Auth, error) {
	t.Helper()
	cfg := loadAuthConfig(t, app.URL, authYAML)
	return NewAuth(b, cfg, app.URL, AuthOptions{})
}

// An SSO login page is on another origin. A test that ends up there was not
// signed in, and scanning it would pass a page the app does not own.
func TestCrossOriginLandingIsARedirect(t *testing.T) {
	app, idp := newAuthApp(t), newAuthApp(t)
	app.bounceTo = idp.URL
	b := launch(t)
	state := "  storageState: " + stateFile(t, app, "-1", true) + "\n  loginPath: /login\n"

	cfg := loadAuthConfig(t, app.URL, state)
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = openAuthed(t, b, a, cfg, app.URL, "/bounce")
	if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "auth.allowedOrigins") || !strings.Contains(err.Error(), idp.URL) {
		t.Fatalf("err = %v, want a redirect naming %s and auth.allowedOrigins", err, idp.URL)
	}

	// allowedOrigins is for the setup flow's hops. A test is not one: landing
	// on the provider is still a login redirect.
	cfg = loadAuthConfig(t, app.URL, state+allowing(idp.URL))
	a, err = NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = openAuthed(t, b, a, cfg, app.URL, "/bounce"); !errors.Is(err, ErrLoginRedirect) {
		t.Fatalf("an allowed origin excused a test landing off the app: err = %v", err)
	}
}

func TestSetupMustEndBackOnTheApp(t *testing.T) {
	app, idp := newAuthApp(t), newAuthApp(t)
	app.idp = idp.URL
	b := launch(t)
	flow := `  setup:
    path: /sso-start
    steps:
      - click: "#stay"
      - waitFor: "#idp-end"
`
	_, err := authWith(t, b, app, flow)
	if !errors.Is(err, ErrLoginRedirect) {
		t.Fatalf("err = %v, want a login redirect", err)
	}
	for _, want := range []string{"auth setup", "ended on " + idp.URL, "finish back on the app"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	// Naming the provider lets the flow pass through it, not end on it. The
	// provider's page carries the marker, so only the origin check can refuse
	// it, and the message must say that is what did.
	_, err = authWith(t, b, app, flow+allowing(idp.URL))
	if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "ended on "+idp.URL) {
		t.Errorf("a flow ending on an allowed origin was not refused for its origin: err = %v", err)
	}
}

// The base URL is where the app answers. If it redirects elsewhere (http to
// https, apex to www) the flow runs on the wrong origin and its storage would
// be dropped: that must be loud.
func TestSetupOnARedirectedBaseOriginFails(t *testing.T) {
	real := newAuthApp(t)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, real.URL+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	t.Cleanup(front.Close)
	b := launch(t)
	cfg := loadAuthConfig(t, front.URL, setupYAML)
	_, err := NewAuth(b, cfg, front.URL, AuthOptions{})
	if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "baseUrl") {
		t.Fatalf("err = %v, want the redirected base origin reported", err)
	}
}

func TestStorageCapturedForAllowedOriginsVisited(t *testing.T) {
	app, idp := newAuthApp(t), newAuthApp(t)
	app.idp = idp.URL
	b := launch(t)
	flow := `  setup:
    path: /sso-start
    steps:
      - click: "#go"
      - waitFor: "[data-testid=app-shell]"
`
	idpValue := func(a *Auth) string {
		s := openUnguarded(t, b, a, idp.URL+"/idp-probe")
		raw, err := s.Eval(`() => document.getElementById('t').textContent`)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Trim(string(raw), `"`)
	}

	a, err := authWith(t, b, app, flow+allowing(idp.URL))
	if err != nil {
		t.Fatal(err)
	}
	if got := idpValue(a); got != "idp=idp-tok" {
		t.Errorf("the allowed provider's storage was not captured: %q", got)
	}
	// Not allowed: the provider is just another origin, and its storage is not
	// part of the session. The flow itself still ends on the app and passes.
	a, err = authWith(t, b, app, flow)
	if err != nil {
		t.Fatal(err)
	}
	if got := idpValue(a); got != "idp=null" {
		t.Errorf("storage of an origin that was not allowed was captured: %q", got)
	}
}

func TestAppUnderAPathPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/prefix/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "valid", Path: "/", HttpOnly: true})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>l</title></head><body>
			<button id="go" onclick="fetch('/prefix/login', {method: 'POST'}).then(() => location.href = '/prefix/home')">go</button></body></html>`)
	})
	mux.HandleFunc("/prefix/home", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("sid"); err != nil || c.Value != "valid" {
			http.Redirect(w, r, "/prefix/login", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>h</title></head><body><main id="shell"><i id="in"></i>home</main></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	b := launch(t)
	app := &authApp{Server: srv}

	// loginPath is relative to the base URL: /login under /prefix is
	// /prefix/login, and a trailing slash on the base changes nothing.
	for _, base := range []string{srv.URL + "/prefix", srv.URL + "/prefix/"} {
		t.Run("expired session, base "+base, func(t *testing.T) {
			cfg := loadAuthConfig(t, base, "  storageState: "+stateFile(t, app, "1", false)+"\n  loginPath: /login\n")
			a, err := NewAuth(b, cfg, base, AuthOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = openAuthed(t, b, a, cfg, strings.TrimRight(base, "/"), "/home")
			if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "/login") {
				t.Errorf("err = %v, want the login redirect", err)
			}
		})
		t.Run("setup, base "+base, func(t *testing.T) {
			cfg := loadAuthConfig(t, base, "  setup:\n    path: /login\n    steps:\n      - click: \"#go\"\n      - waitFor: \"#shell\"\n")
			a, err := NewAuth(b, cfg, base, AuthOptions{})
			if err != nil {
				t.Fatalf("setup through a prefixed base: %v", err)
			}
			s, err := openAuthed(t, b, a, cfg, strings.TrimRight(base, "/"), "/home")
			if err != nil {
				t.Fatalf("signed in session under the prefix failed: %v", err)
			}
			s.Close()
		})
	}
}

// A login rendered in place, at the same URL as the app, cannot be told
// apart by path: only the signedIn selector can.
func TestSignedInSelector(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	auth := func(expires string) string {
		// A login rendered in place has no URL of its own to name: no loginPath.
		return "  storageState: " + stateFile(t, app, expires, false) + "\n  signedIn: \"#shell\"\n"
	}

	cfg := loadAuthConfig(t, app.URL, auth("-1"))
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := openAuthed(t, b, a, cfg, app.URL, "/")
	if err != nil {
		t.Fatalf("signed in page failed: %v", err)
	}
	s.Close()

	cfg = loadAuthConfig(t, app.URL, auth("1"))
	a, err = NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = openAuthed(t, b, a, cfg, app.URL, "/")
	if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), `signedIn selector "#shell"`) {
		t.Fatalf("err = %v, want the missing signedIn selector reported", err)
	}
	// The wait is capped at a quarter of the 8s test budget (2s here), not the budget.
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("waited %s for the selector, want about 2s", d)
	}
	if !strings.Contains(err.Error(), "2s") {
		t.Errorf("error does not say how long it waited: %v", err)
	}
}

func TestSetupSuccessIsChecked(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)

	t.Run("ends on the login page", func(t *testing.T) {
		_, err := authWith(t, b, app, "  setup:\n    path: /login\n    steps:\n      - waitFor: \"#email\"\n  loginPath: /login\n")
		if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "still on the login page /login") {
			t.Errorf("err = %v, want a setup that never logged in to fail", err)
		}
	})
	t.Run("signedIn marker absent", func(t *testing.T) {
		_, err := authWith(t, b, app, setupYAML+"  signedIn: \"#nope\"\n")
		if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), `signedIn selector "#nope"`) {
			t.Errorf("err = %v, want the missing marker reported", err)
		}
	})
	t.Run("signedIn marker present", func(t *testing.T) {
		if _, err := authWith(t, b, app, setupYAML+"  signedIn: \"#marker\"\n"); err != nil {
			t.Errorf("a setup that reached the signed-in app failed: %v", err)
		}
	})
	t.Run("a flow that leaves nothing behind warns even when not verbose", func(t *testing.T) {
		var warns []string
		cfg := loadAuthConfig(t, app.URL, "  setup:\n    path: /login\n    steps:\n      - goto: /probe\n      - waitFor: \"#t\"\n  loginPath: /login\n")
		_, err := NewAuth(b, cfg, app.URL, AuthOptions{Warn: func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }})
		if err != nil {
			t.Fatal(err)
		}
		if len(warns) != 1 || !strings.Contains(warns[0], "no cookies or storage") {
			t.Errorf("warnings = %q", warns)
		}
	})
}

// A token in a setup path or a navigation URL must not reach any error or log
// line: CI logs outlive the run.
func TestNoQueryTokensInMessagesOrLogs(t *testing.T) {
	const tok = "SECRETTOK123"
	app := newAuthApp(t)
	b := launch(t)
	var mu sync.Mutex
	var lines []string
	record := func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(f, a...))
	}
	noToken := func(t *testing.T, what string, err error) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		all := append([]string{}, lines...)
		if err != nil {
			all = append(all, err.Error())
		}
		for _, l := range all {
			if strings.Contains(l, tok) {
				t.Errorf("%s: the query token reached %q", what, l)
			}
		}
	}
	tokenPath := strings.Replace(setupYAML, "path: /login", "path: /login?token="+tok, 1)

	t.Run("setup logs", func(t *testing.T) {
		lines = nil
		cfg := loadAuthConfig(t, app.URL, tokenPath)
		if _, err := NewAuth(b, cfg, app.URL, AuthOptions{Log: record, Warn: record}); err != nil {
			t.Fatal(err)
		}
		if len(lines) == 0 {
			t.Fatal("nothing was logged")
		}
		noToken(t, "setup logs", nil)
	})
	t.Run("failing step", func(t *testing.T) {
		lines = nil
		cfg := loadAuthConfig(t, app.URL, "  setup:\n    path: /login?token="+tok+"\n    steps:\n      - click: \"#absent\"\n      - goto: /x?token="+tok+"\n")
		cfg.Defaults.Timeout = 2_000_000_000
		_, err := NewAuth(b, cfg, app.URL, AuthOptions{Log: record})
		if err == nil {
			t.Fatal("want a setup failure")
		}
		noToken(t, "failing step", err)
	})
	t.Run("login guard message", func(t *testing.T) {
		lines = nil
		cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "1", false)+"\n  loginPath: /login?token="+tok+"\n")
		a, err := NewAuth(b, cfg, app.URL, AuthOptions{Log: record})
		if err != nil {
			t.Fatal(err)
		}
		_, err = openAuthed(t, b, a, cfg, app.URL, "/app")
		if !errors.Is(err, ErrLoginRedirect) {
			t.Fatalf("err = %v, want a login redirect", err)
		}
		noToken(t, "guard", err)
	})
	t.Run("navigation failure", func(t *testing.T) {
		lines = nil
		// /drop answers the signed-out probe's / normally but hangs up on the
		// setup page, so it is setup's own navigation that fails.
		cfg := loadAuthConfig(t, app.URL, "  setup:\n    path: /drop?token="+tok+"&x=1\n    steps:\n      - click: \"#go\"\n")
		_, err := NewAuth(b, cfg, app.URL, AuthOptions{Log: record})
		if err == nil {
			t.Fatal("want a navigation failure")
		}
		noToken(t, "navigation", err)
	})
}

// With a login per context the flow gets its own budget, and the test's
// starts after it: a slow login must not eat the time a test is given.
func TestSetupBudgetIsSeparateFromTheTests(t *testing.T) {
	app := newAuthApp(t)
	app.loginDelay = 2500 * time.Millisecond
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, setupYAML+"  reuse: false\n") // defaults.timeout is 8s
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const testBudget = 2 * time.Second // shorter than the login
	s, err := b.NewAuthSession(a, app.URL+"/app", cfg.Defaults.Viewport, "load", 0, testBudget)
	if err != nil {
		t.Fatalf("a login slower than the test budget failed the test: %v", err)
	}
	defer s.Close()
	if s.timeout != testBudget {
		t.Errorf("session budget after setup = %s, want the test's %s", s.timeout, testBudget)
	}
	if got := markerOf(t, s); got != "authed" {
		t.Errorf("marker = %q", got)
	}
}

func TestCloseDisposesTheContext(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "-1", true)+"\n  loginPath: /login\n")
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	contexts := func() []proto.BrowserBrowserContextID {
		res, err := proto.TargetGetBrowserContexts{}.Call(b.rod)
		if err != nil {
			t.Fatal(err)
		}
		return res.BrowserContextIDs
	}
	s, err := openAuthed(t, b, a, cfg, app.URL, "/app")
	if err != nil {
		t.Fatal(err)
	}
	id := s.ctx.BrowserContextID
	if !slices.Contains(contexts(), id) {
		t.Fatal("the session's context is not live")
	}
	s.Close()
	if slices.Contains(contexts(), id) {
		t.Error("Close left the signed-in context, and its cookies, alive")
	}
}

// The login route is a path on the app's origin. The same path on another
// origin is not it, and a test that asks for that other page is not asking
// for the app's login page.
func TestRedirectIntoTheAppLoginPageFromAnotherOriginFails(t *testing.T) {
	app, other := newAuthApp(t), newAuthApp(t)
	other.loginTo = app.URL // other.URL/login sends the browser to the app's login page
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "1", false)+"\n  loginPath: /login\n")
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = openAuthed(t, b, a, cfg, other.URL, "/login")
	if !errors.Is(err, ErrLoginRedirect) {
		t.Errorf("err = %v, want the redirect into the app's login page caught", err)
	}
}

// Where the URL says nothing (a page that is not the login route and not the
// app either), the marker decides: every landing on the app's origin needs it.
func TestSignedInMarkerIsRequiredOnEveryLanding(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "-1", true)+"\n") // no loginPath
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// /idp-probe is neither the login route (none is named) nor marked.
	_, err = openAuthed(t, b, a, cfg, app.URL, "/idp-probe")
	if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), `signedIn selector "#in"`) {
		t.Errorf("err = %v, want the missing marker reported", err)
	}
	s, err := openAuthed(t, b, a, cfg, app.URL, "/app")
	if err != nil {
		t.Fatalf("a marked page failed: %v", err)
	}
	s.Close()
}

// A marker that is also on the login path cannot tell signed in from signed
// out: the signed-out probe refuses the config before any test runs.
func TestMarkedLoginPathIsRefusedByTheProbe(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	// /probe carries the marker.
	cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "-1", true)+"\n  loginPath: /probe\n")
	_, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "login path") {
		t.Fatalf("err = %v, want the login path probe to refuse the marker", err)
	}
}

// With no loginPath there is no login route: nothing is ever "on" it, and the
// marker alone catches the unmarked login page.
func TestNoLoginPathMeansNoLoginRoute(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, setupYAML) // the setup path is not a login path
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatalf("a setup ending signed in failed: %v", err)
	}
	if a.loginKey != "" {
		t.Errorf("login key = %q, want none: setup.path is not a default login path", a.loginKey)
	}
	_, err = openAuthed(t, b, a, cfg, app.URL, "/login")
	if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "signedIn selector") {
		t.Errorf("err = %v, want the unmarked login page caught by the marker", err)
	}
}

func TestCaptureReadsCookiesAfterTheRevisits(t *testing.T) {
	app, idp := newAuthApp(t), newAuthApp(t)
	app.idp = idp.URL
	idp.rootCookie = "idpseen" // set when the capture revisits the provider's root
	b := launch(t)
	flow := "  setup:\n    path: /sso-start\n    steps:\n      - click: \"#go\"\n      - waitFor: \"[data-testid=app-shell]\"\n" + allowing(idp.URL)
	a, err := authWith(t, b, app, flow)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range a.state.cookies {
		names = append(names, c.Name)
	}
	if !slices.Contains(names, "idpseen") || !slices.Contains(names, "sid") {
		t.Errorf("cookies = %v, want sid from the flow and idpseen from the revisit: the state must describe one moment", names)
	}
}

func TestRevisitFailureWarns(t *testing.T) {
	app, idp := newAuthApp(t), newAuthApp(t)
	app.idp = idp.URL
	idp.rootDelay = 5 * time.Second // the capture's revisit of the provider stalls
	b := launch(t)
	flow := "  setup:\n    path: /sso-start\n    steps:\n      - click: \"#go\"\n      - waitFor: \"[data-testid=app-shell]\"\n" + allowing(idp.URL)
	cfg := loadAuthConfig(t, app.URL, flow)
	cfg.Defaults.Timeout = config.Duration(3 * time.Second)
	var warns []string
	if _, err := NewAuth(b, cfg, app.URL, AuthOptions{Warn: func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }}); err != nil {
		t.Fatal(err)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "could not revisit "+idp.URL) {
		t.Errorf("warnings = %q, want one naming the origin that could not be read", warns)
	}
}

// Without auth the output is what it always was: URLs are not redacted.
func TestNavigationErrorsAreUnredactedWithoutAuth(t *testing.T) {
	dead := newAuthApp(t)
	b := launch(t)
	dead.Close()
	_, err := b.NewSession(dead.URL+"/x?keep=1", config.Viewport{Width: 800, Height: 600}, "load", 0, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "/x?keep=1") {
		t.Errorf("err = %v, want the URL printed as it always was", err)
	}
}

// The marker must not match a page with no session: loading the app with
// none, the run learns that before it scans anything.
func TestSignedOutProbeRefusesAMarkerThatMatchesWithoutASession(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	state := "  storageState: " + stateFile(t, app, "-1", true) + "\n"

	t.Run("the base URL", func(t *testing.T) {
		// The root renders a <main> signed in or out: the in-place login case.
		cfg := loadAuthConfig(t, app.URL, state+"  signedIn: main\n")
		_, err := NewAuth(b, cfg, app.URL, AuthOptions{})
		if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "also matches the base URL") {
			t.Errorf("err = %v, want the marker refused for matching with no session", err)
		}
	})
	t.Run("the login path", func(t *testing.T) {
		cfg := loadAuthConfig(t, app.URL, state+"  loginPath: /login\n  signedIn: \"#email\"\n") // only the login form has it
		_, err := NewAuth(b, cfg, app.URL, AuthOptions{})
		if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "login path") {
			t.Errorf("err = %v, want the login path probe to refuse the marker", err)
		}
	})
	t.Run("the setup page before the steps", func(t *testing.T) {
		// /probe is public and marked, so the end-of-setup check alone would pass
		// a login that never happened.
		cfg := loadAuthConfig(t, app.URL, "  setup:\n    path: /probe\n    steps:\n      - waitFor: \"#t\"\n")
		_, err := NewAuth(b, cfg, app.URL, AuthOptions{})
		if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "already matches the setup page") {
			t.Errorf("err = %v, want the marker refused on the setup page", err)
		}
	})
	t.Run("a good marker passes", func(t *testing.T) {
		cfg := loadAuthConfig(t, app.URL, state+"  loginPath: /login\n")
		if _, err := NewAuth(b, cfg, app.URL, AuthOptions{}); err != nil {
			t.Errorf("a marker only signed-in pages render was refused: %v", err)
		}
	})
}

// An auth: false test belongs to a run with auth: its URLs are still printed
// without their secrets, though it gets no session and no guard.
func TestAnonymousSessionKeepsTheRunsRedaction(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "-1", true)+"\n")
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.NewAuthSession(a.Anonymous(), app.URL+"/drop?token=SECRETTOK123", cfg.Defaults.Viewport, "load", 0, 5*time.Second)
	if err == nil {
		t.Fatal("want a navigation failure")
	}
	if strings.Contains(err.Error(), "SECRETTOK123") {
		t.Errorf("an auth: false test printed a secret: %v", err)
	}
}
