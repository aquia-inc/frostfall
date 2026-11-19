package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aquia-inc/frostfall/internal/browser/chrometest"
)

const attachConfig = "testdata/auth/frostfall-attach.yml"

// closedPort is a loopback port with nothing listening on it.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	return port
}

// --browser-url against a Chrome started the way the README says, with the
// auth fixture's session cookie planted in its own profile beside another
// site's: the run reaches the protected page and finds its planted violation
// (exit 1), and the browser, its tabs and its profile are as they were
// afterwards. Without the cookie the guard stops the run (exit 3).
func TestAttachToARunningChrome(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	// serve: in the fixture config is relative to the repository root.
	t.Chdir("../..")

	t.Run("signed in", func(t *testing.T) {
		c := chrometest.Start(t, false)
		c.SetCookies(t, chrometest.Cookie("ffsession", "ok", "127.0.0.1"),
			chrometest.Cookie("bank", "unrelated-session", "unrelated.example"))
		c.OpenTab(t, "data:text/html,<title>my own tab</title>")
		before := sortedPages(t, c)

		code, stderr := runCaptured(t, "--config", attachConfig, "--browser-url", c.HTTP(), "--no-screenshots", "--baseline", "none")
		if code != exitViolations {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitViolations, stderr)
		}
		// Without --verbose, one line says whose session was taken, with
		// counts and no values.
		if !strings.Contains(stderr, "attach: using the session for http://127.0.0.1:") ||
			!strings.Contains(stderr, "(1 cookies, 0 localStorage keys) from the browser at "+c.HTTP()) ||
			strings.Contains(stderr, "unrelated-session") {
			t.Errorf("stderr lacks the session line, or carries a value: %s", stderr)
		}
		if !c.VersionOK() {
			t.Fatal("the attached Chrome stopped answering /json/version after the run")
		}
		if after := sortedPages(t, c); !slices.Equal(before, after) {
			t.Errorf("the browser's tabs changed:\nbefore %v\nafter  %v", before, after)
		}
		if left := c.Contexts(t); len(left) != 0 {
			t.Errorf("contexts left behind: %v", left)
		}
	})

	t.Run("no session", func(t *testing.T) {
		c := chrometest.Start(t, false)
		c.SetCookies(t, chrometest.Cookie("bank", "unrelated-session", "unrelated.example"))
		code, stderr := runCaptured(t, "--config", attachConfig, "--browser-url", c.HTTP(), "--no-screenshots", "--baseline", "none")
		if code != exitEnvironment {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitEnvironment, stderr)
		}
		for _, want := range []string{"landed on the login page", "holds no cookies or localStorage"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr lacks %q: %s", want, stderr)
			}
		}
		if !c.VersionOK() {
			t.Error("the attached Chrome stopped answering after a failed run")
		}
	})

	// The ws:// form carries the browser's GUID, which is a control key: it is
	// never printed, in --verbose logs or in a report, on success or failure.
	t.Run("ws url stays secret", func(t *testing.T) {
		c := chrometest.Start(t, false)
		c.SetCookies(t, chrometest.Cookie("ffsession", "ok", "127.0.0.1"))
		guid := strings.TrimPrefix(c.WSPath, "/devtools/browser/")
		out := t.TempDir()
		for _, f := range []string{"json", "junit"} {
			report := filepath.Join(out, "report."+f)
			code, stdout, stderr := runBoth(t, "--config", attachConfig, "--browser-url", c.WS(), "--verbose",
				"--format", f, "--output", report, "--no-screenshots", "--baseline", "none")
			if code != exitViolations {
				t.Fatalf("%s: exit code = %d, want %d (stderr: %s)", f, code, exitViolations, stderr)
			}
			if !strings.Contains(stderr, "attached to ") || !strings.Contains(stderr, "ws://127.0.0.1:"+c.Port) {
				t.Errorf("%s: --verbose does not say where it attached: %s", f, stderr)
			}
			raw, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			for where, text := range map[string]string{"stdout": stdout, "stderr": stderr, "report": string(raw)} {
				if strings.Contains(text, guid) || strings.Contains(text, "/devtools/browser") {
					t.Errorf("%s: %s carries the browser path", f, where)
				}
			}
		}
	})
}

// An endpoint nobody answers on is exit 3, with the hint about starting
// Chrome and the policy, and a ws:// path stays out of the message and the
// failure report.
func TestAttachUnreachableIsExit3(t *testing.T) {
	t.Chdir("../..")
	port := closedPort(t)
	const guid = "0f0e0d0c-SECRET-GUID"
	report := filepath.Join(t.TempDir(), "frostfall.json")
	for _, u := range []string{"http://127.0.0.1:" + port, "ws://127.0.0.1:" + port + "/devtools/browser/" + guid} {
		code, stderr := runCaptured(t, "--config", attachConfig, "--browser-url", u, "--format", "json", "--output", report, "--baseline", "none")
		if code != exitEnvironment {
			t.Fatalf("%s: exit code = %d, want %d (stderr: %s)", u, code, exitEnvironment, stderr)
		}
		for _, want := range []string{"--remote-debugging-port", "--user-data-dir", "RemoteDebuggingAllowed", "127.0.0.1:" + port} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%s: stderr lacks %q: %s", u, want, stderr)
			}
		}
		raw, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stderr, guid) || strings.Contains(string(raw), guid) {
			t.Errorf("%s: the browser path was printed", u)
		}
	}
}

// writeAttachProfiles writes a config whose base auth attaches and whose ci
// profile switches to the auth fixture's storage-state file.
func writeAttachProfiles(t *testing.T) string {
	t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	body := "version: 1\nserver:\n  serve: " + filepath.Join(repo, "testdata/auth/site") + "\n" +
		"auth:\n  attach: true\n  loginPath: /login.html\n  signedIn: \"[data-testid=app-shell]\"\n" +
		"defaults:\n  waitFor: load\n  expect:\n    severity: serious\n    maxViolations: 0\n" +
		"tests:\n  - id: protected\n    path: /protected.html\n" +
		"profiles:\n  ci:\n    auth:\n      storageState: " + filepath.Join(repo, "testdata/auth/state-valid.json") + "\n" +
		"      loginPath: /login.html\n      signedIn: \"[data-testid=app-shell]\"\n"
	path := filepath.Join(t.TempDir(), "frostfall.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --browser-url and auth.attach go together, strictly, in the auth block in
// effect after the profile; --validate checks the config without the flag.
// None of these launches a browser.
func TestAttachPairing(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	at := func(p string) string { return filepath.Join(repo, p) }
	profiles := writeAttachProfiles(t)
	port := closedPort(t)
	local := "http://127.0.0.1:" + port
	cases := []struct {
		name string
		args []string
		want int
		has  string
	}{
		{"flag with browser-path", []string{"--config", at(attachConfig), "--browser-url", local, "--browser-path", "/usr/bin/chrome"}, exitBadConfig, "pass one or the other"},
		{"remote host", []string{"--config", at(attachConfig), "--browser-url", "http://10.1.2.3:9222"}, exitBadConfig, "only a browser on this machine"},
		{"flag with storageState", []string{"--config", at("testdata/auth/frostfall.yml"), "--browser-url", local}, exitBadConfig, "does not set attach: true"},
		{"flag without auth", []string{"--config", at("testdata/static/frostfall.yml"), "--browser-url", local}, exitBadConfig, "has no auth block"},
		{"flag with storageState under validate", []string{"--config", at("testdata/auth/frostfall.yml"), "--browser-url", local, "--validate"}, exitBadConfig, "does not set attach: true"},
		{"attach without flag", []string{"--config", at(attachConfig)}, exitBadConfig, "pass --browser-url"},
		{"attach without flag in a dry run", []string{"--config", at(attachConfig), "--discover", "--dry-run"}, exitBadConfig, "pass --browser-url"},
		{"attach validates without flag", []string{"--config", at(attachConfig), "--validate"}, exitOK, ""},
		{"attach validates with flag", []string{"--config", at(attachConfig), "--validate", "--browser-url", local}, exitOK, ""},
		{"base attach validates", []string{"--config", profiles, "--validate"}, exitOK, ""},
		{"ci profile validates", []string{"--config", profiles, "--validate", "--profile", "ci"}, exitOK, ""},
		{"flag with the ci profile's storageState", []string{"--config", profiles, "--profile", "ci", "--browser-url", local}, exitBadConfig, `profile "ci" applied`},
		{"base attach without flag", []string{"--config", profiles, "--profile", "none"}, exitBadConfig, "pass --browser-url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stderr := runCaptured(t, append(tc.args, "--no-screenshots", "--baseline", "none")...)
			if code != tc.want {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tc.want, stderr)
			}
			if !strings.Contains(stderr, tc.has) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.has)
			}
		})
	}
}

// The local and CI split end to end: the same config scans with the ci
// profile's storage state and no browser URL, which the pairing must allow.
func TestAttachConfigWithCIProfileScansInCI(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	t.Setenv("GITHUB_ACTIONS", "")
	profiles := writeAttachProfiles(t)
	t.Chdir(t.TempDir())
	code, stderr := runCaptured(t, "--config", profiles, "--profile", "ci", "--no-screenshots", "--baseline", "none")
	if code != exitViolations {
		t.Errorf("exit code = %d, want %d (stderr: %s)", code, exitViolations, stderr)
	}
}

func sortedPages(t *testing.T, c *chrometest.Chrome) []chrometest.Target {
	t.Helper()
	ps := c.Pages(t)
	slices.SortFunc(ps, func(a, b chrometest.Target) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return ps
}

// TestHelperRunMain is not a test: run as a subprocess with
// FROSTFALL_RUN_MAIN=1, it is the frostfall command, taking the arguments
// after "--". The signal tests need a real process to signal.
func TestHelperRunMain(t *testing.T) {
	if os.Getenv("FROSTFALL_RUN_MAIN") != "1" {
		t.Skip("subprocess helper")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Exit(run(args))
}

// slowAttachConfig serves a protected page that takes delay to answer and
// needs the ffsession cookie (the auth fixture's check), and writes an attach
// config for it. The delay keeps a run busy long enough for a signal to land
// mid-run.
func slowAttachConfig(t *testing.T, delay time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/protected.html":
			if _, err := r.Cookie("ffsession"); err == nil {
				time.Sleep(delay) // signed in: the test's own load, not the probe's
			}
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>p</title><script>
if (!document.cookie.split('; ').includes('ffsession=ok')) location.replace('/login.html');
</script></head><body><main data-testid="app-shell"><h1>Protected</h1><img src="x.png"></main></body></html>`)
		default:
			fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main><h1>Sign in</h1></main></body></html>`)
		}
	}))
	t.Cleanup(srv.Close)
	body := "version: 1\nserver:\n  baseUrl: " + srv.URL + "\nauth:\n  attach: true\n  loginPath: /login.html\n" +
		"  signedIn: \"[data-testid=app-shell]\"\ndefaults:\n  waitFor: load\n  timeout: 8s\n" +
		"tests:\n  - id: protected\n    path: /protected.html\n"
	path := filepath.Join(t.TempDir(), "frostfall.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// An interrupt in attach mode, by any of the signals a terminal or a
// supervisor sends, closes what Frostfall opened in the user's Chrome and
// exits as that signal is reported (128 plus its number), without printing an
// error of its own or replacing the report. The signal is sent once the
// session line is on stderr, while the slow protected page is loading.
func TestAttachInterruptCleansUp(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals")
	}
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGINT, 130}, {syscall.SIGHUP, 129}, {syscall.SIGQUIT, 131}, {syscall.SIGTERM, 143}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			cfg := slowAttachConfig(t, 3*time.Second)
			c := chrometest.Start(t, false)
			c.SetCookies(t, chrometest.Cookie("ffsession", "ok", "127.0.0.1"))
			before := sortedPages(t, c)
			report := filepath.Join(t.TempDir(), "frostfall.json")
			if err := os.WriteFile(report, []byte("stale"), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRunMain$", "--",
				"--config", cfg, "--browser-url", c.HTTP(), "--no-screenshots", "--baseline", "none",
				"--format", "json", "--output", report)
			cmd.Env = append(os.Environ(), "FROSTFALL_RUN_MAIN=1", "GITHUB_ACTIONS=")
			pipe, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Every stderr line is kept; the session line says the run is past
			// the capture and into the tests.
			var lines []string
			ready := make(chan struct{})
			eof := make(chan struct{})
			go func() {
				defer close(eof)
				sc := bufio.NewScanner(pipe)
				signalled := false
				for sc.Scan() {
					lines = append(lines, sc.Text())
					if !signalled && strings.HasPrefix(sc.Text(), "attach: using the session for ") {
						signalled = true
						close(ready)
					}
				}
			}()
			select {
			case <-ready:
			case <-eof:
			case <-time.After(30 * time.Second):
			}
			select {
			case <-ready:
				time.Sleep(200 * time.Millisecond) // into the slow page load
				if err := cmd.Process.Signal(tc.sig); err != nil {
					t.Fatal(err)
				}
			default:
				_ = cmd.Process.Kill()
			}
			<-eof
			err = cmd.Wait()
			out := strings.Join(lines, "\n")
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.code {
				t.Fatalf("exit = %v, want code %d (stderr: %s)", err, tc.code, out)
			}
			if !strings.Contains(out, "interrupted: closing what Frostfall opened") {
				t.Errorf("stderr lacks the interrupt line: %s", out)
			}
			for _, bad := range []string{"run error", "auth error", "discover error", "context canceled", "report written"} {
				if strings.Contains(out, bad) {
					t.Errorf("stderr has %q after an interrupt: %s", bad, out)
				}
			}
			if raw, _ := os.ReadFile(report); string(raw) != "stale" {
				t.Errorf("the report was rewritten after an interrupt: %.80s", raw)
			}
			if !c.VersionOK() {
				t.Fatal("the attached Chrome stopped answering")
			}
			if after := sortedPages(t, c); !slices.Equal(before, after) {
				t.Errorf("tabs left after the interrupt:\nbefore %v\nafter  %v", before, after)
			}
			if left := c.Contexts(t); len(left) != 0 {
				t.Errorf("contexts left after the interrupt: %v", left)
			}
		})
	}
}

// go-rod reads -rod options from anywhere in os.Args before main runs, and
// its cdp option logs every cookie of an attached profile: no such argument
// is accepted, in any position or form.
func TestRodArgumentsAreRefused(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// Somewhere empty: were the refusal missing, "init" would write a config.
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"-rod=cdp"},
		{"--rod=cdp"},
		{"-rod", "cdp"},
		{"--rod"},
		{"--screenshot-dir", "-rod=cdp", "--validate"},
		{"--id", "-rod=show"},
		{"init", "-rod=cdp"},
		{"--version", "--rod=trace"},
	} {
		code, stderr := runCaptured(t, args...)
		if code != exitBadConfig || !strings.Contains(stderr, "starts with -rod") {
			t.Errorf("%q: exit %d, stderr %q; want exit 2 and the refusal", args, code, stderr)
		}
	}
	// A value that only contains it, past the start, is an ordinary value.
	if code, stderr := runCaptured(t, "--config", filepath.Join(repo, "testdata/static/frostfall.yml"), "--validate", "--id", "x-rod=1"); code != exitOK {
		t.Errorf("--id x-rod=1: exit %d (stderr %s)", code, stderr)
	}
}

func TestBrowserURLFlagEdges(t *testing.T) {
	cfg := "../../testdata/auth/frostfall-attach.yml"
	if code, stderr := runCaptured(t, "--config", cfg, "--browser-url="); code != exitBadConfig || !strings.Contains(stderr, "--browser-url: empty") {
		t.Errorf("--browser-url=: exit %d, stderr %q", code, stderr)
	}
	if code, stderr := runCaptured(t, "--config", cfg, "--browser-url", ""); code != exitBadConfig || !strings.Contains(stderr, "--browser-url: empty") {
		t.Errorf("--browser-url '': exit %d, stderr %q", code, stderr)
	}
	// --version answers whatever the other flags hold.
	if code, _, stderr := runBoth(t, "--version", "--browser-url", "http://10.0.0.1:9222"); code != exitOK {
		t.Errorf("--version with a bad --browser-url: exit %d (stderr %s)", code, stderr)
	}
}

// Once an interrupt has been handled, the run's own failure path stands
// still for the handler's exit: no message, no failure report. The handler
// exits a moment after closing the browser, so in a real interrupt this is a
// race the run usually loses anyway; here it is forced.
func TestNoReportAfterAnInterrupt(t *testing.T) {
	report := filepath.Join(t.TempDir(), "frostfall.json")
	if err := os.WriteFile(report, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	interrupted.Store(true)
	defer interrupted.Store(false)
	returned := make(chan int, 1)
	go func() {
		// A config that does not exist: the run aborts at once. The goroutine
		// stays parked once halted; it is a test, the process ends soon.
		returned <- run([]string{"--config", filepath.Join(t.TempDir(), "missing.yml"), "--format", "json", "--output", report})
	}()
	select {
	case code := <-returned:
		t.Errorf("the run returned %d after an interrupt instead of waiting for the handler's exit", code)
	case <-time.After(500 * time.Millisecond):
	}
	if raw, _ := os.ReadFile(report); string(raw) != "stale" {
		t.Errorf("a failure report was written after an interrupt: %.80s", raw)
	}
}

type closeCounter struct{ n atomic.Int32 }

func (c *closeCounter) Close() { c.n.Add(1) }

// A signal that arrives as the run stops listening (the tests have finished,
// the browser was just released) is honoured, never dropped: the run exits as
// interrupted instead of going on to write its report. Repeated, because the
// handler's select picks either ready case at random.
func TestStoppingTheGuardHonoursAPendingSignal(t *testing.T) {
	defer interrupted.Store(false)
	for i := range 200 {
		interrupted.Store(false)
		sigs := make(chan os.Signal, 1)
		var b closeCounter
		var exited atomic.Int32
		stop := guardSignals(&b, sigs, func() {}, func(code int) { exited.Store(int32(code)) })
		sigs <- syscall.SIGHUP // arrives just before the run stops listening
		stop()
		if got := exited.Load(); got != 129 {
			t.Fatalf("run %d: exit = %d, want 129 for a SIGHUP pending at stop", i, got)
		}
		if !interrupted.Load() || b.n.Load() != 1 {
			t.Fatalf("run %d: interrupted = %v, closes = %d", i, interrupted.Load(), b.n.Load())
		}
	}
	// With nothing pending, stopping exits nothing and closes nothing.
	interrupted.Store(false)
	sigs := make(chan os.Signal, 1)
	var b closeCounter
	var exited atomic.Int32
	stop := guardSignals(&b, sigs, func() {}, func(code int) { exited.Store(int32(code)) })
	stop()
	stop()
	if exited.Load() != 0 || b.n.Load() != 0 || interrupted.Load() {
		t.Errorf("a stop with no signal exited %d, closed %d times", exited.Load(), b.n.Load())
	}
}

// blockingCloser is a browser whose Close does not return until released.
type blockingCloser struct{ release chan struct{} }

func (c *blockingCloser) Close() { <-c.release }

// A second interrupt while the browser is being closed exits at once, with
// the first signal's code, instead of waiting for the close to finish.
func TestSecondInterruptExitsAtOnce(t *testing.T) {
	defer interrupted.Store(false)
	sigs := make(chan os.Signal, 1)
	b := &blockingCloser{release: make(chan struct{})}
	defer close(b.release)
	exited := make(chan int, 1)
	guardSignals(b, sigs, func() {}, func(code int) { exited <- code })
	sigs <- syscall.SIGINT
	select {
	case code := <-exited:
		t.Fatalf("exited %d before Close finished or a second signal came", code)
	case <-time.After(200 * time.Millisecond):
	}
	sigs <- syscall.SIGTERM
	select {
	case code := <-exited:
		if code != 130 {
			t.Errorf("exit = %d, want 130 (the first signal's)", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second interrupt did not exit while Close was still running")
	}
}

// The capture failing on the app's own page is exit 3 through the CLI, with
// the reason. A 1ms budget makes that visit fail; the probe only warns.
func TestAttachCaptureFailureIsExit3(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body>page</body></html>`)
	}))
	defer srv.Close()
	cfg := filepath.Join(t.TempDir(), "frostfall.yml")
	body := "version: 1\nserver:\n  baseUrl: " + srv.URL + "\ndefaults:\n  timeout: 1ms\nauth:\n  attach: true\n  signedIn: \"#in\"\ntests:\n  - id: t\n    path: /\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c := chrometest.Start(t, false)
	before := sortedPages(t, c)
	code, stderr := runCaptured(t, "--config", cfg, "--browser-url", c.HTTP(), "--no-screenshots", "--baseline", "none")
	if code != exitEnvironment || !strings.Contains(stderr, "could not be read from the attached browser") {
		t.Errorf("exit %d, stderr %s; want exit 3 and the capture failure", code, stderr)
	}
	if after := sortedPages(t, c); !slices.Equal(before, after) {
		t.Errorf("tabs left: %v", after)
	}
}
