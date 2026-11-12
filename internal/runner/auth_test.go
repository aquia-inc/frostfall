package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/engine/axe"
)

// runWithAuth executes one test against a site whose /app needs a session
// cookie, with the session cookie present or not.
func runWithAuth(t *testing.T, withSession bool) error {
	t.Helper()
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app" {
			if c, err := r.Cookie("sid"); err != nil || c.Value != "valid" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>page</main></body></html>`)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	cookies := "[]"
	if withSession {
		cookies = fmt.Sprintf(`[{"name":"sid","value":"valid","domain":%q,"path":"/","expires":-1}]`, u.Hostname())
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"cookies":`+cookies+`,"origins":[]}`), 0o600)
	yml := "version: 1\nserver:\n  baseUrl: " + srv.URL + "\ndefaults:\n  waitFor: load\n  settleTime: 10ms\ntests:\n  - id: dashboard\n    path: /app\nauth:\n  storageState: state.json\n  loginPath: /login\n"
	os.WriteFile(filepath.Join(dir, "frostfall.yml"), []byte(yml), 0o644)
	cfg, err := config.Load(filepath.Join(dir, "frostfall.yml"), "")
	if err != nil {
		t.Fatal(err)
	}

	b, err := browser.Launch("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	a, err := browser.NewAuth(b, cfg, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Browser: b, Engine: axe.New(), Config: cfg, BaseURL: srv.URL, Auth: a}
	_, err = r.Execute(context.Background(), cfg.Tests)
	return err
}

func TestAuthenticatedRunScansThePage(t *testing.T) {
	if err := runWithAuth(t, true); err != nil {
		t.Fatalf("authenticated run failed: %v", err)
	}
}

// The run must stop with an environment error rather than scan the page the
// redirect led to.
func TestRunFailsWhenTestLandsOnLoginPage(t *testing.T) {
	err := runWithAuth(t, false)
	if !errors.Is(err, browser.ErrLoginRedirect) {
		t.Fatalf("err = %v, want the login redirect error", err)
	}
	want := `test "dashboard": landed on the login page /login: the auth session was not applied or has expired`
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
}
