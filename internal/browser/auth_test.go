package browser

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/config"
)

// authApp is a small app with a real login: the form POSTs, the server sets
// an HttpOnly session cookie, and the page stores a token in localStorage.
// /app needs both, so a run that restores only one of them is told apart
// from one that restores the session.
type authApp struct {
	*httptest.Server
	logins atomic.Int32
	// idp is another app's URL that /sso-start sends the browser to, and
	// bounceTo where /bounce redirects; loginTo where GET /login redirects
	// instead of rendering the form. loginDelay slows POST /login.
	idp, bounceTo, loginTo string
	loginDelay, rootDelay  time.Duration
	rootCookie             string
}

func newAuthApp(t *testing.T) *authApp {
	t.Helper()
	app := &authApp{}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			time.Sleep(app.loginDelay)
			app.logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "valid", Path: "/", HttpOnly: true})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if app.loginTo != "" {
			http.Redirect(w, r, app.loginTo+"/login", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>login</title></head><body>
			<form id="f"><input id="email" name="email"><input id="password" type="password" name="password">
			<button type="submit">go</button></form>
			<script>
			f.addEventListener('submit', async (e) => {
				e.preventDefault();
				const res = await fetch('/login', { method: 'POST', body: new URLSearchParams(new FormData(f)) });
				if (res.ok) { localStorage.setItem('token', 'tok-123'); location.href = '/app'; }
			});
			</script></body></html>`)
	})
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("sid"); err != nil || c.Value != "valid" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>app</title></head><body>
			<div data-testid="app-shell"><i id="in"></i><span id="marker"></span></div>
			<script>marker.textContent = localStorage.getItem('token') === 'tok-123' ? 'authed' : 'no-token'</script>
			</body></html>`)
	})
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>probe</title></head><body>
			<i id="in"></i><pre id="t"></pre><script>t.textContent = 'token=' + localStorage.getItem('token')</script></body></html>`)
	})
	// A hash-routed page: it sends a signed-out visitor to /spa#/login
	// without any HTTP redirect.
	mux.HandleFunc("/spa", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>spa</title></head><body>
			<i id="in"></i><script>if (!localStorage.getItem('token')) location.hash = location.search.includes('withquery') ? '#/login?redirect=/x' : '#/login'</script></body></html>`)
	})
	// The app side of an SSO round trip: /sso-start sends the browser to the
	// identity provider, which stores something of its own and comes back to
	// /cb, where the session cookie is issued.
	mux.HandleFunc("/sso-start", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><title>sso</title></head><body>
			<button id="go" onclick="location.href='%[1]s/idp?cb=' + encodeURIComponent(location.origin + '/cb')">sign in</button>
			<button id="stay" onclick="location.href='%[1]s/idp-end'">stay at the provider</button></body></html>`, app.idp)
	})
	mux.HandleFunc("/cb", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "valid", Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/app", http.StatusFound)
	})
	mux.HandleFunc("/idp", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>idp</title></head><body><script>
			localStorage.setItem('idp', 'idp-tok'); location.href = new URLSearchParams(location.search).get('cb')</script></body></html>`)
	})
	mux.HandleFunc("/idp-end", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>idp</title></head><body><main id="idp-end">idp</main></body></html>`)
	})
	mux.HandleFunc("/idp-probe", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>p</title></head><body>
			<pre id="t"></pre><script>t.textContent = 'idp=' + localStorage.getItem('idp')</script></body></html>`)
	})
	mux.HandleFunc("/bounce", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, app.bounceTo+"/probe", http.StatusFound)
	})
	// A login rendered in place at the root: same URL signed in and out. The
	// root also answers the capture's revisit of an origin: rootCookie is set
	// then, and rootDelay stalls it.
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(app.rootDelay)
		if app.rootCookie != "" {
			http.SetCookie(w, &http.Cookie{Name: app.rootCookie, Value: "1", Path: "/"})
		}
		shell := ""
		if c, err := r.Cookie("sid"); err == nil && c.Value == "valid" {
			shell = `<div id="shell">app</div>`
		}
		fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><title>home</title></head><body><main>%s<form><button>sign in</button></form></main></body></html>`, shell)
	})
	app.Server = httptest.NewServer(mux)
	t.Cleanup(app.Close)
	return app
}

const (
	testUser = "alice@example.com"
	testPass = "correct-horse-battery"
)

// loadAuthConfig writes a config around authYAML (the body of the auth:
// block) and loads it, with a short timeout so failure cases stay quick.
func loadAuthConfig(t *testing.T, baseURL, authYAML string) *config.Config {
	t.Helper()
	t.Setenv("FF_TEST_USER", testUser)
	t.Setenv("FF_TEST_PASS", testPass)
	path := filepath.Join(t.TempDir(), "frostfall.yml")
	if !strings.Contains(authYAML, "signedIn") {
		authYAML += "  signedIn: \"#in\"\n"
	}
	body := "version: 1\nserver:\n  baseUrl: " + baseURL + "\ndefaults:\n  timeout: 8s\n  settleTime: 50ms\ntests:\n  - id: t\n    path: /\nauth:\n" + authYAML
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, "")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

const setupYAML = `  setup:
    path: /login
    steps:
      - fill: { "#email": "${FF_TEST_USER}" }
      - fill: { "#password": "${FF_TEST_PASS}" }
      - click: "button[type=submit]"
      - waitFor: "[data-testid=app-shell]"
`

func launch(t *testing.T) *Browser {
	t.Helper()
	if testing.Short() {
		t.Skip("launches a browser")
	}
	b, err := Launch("")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

// open starts an authenticated session at path and returns its page text.
func openAuthed(t *testing.T, b *Browser, a *Auth, cfg *config.Config, base, path string) (*Session, error) {
	t.Helper()
	d := cfg.Defaults
	return b.NewAuthSession(a, base+path, d.Viewport, "load", 0, d.Timeout.Std())
}

func markerOf(t *testing.T, s *Session) string {
	t.Helper()
	raw, err := s.Eval(`() => document.getElementById('marker')?.textContent ?? ''`)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Trim(string(raw), `"`)
}

func TestSetupReuseRunsOnceAndSeedsEveryTest(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, setupYAML)

	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := app.logins.Load(); got != 1 {
		t.Fatalf("logins after setup = %d, want 1", got)
	}
	for i := range 2 {
		s, err := openAuthed(t, b, a, cfg, app.URL, "/app")
		if err != nil {
			t.Fatalf("test %d: %v", i, err)
		}
		if got := markerOf(t, s); got != "authed" {
			t.Errorf("test %d: marker = %q, want authed (cookie and localStorage token)", i, got)
		}
		s.Close()
	}
	if got := app.logins.Load(); got != 1 {
		t.Errorf("logins after two tests = %d, want 1: reuse must not log in again", got)
	}
}

func TestSetupWithoutReuseRunsPerTest(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, setupYAML+"  reuse: false\n")

	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := app.logins.Load(); got != 0 {
		t.Fatalf("logins before any test = %d, want 0", got)
	}
	for i := range 2 {
		s, err := openAuthed(t, b, a, cfg, app.URL, "/app")
		if err != nil {
			t.Fatalf("test %d: %v", i, err)
		}
		if got := markerOf(t, s); got != "authed" {
			t.Errorf("test %d: marker = %q, want authed", i, got)
		}
		s.Close()
	}
	if got := app.logins.Load(); got != 2 {
		t.Errorf("logins after two tests = %d, want 2", got)
	}
}

// stateFile writes a Playwright-shaped storage state for the app.
func stateFile(t *testing.T, app *authApp, cookieExpires string, withToken bool) string {
	t.Helper()
	u, _ := url.Parse(app.URL)
	origins := "[]"
	if withToken {
		origins = fmt.Sprintf(`[{"origin":%q,"localStorage":[{"name":"token","value":"tok-123"}]}]`, app.URL)
	}
	body := fmt.Sprintf(`{"cookies":[{"name":"sid","value":"valid","domain":%q,"path":"/","expires":%s,"httpOnly":true,"secure":false,"sameSite":"Lax"}],"origins":%s}`,
		u.Hostname(), cookieExpires, origins)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStorageStateInjection(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	for name, expires := range map[string]string{
		"session cookie": "-1",
		"persistent":     fmt.Sprint(time.Now().Add(time.Hour).Unix()),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, expires, true)+"\n  loginPath: /login\n")
			a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
			if err != nil {
				t.Fatal(err)
			}
			s, err := openAuthed(t, b, a, cfg, app.URL, "/app")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if got := markerOf(t, s); got != "authed" {
				t.Errorf("marker = %q, want authed", got)
			}
		})
	}
	if got := app.logins.Load(); got != 0 {
		t.Errorf("storageState must not run a login; logins = %d", got)
	}
}

// The worst failure of an auth feature is scanning the login page and
// passing. Each way a session can be missing must end in the guard's error.
func TestLoginRedirectGuard(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	const want = "landed on the login page /login: the auth session was not applied or has expired"

	t.Run("expired cookie in storage state", func(t *testing.T) {
		cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "1", true)+"\n  loginPath: /login\n")
		a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
		if err != nil {
			t.Fatal(err)
		}
		s, err := openAuthed(t, b, a, cfg, app.URL, "/app")
		if err == nil {
			s.Close()
			t.Fatal("scanned the login page without an error")
		}
		if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	})

	t.Run("a test that targets the login page is allowed to be there", func(t *testing.T) {
		cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "1", false)+"\n  loginPath: /login\n")
		a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
		if err != nil {
			t.Fatal(err)
		}
		s, err := openAuthed(t, b, a, cfg, app.URL, "/login")
		if err != nil {
			t.Fatalf("login page test failed the guard: %v", err)
		}
		s.Close()
	})

	t.Run("hash route", func(t *testing.T) {
		cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "1", false)+"\n  loginPath: /spa#/login\n")
		a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = openAuthed(t, b, a, cfg, app.URL, "/spa#/dashboard")
		if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), "/spa#/login") {
			t.Errorf("err = %v, want a login redirect naming /spa#/login", err)
		}
		// A query on the hash route is not part of which route it is.
		_, err = openAuthed(t, b, a, cfg, app.URL, "/spa?withquery")
		if !errors.Is(err, ErrLoginRedirect) {
			t.Errorf("/spa?withquery lands on #/login?redirect=/x: err = %v, want a login redirect", err)
		}
		// The same document is fine when the hash route it lands on is not
		// the login route: /spa itself without a token only ever reaches
		// #/login, so point the guard elsewhere.
		cfg = loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "1", false)+"\n  loginPath: /spa#/signin\n")
		a, err = NewAuth(b, cfg, app.URL, AuthOptions{})
		if err != nil {
			t.Fatal(err)
		}
		s, err := openAuthed(t, b, a, cfg, app.URL, "/spa")
		if err != nil {
			t.Fatalf("a different hash route tripped the guard: %v", err)
		}
		s.Close()
	})
}

func TestStorageNotInjectedOnOtherOrigin(t *testing.T) {
	app := newAuthApp(t)
	other := newAuthApp(t) // another port is another origin
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "-1", true)+"\n  loginPath: /login\n")
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	probe := func(base string) string {
		s, err := openAuthed(t, b, a, cfg, base, "/probe")
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
	if got := probe(app.URL); got != "token=tok-123" {
		t.Errorf("app origin: %q, want the token", got)
	}
	if got := probe(other.URL); got != "token=null" {
		t.Errorf("other origin: %q, want no token: storage must not cross origins", got)
	}
}

// The seed must run for the first load only: a page that removes its token
// (a logout) and navigates must not have it put back.
func TestSeedDoesNotOutliveFirstLoad(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, "  storageState: "+stateFile(t, app, "-1", true)+"\n  loginPath: /login\n")
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := openAuthed(t, b, a, cfg, app.URL, "/app")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Eval(`() => localStorage.removeItem('token')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Step(config.Step{Goto: "/probe"}, app.URL, "load", 0); err != nil {
		t.Fatal(err)
	}
	raw, err := s.Eval(`() => document.getElementById('t').textContent`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Trim(string(raw), `"`); got != "token=null" {
		t.Errorf("after removal and navigation: %q, want the token to stay gone", got)
	}
}

func TestSetupFailureNamesStepAndHidesValues(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	const secret = "hunter2-never-print-me"
	t.Setenv("FF_TEST_SECRET", secret)
	yaml := `  setup:
    path: /login
    steps:
      - fill: { "#email": "${FF_TEST_USER}" }
      - fill: { "#missing-field": "${FF_TEST_SECRET}" }
`
	cfg := loadAuthConfig(t, app.URL, yaml)
	cfg.Defaults.Timeout = config.Duration(2 * time.Second)

	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("setup with a missing selector succeeded")
		}
		msg := err.Error()
		if !strings.Contains(msg, "auth setup") || !strings.Contains(msg, "steps[1]") || !strings.Contains(msg, "#missing-field") {
			t.Errorf("error = %q, want auth setup, the step index, and the selector", msg)
		}
		for _, leaked := range append([]string{msg}, logs...) {
			if strings.Contains(leaked, secret) || strings.Contains(leaked, testUser) {
				t.Errorf("a fill value leaked: %q", leaked)
			}
		}
	}

	_, err := NewAuth(b, cfg, app.URL, AuthOptions{Log: logf})
	check(t, err)
	if !strings.Contains(strings.Join(logs, "\n"), `fill "#email"`) {
		t.Errorf("setup steps were not logged by action and selector: %q", logs)
	}

	// The per-test flavor reports the same way.
	logs = nil
	cfg = loadAuthConfig(t, app.URL, yaml+"  reuse: false\n")
	cfg.Defaults.Timeout = config.Duration(2 * time.Second)
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{Log: logf})
	if err != nil {
		t.Fatal(err)
	}
	_, err = openAuthed(t, b, a, cfg, app.URL, "/app")
	check(t, err)
}

func TestStateIsRedacted(t *testing.T) {
	st := &State{cookies: []*proto.NetworkCookieParam{{Name: "sid", Value: "s3cret"}}}
	for _, out := range []string{fmt.Sprintf("%v", st), fmt.Sprintf("%+v", st), fmt.Sprintf("%#v", st)} {
		if strings.Contains(out, "s3cret") {
			t.Errorf("state printed its values: %s", out)
		}
	}
}
