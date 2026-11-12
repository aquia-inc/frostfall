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
	"strings"
	"testing"

	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/engine/axe"
)

// runWithAuth executes one test against a site whose /app needs a session
// cookie, with the session cookie present or not.
func runWithAuth(t *testing.T, withSession bool) error {
	t.Helper()
	return runTests(t, withSession, "  - id: dashboard\n    path: /app\n", nil)
}

// runTests is runWithAuth with the tests: block and the verbose log under the
// caller's control.
func runTests(t *testing.T, withSession bool, testsYAML string, log func(string, ...any)) error {
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
	yml := "version: 1\nserver:\n  baseUrl: " + srv.URL + "\ndefaults:\n  waitFor: load\n  settleTime: 10ms\ntests:\n" + testsYAML + "auth:\n  storageState: state.json\n  loginPath: /login\n"
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
	a, err := browser.NewAuth(b, cfg, srv.URL, browser.AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Browser: b, Engine: axe.New(), Config: cfg, BaseURL: srv.URL, Auth: a, Verbose: log != nil, Log: log}
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

// The first load passed its check; a step can still walk into the login page.
// Nothing may be scanned, audited or transcribed from it.
func TestStepsThatReportRecheckTheLanding(t *testing.T) {
	for _, step := range []string{
		"scan: {}",
		"keyboard:",
		"transcript:",
		"expectFocus: { role: button }",
	} {
		t.Run(step, func(t *testing.T) {
			err := runTests(t, true, "  - id: walk\n    path: /app\n    scan: false\n    steps:\n      - goto: /login\n      - "+step+"\n", nil)
			if !errors.Is(err, browser.ErrLoginRedirect) {
				t.Fatalf("err = %v, want the login redirect", err)
			}
			if want := `test "walk": steps[1]: landed on the login page /login`; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("err = %q, want it to start %q (the step index)", err, want)
			}
		})
	}
	// Driving the page into the login page is not itself an error: a later
	// step may navigate back before anything reports.
	err := runTests(t, true, "  - id: back\n    path: /app\n    scan: false\n    steps:\n      - goto: /login\n      - goto: /app\n      - scan: {}\n", nil)
	if err != nil {
		t.Errorf("a detour through the login page that came back failed: %v", err)
	}
}

func TestVerboseLogOmitsQueryStrings(t *testing.T) {
	var lines []string
	err := runTests(t, true, "  - id: q\n    path: /app?token=SECRETTOK123\n", func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatal("nothing logged")
	}
	for _, l := range lines {
		if strings.Contains(l, "SECRETTOK123") {
			t.Errorf("a query token reached the log: %q", l)
		}
	}
}
