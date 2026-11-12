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
)

// allowing returns the auth YAML line that allows origins.
func allowing(origins ...string) string {
	q := make([]string, len(origins))
	for i, o := range origins {
		q[i] = fmt.Sprintf("%q", o)
	}
	return "  allowedOrigins: [" + strings.Join(q, ", ") + "]\n"
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

	cfg = loadAuthConfig(t, app.URL, state+allowing(idp.URL))
	a, err = NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := openAuthed(t, b, a, cfg, app.URL, "/bounce")
	if err != nil {
		t.Fatalf("an allowed origin still failed: %v", err)
	}
	s.Close()
}

func TestSetupMustEndOnTheAppOrAnAllowedOrigin(t *testing.T) {
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
	for _, want := range []string{"auth setup", idp.URL, "baseUrl", "auth.allowedOrigins"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if _, err := authWith(t, b, app, flow+allowing(idp.URL)); err != nil {
		t.Errorf("a flow ending on an allowed origin failed: %v", err)
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
		cfg := loadAuthConfig(t, app.URL, flow)
		s, err := b.NewAuthSession(a, idp.URL+"/idp-probe", cfg.Defaults.Viewport, "load", 0, 8*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
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
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>h</title></head><body><main id="shell">home</main></body></html>`)
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
		return "  storageState: " + stateFile(t, app, expires, false) + "\n  loginPath: /\n  signedIn: \"#shell\"\n"
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
		_, err := authWith(t, b, app, "  setup:\n    path: /login\n    steps:\n      - waitFor: \"#email\"\n")
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
		cfg := loadAuthConfig(t, app.URL, "  setup:\n    path: /probe\n    steps:\n      - waitFor: \"#t\"\n  loginPath: /login\n")
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
		dead := newAuthApp(t)
		cfg := loadAuthConfig(t, dead.URL, tokenPath)
		dead.Close()
		_, err := NewAuth(b, cfg, dead.URL, AuthOptions{Log: record})
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
func TestLoginRouteIsScopedToTheAppOrigin(t *testing.T) {
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
