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
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body>`)
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

func authFor(t *testing.T, b *browser.Browser, base string, withSession bool) *browser.Auth {
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
	yml := "version: 1\nserver:\n  baseUrl: " + base + "\ntests:\n  - id: t\n    path: /\nauth:\n  storageState: state.json\n  loginPath: /login\n"
	if err := os.WriteFile(filepath.Join(dir, "frostfall.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(dir, "frostfall.yml"), "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := browser.NewAuth(b, cfg, base, nil)
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
