package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aquia-inc/frostfall/internal/config"
)

// runCaptured runs the CLI and returns its exit code and what it wrote to
// stderr, which is where every failure message goes.
func runCaptured(t *testing.T, args ...string) (int, string) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := run(args)
	os.Stderr = orig
	w.Close()
	return code, <-done
}

// The auth fixtures through the real entry point: a valid storage state
// reaches the protected page and finds its planted violation (exit 1), and an
// empty one is sent to the login page and must stop with exit 3 and say so,
// instead of scanning it. CI runs the same two configs against the built
// binary.
func TestAuthFixtureExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	// serve: in the fixture configs is relative to the repository root.
	t.Chdir("../..")
	cases := []struct {
		config    string
		want      int
		stderrHas string
	}{
		{"testdata/auth/frostfall.yml", exitViolations, ""},
		{"testdata/auth/frostfall-nosession.yml", exitEnvironment, "landed on the login page"},
	}
	for _, tc := range cases {
		t.Run(tc.config, func(t *testing.T) {
			code, stderr := runCaptured(t, "--config", tc.config, "--no-screenshots", "--baseline", "none")
			if code != tc.want {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tc.want, stderr)
			}
			if !strings.Contains(stderr, tc.stderrHas) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.stderrHas)
			}
		})
	}
}

// A trailing slash on --base-url must not put a double slash in every URL
// built from it.
func TestBaseURLTrailingSlashIsTrimmed(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>page</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	cfg := filepath.Join(t.TempDir(), "frostfall.yml")
	if err := os.WriteFile(cfg, []byte("version: 1\ndefaults:\n  waitFor: load\ntests:\n  - id: p\n    path: /p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if code, stderr := runCaptured(t, "--config", cfg, "--base-url", srv.URL+"/", "--no-screenshots", "--baseline", "none"); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if strings.HasPrefix(p, "//") {
			t.Errorf("request for %q: the trailing slash of the base URL was kept", p)
		}
	}
	if len(paths) == 0 || !strings.Contains(strings.Join(paths, " "), "/p") {
		t.Errorf("paths = %v, want /p requested", paths)
	}
}

// With auth, a test whose url: is on another origin would be sent the
// session's cookies and escape the guard: a config error (exit 2) before a
// browser starts, unless it opts out with auth: false.
func TestCrossOriginURLTestWithAuthIsAConfigError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"cookies":[],"origins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgYAML := func(testAuth string) string {
		return "version: 1\nauth:\n  storageState: state.json\n  signedIn: \"#in\"\ntests:\n  - id: local\n    url: http://localhost:" + port + "/\n" + testAuth
	}
	cfg := filepath.Join(dir, "frostfall.yml")
	if err := os.WriteFile(cfg, []byte(cfgYAML("")), 0o644); err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1 and localhost are different origins, and different cookie hosts.
	code, stderr := runCaptured(t, "--config", cfg, "--base-url", srv.URL, "--no-screenshots", "--baseline", "none")
	if code != exitBadConfig {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitBadConfig, stderr)
	}
	if !strings.Contains(stderr, `test "local"`) || !strings.Contains(stderr, "auth: false") {
		t.Errorf("stderr = %q, want the test named and auth: false suggested", stderr)
	}
	if err := os.WriteFile(cfg, []byte(cfgYAML("    auth: false\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if testing.Short() {
		return
	}
	if code, stderr := runCaptured(t, "--config", cfg, "--base-url", srv.URL, "--no-screenshots", "--baseline", "none"); code == exitBadConfig {
		t.Errorf("auth: false did not opt the test out: %s", stderr)
	}
}

func TestDiscoverAuthWarning(t *testing.T) {
	load := func(auth, discover string) *config.Config {
		cfg := filepath.Join(t.TempDir(), "frostfall.yml")
		body := "version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\n" + auth + discover
		if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(cfg, "")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	setup := "auth:\n  signedIn: \"#in\"\n  setup:\n    path: /login\n    steps:\n      - click: a\n"
	if w := discoverAuthWarning(load(setup+"  reuse: false\n", "discover:\n  maxPages: 7\n")); !strings.Contains(w, "once per page it loads") || !strings.Contains(w, "(7) loads") || !strings.Contains(w, "each discovered test logs in again") {
		t.Errorf("reuse false: warning = %q", w)
	}
	if w := discoverAuthWarning(load(setup+"  reuse: false\n", "")); !strings.Contains(w, "(100)") {
		t.Errorf("default maxPages: warning = %q", w)
	}
	if w := discoverAuthWarning(load(setup, "")); w != "" {
		t.Errorf("reuse true warned: %q", w)
	}
	if w := discoverAuthWarning(load("", "")); w != "" {
		t.Errorf("no auth warned: %q", w)
	}
}

// authSite serves a marker (#in) on every page, so the signed-out probe
// refuses it whenever it runs.
func authSite(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main><i id="in"></i>page</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeAuthConfig(t *testing.T, base, tests string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"cookies":[],"origins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "version: 1\nserver:\n  baseUrl: " + base + "\ndefaults:\n  waitFor: load\n  timeout: 8s\nauth:\n  storageState: state.json\n  signedIn: \"#in\"\ntests:\n" + tests
	cfg := filepath.Join(dir, "frostfall.yml")
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// When every selected test says auth: false and there is no discovery,
// nothing signs in: no probe, no login flow. The probe here would refuse the
// marker (it is on every page), so reaching exit 0 shows it never ran.
func TestAuthIsSkippedWhenNoTestUsesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := authSite(t)
	t.Chdir(t.TempDir())
	off := writeAuthConfig(t, srv.URL, "  - id: pub\n    path: /\n    auth: false\n")
	if code, stderr := runCaptured(t, "--config", off, "--no-screenshots", "--baseline", "none"); code != exitOK {
		t.Errorf("all tests auth: false: exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	on := writeAuthConfig(t, srv.URL, "  - id: pub\n    path: /\n")
	if code, stderr := runCaptured(t, "--config", on, "--no-screenshots", "--baseline", "none"); code != exitEnvironment {
		t.Errorf("a test uses auth: exit code = %d, want %d (stderr: %s)", code, exitEnvironment, stderr)
	}
}

func TestValidateChecksTestOriginsAndFilters(t *testing.T) {
	cfg := writeAuthConfig(t, "http://127.0.0.1:9", "  - id: ok\n    path: /\n  - id: other\n    url: http://localhost:9/x\n")
	// --validate with a known base URL runs the check.
	code, stderr := runCaptured(t, "--config", cfg, "--validate")
	if code != exitBadConfig || !strings.Contains(stderr, `test "other"`) {
		t.Errorf("--validate: exit code = %d, stderr = %q, want a config error naming the test", code, stderr)
	}
	// A filtered run is only judged on the tests it selects.
	if code, stderr := runCaptured(t, "--config", cfg, "--validate", "--id", "^ok$"); code != exitOK {
		t.Errorf("--validate --id ok: exit code = %d, stderr = %q", code, stderr)
	}
}

// A filtered run is judged only on the tests it selects: the cross-origin
// test is not run, so it is not refused.
func TestScanChecksOnlySelectedTestsForOrigins(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := authSite(t)
	t.Chdir(t.TempDir())
	cfg := writeAuthConfig(t, srv.URL, "  - id: ok\n    path: /\n    auth: false\n  - id: other\n    url: http://localhost:9/x\n")
	if code, stderr := runCaptured(t, "--config", cfg, "--no-screenshots", "--baseline", "none"); code != exitBadConfig {
		t.Fatalf("unfiltered: exit code = %d, want %d (stderr: %s)", code, exitBadConfig, stderr)
	}
	if code, stderr := runCaptured(t, "--config", cfg, "--id", "^ok$", "--no-screenshots", "--baseline", "none"); code != exitOK {
		t.Errorf("filtered to the test that is fine: exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
}
