package discover_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/discover"
)

// protectedSite sends everything but /login to /login without the session
// cookie. Its pages link to each other only once signed in.
func protectedSite(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := func(links ...string) {
			marker := `<i id="in"></i>`
			if r.URL.Path == "/login" {
				marker = ""
			}
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body>`+marker)
			for _, l := range links {
				fmt.Fprintf(w, `<a href=%q>%s</a>`, l, l)
			}
			fmt.Fprint(w, `</body></html>`)
		}
		if r.URL.Path == "/login" {
			page("/login-help")
			return
		}
		if c, err := r.Cookie("sid"); err != nil || c.Value != "valid" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		switch r.URL.Path {
		case "/":
			page("/reports", "/settings")
		default:
			page()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func authFor(t *testing.T, b *browser.Browser, base string, withSession bool, allowed ...string) *browser.Auth {
	t.Helper()
	u, _ := url.Parse(base)
	cookies := "[]"
	if withSession {
		cookies = fmt.Sprintf(`[{"name":"sid","value":"valid","domain":%q,"path":"/","expires":-1}]`, u.Hostname())
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"cookies":`+cookies+`,"origins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	yml := "version: 1\nserver:\n  baseUrl: " + base + "\ntests:\n  - id: t\n    path: /\nauth:\n  storageState: state.json\n  loginPath: /login\n  signedIn: \"#in\"\n"
	for _, o := range allowed {
		yml += "  allowedOrigins: [\"" + o + "\"]\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "frostfall.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "frostfall.yml"), "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := browser.NewAuth(b, cfg, base, browser.AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func crawler(b *browser.Browser, a *browser.Auth, base string) *discover.Crawler {
	return &discover.Crawler{
		Browser: b, Auth: a, BaseURL: base,
		Options:  config.Discover{MaxDepth: 2, MaxPages: 10},
		Defaults: config.Defaults{Viewport: config.Viewport{Width: 800, Height: 600}, WaitFor: "load"},
	}
}

func TestCrawlUsesAuthenticatedState(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := protectedSite(t)
	b, err := browser.Launch("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	got, err := crawler(b, authFor(t, b, srv.URL, true), srv.URL).Run()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/", "/reports", "/settings"} {
		if !slices.Contains(got, want) {
			t.Errorf("crawl missed %s: %v (it crawled the login page instead of the app)", want, got)
		}
	}
	if slices.Contains(got, "/login-help") {
		t.Errorf("crawl followed the login page's links: %v", got)
	}
}

// A crawl whose session was not applied would otherwise cover only the login
// page and report success.
func TestCrawlFailsWhenRootRedirectsToLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := protectedSite(t)
	b, err := browser.Launch("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	_, err = crawler(b, authFor(t, b, srv.URL, false), srv.URL).Run()
	if !errors.Is(err, browser.ErrLoginRedirect) {
		t.Errorf("err = %v, want the login redirect error", err)
	}
}

// richSite links, from its root, to pages a signed-in crawl must not turn
// into tests: logout links in several spellings, the login page, and a page
// that sends this user to login anyway (a role it does not have).
func richSite(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>l</title></head><body>login</body></html>`)
			return
		}
		if c, err := r.Cookie("sid"); err != nil || c.Value != "valid" || r.URL.Path == "/secret" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><i id="in"></i>`)
		if r.URL.Path == "/" {
			for _, l := range []string{"/about", "/reports", "/logout", "/sign-out", "/Log_Off", "/signoff", "/login", "/secret"} {
				fmt.Fprintf(w, `<a href=%q>x</a>`, l)
			}
		}
		fmt.Fprint(w, `</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCrawlWhenSignedInSkipsLogoutLoginAndRedirectedPages(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := richSite(t)
	b, err := browser.Launch("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	c := crawler(b, authFor(t, b, srv.URL, true), srv.URL)
	c.Options.MaxDepth = 1 // every link is at the depth limit
	var logs []string
	c.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	got, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/", "/about", "/reports"}; !slices.Equal(got, want) {
		t.Errorf("found = %v, want %v (no logout links, no login page, no page that redirected to it)", got, want)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "dropping /secret") || !strings.Contains(joined, "explicit test with auth: false") {
		t.Errorf("the dropped page was not logged with what to do about it: %q", logs)
	}
}

// offOriginSite links to a page that sends the browser to another origin
// (an identity provider, say), which is never a page of the app.
func offOriginSite(t *testing.T, other string, rootOff bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("sid"); err != nil || c.Value != "valid" {
			http.Redirect(w, r, other+"/idp", http.StatusFound)
			return
		}
		switch {
		case r.URL.Path == "/" && rootOff:
			http.Redirect(w, r, other+"/idp", http.StatusFound)
		case r.URL.Path == "/off":
			http.Redirect(w, r, other+"/idp", http.StatusFound)
		case r.URL.Path == "/":
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><i id="in"></i><a href="/off">off</a><a href="/ok">ok</a></body></html>`)
		default:
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><i id="in"></i></body></html>`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// allowedOrigins is for a setup flow's hops: discovery landing on another
// origin is a redirect whether or not it is listed.
func TestCrawlOffOriginLandings(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>idp</title></head><body>idp</body></html>`)
	}))
	t.Cleanup(idp.Close)
	b, err := browser.Launch("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	srv := offOriginSite(t, idp.URL, false)
	c := crawler(b, authFor(t, b, srv.URL, true, idp.URL), srv.URL)
	got, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/", "/ok"}; !slices.Equal(got, want) {
		t.Errorf("found = %v, want %v: a page that lands off the app's origin is dropped", got, want)
	}

	srv = offOriginSite(t, idp.URL, true)
	if _, err := crawler(b, authFor(t, b, srv.URL, true, idp.URL), srv.URL).Run(); !errors.Is(err, browser.ErrLoginRedirect) {
		t.Errorf("err = %v, want a root that lands off-origin to fail the crawl", err)
	}
}

// loginSite has a real login (POST sets the cookie) and counts how many times
// it ran, so a crawl that logs in once per page shows.
func loginSite(t *testing.T, logins *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			if r.Method == http.MethodPost {
				mu.Lock()
				*logins++
				mu.Unlock()
				http.SetCookie(w, &http.Cookie{Name: "sid", Value: "valid", Path: "/", HttpOnly: true})
				w.WriteHeader(http.StatusNoContent)
				return
			}
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>l</title></head><body>
				<button id="go" onclick="fetch('/login', {method: 'POST'}).then(() => location.href = '/')">go</button></body></html>`)
			return
		}
		if c, err := r.Cookie("sid"); err != nil || c.Value != "valid" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><i id="in"></i>`)
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<a href="/a">a</a><a href="/b">b</a><a href="/c">c</a>`)
		}
		fmt.Fprint(w, `</body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupAuth(t *testing.T, b *browser.Browser, base string, reuse bool) *browser.Auth {
	t.Helper()
	dir := t.TempDir()
	yml := fmt.Sprintf("version: 1\nserver:\n  baseUrl: %s\ntests:\n  - id: t\n    path: /\nauth:\n  reuse: %v\n  signedIn: \"#in\"\n"+
		"  setup:\n    path: /login\n    steps:\n      - click: \"#go\"\n      - waitFor: \"#in\"\n", base, reuse)
	if err := os.WriteFile(filepath.Join(dir, "frostfall.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "frostfall.yml"), "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := browser.NewAuth(b, cfg, base, browser.AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// With reuse: false a crawl would log in once per page it loads. It logs in
// once and crawls with that session.
func TestCrawlLogsInOnceWithoutReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	var logins int
	var mu sync.Mutex
	srv := loginSite(t, &logins, &mu)
	b, err := browser.Launch("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	a := setupAuth(t, b, srv.URL, false)
	crawlAuth, err := a.ForCrawl(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := crawler(b, crawlAuth, srv.URL).Run()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/", "/a", "/b", "/c"}; !slices.Equal(got, want) {
		t.Errorf("found = %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if logins != 1 {
		t.Errorf("logins = %d, want 1 for the whole crawl", logins)
	}
}

// Signed out, a page at the depth limit is not loaded: its links would not be
// followed, so loading it would be a wasted visit per page.
func TestUnauthenticatedCrawlDoesNotLoadDepthLimitPages(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><a href="/a">a</a></body></html>`)
	}))
	t.Cleanup(srv.Close)
	b, err := browser.Launch("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	c := crawler(b, nil, srv.URL)
	c.Options.MaxDepth = 1
	got, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/", "/a"}; !slices.Equal(got, want) {
		t.Errorf("found = %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/"] == 0 || hits["/a"] != 0 {
		t.Errorf("hits = %v, want / loaded and the depth-limit page /a left alone", hits)
	}
}
