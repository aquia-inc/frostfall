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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/engine/axe"
	"github.com/aquia-inc/frostfall/internal/fingerprint"
)

// runWithAuth executes one test against a site whose /app needs a session
// cookie, with the session cookie present or not.
func runWithAuth(t *testing.T, withSession bool) error {
	t.Helper()
	return runTests(t, withSession, "  - id: dashboard\n    path: /app\n", nil)
}

// runOpts shape one run against the test site.
type runOpts struct {
	withSession bool
	noAuth      bool // a config with no auth block at all
	tests       string
	log         func(string, ...any)
	engine      engine.Engine
	// after runs when a check that reports on the page has just finished,
	// before its landing re-check (see Runner.afterOp).
	after func(s *browser.Session, op, base string)
	// seen collects each request's URI; baseSuffix is appended to the base URL
	// the runner gets; shots turns screenshots on.
	seen       *[]string
	baseSuffix string
	shots      string
}

func runTests(t *testing.T, withSession bool, testsYAML string, log func(string, ...any)) error {
	t.Helper()
	_, err := runSite(t, runOpts{withSession: withSession, tests: testsYAML, log: log})
	return err
}

// runSite runs tests against a site whose /app needs a session cookie and
// whose pages other than /login carry the signed-in marker (#in).
func runSite(t *testing.T, o runOpts) (*Run, error) {
	t.Helper()
	if testing.Short() {
		t.Skip("launches a browser")
	}
	var seenMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.seen != nil {
			seenMu.Lock()
			*o.seen = append(*o.seen, r.URL.RequestURI())
			seenMu.Unlock()
		}
		if r.URL.Path == "/linkpage" {
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main><i id="in"></i><a href="/dl?token=Q7xK2pLm9Zw">download</a></main></body></html>`)
			return
		}
		if r.URL.Path == "/app" {
			if c, err := r.Cookie("sid"); err != nil || c.Value != "valid" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
		}
		marker := `<i id="in"></i>`
		if r.URL.Path == "/login" || r.URL.Path == "/" {
			marker = "" // signed out, the root and the login page have no marker
		}
		fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>%spage</main></body></html>`, marker)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	cookies := "[]"
	if o.withSession {
		cookies = fmt.Sprintf(`[{"name":"sid","value":"valid","domain":%q,"path":"/","expires":-1}]`, u.Hostname())
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"cookies":`+cookies+`,"origins":[]}`), 0o600)
	yml := "version: 1\nserver:\n  baseUrl: " + srv.URL + "\ndefaults:\n  waitFor: load\n  settleTime: 10ms\n  timeout: 8s\ntests:\n" + o.tests
	if !o.noAuth {
		yml += "auth:\n  storageState: state.json\n  loginPath: /login\n  signedIn: \"#in\"\n"
	}
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
	var eng engine.Engine = axe.New()
	if o.engine != nil {
		eng = o.engine
	}
	r := &Runner{Browser: b, Engine: eng, Config: cfg, BaseURL: srv.URL + o.baseSuffix, Auth: a, ScreenshotDir: o.shots, Verbose: o.log != nil, Log: o.log}
	if o.after != nil {
		r.afterOp = func(s *browser.Session, op string) { o.after(s, op, srv.URL) }
	}
	return r.Execute(context.Background(), cfg.Tests)
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
	if !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "auth: false") {
		t.Errorf("err = %q, want it to start %q and point at auth: false", err, want)
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

// navigatingEngine reports one violation, but only after walking the page into
// the login page, the way a late client-side redirect during a scan would.
type navigatingEngine struct{}

func (navigatingEngine) Name() string    { return "navigating" }
func (navigatingEngine) Version() string { return "0" }
func (navigatingEngine) Audit(_ context.Context, page engine.Page, _ engine.ScanOptions) ([]engine.Violation, error) {
	if _, err := page.Eval(`() => { setTimeout(() => { location.href = '/login' }, 0) }`); err != nil {
		return nil, err
	}
	time.Sleep(1500 * time.Millisecond)
	return []engine.Violation{{RuleID: "image-alt", Impact: engine.Critical, Target: "img"}}, nil
}

// Results from a page that navigated to login while it was being checked are
// the login page's: the run must report the redirect, not them.
func TestScanThatEndsOnTheLoginPageIsRejected(t *testing.T) {
	for name, tests := range map[string]string{
		"initial scan": "  - id: late\n    path: /app\n",
		"step scan":    "  - id: late\n    path: /app\n    scan: false\n    steps:\n      - scan: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			run, err := runSite(t, runOpts{withSession: true, tests: tests, engine: navigatingEngine{}})
			if !errors.Is(err, browser.ErrLoginRedirect) || !strings.Contains(err.Error(), "after scan") {
				t.Fatalf("err = %v, want the redirect reported after the scan", err)
			}
			if run != nil {
				t.Errorf("results of the login page were returned: %+v", run.Results)
			}
		})
	}
}

// auth: false is a fresh, signed-out context with no guard: the page that
// redirects to login is just scanned. The session is withheld on purpose, so
// the same page would otherwise fail the guard.
func TestAuthFalseTestHasNoSessionAndNoGuard(t *testing.T) {
	run, err := runSite(t, runOpts{withSession: false, tests: "  - id: public\n    path: /app\n    auth: false\n"})
	if err != nil {
		t.Fatalf("a test with auth: false was guarded: %v", err)
	}
	if run.TestsRun != 1 {
		t.Errorf("tests run = %d", run.TestsRun)
	}
	// Default (auth on) is guarded, and is what discovered tests get.
	_, err = runSite(t, runOpts{withSession: false, tests: "  - id: signed-in\n    path: /app\n"})
	if !errors.Is(err, browser.ErrLoginRedirect) {
		t.Errorf("without a session the signed-in test must fail: %v", err)
	}
}

// A config without auth reports exactly what it always did: nothing redacted.
func TestVerboseLogIsUnredactedWithoutAuth(t *testing.T) {
	var lines []string
	_, err := runSite(t, runOpts{noAuth: true, tests: "  - id: q\n    path: /plain?keep=1\n",
		log: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "/plain?keep=1") {
		t.Errorf("log = %q, want the URL as printed before auth existed", lines)
	}
}

// toLogin moves the session's page to /login when op starts with prefix, the
// way a handler fired by the check itself might.
func toLogin(prefix string) func(*browser.Session, string, string) {
	return func(s *browser.Session, op, base string) {
		if strings.HasPrefix(op, prefix) {
			s.Step(config.Step{Goto: "/login"}, base, "load", 0)
		}
	}
}

// Each check that reports on the page is followed by a landing re-check: if
// the page ended up on the login page while it ran, the redirect is reported
// and none of its results are. The page is moved at exactly that point.
// (The re-checks before the initial transcript and keyboard audit run
// immediately after the check that follows navigation, so nothing can
// change the page between the two, and they have no test of their own.)
func TestEveryCheckIsFollowedByALandingRecheck(t *testing.T) {
	cases := []struct {
		name, tests, op string
	}{
		{"initial transcript", "  - id: t\n    path: /app\n    scan: false\n    transcript: true\n", "the initial transcript"},
		{"initial keyboard audit", "  - id: t\n    path: /app\n    scan: false\n    keyboard: true\n", "keyboard audit"},
		{"step transcript", "  - id: t\n    path: /app\n    scan: false\n    steps:\n      - transcript:\n", "the transcript"},
		{"step keyboard audit", "  - id: t\n    path: /app\n    scan: false\n    steps:\n      - keyboard:\n", "keyboard audit"},
		{"step expectFocus", "  - id: t\n    path: /app\n    scan: false\n    steps:\n      - expectFocus: { role: button }\n", "expectFocus"},
		{"initial scan", "  - id: t\n    path: /app\n", "scan"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, err := runSite(t, runOpts{withSession: true, tests: tc.tests, after: toLogin(tc.op)})
			if !errors.Is(err, browser.ErrLoginRedirect) || !strings.Contains(err.Error(), "after "+tc.op) {
				t.Fatalf("err = %v, want the redirect reported after %q", err, tc.op)
			}
			if run != nil {
				t.Errorf("results were returned from the login page: %+v", run.Results)
			}
		})
	}
}

// oneViolation reports a single finding on every scan.
type oneViolation struct{}

func (oneViolation) Name() string    { return "one" }
func (oneViolation) Version() string { return "0" }
func (oneViolation) Audit(context.Context, engine.Page, engine.ScanOptions) ([]engine.Violation, error) {
	return []engine.Violation{{RuleID: "image-alt", Impact: engine.Critical, Target: "img"}}, nil
}

// PageURL ends up in SARIF locations and the HTML report. With auth it is
// redacted like every other printed URL; without, it is what it always was.
func TestReportedPageURLsFollowTheAuthConfig(t *testing.T) {
	tests := "  - id: q\n    path: /app?token=SECRETTOK123#/route&access_token=ZZZ\n    transcript: true\n"
	run, err := runSite(t, runOpts{withSession: true, tests: tests, engine: oneViolation{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Results) == 0 || len(run.Transcripts) == 0 {
		t.Fatalf("results = %d, transcripts = %d", len(run.Results), len(run.Transcripts))
	}
	urls := []string{run.Results[0].PageURL, run.Transcripts[0].PageURL}
	for _, u := range urls {
		if strings.Contains(u, "SECRETTOK123") || strings.Contains(u, "ZZZ") || strings.Contains(u, "?") {
			t.Errorf("a secret reached a reported PageURL: %q", u)
		}
		if !strings.HasSuffix(u, "/app#/route") {
			t.Errorf("PageURL = %q, want the redacted page identity", u)
		}
	}

	plain, err := runSite(t, runOpts{noAuth: true, tests: "  - id: q\n    path: /plain?keep=1\n", engine: oneViolation{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(plain.Results[0].PageURL, "/plain?keep=1") {
		t.Errorf("without auth PageURL = %q, want it unchanged", plain.Results[0].PageURL)
	}
}

// targetEngine reports one violation with a fixed selector.
type targetEngine struct{ target string }

func (targetEngine) Name() string    { return "target" }
func (targetEngine) Version() string { return "0" }
func (e targetEngine) Audit(context.Context, engine.Page, engine.ScanOptions) ([]engine.Violation, error) {
	return []engine.Violation{{RuleID: "image-alt", Impact: engine.Critical, Target: e.target}}, nil
}

// axe anchors on a short href, so a download link's token lands in the selector
// and from there in the fingerprint, the baseline and every report. With auth
// the selector is scrubbed first; without it nothing changes, fingerprint
// included.
func TestSelectorTokensAreScrubbedWithAuthOnly(t *testing.T) {
	const raw = `main > a[href="/dl?token=Q7xK2pLm9Zw"]`
	tests := "  - id: dl\n    path: /linkpage\n"

	run, err := runSite(t, runOpts{withSession: true, tests: tests, engine: targetEngine{raw}, shots: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	res := run.Results[0]
	for _, field := range []string{res.Target, res.StableTarget} {
		if strings.Contains(field, "Q7xK2pLm9Zw") || strings.Contains(field, "token") {
			t.Errorf("a token reached %q", field)
		}
	}
	const scrubbed = `main > a[href="/dl"]`
	if want := fingerprint.Fingerprint("dl", "initial", "image-alt", scrubbed); res.Fingerprint != want {
		t.Errorf("fingerprint = %s, want the scrubbed selector's %s", res.Fingerprint, want)
	}
	// The element is still found by its real selector for the screenshot.
	if res.ScreenshotPath == "" {
		t.Error("no screenshot: the scrubbed selector was used to find the element")
	}

	plain, err := runSite(t, runOpts{noAuth: true, tests: tests, engine: targetEngine{raw}})
	if err != nil {
		t.Fatal(err)
	}
	p := plain.Results[0]
	if p.Target != raw {
		t.Errorf("without auth the target = %q, want it untouched", p.Target)
	}
	if want := fingerprint.Fingerprint("dl", "initial", "image-alt", raw); p.Fingerprint != want {
		t.Errorf("without auth the fingerprint = %s, want the unchanged scheme's %s", p.Fingerprint, want)
	}
}

// A path is joined to the base URL, not required to start with a slash: the
// forms that worked before still reach the same URLs, and a base's trailing
// slash does not double up.
func TestTestPathsAreJoinedToTheBase(t *testing.T) {
	var seen []string
	tests := "  - id: a\n    path: index.html\n  - id: b\n    path: \"?a=1\"\n  - id: c\n    path: \"#/route\"\n" +
		"  - id: d\n    path: \"@evil.example/\"\n  - id: e\n    path: /plain\n"
	if _, err := runSite(t, runOpts{noAuth: true, tests: tests, seen: &seen, baseSuffix: "/"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/index.html", "/?a=1", "/@evil.example/", "/plain"} {
		if !slices.Contains(seen, want) {
			t.Errorf("requests = %q, want %q among them", seen, want)
		}
	}
	for _, got := range seen {
		if strings.HasPrefix(got, "//") {
			t.Errorf("request %q: a slash was doubled", got)
		}
	}
}
