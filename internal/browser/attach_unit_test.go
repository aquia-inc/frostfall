package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

const secretGUID = "6b1e6d9e-7f43-4c55-9a8e-SECRETGUID"

func TestParseBrowserURLAcceptsLoopback(t *testing.T) {
	cases := map[string]string{
		"http://localhost:9222":                              "http://localhost:9222",
		"http://LOCALHOST:9222/":                             "http://localhost:9222",
		"http://127.0.0.1:9222":                              "http://127.0.0.1:9222",
		"http://127.1.2.3:9222/json/version":                 "http://127.1.2.3:9222",
		"http://[::1]:9222":                                  "http://[::1]:9222",
		"ws://127.0.0.1:9222/devtools/browser/" + secretGUID: "ws://127.0.0.1:9222",
		"ws://localhost:9333/devtools/browser/" + secretGUID: "ws://localhost:9333",
	}
	for raw, want := range cases {
		e, err := ParseBrowserURL(raw)
		if err != nil {
			t.Errorf("ParseBrowserURL(%q): %v", raw, err)
			continue
		}
		// Every way the endpoint can reach a log line shows the origin only.
		for _, got := range []string{e.String(), fmt.Sprintf("%v", e), fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e), fmt.Sprint(e)} {
			if got != want {
				t.Errorf("ParseBrowserURL(%q) prints %q, want %q", raw, got, want)
			}
		}
		if e.ws != strings.HasPrefix(raw, "ws://") {
			t.Errorf("ParseBrowserURL(%q).ws = %v", raw, e.ws)
		}
	}
}

func TestParseBrowserURLRefuses(t *testing.T) {
	cases := map[string]string{
		"localhost:9222":         "not a URL",
		"9222":                   "not a URL",
		"":                       "not a URL",
		"https://localhost:9222": "scheme must be http or ws",
		"wss://localhost:9222/devtools/browser/" + secretGUID: "scheme must be http or ws",
		"ftp://localhost:9222":                                "scheme must be http or ws",
		"http://192.168.1.10:9222":                            "only a browser on this machine",
		"http://example.com:9222":                             "only a browser on this machine",
		"http://0.0.0.0:9222":                                 "only a browser on this machine",
		"http://localhost.example.com:9222":                   "only a browser on this machine",
		"http://[::ffff:10.0.0.1]:9222":                       "only a browser on this machine",
		"ws://10.0.0.5:9222/devtools/browser/" + secretGUID:   "only a browser on this machine",
		"http://user:pw@localhost:9222":                       "remove the credentials",
		"http://localhost":                                    "give the port",
		"http://localhost:9222/?x=1":                          "remove the query",
		"http://localhost:9222/#f":                            "remove the query",
		"http://localhost:9222/json/list":                     "without a path",
		"ws://localhost:9222/devtools/page/" + secretGUID:     "must be the browser's own",
		"ws://localhost:9222/":                                "must be the browser's own",
	}
	for raw, want := range cases {
		_, err := ParseBrowserURL(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseBrowserURL(%q) = %v, want an error containing %q", raw, err, want)
			continue
		}
		for _, leak := range []string{secretGUID, "pw@", "x=1", "/json/list"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("ParseBrowserURL(%q) error repeats %q: %v", raw, leak, err)
			}
		}
	}
}

// The unreachable message names the two usual causes, and only the
// endpoint's origin: a ws:// path never reaches it.
func TestUnreachableMessage(t *testing.T) {
	e, err := ParseBrowserURL("ws://127.0.0.1:9/devtools/browser/" + secretGUID)
	if err != nil {
		t.Fatal(err)
	}
	msg := e.unreachable("connection refused").Error()
	for _, want := range []string{"ws://127.0.0.1:9", "--remote-debugging-port", "--user-data-dir", "Chrome 136", "RemoteDebuggingAllowed", "http://127.0.0.1:9/json/version"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, secretGUID) || strings.Contains(msg, "/devtools/") {
		t.Errorf("message carries the browser path: %s", msg)
	}
}

// reason never passes an error's own text through, since a url.Error carries
// the URL; only fixed phrases come out.
func TestReasonUsesFixedPhrases(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	leaky := fmt.Errorf("Get \"ws://127.0.0.1:9/devtools/browser/%s\": %w", secretGUID, errors.New("boom"))
	cases := map[error]string{
		refused:        "connection refused",
		leaky:          "the connection failed",
		errNotLoopback: "did not resolve to a loopback address",
	}
	for err, want := range cases {
		got := reason(err)
		if !strings.Contains(got, want) || strings.Contains(got, secretGUID) {
			t.Errorf("reason(%v) = %q, want %q", err, got, want)
		}
	}
}

func cookie(name, domain, path string, secure bool) *proto.NetworkCookie {
	return &proto.NetworkCookie{Name: name, Value: "v-" + name, Domain: domain, Path: path, Secure: secure, Session: true}
}

// The filter is what keeps the rest of the user's browser out of a scan: of
// every cookie the profile holds, only those the app's origin (or an allowed
// origin) would be sent are kept.
func TestAttachedCookiesKeepsOnlyTheAppsCookies(t *testing.T) {
	app, err := scopeOf("https://app.agency.gov", "/portal")
	if err != nil {
		t.Fatal(err)
	}
	idp, err := scopeOf("https://login.idp.example", "/")
	if err != nil {
		t.Fatal(err)
	}
	local, err := scopeOf("http://127.0.0.1:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	all := []*proto.NetworkCookie{
		cookie("host-only", "app.agency.gov", "/", true),
		cookie("parent-domain", ".agency.gov", "/", true),
		cookie("own-domain-cookie", ".app.agency.gov", "/", false),
		cookie("under-base", "app.agency.gov", "/portal/admin", false), // deeper than the base: not sent to it
		cookie("root-path", "app.agency.gov", "/", false),
		cookie("base-itself", "app.agency.gov", "/portal", false),
		cookie("other-path", "app.agency.gov", "/other", false),
		cookie("prefix-not-path", "app.agency.gov", "/port", false),
		cookie("sibling-host", "mail.agency.gov", "/", false),
		cookie("sibling-host-dot", ".mail.agency.gov", "/", false),
		cookie("lookalike", "evilagency.gov", "/", false),
		cookie("lookalike-dot", ".gov.evil", "/", false),
		cookie("unrelated", "unrelated.example", "/", false),
		cookie("unrelated-dot", ".bank.example", "/", false),
		cookie("idp", "login.idp.example", "/", true),
		cookie("idp-parent", ".idp.example", "/", true),
		cookie("idp-sibling", "other.idp.example", "/", true),
		cookie("local", "127.0.0.1", "/", false),
		cookie("local-secure", "127.0.0.1", "/", true),
		cookie("local-path", "127.0.0.1", "/deep", false), // deeper than /: not sent to the root
		cookie("other-ip", "127.0.0.2", "/", false),
	}
	partitioned := cookie("partitioned-own", "app.agency.gov", "/", true)
	partitioned.PartitionKey = &proto.NetworkCookiePartitionKey{TopLevelSite: "https://agency.gov"}
	foreign := cookie("partitioned-foreign", "app.agency.gov", "/", true)
	foreign.PartitionKey = &proto.NetworkCookiePartitionKey{TopLevelSite: "https://tracker.example"}
	crossSite := cookie("partitioned-cross", "app.agency.gov", "/", true)
	crossSite.PartitionKey = &proto.NetworkCookiePartitionKey{TopLevelSite: "https://agency.gov", HasCrossSiteAncestor: true}
	opaque := cookie("partitioned-opaque", "app.agency.gov", "/", true)
	opaque.PartitionKeyOpaque = true
	all = append(all, partitioned, foreign, crossSite, opaque)

	var got []string
	for _, c := range attachedCookies(all, []cookieScope{app, idp, local}) {
		got = append(got, c.Name)
	}
	want := []string{
		"host-only", "parent-domain", "own-domain-cookie", "root-path", "base-itself",
		"idp", "idp-parent", "local", "partitioned-own",
	}
	if !slices.Equal(got, want) {
		t.Errorf("kept %v\nwant %v", got, want)
	}
}

// Only https scopes take a secure cookie.
func TestAttachedCookiesSecureNeedsHTTPS(t *testing.T) {
	plain, _ := scopeOf("http://app.agency.gov", "")
	secure := cookie("s", "app.agency.gov", "/", true)
	if got := attachedCookies([]*proto.NetworkCookie{secure}, []cookieScope{plain}); len(got) != 0 {
		t.Errorf("a secure cookie was kept for an http origin: %v", got)
	}
}

func TestCookieParamKeepsSessionCookiesSession(t *testing.T) {
	c := cookie("sid", "x", "/", false)
	c.Expires = 12345
	if p := cookieParam(c); p.Expires != 0 {
		t.Errorf("a session cookie got an expiry: %v", p.Expires)
	}
	c.Session = false
	if p := cookieParam(c); p.Expires != 12345 {
		t.Errorf("a persistent cookie lost its expiry: %v", p.Expires)
	}
}

// A copy of an Endpoint prints as its origin too: with pointer receivers,
// %v of the value would fall back to its fields, browser path included.
func TestEndpointValuePrintsNoPath(t *testing.T) {
	e, err := ParseBrowserURL("ws://127.0.0.1:9222/devtools/browser/" + secretGUID)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{fmt.Sprintf("%v", *e), fmt.Sprintf("%+v", *e), fmt.Sprintf("%#v", *e), fmt.Sprint(*e), fmt.Sprintf("%s", *e)} {
		if got != "ws://127.0.0.1:9222" {
			t.Errorf("Endpoint value prints %q", got)
		}
	}
}

// fakeConn is a connection whose remote end is wherever the test says.
type fakeConn struct {
	net.Conn
	remote net.Addr
	closed bool
}

func (c *fakeConn) RemoteAddr() net.Addr        { return c.remote }
func (c *fakeConn) Close() error                { c.closed = true; return nil }
func (c *fakeConn) SetDeadline(time.Time) error { return nil }

// The dialer refuses a connection that reached anything but a loopback
// address, whatever name was dialed, and closes it.
func TestLoopbackDialerRefusesNonLoopback(t *testing.T) {
	for _, ip := range []string{"192.0.2.10", "10.0.0.1", "::ffff:192.168.1.1", "2001:db8::1"} {
		fc := &fakeConn{remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 9222}}
		d := &loopbackDialer{dial: func(context.Context, string, string) (net.Conn, error) { return fc, nil }}
		c, err := d.DialContext(context.Background(), "tcp", "localhost:9222")
		if !errors.Is(err, errNotLoopback) || c != nil {
			t.Errorf("%s: DialContext = %v, %v; want errNotLoopback", ip, c, err)
		}
		if !fc.closed {
			t.Errorf("%s: the refused connection was left open", ip)
		}
		if d.conn != nil {
			t.Errorf("%s: the refused connection was kept", ip)
		}
	}
	for _, ip := range []string{"127.0.0.1", "127.8.9.10", "::1"} {
		fc := &fakeConn{remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 9222}}
		d := &loopbackDialer{dial: func(context.Context, string, string) (net.Conn, error) { return fc, nil }}
		if c, err := d.DialContext(context.Background(), "tcp", "localhost:9222"); err != nil || c != fc {
			t.Errorf("%s: DialContext = %v, %v; want the connection", ip, c, err)
		}
	}
	// A real listener on a non-loopback address of this machine, when it has one.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(ipn.IP.String(), "0"))
		if err != nil {
			continue
		}
		defer ln.Close()
		go func() {
			if c, err := ln.Accept(); err == nil {
				c.Close()
			}
		}()
		d := &loopbackDialer{dial: (&net.Dialer{}).DialContext}
		if _, err := d.DialContext(context.Background(), "tcp", ln.Addr().String()); !errors.Is(err, errNotLoopback) {
			t.Errorf("dialing %s: err = %v, want errNotLoopback", ln.Addr(), err)
		}
		break
	}
}

func versionServer(t *testing.T, h http.HandlerFunc) *Endpoint {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e, err := ParseBrowserURL(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// /json/version is read with no redirect followed, a capped body, and the
// WebSocket host taken from the endpoint given, never from the answer.
func TestResolveDevTools(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/x"}`)
	}))
	defer other.Close()

	e := versionServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/json/version", http.StatusFound)
	})
	if _, err := resolveDevTools(e); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Errorf("redirect: err = %v, want a refusal naming HTTP 302", err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the redirect was followed (%d request(s) to its target)", n)
	}

	e = versionServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://`+strings.Repeat("a", 2*maxVersionBody)+`/devtools/browser/x"}`)
	})
	if _, err := resolveDevTools(e); err == nil || !strings.Contains(err.Error(), "not Chrome's DevTools") {
		t.Errorf("oversized body: err = %v", err)
	}

	e = versionServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://192.0.2.7:9222/devtools/browser/`+secretGUID+`"}`)
	})
	got, err := resolveDevTools(e)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ws://" + strings.TrimPrefix(e.String(), "http://") + "/devtools/browser/" + secretGUID; got != want {
		t.Errorf("resolved %q, want %q (the given host, not the answer's)", got, want)
	}

	for name, body := range map[string]string{
		"not json":    `<html>hello</html>`,
		"no ws url":   `{"Browser":"x"}`,
		"page target": `{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/page/x"}`,
		"wss":         `{"webSocketDebuggerUrl":"wss://127.0.0.1:1/devtools/browser/x"}`,
	} {
		e = versionServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if _, err := resolveDevTools(e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Once Close has taken the registry, nothing more can be added to it, so a
// caller knows to remove what it just made itself.
func TestOwnedTargetsCloseOnTake(t *testing.T) {
	var o ownedTargets
	if !o.addTarget("t1") || !o.addContext("c1") {
		t.Fatal("adds refused on an open registry")
	}
	cs, ts := o.take()
	if len(cs) != 1 || len(ts) != 1 {
		t.Fatalf("take = %v, %v", cs, ts)
	}
	if o.addTarget("t2") || o.addContext("c2") {
		t.Error("adds accepted after take")
	}
	if cs, ts := o.take(); len(cs) != 0 || len(ts) != 0 {
		t.Errorf("second take = %v, %v", cs, ts)
	}
}

// An IPv6 literal origin: Chrome writes the cookie domain with brackets.
func TestAttachedCookiesIPv6Origin(t *testing.T) {
	sc, err := scopeOf("http://[::1]:3000", "")
	if err != nil {
		t.Fatal(err)
	}
	if sc.host != "[::1]" {
		t.Errorf("scope host = %q, want [::1]", sc.host)
	}
	got := attachedCookies([]*proto.NetworkCookie{cookie("six", "[::1]", "/", false), cookie("four", "127.0.0.1", "/", false)}, []cookieScope{sc})
	if len(got) != 1 || got[0].Name != "six" {
		t.Errorf("kept %v, want only the [::1] cookie", got)
	}
	part := cookie("six-part", "[::1]", "/", true)
	part.PartitionKey = &proto.NetworkCookiePartitionKey{TopLevelSite: "https://[::1]"}
	tls, _ := scopeOf("https://[::1]:3443", "")
	if got := attachedCookies([]*proto.NetworkCookie{part}, []cookieScope{tls}); len(got) != 1 {
		t.Errorf("a first-party partitioned cookie on [::1] was dropped")
	}
}

// Creating a tab and registering it is one step as far as Close is
// concerned: a take that starts while the tab is being created waits, and
// then finds it.
func TestCreateTargetIsAtomicWithTake(t *testing.T) {
	var o ownedTargets
	creating := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = o.createTarget(func() (proto.TargetTargetID, error) {
			close(creating)
			<-release
			return "tab", nil
		})
	}()
	<-creating
	took := make(chan []proto.TargetTargetID, 1)
	go func() {
		_, ts := o.take()
		took <- ts
	}()
	select {
	case ts := <-took:
		t.Fatalf("take returned %v while the tab was still being created", ts)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if ts := <-took; !slices.Equal(ts, []proto.TargetTargetID{"tab"}) {
		t.Errorf("take = %v, want the tab created meanwhile", ts)
	}
	if _, err := o.createTarget(func() (proto.TargetTargetID, error) {
		t.Error("created after Close")
		return "late", nil
	}); err == nil {
		t.Error("createTarget succeeded after take")
	}
}
