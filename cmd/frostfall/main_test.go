package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// runBoth is runCaptured that also returns stdout.
func runBoth(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(ro); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(re); errC <- string(b) }()
	code = run(args)
	os.Stdout, os.Stderr = origOut, origErr
	wo.Close()
	we.Close()
	return code, <-outC, <-errC
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
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main><i id="in"></i>page<img src="x.png"></main></body></html>`)
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

// Every output of a run whose config has auth, even one where nothing signs in,
// is free of what the paths and the base URL carry: stdout, stderr (verbose),
// the SARIF file and the HTML report.
func TestSkippedAuthStillRedactsEveryOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := authSite(t)
	t.Chdir(t.TempDir())
	cfg := writeAuthConfig(t, srv.URL, "  - id: pub\n    path: /p?token=SECRETTOK123\n    auth: false\n")
	base := strings.Replace(srv.URL, "http://", "http://ciuser:S3cretBasicPW@", 1)
	secrets := []string{"SECRETTOK123", "S3cretBasicPW"}
	for _, format := range []string{"sarif", "html"} {
		out := filepath.Join(t.TempDir(), "report."+format)
		code, stdout, stderr := runBoth(t, "--config", cfg, "--base-url", base, "--verbose", "--format", format, "--output", out, "--no-screenshots", "--baseline", "none")
		if code != exitOK {
			t.Fatalf("%s: exit code = %d (stderr: %s)", format, code, stderr)
		}
		report, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		for name, text := range map[string]string{"stdout": stdout, "stderr": stderr, format + " report": string(report)} {
			for _, secret := range secrets {
				if strings.Contains(text, secret) {
					t.Errorf("%s: %q reached the %s", format, secret, name)
				}
			}
		}
		if len(report) == 0 || !strings.Contains(string(report), "/p") {
			t.Errorf("%s report is empty or lost the page", format)
		}
	}
}

// Without auth the base URL in the report is what it always was.
func TestBaseURLInTheReportIsUnchangedWithoutAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := authSite(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "frostfall.yml")
	if err := os.WriteFile(cfg, []byte("version: 1\ndefaults:\n  waitFor: load\ntests:\n  - id: p\n    path: /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	base := strings.Replace(srv.URL, "http://", "http://ciuser:S3cretBasicPW@", 1)
	out := filepath.Join(dir, "r.html")
	if code, _, stderr := runBoth(t, "--config", cfg, "--base-url", base, "--format", "html", "--output", out, "--no-screenshots", "--baseline", "none"); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	report, _ := os.ReadFile(out)
	if !strings.Contains(string(report), "S3cretBasicPW") {
		t.Error("without auth the report's Target changed: it must be byte-identical to before")
	}
}

// The "cannot reach" line names the base URL; with auth it must not carry the
// userinfo in it, and without auth it is as it always was.
func TestCannotReachMessageRedactsOnlyWithAuth(t *testing.T) {
	dir := t.TempDir()
	withAuth := writeAuthConfig(t, "http://127.0.0.1:1", "  - id: a\n    path: /\n")
	plain := filepath.Join(dir, "frostfall.yml")
	if err := os.WriteFile(plain, []byte("version: 1\ntests:\n  - id: a\n    path: /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := "http://ciuser:S3cretBasicPW@127.0.0.1:1"
	_, stderr := runCaptured(t, "--config", withAuth, "--base-url", base, "--no-screenshots")
	if !strings.Contains(stderr, "cannot reach") || strings.Contains(stderr, "S3cretBasicPW") {
		t.Errorf("with auth: stderr = %q", stderr)
	}
	_, stderr = runCaptured(t, "--config", plain, "--base-url", base, "--no-screenshots")
	if !strings.Contains(stderr, "cannot reach "+base) {
		t.Errorf("without auth: stderr = %q, want the line unchanged", stderr)
	}
}

// --discover needs the session even when every explicit test says auth: false:
// the crawl itself is signed in. The marker is on every page here, so the probe
// that discovery brings back refuses it.
func TestDiscoverSetsUpAuthEvenWhenNoExplicitTestUsesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := authSite(t)
	t.Chdir(t.TempDir())
	cfg := writeAuthConfig(t, srv.URL, "  - id: pub\n    path: /\n    auth: false\n")
	if code, stderr := runCaptured(t, "--config", cfg, "--discover", "--no-screenshots", "--baseline", "none"); code != exitEnvironment {
		t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitEnvironment, stderr)
	}
}

// The first test that uses auth is probed, whether it is written with a path or
// a url:. Here the root has no marker and the test's page does.
func TestFirstTestWithAURLIsProbed(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		marker := ""
		if r.URL.Path == "/p" {
			marker = `<i id="in"></i>`
		}
		fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>%spage</main></body></html>`, marker)
	}))
	t.Cleanup(srv.Close)
	t.Chdir(t.TempDir())
	cfg := writeAuthConfig(t, srv.URL, "  - id: a\n    url: "+srv.URL+"/p\n")
	code, stderr := runCaptured(t, "--config", cfg, "--no-screenshots", "--baseline", "none")
	if code != exitEnvironment || !strings.Contains(stderr, "the first test that uses auth") {
		t.Errorf("exit code = %d, stderr = %q, want the probe to refuse the marker on the test's page", code, stderr)
	}
}

func TestValidateAppliesFiltersLikeARealRun(t *testing.T) {
	cfg := writeAuthConfig(t, "http://127.0.0.1:9", "  - id: ok\n    path: /\n  - id: other\n    url: http://localhost:9/x\n")
	// --path selects by path: the url: test has none, so it is not checked.
	if code, stderr := runCaptured(t, "--config", cfg, "--validate", "--path", "^/$"); code != exitOK {
		t.Errorf("--validate --path: exit code = %d, stderr = %q", code, stderr)
	}
	// A bad filter regex fails --validate the way it fails a run, with or without auth.
	plain := filepath.Join(t.TempDir(), "frostfall.yml")
	if err := os.WriteFile(plain, []byte("version: 1\nserver:\n  baseUrl: http://x\ntests:\n  - id: a\n    path: /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--id", "--path"} {
		code, stderr := runCaptured(t, "--config", plain, "--validate", flag, "(")
		if code != exitBadConfig || !strings.Contains(stderr, flag+":") {
			t.Errorf("%s ( : exit code = %d, stderr = %q, want the same error a run gives", flag, code, stderr)
		}
	}
}

// Through the real entry point, a --base-url that ends in a slash keeps it for a
// ? path (what plain concatenation produced) and never doubles it for a / path.
func TestTrailingSlashBaseURLEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	var mu sync.Mutex
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.URL.RequestURI())
		mu.Unlock()
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>page</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "frostfall.yml")
	if err := os.WriteFile(cfg, []byte("version: 1\ndefaults:\n  waitFor: load\ntests:\n  - id: q\n    path: \"?a=1\"\n  - id: x\n    path: /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if code, _, stderr := runBoth(t, "--config", cfg, "--base-url", srv.URL+"/sub/", "--no-screenshots", "--baseline", "none"); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	// Under a path prefix the slash is visible on the wire: /sub/?a=1 is not /sub?a=1.
	if !slices.Contains(uris, "/sub/?a=1") || !slices.Contains(uris, "/sub/x") || slices.Contains(uris, "/sub?a=1") {
		t.Errorf("requests = %q, want /sub/?a=1 and /sub/x", uris)
	}
}

// --dry-run belongs to discovery and says so when it is used alone or with a
// flag that would write something. All of these are config errors (exit 2)
// found before a config is read or a browser starts.
func TestDryRunNeedsDiscoverAndNothingThatWrites(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--dry-run"}, "--discover --dry-run"},
		{[]string{"--discover", "--dry-run", "--update-baseline"}, "--update-baseline"},
		{[]string{"--discover", "--dry-run", "--gh-issues"}, "--gh-issues"},
		{[]string{"--discover", "--dry-run", "--gh-issues-dry-run"}, "--gh-issues"},
		// Flags a path listing cannot use are refused, not ignored. --format
		// has a default, so passing the default value counts as passing it.
		{[]string{"--discover", "--dry-run", "--validate"}, "--validate"},
		{[]string{"--discover", "--dry-run", "--format", "html"}, "--format"},
		{[]string{"--discover", "--dry-run", "--format", "text"}, "--format"},
		{[]string{"--discover", "--dry-run", "--output", "report.html"}, "--output"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runBoth(t, tc.args...)
		if code != exitBadConfig || !strings.Contains(stderr, tc.want) || stdout != "" {
			t.Errorf("%v: exit code = %d, stdout = %q, stderr = %q, want exit %d and %q", tc.args, code, stdout, stderr, exitBadConfig, tc.want)
		}
	}
}

// A dry run prints the discovered paths, sorted, one per line, and nothing
// else on stdout. It runs no scan (the page here has a violation that would
// make a real run exit 1) and reads no baseline (the file named is not JSON).
// The crawl follows the config's excludes, and the summary is on stderr.
func TestDiscoverDryRunListsPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		links := map[string][]string{
			"/":      {"/zeta", "/alpha", "/admin/users", "/alpha/"},
			"/alpha": {"/#/routes/one", "/zeta"},
		}[r.URL.Path]
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main><img src="x.png">`)
		for _, l := range links {
			fmt.Fprintf(w, `<a href=%q>%s</a>`, l, l)
		}
		fmt.Fprint(w, `</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "frostfall.yml")
	yml := "version: 1\ndefaults:\n  waitFor: load\ndiscover:\n  maxDepth: 2\n  maxPages: 20\n  exclude: [\"^/admin\"]\ntests:\n  - id: home\n    path: /\n"
	if err := os.WriteFile(cfg, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(broken, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	args := []string{"--config", cfg, "--base-url", srv.URL, "--discover", "--dry-run", "--baseline", broken, "--no-screenshots"}
	code, stdout, stderr := runBoth(t, args...)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if want := "/\n/#/routes/one\n/alpha\n/zeta\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "discovered 4 page(s), 3 not covered by an explicit test") {
		t.Errorf("stderr = %q, want the summary", stderr)
	}
	// The same crawl again lists the same thing in the same order.
	if _, again, _ := runBoth(t, args...); again != stdout {
		t.Errorf("second run listed %q, first listed %q", again, stdout)
	}
	// The exit codes are a real run's: an unreachable server is an environment
	// error, and nothing is listed.
	srv.Close()
	if code, stdout, _ := runBoth(t, args...); code != exitEnvironment || stdout != "" {
		t.Errorf("unreachable server: exit code = %d, stdout = %q, want %d and nothing", code, stdout, exitEnvironment)
	}
}

// A bad config is exit 2 under --dry-run as it is anywhere else.
func TestDiscoverDryRunBadConfigIsExit2(t *testing.T) {
	code, stdout, stderr := runBoth(t, "--config", filepath.Join(t.TempDir(), "missing.yml"), "--discover", "--dry-run")
	if code != exitBadConfig || stdout != "" || !strings.Contains(stderr, "config error") {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q, want %d", code, stdout, stderr, exitBadConfig)
	}
}

// Signed in, the listing is what discovery's own output is: credential-looking
// links are left out, a query or ;param never reaches it, and the session's
// cookie value is nowhere in it. The crawl is signed in, so it sees the pages
// behind the marker.
func TestDiscoverDryRunWithAuthPrintsNoCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	const secret = "s3cr3tvalue"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		marker := ""
		if c, err := r.Cookie("sid"); err == nil && c.Value == secret {
			marker = `<i id="in"></i>`
		}
		links := map[string][]string{
			"/": {"/reports?token=" + secret, "/files;jsessionid=" + secret + "/x", "/cb#/home#access_token=" + secret, "/reset/0123456789abcdef0123456789abcdef", "/plain"},
		}[r.URL.Path]
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>`+marker)
		for _, l := range links {
			fmt.Fprintf(w, `<a href=%q>x</a>`, l)
		}
		fmt.Fprint(w, `</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	state := fmt.Sprintf(`{"cookies":[{"name":"sid","value":%q,"domain":"127.0.0.1","path":"/","expires":-1}],"origins":[]}`, secret)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	yml := "version: 1\nserver:\n  baseUrl: " + srv.URL + "\ndefaults:\n  waitFor: load\n  timeout: 8s\nauth:\n  storageState: state.json\n  signedIn: \"#in\"\ntests:\n  - id: home\n    path: /\n"
	cfg := filepath.Join(dir, "frostfall.yml")
	if err := os.WriteFile(cfg, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	code, stdout, stderr := runBoth(t, "--config", cfg, "--discover", "--dry-run", "--no-screenshots", "--baseline", "none")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if strings.Contains(stdout+stderr, secret) {
		t.Errorf("the session value was printed: stdout = %q, stderr = %q", stdout, stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.ContainsAny(line, "?;=") || strings.Contains(line, "0123456789abcdef") {
			t.Errorf("listed %q, which carries what the crawl removes", line)
		}
	}
	if !strings.Contains(stdout, "/plain\n") || !strings.Contains(stdout, "/reports\n") {
		t.Errorf("stdout = %q, want /plain and the cleaned /reports", stdout)
	}
}

// The listing is what the crawl found, deduped, and the uncovered count is of
// those lines against every test in the config: never more than were listed,
// and not narrowed by anything else. A test with an absolute url: has no path
// and covers nothing.
func TestDryRunListingCountsAgainstAllTestsAfterDedupe(t *testing.T) {
	tests := []config.Test{
		{ID: "home", Path: "/"},
		{ID: "a", Path: "/a"},
		{ID: "abs", URL: "http://example.test/c"},
	}
	lines, uncovered := dryRunListing([]string{"/", "/a", "/b", "/b", "/c"}, false, tests)
	if want := []string{"/", "/a", "/b", "/c"}; !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	if uncovered != 2 {
		t.Errorf("uncovered = %d, want 2 (/b and /c; the url: test covers nothing)", uncovered)
	}
	// Duplicates that are not covered are counted once, so the count cannot
	// exceed the pages listed.
	lines, uncovered = dryRunListing([]string{"/x", "/x", "/x"}, false, nil)
	if len(lines) != 1 || uncovered != 1 {
		t.Errorf("lines = %q, uncovered = %d, want one line and 1", lines, uncovered)
	}
}

// With auth the paths are the crawler's decoded, cleaned ones, and printing
// them must not parse them as URLs again: a literal % and a leading // come
// out as they are, and nothing collapses into a placeholder line. A path that
// is a secret by shape is still masked, so a token cannot be printed.
func TestDryRunListingAuthKeepsDecodedPathsAndMasksTokens(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	lines, _ := dryRunListing([]string{"/100%", "//x", "/50%/off", "/reset/" + token}, true, nil)
	want := []string{"//x", "/100%", "/50%/off", "/reset/[redacted]"}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	for _, l := range lines {
		if strings.Contains(l, token) || strings.Contains(l, "<unparsable url>") {
			t.Errorf("listed %q", l)
		}
	}
}

// --id and --path narrow what a run scans, not what a dry run counts as
// covered: the same crawl reports the same uncovered count with or without
// them, and a test with an absolute url: still covers nothing.
func TestDiscoverDryRunCoverageIgnoresFilters(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>`)
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<a href="/a">a</a><a href="/b">b</a><a href="/c">c</a>`)
		}
		fmt.Fprint(w, `</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "frostfall.yml")
	yml := "version: 1\ndefaults:\n  waitFor: load\ntests:\n  - id: home\n    path: /\n  - id: a\n    path: /a\n  - id: b\n    path: /b\n  - id: abs\n    url: " + srv.URL + "/c\n"
	if err := os.WriteFile(cfg, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	base := []string{"--config", cfg, "--base-url", srv.URL, "--discover", "--dry-run", "--no-screenshots", "--baseline", "none"}
	for _, extra := range [][]string{nil, {"--id", "^home$"}, {"--path", "^/$"}} {
		code, stdout, stderr := runBoth(t, append(slices.Clone(base), extra...)...)
		if code != exitOK {
			t.Fatalf("%v: exit code = %d (stderr: %s)", extra, code, stderr)
		}
		if want := "/\n/a\n/b\n/c\n"; stdout != want {
			t.Errorf("%v: stdout = %q, want %q", extra, stdout, want)
		}
		if !strings.Contains(stderr, "discovered 4 page(s), 1 not covered by an explicit test") {
			t.Errorf("%v: stderr = %q, want 4 discovered and 1 not covered", extra, stderr)
		}
	}
}

// Signed in, links whose decoded path holds a literal % or starts with // are
// listed as they are (not as <unparsable url>, not with a slash lost), and a
// token in a query or a path never reaches stdout or stderr.
func TestDiscoverDryRunWithAuthKeepsOddPathsAndNoTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	const secret = "s3cr3tvalue"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		marker := ""
		if c, err := r.Cookie("sid"); err == nil && c.Value == secret {
			marker = `<i id="in"></i>`
		}
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>`+marker)
		if r.URL.Path == "/" {
			for _, l := range []string{"/100%25", "/%2Fx", "/reports?token=" + secret, "/reset/0123456789abcdef0123456789abcdef", "/plain"} {
				fmt.Fprintf(w, `<a href=%q>x</a>`, l)
			}
		}
		fmt.Fprint(w, `</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	state := fmt.Sprintf(`{"cookies":[{"name":"sid","value":%q,"domain":"127.0.0.1","path":"/","expires":-1}],"origins":[]}`, secret)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	yml := "version: 1\nserver:\n  baseUrl: " + srv.URL + "\ndefaults:\n  waitFor: load\n  timeout: 8s\nauth:\n  storageState: state.json\n  signedIn: \"#in\"\ntests:\n  - id: home\n    path: /\n"
	cfg := filepath.Join(dir, "frostfall.yml")
	if err := os.WriteFile(cfg, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	code, stdout, stderr := runBoth(t, "--config", cfg, "--discover", "--dry-run", "--no-screenshots", "--baseline", "none")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr)
	}
	if strings.Contains(stdout+stderr, secret) || strings.Contains(stdout, "0123456789abcdef") {
		t.Errorf("a token was printed: stdout = %q, stderr = %q", stdout, stderr)
	}
	if want := "/\n//x\n/100%\n/plain\n/reports\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}
