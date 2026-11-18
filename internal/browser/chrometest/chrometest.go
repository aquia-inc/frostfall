// Package chrometest starts a Chrome the way a user does to attach Frostfall
// (--remote-debugging-port and a separate --user-data-dir) and inspects it
// over its own DevTools connection, for tests of --browser-url.
package chrometest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Chrome is a running browser started for a test. Its process and profile go
// away when the test ends.
type Chrome struct {
	// Port is the DevTools port Chrome picked, from DevToolsActivePort.
	Port string
	// WSPath is the browser's WebSocket path, /devtools/browser/<guid>: the
	// secret that must never be printed.
	WSPath string
	// Dir is the profile directory (--user-data-dir).
	Dir string

	cmd *exec.Cmd
}

// HTTP is the DevTools address, http://127.0.0.1:port.
func (c *Chrome) HTTP() string { return "http://127.0.0.1:" + c.Port }

// WS is the browser's WebSocket URL.
func (c *Chrome) WS() string { return "ws://127.0.0.1:" + c.Port + c.WSPath }

// Start launches Chrome with --remote-debugging-port=0 on a fresh profile
// directory and waits for it to write DevToolsActivePort. headed opens a
// real window (on the display in DISPLAY); otherwise it runs headless.
func Start(t testing.TB, headed bool) *Chrome {
	t.Helper()
	return StartWith(t, headed)
}

// StartWith is Start with extra Chrome command-line flags, such as
// --ignore-certificate-errors for a self-signed https fixture.
func StartWith(t testing.TB, headed bool, extra ...string) *Chrome {
	t.Helper()
	// FROSTFALL_TEST_CHROME picks a specific binary, such as a Chrome for
	// Testing build of the version a CI runner has.
	bin, ok := os.Getenv("FROSTFALL_TEST_CHROME"), true
	if bin == "" {
		bin, ok = launcher.LookPath()
	}
	if !ok {
		t.Fatal("no Chrome or Chromium found")
	}
	// Not t.TempDir: Chrome's helper processes can still be writing the profile
	// for a moment after the browser exits, and a failed removal there fails
	// the test. This directory is removed best effort.
	dir, err := os.MkdirTemp("", "frostfall-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--remote-debugging-port=0", "--user-data-dir=" + dir,
		"--no-first-run", "--no-default-browser-check", "--disable-dev-shm-usage",
	}
	if !headed {
		args = append(args, "--headless")
	}
	args = append(args, extra...)
	args = append(args, "about:blank")
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		for range 20 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
		if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); err == nil && len(lines) == 2 {
			c := &Chrome{Port: strings.TrimSpace(lines[0]), WSPath: strings.TrimSpace(lines[1]), Dir: dir, cmd: cmd}
			if c.VersionOK() {
				// Which browser a failure ran against, for CI logs (shown with -v
				// or on failure).
				t.Logf("chrometest: %s (%s)", c.product(), bin)
				return c
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Chrome did not write DevToolsActivePort")
	return nil
}

// Quit closes the browser the way a user does (Browser.close), so it writes
// its profile, the History database included, before exiting.
func (c *Chrome) Quit(t testing.TB) {
	t.Helper()
	conn := c.Connect(t)
	_ = proto.BrowserClose{}.Call(conn)
	done := make(chan struct{})
	go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Chrome did not exit after Browser.close")
	}
}

// product is the Browser field of /json/version, such as Chrome/157.0.8103.50.
func (c *Chrome) product() string {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(c.HTTP() + "/json/version")
	if err != nil {
		return "unknown browser"
	}
	defer resp.Body.Close()
	var v struct {
		Browser string `json:"Browser"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil || v.Browser == "" {
		return "unknown browser"
	}
	return v.Browser
}

// VersionOK reports whether /json/version still answers, which is how a test
// tells that the browser is still running.
func (c *Chrome) VersionOK() bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(c.HTTP() + "/json/version")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&v) == nil && v.WS != ""
}

// UserAgent is the browser's own User-Agent, from /json/version.
func (c *Chrome) UserAgent(t testing.TB) string {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(c.HTTP() + "/json/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		UA string `json:"User-Agent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.UA == "" {
		t.Fatalf("reading the user-agent: %v", err)
	}
	return v.UA
}

// Conn is a DevTools connection of the test's own, which it closes without
// ever sending Browser.close.
type Conn struct {
	*rod.Browser
	ws *cdp.WebSocket
}

// Close disconnects.
func (c *Conn) Close() { _ = c.ws.Close() }

// Connect opens a test connection; it is closed when the test ends.
func (c *Chrome) Connect(t testing.TB) *Conn {
	t.Helper()
	ws := &cdp.WebSocket{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ws.Connect(ctx, c.WS(), nil); err != nil {
		t.Fatalf("connecting to the test Chrome: %v", err)
	}
	b := rod.New().ControlURL("").Client(cdp.New().Start(ws))
	if err := b.Connect(); err != nil {
		_ = ws.Close()
		t.Fatalf("connecting to the test Chrome: %v", err)
	}
	conn := &Conn{Browser: b, ws: ws}
	t.Cleanup(conn.Close)
	return conn
}

// SetCookies plants cookies in the browser's own (default) context, where a
// user's sign-in leaves them.
func (c *Chrome) SetCookies(t testing.TB, cookies ...*proto.NetworkCookieParam) {
	t.Helper()
	conn := c.Connect(t)
	defer conn.Close()
	if err := (proto.StorageSetCookies{Cookies: cookies}).Call(conn); err != nil {
		t.Fatalf("planting cookies: %v", err)
	}
}

// OpenTab opens a tab in the default context at url, as a user's own tab,
// and returns its target id. It stays open.
func (c *Chrome) OpenTab(t testing.TB, url string) proto.TargetTargetID {
	t.Helper()
	conn := c.Connect(t)
	defer conn.Close()
	p, err := conn.Page(proto.TargetCreateTarget{URL: url})
	if err != nil {
		t.Fatalf("opening a tab: %v", err)
	}
	if err := p.WaitLoad(); err != nil {
		t.Fatalf("loading %s: %v", url, err)
	}
	return p.TargetID
}

// SetLocalStorage writes key=value into the localStorage of url's origin, in
// a tab of the default context that it closes afterwards.
func (c *Chrome) SetLocalStorage(t testing.TB, url, key, value string) {
	t.Helper()
	conn := c.Connect(t)
	defer conn.Close()
	p, err := conn.Page(proto.TargetCreateTarget{URL: url})
	if err != nil {
		t.Fatalf("opening %s: %v", url, err)
	}
	defer p.Close()
	if err := p.WaitLoad(); err != nil {
		t.Fatalf("loading %s: %v", url, err)
	}
	if _, err := p.Eval(`(k, v) => localStorage.setItem(k, v)`, key, value); err != nil {
		t.Fatalf("setting localStorage: %v", err)
	}
}

// Target is one page target: its id and URL.
type Target struct {
	ID  proto.TargetTargetID
	URL string
}

// Pages lists the browser's page targets.
func (c *Chrome) Pages(t testing.TB) []Target {
	t.Helper()
	conn := c.Connect(t)
	defer conn.Close()
	res, err := proto.TargetGetTargets{}.Call(conn)
	if err != nil {
		t.Fatalf("listing targets: %v", err)
	}
	var out []Target
	for _, ti := range res.TargetInfos {
		if ti.Type == proto.TargetTargetInfoTypePage {
			out = append(out, Target{ID: ti.TargetID, URL: ti.URL})
		}
	}
	return out
}

// Contexts lists the browser contexts created over DevTools (not the
// default one) that still exist.
func (c *Chrome) Contexts(t testing.TB) []proto.BrowserBrowserContextID {
	t.Helper()
	conn := c.Connect(t)
	defer conn.Close()
	res, err := proto.TargetGetBrowserContexts{}.Call(conn)
	if err != nil {
		t.Fatalf("listing contexts: %v", err)
	}
	return res.BrowserContextIDs
}

// Cookie is a test cookie for host, path /, not secure.
func Cookie(name, value, domain string) *proto.NetworkCookieParam {
	return &proto.NetworkCookieParam{Name: name, Value: value, Domain: domain, Path: "/", SameSite: proto.NetworkCookieSameSiteLax}
}

// String is for failure messages.
func (tg Target) String() string { return fmt.Sprintf("%s %s", tg.ID, tg.URL) }
