package browser

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/browser/chrometest"
	"github.com/aquia-inc/frostfall/internal/config"
)

func cfgViewport() config.Viewport { return config.Viewport{Width: 1280, Height: 800} }

// pages lists the browser's page targets in a stable order.
func pages(t *testing.T, c *chrometest.Chrome) []chrometest.Target {
	t.Helper()
	ps := c.Pages(t)
	slices.SortFunc(ps, func(a, b chrometest.Target) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return ps
}

func startChrome(t *testing.T) *chrometest.Chrome {
	t.Helper()
	if testing.Short() {
		t.Skip("launches a browser")
	}
	return chrometest.Start(t, false)
}

func connectTo(t *testing.T, c *chrometest.Chrome, raw string) *Browser {
	t.Helper()
	e, err := ParseBrowserURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Connect(e)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

// The whole attach path against a real Chrome: the user's sign-in (an
// HttpOnly session cookie and a localStorage token, as authApp's /app needs)
// lives in the browser's own profile, next to another site's cookie and a tab
// the user has open. Frostfall takes the app's session and nothing else,
// reaches the signed-in page in an incognito context of its own, and leaves
// the browser, its tabs and its profile as they were.
func TestAttachTakesTheAppsSessionAndLeavesTheBrowserAlone(t *testing.T) {
	app := newAuthApp(t)
	idp := newAuthApp(t)
	idpOrigin := strings.Replace(idp.URL, "127.0.0.1", "localhost", 1)
	c := startChrome(t)

	httpOnly := chrometest.Cookie("sid", "valid", "127.0.0.1")
	httpOnly.HTTPOnly = true
	c.SetCookies(t, httpOnly,
		chrometest.Cookie("bank", "unrelated-session", "unrelated.example"),
		chrometest.Cookie("idpsid", "idp-session", "localhost"))
	c.SetLocalStorage(t, app.URL+"/probe", "token", "tok-123")
	c.SetLocalStorage(t, idpOrigin+"/probe", "idp", "idp-tok")
	userTab := c.OpenTab(t, app.URL+"/probe")
	before := pages(t, c)

	b := connectTo(t, c, c.HTTP())
	cfg := loadAuthConfig(t, app.URL, "  attach: true\n  allowedOrigins: [\""+idpOrigin+"\"]\n")
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, ck := range a.state.cookies {
		names = append(names, ck.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"idpsid", "sid"}) {
		t.Errorf("captured cookies %v, want [idpsid sid]: only the app's and the allowed origin's", names)
	}
	stored := map[string]bool{}
	for _, o := range a.state.origins {
		stored[o.origin] = true
		if len(o.session) > 0 {
			t.Errorf("sessionStorage captured for %s", o.origin)
		}
	}
	if !stored[strings.TrimRight(app.URL, "/")] || !stored[idpOrigin] || len(stored) != 2 {
		t.Errorf("captured localStorage of %v, want the app's and the allowed origin's", stored)
	}

	s, err := openAuthed(t, b, a, cfg, app.URL, "/app")
	if err != nil {
		t.Fatalf("the attached session did not reach the signed-in page: %v", err)
	}
	if got := markerOf(t, s); got != "authed" {
		t.Errorf("marker = %q, want authed (cookie and localStorage both restored)", got)
	}
	// Pages present the browser's own user-agent, the one the user signed in
	// with, not an emulated device's.
	ua, err := s.Eval(`() => navigator.userAgent`)
	if err != nil {
		t.Fatal(err)
	}
	if want := c.UserAgent(t); strings.Trim(string(ua), `"`) != want {
		t.Errorf("page user-agent = %s, want the browser's %q", ua, want)
	}
	s.Close()
	b.Close()

	if !c.VersionOK() {
		t.Fatal("the browser stopped answering after Close")
	}
	after := pages(t, c)
	if !slices.Equal(before, after) {
		t.Errorf("page targets changed:\nbefore %v\nafter  %v", before, after)
	}
	if !slices.ContainsFunc(after, func(tg chrometest.Target) bool { return tg.ID == userTab && tg.URL == app.URL+"/probe" }) {
		t.Errorf("the user's tab was closed or navigated: %v", after)
	}
	if left := c.Contexts(t); len(left) != 0 {
		t.Errorf("contexts left behind in the user's browser: %v", left)
	}
}

// A connection that drops without Close (a crash, a kill) still leaves no
// signed-in context behind: Chrome disposes them itself (disposeOnDetach).
func TestAttachedContextsGoWhenTheConnectionDrops(t *testing.T) {
	app := newAuthApp(t)
	c := startChrome(t)
	e, err := ParseBrowserURL(c.WS())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Connect(e)
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.NewSession(app.URL+"/probe", cfgViewport(), "load", 0, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = s
	if len(c.Contexts(t)) != 1 {
		t.Fatalf("contexts while open = %v, want one", c.Contexts(t))
	}
	_ = b.ws.Close() // what a killed process's socket does; Close never runs
	deadline := time.Now().Add(5 * time.Second)
	for len(c.Contexts(t)) != 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if left := c.Contexts(t); len(left) != 0 {
		t.Errorf("contexts left after the connection dropped: %v", left)
	}
	if !c.VersionOK() {
		t.Error("the browser stopped answering")
	}
}

// Close removes contexts a test forgot to close and the capture tab, never
// the browser.
func TestAttachedCloseRemovesLeftovers(t *testing.T) {
	app := newAuthApp(t)
	c := startChrome(t)
	before := pages(t, c)
	b := connectTo(t, c, c.HTTP())
	if _, err := b.NewSession(app.URL+"/probe", cfgViewport(), "load", 0, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	target, err := proto.TargetCreateTarget{URL: "about:blank", Background: true}.Call(b.rod)
	if err != nil {
		t.Fatal(err)
	}
	b.owned.addTarget(target.TargetID)
	b.Close()
	b.Close() // twice is harmless
	if left := c.Contexts(t); len(left) != 0 {
		t.Errorf("contexts left: %v", left)
	}
	if after := pages(t, c); !slices.Equal(before, after) {
		t.Errorf("page targets changed:\nbefore %v\nafter  %v", before, after)
	}
	if !c.VersionOK() {
		t.Error("Close quit the attached browser")
	}
}

// With nothing for the app in the user's browser, the capture warns, and the
// guard then refuses the login page as it does for any auth source.
func TestAttachWithNoSessionWarnsAndTheGuardRefuses(t *testing.T) {
	app := newAuthApp(t)
	c := startChrome(t)
	c.SetCookies(t, chrometest.Cookie("bank", "unrelated-session", "unrelated.example"))
	b := connectTo(t, c, c.HTTP())
	cfg := loadAuthConfig(t, app.URL, "  attach: true\n  loginPath: /login\n")
	var warns []string
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{Warn: func(f string, args ...any) { warns = append(warns, f) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.state.cookies) != 0 {
		t.Errorf("captured %d cookies from a browser with none for the app", len(a.state.cookies))
	}
	if !slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "holds no cookies or localStorage") }) {
		t.Errorf("no warning about the missing session: %v", warns)
	}
	if _, err := openAuthed(t, b, a, cfg, app.URL, "/app"); err == nil || !strings.Contains(err.Error(), "landed on the login page") {
		t.Errorf("err = %v, want the login guard", err)
	}
}

// attach: true on a launched browser is refused, not run signed out.
func TestAttachNeedsAnAttachedBrowser(t *testing.T) {
	app := newAuthApp(t)
	b := launch(t)
	cfg := loadAuthConfig(t, app.URL, "  attach: true\n")
	if _, err := NewAuth(b, cfg, app.URL, AuthOptions{}); err == nil || !strings.Contains(err.Error(), "--browser-url") {
		t.Errorf("err = %v, want it to ask for --browser-url", err)
	}
}

// Nothing listening is an error that says how to start Chrome, without the
// browser path.
func TestConnectUnreachable(t *testing.T) {
	e, err := ParseBrowserURL("ws://127.0.0.1:1/devtools/browser/" + secretGUID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Connect(e)
	if err == nil || !strings.Contains(err.Error(), "--user-data-dir") || strings.Contains(err.Error(), secretGUID) {
		t.Errorf("err = %v", err)
	}
	e, _ = ParseBrowserURL("http://127.0.0.1:1")
	if _, err := Connect(e); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("http endpoint: err = %v", err)
	}
}

// A browser closed while the session is being read (an interrupt) still has
// its capture tab removed: the tab is registered as soon as it exists.
func TestCloseDuringCaptureLeavesNoTab(t *testing.T) {
	app := newAuthApp(t)
	c := startChrome(t)
	before := pages(t, c)
	b := connectTo(t, c, c.HTTP())
	cfg := loadAuthConfig(t, app.URL, "  attach: true\n")
	captureTabOpened = b.Close
	t.Cleanup(func() { captureTabOpened = nil })
	if _, err := NewAuth(b, cfg, app.URL, AuthOptions{}); err == nil {
		t.Error("the capture went on after the browser was closed")
	}
	if after := pages(t, c); !slices.Equal(before, after) {
		t.Errorf("page targets changed:\nbefore %v\nafter  %v", before, after)
	}
	if left := c.Contexts(t); len(left) != 0 {
		t.Errorf("contexts left: %v", left)
	}
}

// A tab or context made after Close took the registry is removed at once,
// since Close will not run again.
func TestAddAfterCloseRemovesAtOnce(t *testing.T) {
	c := startChrome(t)
	before := pages(t, c)
	b := connectTo(t, c, c.HTTP())
	b.owned.take() // what Close does first
	if _, _, err := b.openCaptureTab(); err == nil {
		t.Error("a capture tab was opened after Close")
	}
	if _, err := b.incognito(); err == nil {
		t.Error("a context was created after Close")
	}
	if after := pages(t, c); !slices.Equal(before, after) {
		t.Errorf("page targets changed:\nbefore %v\nafter  %v", before, after)
	}
	if left := c.Contexts(t); len(left) != 0 {
		t.Errorf("contexts left: %v", left)
	}
}

// Reading the session sends nothing to the app (every request of the capture
// tab is answered by Frostfall or failed), leaves no tab, and leaves no
// visible history entry in the user's profile: the one navigation is marked
// auto_subframe, which Chrome stores as a hidden, never-typed row.
func TestCaptureSendsNothingToTheAppAndLeavesNoVisibleHistory(t *testing.T) {
	var userRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("sid"); err == nil && ck.Value == "valid" {
			userRequests.Add(1)
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body>page</body></html>`)
	}))
	defer srv.Close()
	c := startChrome(t)
	c.SetCookies(t, chrometest.Cookie("sid", "valid", "127.0.0.1"))
	c.SetLocalStorage(t, srv.URL+"/seed", "token", "tok-123")
	before := pages(t, c)
	userRequests.Store(0)

	b := connectTo(t, c, c.HTTP())
	cfg := loadAuthConfig(t, srv.URL, "  attach: true\n")
	var notes []string
	a, err := NewAuth(b, cfg, srv.URL, AuthOptions{Note: func(f string, args ...any) { notes = append(notes, fmt.Sprintf(f, args...)) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.state.cookies) != 1 || len(a.state.origins) != 1 || a.state.origins[0].local[0] != [2]string{"token", "tok-123"} {
		t.Errorf("captured %d cookies and storage %v", len(a.state.cookies), a.state.origins)
	}
	if n := userRequests.Load(); n != 0 {
		t.Errorf("%d request(s) with the user's session reached the app while it was read", n)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "(1 cookies, 1 localStorage keys)") || strings.Contains(notes[0], "tok-123") || strings.Contains(notes[0], "valid") {
		t.Errorf("notes = %q", notes)
	}
	b.Close()
	if after := pages(t, c); !slices.Equal(before, after) {
		t.Errorf("page targets changed:\nbefore %v\nafter  %v", before, after)
	}

	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not installed: the history check needs it")
	}
	c.Quit(t)
	out, err := exec.Command(sqlite, filepath.Join(c.Dir, "Default", "History"),
		"select hidden, typed_count from urls where url = '"+srv.URL+"/'").CombinedOutput()
	if err != nil {
		t.Fatalf("reading History: %v: %s", err, out)
	}
	if row := strings.TrimSpace(string(out)); row != "" && row != "1|0" {
		t.Errorf("history row for the app root = %q, want none or hidden and never typed (1|0)", row)
	}
}

// The user's Chrome is headed, and the window a test runs in is often not the
// focused one. Focus emulation keeps document.hasFocus() true on Frostfall's
// pages anyway. The check needs a display; CI runs it under xvfb-run with
// FF_REQUIRE_HEADED=1, which turns a missing display into a failure.
func TestAttachedPagesKeepFocusInAHeadedBrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		if os.Getenv("FF_REQUIRE_HEADED") != "" {
			t.Fatal("FF_REQUIRE_HEADED is set but there is no display (DISPLAY or WAYLAND_DISPLAY)")
		}
		t.Skip("needs a display for a headed Chrome")
	}
	app := newAuthApp(t)
	c := chrometest.Start(t, true)
	conn := c.Connect(t)
	// No device emulation on the test's own windows, so they match
	// Frostfall's attached pages in everything but focus emulation.
	conn.NoDefaultDevice()
	b := connectTo(t, c, c.HTTP())
	for i := range 3 {
		s, err := b.NewSession(app.URL+"/probe", cfgViewport(), "load", 0, 8*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// The negative control: a window of the test's own, with no focus
		// emulation, opened beside Frostfall's.
		ctl, err := conn.Incognito()
		if err != nil {
			t.Fatal(err)
		}
		ctlPage, err := ctl.Page(proto.TargetCreateTarget{URL: app.URL + "/probe"})
		if err != nil {
			t.Fatal(err)
		}
		// The user opens a window of their own after Frostfall's, and it takes
		// focus. Only one document has focus at a time, so once this window's
		// page reports it, Frostfall's window does not have focus. A window
		// manager may refuse a window focus now and then: up to three tries.
		var other *rod.Browser
		for try := 0; try < 3 && other == nil; try++ {
			ctx, err := conn.Incognito()
			if err != nil {
				t.Fatal(err)
			}
			if windowTakesFocus(t, ctx, app.URL+"/probe") {
				other = ctx
			} else {
				_ = ctx.Close()
			}
		}
		cantProve := func(why string) {
			_ = ctl.Close()
			s.Close()
			if os.Getenv("FF_REQUIRE_HEADED") != "" {
				t.Fatal("FF_REQUIRE_HEADED is set but " + why + ", so the guard was not checked")
			}
			t.Skip(why + ", so the guard cannot be checked here")
		}
		if other == nil {
			cantProve("a new window never took focus on this display")
		}
		// With another window focused, a page without emulation must come to
		// read false (Chrome updates it a moment after the switch); if it
		// never does, this display proves nothing either way.
		ctlUnfocused := false
		for deadline := time.Now().Add(3 * time.Second); !ctlUnfocused && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			r, err := ctlPage.Eval(`() => document.hasFocus()`)
			ctlUnfocused = err == nil && !r.Value.Bool()
		}
		if !ctlUnfocused {
			_ = other.Close()
			cantProve("a page without focus emulation still reads document.hasFocus() true while another window has focus")
		}
		raw, err := s.Eval(`() => document.hasFocus()`)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != "true" {
			t.Errorf("session %d: document.hasFocus() = %s on an attached page while another window has focus", i, raw)
		}
		_ = other.Close() // the test's own contexts only
		_ = ctl.Close()
		s.Close()
	}
}

// windowTakesFocus opens url in a new window of ctx and reports whether its
// page gets focus within a few seconds.
func windowTakesFocus(t *testing.T, ctx *rod.Browser, url string) bool {
	t.Helper()
	p, err := ctx.Page(proto.TargetCreateTarget{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if r, err := p.Eval(`() => document.hasFocus()`); err == nil && r.Value.Bool() {
			return true
		}
	}
	return false
}

// swApp is an app whose origin has a service worker that, unless bypassed,
// answers every navigation with a shell whose script pings /beacon, and pings
// /sw-beacon itself with the user's cookies.
type swApp struct {
	*httptest.Server
	beacons, withCookie atomic.Int32
}

func newSWApp(t *testing.T) *swApp {
	t.Helper()
	app := &swApp{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, `
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));
self.addEventListener('fetch', (e) => {
  if (e.request.mode !== 'navigate') return;
  fetch('/sw-beacon', { credentials: 'include' });
  e.respondWith(new Response('<!DOCTYPE html><title>shell</title><script>fetch("/beacon")</script>', { headers: { 'Content-Type': 'text/html' } }));
});`)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><title>register</title><script>
localStorage.setItem('token', 'tok-123');
navigator.serviceWorker.register('/sw.js').then(() => navigator.serviceWorker.ready).then(() => { document.title = 'ready' });
</script>`)
	})
	beacon := func(w http.ResponseWriter, r *http.Request) { app.beacons.Add(1) }
	mux.HandleFunc("/beacon", beacon)
	mux.HandleFunc("/sw-beacon", beacon)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("sid"); err == nil && ck.Value == "valid" {
			app.withCookie.Add(1)
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>app</title></head><body>page</body></html>`)
	})
	app.Server = httptest.NewServer(mux)
	t.Cleanup(app.Close)
	return app
}

// registerWorker registers the app's service worker in the browser's own
// context, the way a user's earlier visit would, and checks that it answers
// navigations.
func registerWorker(t *testing.T, c *chrometest.Chrome, app *swApp) {
	t.Helper()
	conn := c.Connect(t)
	defer conn.Close()
	p, err := conn.Page(proto.TargetCreateTarget{URL: app.URL + "/register"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if info, err := p.Info(); err == nil && info.Title == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the service worker did not become ready")
		}
	}
	if err := p.Navigate(app.URL + "/"); err != nil {
		t.Fatal(err)
	}
	_ = p.WaitLoad()
	if info, _ := p.Info(); info == nil || info.Title != "shell" {
		t.Fatalf("the service worker does not answer navigations (title %v), so the test proves nothing", info)
	}
}

// A service worker the app registered in the user's profile cannot answer
// the capture tab's navigation: it is bypassed, so the app's shell never runs
// and nothing leaves with the user's cookies, and localStorage is still read.
func TestCaptureBypassesServiceWorkers(t *testing.T) {
	app := newSWApp(t)
	c := startChrome(t)
	c.SetCookies(t, chrometest.Cookie("sid", "valid", "127.0.0.1"))
	registerWorker(t, c, app)
	app.beacons.Store(0)
	app.withCookie.Store(0)

	b := connectTo(t, c, c.HTTP())
	cfg := loadAuthConfig(t, app.URL, "  attach: true\n")
	a, err := NewAuth(b, cfg, app.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // a beacon in flight lands by now
	if n := app.beacons.Load(); n != 0 {
		t.Errorf("%d beacon(s): the service worker answered the capture tab", n)
	}
	if n := app.withCookie.Load(); n != 0 {
		t.Errorf("%d request(s) with the user's cookie reached the app", n)
	}
	if len(a.state.origins) != 1 || a.state.origins[0].local[0] != [2]string{"token", "tok-123"} {
		t.Errorf("localStorage not read: %v", a.state.origins)
	}
}

// A request still paused when the capture gives up (here: the handler is
// stopped before the navigation, and the navigation times out) is aborted
// with the tab, never released to the network.
func TestPausedCaptureRequestNeverReachesTheApp(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "<!DOCTYPE html><title>app</title>")
	}))
	defer srv.Close()
	c := startChrome(t)
	before := pages(t, c)
	b := connectTo(t, c, c.HTTP())
	page, closeTab, err := b.openCaptureTab()
	if err != nil {
		t.Fatal(err)
	}
	stop, err := interceptCaptureTab(page, []string{srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	stop() // nobody answers paused requests any more
	a := &Auth{defaults: config.Defaults{Timeout: config.Duration(500 * time.Millisecond)}}
	if _, err := a.readOrigin(page, srv.URL, srv.URL+"/"); err == nil {
		t.Fatal("the navigation finished with nobody answering it")
	}
	closeTab()
	time.Sleep(500 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d request(s) reached the app after the capture gave up", n)
	}
	if after := pages(t, c); !slices.Equal(before, after) {
		t.Errorf("tabs left: %v", after)
	}
}

// A server that accepts the WebSocket and then never answers fails the
// connection within the connect timeout instead of hanging the run.
func TestConnectToASilentBrowserTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				// Read the upgrade request, accept it, then say nothing.
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					conn.Close()
					return
				}
				h := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
					base64.StdEncoding.EncodeToString(h[:]))
				time.Sleep(30 * time.Second)
				conn.Close()
			}()
		}
	}()
	old := connectTimeout
	connectTimeout = time.Second
	defer func() { connectTimeout = old }()
	e, err := ParseBrowserURL("ws://" + ln.Addr().String() + "/devtools/browser/" + secretGUID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		b, err := Connect(e)
		if err == nil {
			b.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not answer over DevTools") {
			t.Errorf("err = %v, want the browser-did-not-answer error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect hung on a browser that never answers")
	}
}

// The capture's visit to an origin root does not rename a history row the
// user already has for it: the empty page has no title.
func TestCaptureKeepsAnExistingHistoryTitle(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not installed: the history check needs it")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>My Real App</title></head><body>page</body></html>`)
	}))
	defer srv.Close()
	c := startChrome(t)
	c.SetCookies(t, chrometest.Cookie("sid", "valid", "127.0.0.1"))
	tab := c.OpenTab(t, srv.URL+"/") // the user's own visit
	conn := c.Connect(t)
	if _, err := (proto.TargetCloseTarget{TargetID: tab}).Call(conn); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	b := connectTo(t, c, c.HTTP())
	cfg := loadAuthConfig(t, srv.URL, "  attach: true\n")
	if _, err := NewAuth(b, cfg, srv.URL, AuthOptions{}); err != nil {
		t.Fatal(err)
	}
	b.Close()
	time.Sleep(300 * time.Millisecond)
	c.Quit(t)
	out, err := exec.Command(sqlite, filepath.Join(c.Dir, "Default", "History"),
		"select title, hidden from urls where url = '"+srv.URL+"/'").CombinedOutput()
	if err != nil {
		t.Fatalf("reading History: %v: %s", err, out)
	}
	if row := strings.TrimSpace(string(out)); row != "My Real App|0" {
		t.Errorf("history row for the app root = %q, want the user's own title, still visible (My Real App|0)", row)
	}
}

// A first-party partitioned cookie (CHIPS: Partitioned; Secure;
// SameSite=None) is carried over: the cookies are asked for from a page on
// the origin, which is in that cookie's partition.
func TestCaptureCarriesFirstPartyPartitionedCookies(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/set" {
			w.Header().Add("Set-Cookie", "chip=1; Secure; SameSite=None; Partitioned; Path=/")
			w.Header().Add("Set-Cookie", "plain=1; Secure; SameSite=None; Path=/")
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body>page</body></html>`)
	}))
	defer srv.Close()
	c := chrometest.StartWith(t, false, "--ignore-certificate-errors")
	c.OpenTab(t, srv.URL+"/set")
	b := connectTo(t, c, c.HTTP())
	cfg := loadAuthConfig(t, srv.URL, "  attach: true\n")
	a, err := NewAuth(b, cfg, srv.URL, AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ck := range a.state.cookies {
		names = append(names, ck.Name)
		if ck.Name == "chip" && ck.PartitionKey == nil {
			t.Error("the partitioned cookie lost its partition key")
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"chip", "plain"}) {
		t.Errorf("captured %v, want [chip plain]", names)
	}
}
