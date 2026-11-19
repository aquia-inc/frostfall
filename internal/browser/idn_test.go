package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"

	"github.com/aquia-inc/frostfall/internal/browser/chrometest"
	"github.com/aquia-inc/frostfall/internal/config"
)

// config.CanonicalHost must write a non-ASCII host exactly as Chrome's URL
// parser does, and refuse what Chrome refuses: every origin comparison the
// guard makes is between the two. The rows are the ones where IDNA profiles
// differ (STD3 rules, hyphen checks, transitional processing, joiners):
// idna.Lookup, for one, refuses the underscore and leading-hyphen rows that
// Chrome loads, and would turn those hosts into config errors.
func TestCanonicalHostMatchesChrome(t *testing.T) {
	b := launch(t)
	s, err := b.NewSession("about:blank", config.Viewport{Width: 800, Height: 600}, "load", 0, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hosts := []string{
		"café.example", "CAFÉ.Example", "faß.de", "a_b.café.example", "-café.example", "café-.example",
		"ab--café.example", "ｃａｆｅ.example", "café。example", "xn--caf-dma.café.example", "a­b.café.example",
		"a‍b.example", "�.example", "א.example", "א1.example", "1א.example", "é.example.",
	}
	in, _ := json.Marshal(hosts)
	raw, err := s.Eval(`(hs) => JSON.stringify(JSON.parse(hs).map(h => { try { return new URL('http://' + h + '/').hostname } catch (e) { return null } }))`, string(in))
	if err != nil {
		t.Fatal(err)
	}
	var js string
	if err := json.Unmarshal(raw, &js); err != nil {
		t.Fatal(err)
	}
	var chrome []*string
	if err := json.Unmarshal([]byte(js), &chrome); err != nil {
		t.Fatal(err)
	}
	for i, h := range hosts {
		got, err := config.CanonicalHost(h)
		switch c := chrome[i]; {
		case c == nil && err == nil:
			t.Errorf("%q: Chrome refuses it, CanonicalHost gave %q", h, got)
		case c != nil && err != nil:
			t.Errorf("%q: Chrome loads it as %q, CanonicalHost refused it: %v", h, *c, err)
		case c != nil && got != *c:
			t.Errorf("%q: CanonicalHost = %q, Chrome = %q", h, got, *c)
		}
	}
}

// idnPuny is the fixture's IDN host as the browser writes it; the tests'
// base URLs spell it café.example. Chrome resolves it to the test server.
const (
	idnPuny  = "xn--caf-dma.example"
	idnRules = "MAP " + idnPuny + " 127.0.0.1"
)

// idnBase is the app's base URL on the IDN host, as a user would write it.
func idnBase(app *authApp) string {
	return "http://café.example:" + app.URL[strings.LastIndex(app.URL, ":")+1:]
}

// launchIDN launches a browser as Launch does, with the IDN host mapped to the
// loopback address.
func launchIDN(t *testing.T) *Browser {
	t.Helper()
	if testing.Short() {
		t.Skip("launches a browser")
	}
	l := launcher.New().Headless(true).Set("disable-dev-shm-usage").Set("host-resolver-rules", idnRules)
	if path, ok := launcher.LookPath(); ok {
		l = l.Bin(path)
	}
	u, err := l.Launch()
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	r := rod.New().ControlURL(u)
	if err := r.Connect(); err != nil {
		l.Kill()
		l.Cleanup()
		t.Fatalf("connect: %v", err)
	}
	b := &Browser{rod: r, launcher: l}
	t.Cleanup(b.Close)
	return b
}

// warnings collects an auth run's warnings, and logs everything else.
type warnings struct {
	mu   sync.Mutex
	list []string
}

func (w *warnings) opts(t *testing.T) AuthOptions {
	o := logOpts(t)
	o.Warn = func(f string, args ...any) {
		w.mu.Lock()
		w.list = append(w.list, fmt.Sprintf(f, args...))
		w.mu.Unlock()
	}
	o.FirstTest = "/app"
	return o
}

func (w *warnings) none(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.list) > 0 {
		t.Errorf("warnings on an IDN base URL: %q", w.list)
	}
}

// idnStateFile writes a storage state for the IDN app: the cookie's domain as
// Chrome writes it (punycode) and the localStorage origin in Unicode, so the
// token reaches the page only if the origin was normalized to what the page
// reports as location.origin.
func idnStateFile(t *testing.T, base, cookieExpires string) string {
	t.Helper()
	body := fmt.Sprintf(`{"cookies":[{"name":"sid","value":"valid","domain":%q,"path":"/","expires":%s,"httpOnly":true,"secure":false,"sameSite":"Lax"}],`+
		`"origins":[{"origin":%q,"localStorage":[{"name":"token","value":"tok-123"}]}]}`, idnPuny, cookieExpires, base)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantGuard checks that err is the guard's own refusal named by want, not the
// cross-origin one every IDN landing tripped before the host was normalized.
func wantGuard(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, ErrLoginRedirect) || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want the guard's %q", err, want)
	}
	if err != nil && strings.Contains(err.Error(), "not on the app's origin") {
		t.Errorf("the guard compared the IDN origin in two spellings: %v", err)
	}
}

// An IDN base URL with setup: the probe passes without a warning, the flow
// ends on the app, a signed-in test is reached, and the guard still refuses
// the login page.
func TestIDNBaseURLSetup(t *testing.T) {
	app := newAuthApp(t)
	b := launchIDN(t)
	base := idnBase(app)
	cfg := loadAuthConfig(t, base, setupYAML+"  loginPath: /login\n")
	var w warnings
	a, err := NewAuth(b, cfg, base, w.opts(t))
	if err != nil {
		t.Fatalf("setup on an IDN base URL: %v", err)
	}
	w.none(t)
	if a.origin != "http://"+idnPuny+app.URL[strings.LastIndex(app.URL, ":"):] {
		t.Errorf("origin = %q, want the punycode origin", a.origin)
	}
	s, err := openAuthed(t, b, a, cfg, base, "/app")
	if err != nil {
		t.Fatalf("a signed-in test on the IDN host: %v", err)
	}
	if got := markerOf(t, s); got != "authed" {
		t.Errorf("marker = %q, want authed", got)
	}
	s.Close()
	_, err = openAuthed(t, b, a, cfg, base, "/login")
	wantGuard(t, err, "landed on the login page /login")
}

// An IDN base URL with storageState: the Unicode origin in the file restores
// its localStorage, and an expired session still ends at the guard.
func TestIDNBaseURLStorageState(t *testing.T) {
	app := newAuthApp(t)
	b := launchIDN(t)
	base := idnBase(app)
	cfg := loadAuthConfig(t, base, "  storageState: "+idnStateFile(t, base, "-1")+"\n  loginPath: /login\n")
	var w warnings
	a, err := NewAuth(b, cfg, base, w.opts(t))
	if err != nil {
		t.Fatal(err)
	}
	w.none(t)
	s, err := openAuthed(t, b, a, cfg, base, "/app")
	if err != nil {
		t.Fatalf("a signed-in test on the IDN host: %v", err)
	}
	if got := markerOf(t, s); got != "authed" {
		t.Errorf("marker = %q, want authed (cookie and localStorage both restored)", got)
	}
	s.Close()

	cfg = loadAuthConfig(t, base, "  storageState: "+idnStateFile(t, base, "1")+"\n  loginPath: /login\n")
	a, err = NewAuth(b, cfg, base, logOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = openAuthed(t, b, a, cfg, base, "/app")
	wantGuard(t, err, "landed on the login page /login")
}

// An IDN base URL with attach: the session in the user's browser is taken
// and reaches the signed-in page, the probe says nothing about its own load,
// and a browser with no session still ends at the guard.
func TestIDNBaseURLAttach(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	app := newAuthApp(t)
	base := idnBase(app)
	port := app.URL[strings.LastIndex(app.URL, ":")+1:]

	t.Run("signed in", func(t *testing.T) {
		c := chrometest.StartWith(t, false, "--host-resolver-rules="+idnRules)
		sid := chrometest.Cookie("sid", "valid", idnPuny)
		sid.HTTPOnly = true
		c.SetCookies(t, sid)
		c.SetLocalStorage(t, "http://"+idnPuny+":"+port+"/probe", "token", "tok-123")
		b := connectTo(t, c, c.HTTP())
		cfg := loadAuthConfig(t, base, "  attach: true\n  loginPath: /login\n")
		var w warnings
		a, err := NewAuth(b, cfg, base, w.opts(t))
		if err != nil {
			t.Fatal(err)
		}
		w.none(t)
		s, err := openAuthed(t, b, a, cfg, base, "/app")
		if err != nil {
			t.Fatalf("the attached session did not reach the signed-in page: %v", err)
		}
		if got := markerOf(t, s); got != "authed" {
			t.Errorf("marker = %q, want authed", got)
		}
		s.Close()
	})
	t.Run("signed out", func(t *testing.T) {
		c := chrometest.StartWith(t, false, "--host-resolver-rules="+idnRules)
		b := connectTo(t, c, c.HTTP())
		cfg := loadAuthConfig(t, base, "  attach: true\n  loginPath: /login\n")
		a, err := NewAuth(b, cfg, base, logOpts(t))
		if err != nil {
			t.Fatal(err)
		}
		_, err = openAuthed(t, b, a, cfg, base, "/app")
		wantGuard(t, err, "landed on the login page /login")
	})
}

// A non-ASCII base URL host that the browser cannot load either is refused
// when the run starts, not turned into a failure on every test.
func TestInvalidIDNBaseURLIsRefused(t *testing.T) {
	b := launch(t)
	cfg := loadAuthConfig(t, "http://127.0.0.1:1", "  storageState: "+idnStateFile(t, "http://127.0.0.1:1", "-1")+"\n")
	_, err := NewAuth(b, cfg, "http://a‍b.example:8080/?token=SECRETTOK123", AuthOptions{})
	if !errors.Is(err, config.ErrInvalidIDN) {
		t.Errorf("err = %v, want ErrInvalidIDN", err)
	}
	if err != nil && strings.Contains(err.Error(), "SECRETTOK123") {
		t.Errorf("the error printed the query: %v", err)
	}
}
