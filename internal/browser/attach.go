package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

// connectTimeout bounds each step of reaching an attached browser: the
// /json/version lookup, and the dial, WebSocket handshake and first DevTools
// exchange together. A variable only so tests can shorten it.
var connectTimeout = 10 * time.Second

// Endpoint is a parsed --browser-url: the DevTools endpoint of a Chrome the
// user started with --remote-debugging-port. A ws:// URL carries the
// browser's GUID in its path, and anyone who has it can drive that browser,
// so the URL itself is never printed: String gives scheme://host:port only.
type Endpoint struct {
	raw     string // as given, for connecting only
	ws      bool   // raw is the browser's WebSocket URL itself
	host    string // hostname, for the loopback check at dial time
	display string // scheme://host:port
	version string // http://host:port/json/version, for the error hint
}

// Value receivers: a copy of an Endpoint printed with %v or %#v must not fall
// back to printing its fields, raw URL included.
func (e Endpoint) String() string   { return e.display }
func (e Endpoint) GoString() string { return e.display }

// ParseBrowserURL checks a --browser-url value: an http:// DevTools address
// (http://localhost:9222) or a browser ws:// URL, on a loopback host
// (localhost, 127.0.0.0/8, ::1) and with an explicit port. A remote browser
// is refused: the session it holds would travel over the network to reach
// Frostfall, and the debugging port has no authentication of its own. Errors
// never repeat the value past its host and port.
func ParseBrowserURL(raw string) (*Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("--browser-url: not a URL; give the DevTools address, such as http://localhost:9222, or a ws:// browser URL")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	shown := scheme + "://" + hostPort(host, port)
	switch {
	case scheme != "http" && scheme != "ws":
		return nil, fmt.Errorf("--browser-url: %s: the scheme must be http or ws (Chrome serves DevTools as http://host:port and ws://host:port/devtools/browser/...)", shown)
	case u.User != nil:
		return nil, fmt.Errorf("--browser-url: %s: remove the credentials; the DevTools endpoint takes none", shown)
	case !isLoopback(host):
		return nil, fmt.Errorf("--browser-url: %s: only a browser on this machine can be attached to (localhost, 127.0.0.1 or ::1); "+
			"the debugging port gives full control of the browser with no authentication, so it must not be reached over a network", shown)
	case port == "":
		return nil, fmt.Errorf("--browser-url: %s: give the port Chrome was started with, such as http://localhost:9222", shown)
	case u.RawQuery != "" || u.Fragment != "":
		return nil, fmt.Errorf("--browser-url: %s: remove the query and fragment", shown)
	}
	e := &Endpoint{
		raw:     raw,
		ws:      scheme == "ws",
		host:    host,
		display: shown,
		version: "http://" + hostPort(host, port) + "/json/version",
	}
	if e.ws {
		if !strings.HasPrefix(u.Path, "/devtools/browser/") {
			return nil, fmt.Errorf("--browser-url: %s: a ws:// URL must be the browser's own (its path starts with /devtools/browser/, as webSocketDebuggerUrl in /json/version shows); "+
				"or give the http://host:port address instead", shown)
		}
		return e, nil
	}
	switch u.Path {
	case "", "/", "/json/version":
	default:
		return nil, fmt.Errorf("--browser-url: %s: give the address without a path, such as http://localhost:9222", shown)
	}
	return e, nil
}

func hostPort(host, port string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// isLoopback accepts the name localhost and loopback addresses. The name is
// checked again where it matters, at dial time: the connection must reach a
// loopback address whatever the resolver said.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// unreachable is the error for an endpoint Frostfall could not attach to:
// what went wrong in a few words, and the two usual causes.
func (e *Endpoint) unreachable(reason string) error {
	return fmt.Errorf("cannot attach to Chrome at %s: %s. Start Chrome with --remote-debugging-port and a separate --user-data-dir "+
		"(since Chrome 136 the debugging port is ignored on the default profile directory), sign in to the app in that window, "+
		"and check that the RemoteDebuggingAllowed enterprise policy permits remote debugging: %s should return JSON. "+
		"If another Chrome is already running on that --user-data-dir, the new one hands off to it and never opens the port: close it first",
		e.display, reason, e.version)
}

// reason describes a connection error in words that cannot carry the URL:
// the system call's own message for a refused or failed dial, a timeout, or
// a generic line. A url.Error or a handshake error would repeat the URL or
// whatever the server sent, so neither is ever printed.
func reason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused (nothing is listening on that port)"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return fmt.Sprintf("no answer within %s", connectTimeout)
	case errors.Is(err, errNotLoopback):
		return "the name did not resolve to a loopback address"
	}
	return "the connection failed"
}

var errNotLoopback = errors.New("not a loopback address")

// Connect attaches to the Chrome at e. Nothing about the browser changes:
// tests run in contexts Frostfall creates (and removes), and Close
// disconnects without quitting it. An endpoint that cannot be reached, or is
// not Chrome's DevTools, is an error worded for exit 3.
func Connect(e *Endpoint) (*Browser, error) {
	wsURL := e.raw
	if !e.ws {
		var err error
		if wsURL, err = resolveDevTools(e); err != nil {
			return nil, err
		}
	}
	d := &loopbackDialer{dial: (&net.Dialer{}).DialContext}
	ws := &cdp.WebSocket{Dialer: d}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := ws.Connect(ctx, wsURL, nil); err != nil {
		if d.conn != nil {
			_ = d.conn.Close()
		}
		var bad *cdp.BadHandshakeError
		if errors.As(err, &bad) {
			return nil, e.unreachable("the WebSocket handshake was refused (is the browser URL current? it changes every time Chrome starts)")
		}
		return nil, e.unreachable(reason(err))
	}
	// No default device: rod otherwise emulates a laptop on every page it
	// opens, user-agent included, and an SSO session bound to the browser's
	// real user-agent could be refused, or revoked, when a test page or the
	// capture tab presents another. The viewport is set per test anyway.
	r := rod.New().ControlURL("").NoDefaultDevice().Client(cdp.New().Start(ws))
	// Still under the dial deadline: a server that accepts the WebSocket and
	// then says nothing fails here instead of hanging the run.
	if err := r.Connect(); err != nil {
		_ = ws.Close()
		return nil, e.unreachable("the browser did not answer over DevTools")
	}
	// Connected: the connection itself must not expire.
	_ = d.conn.SetDeadline(time.Time{})
	return &Browser{rod: r, ws: ws, endpoint: e.display}, nil
}

// Version is the attached browser's product string (Chrome/136.0...), for
// the --verbose log; "" when it cannot be read.
func (b *Browser) Version() string {
	res, err := proto.BrowserGetVersion{}.Call(b.rod.Timeout(5 * time.Second))
	if err != nil {
		return ""
	}
	return res.Product
}

// maxVersionBody caps how much of a /json/version answer is read; Chrome's
// is well under a kilobyte.
const maxVersionBody = 64 << 10

// resolveDevTools turns an http:// endpoint into the browser's WebSocket URL
// by reading /json/version, as go-rod's launcher.ResolveURL does, but bounded:
// the connection must reach a loopback address, a redirect is a failure (a
// local server could otherwise send the request, and then the browser
// connection, to another host), the body is capped, and one deadline covers
// it all. The answer must be a ws:// URL for the browser target; its host is
// replaced with the one given, as ResolveURL does, so the WebSocket goes
// where the user pointed and nowhere the answer names.
func resolveDevTools(e *Endpoint) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       (&loopbackDialer{dial: (&net.Dialer{}).DialContext}).dialChecked,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.version, nil)
	if err != nil {
		return "", e.unreachable("the connection failed")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", e.unreachable(reason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A redirect lands here too, unfollowed.
		return "", e.unreachable(fmt.Sprintf("/json/version answered HTTP %d, not Chrome's DevTools JSON", resp.StatusCode))
	}
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVersionBody)).Decode(&v); err != nil {
		return "", e.unreachable("the answer was not Chrome's DevTools /json/version")
	}
	u, err := url.Parse(v.WS)
	want, _ := url.Parse(e.version)
	if err != nil || u.Scheme != "ws" || !strings.HasPrefix(u.Path, "/devtools/browser/") || u.RawQuery != "" || u.User != nil {
		return "", e.unreachable("the answer was not Chrome's DevTools /json/version")
	}
	u.Host = want.Host
	return u.String(), nil
}

// loopbackDialer dials and refuses a connection that did not reach a
// loopback address (a localhost entry that resolves elsewhere). As the
// WebSocket's dialer it also gives the connection a deadline, since go-rod's
// handshake has none of its own, until Connect clears it; dial is the
// underlying dialer, replaceable in tests.
type loopbackDialer struct {
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	conn net.Conn
}

// dialChecked dials and checks the remote address, without a deadline.
func (d *loopbackDialer) dialChecked(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := d.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	addr, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		_ = c.Close()
		return nil, errNotLoopback
	}
	return c, nil
}

// DialContext is dialChecked with the handshake deadline, for go-rod.
func (d *loopbackDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := d.dialChecked(ctx, network, address)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(connectTimeout))
	d.conn = c
	return c, nil
}

// cookieScope is where a captured cookie may be used: a host, whether it is
// served over https, and the path the app lives under.
type cookieScope struct {
	host  string
	https bool
	path  string
}

// scopeOf is the cookie scope of an origin, with the app's base path (or
// "/" for an allowed origin).
func scopeOf(origin, path string) (cookieScope, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return cookieScope{}, err
	}
	if path == "" {
		path = "/"
	}
	return cookieScope{host: cookieHost(u), https: u.Scheme == "https", path: path}, nil
}

// cookieHost is a URL's host as Chrome writes it in a cookie's domain:
// lower-cased, and an IPv6 literal in brackets ([::1], which Hostname strips).
func cookieHost(u *url.URL) string {
	h := strings.ToLower(u.Hostname())
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	return h
}

// attachedCookies keeps only the cookies that one of scopes would send. The
// input is already Chrome's answer for those URLs (Network.getCookies), so
// this is defense in depth: the browser holds every site's session, and none
// of them but the app's (and its identity provider's) has any business in a
// scan. A cookie qualifies by domain (the host itself, or a parent domain for
// a .domain cookie), by secure (an https scope only), by path (its path covers
// the scope's: Chrome answers for the base URL itself, so a cookie scoped to a
// deeper path, such as Keycloak's on /realms/<realm>/, is never in the input),
// and, for a partitioned cookie, by a partition that is the scope's own site.
func attachedCookies(all []*proto.NetworkCookie, scopes []cookieScope) []*proto.NetworkCookie {
	var out []*proto.NetworkCookie
	for _, c := range all {
		if slices.ContainsFunc(scopes, func(s cookieScope) bool { return cookieApplies(c, s) }) {
			out = append(out, c)
		}
	}
	return out
}

func cookieApplies(c *proto.NetworkCookie, s cookieScope) bool {
	if !domainMatch(s.host, c.Domain) {
		return false
	}
	if c.Secure && !s.https {
		return false
	}
	p := c.Path
	if p == "" {
		p = "/"
	}
	if !pathMatch(s.path, p) {
		return false
	}
	if c.PartitionKeyOpaque {
		return false
	}
	if pk := c.PartitionKey; pk != nil {
		site, err := url.Parse(pk.TopLevelSite)
		if err != nil || pk.HasCrossSiteAncestor || !domainMatch(s.host, "."+cookieHost(site)) {
			return false
		}
	}
	return true
}

// domainMatch is RFC 6265's domain match as Chrome reports cookies: a domain
// with a leading dot covers that domain and its subdomains, one without is
// the host only.
func domainMatch(host, domain string) bool {
	domain = strings.ToLower(domain)
	if d, ok := strings.CutPrefix(domain, "."); ok {
		return d != "" && (host == d || strings.HasSuffix(host, "."+d))
	}
	return host == domain
}

// pathMatch is RFC 6265's path match: cookiePath applies to reqPath.
func pathMatch(reqPath, cookiePath string) bool {
	if reqPath == cookiePath {
		return true
	}
	if !strings.HasPrefix(reqPath, cookiePath) {
		return false
	}
	return strings.HasSuffix(cookiePath, "/") || reqPath[len(cookiePath)] == '/'
}

// cookieParam converts a cookie the browser reported into one it accepts
// back. A session cookie stays one.
func cookieParam(c *proto.NetworkCookie) *proto.NetworkCookieParam {
	p := &proto.NetworkCookieParam{
		Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
		Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite,
		Priority: c.Priority, PartitionKey: c.PartitionKey,
	}
	if !c.Session {
		p.Expires = c.Expires
	}
	return p
}

// captureTabOpened, when set, runs right after the capture tab is created
// and registered. Tests use it to close the browser mid-capture.
var captureTabOpened func()

// captureAttached reads the session the user signed in with from the attached
// browser's own (default) context, once, through one new background tab that
// is closed afterwards. The user's tabs are never read, navigated or touched.
//
// Chrome reads an origin's localStorage only through a frame of that origin
// (DOMStorage.getDOMStorageItems refuses one with no such frame), and gives a
// first-party partitioned cookie only to a page on its site, so the tab visits
// each origin's root (the app's base path for the app). Nothing of that visit
// reaches the app:
//
//   - Service workers are bypassed for the tab, so a worker registered on the
//     origin cannot answer the navigation with the real app, or fetch with the
//     user's cookies.
//   - Every request the tab makes is intercepted for the whole life of the tab
//     (Fetch is never disabled while it is open): the document is answered with
//     an empty page by Frostfall and anything else is failed, so nothing goes
//     to the network, the app's code never runs, and no redirect can happen. A
//     request still paused when the tab closes is aborted with it.
//   - The empty page is answered with HTTP 404, which Chrome records as a
//     hidden history row, left out of the visible history and the address
//     bar's suggestions, and it has no title to give a history row. (Marking
//     the navigation auto_subframe did the same up to Chrome 154; Chrome 157
//     never commits such a top-level navigation.)
//
// On each origin's page the cookies come from Network.getCookies for that URL,
// so Chrome does the matching and no other site's cookie is read at all;
// attachedCookies filters the answer again. A cookie scoped to a path below
// the base URL is not included. Session storage is per tab, so a session kept
// only there cannot be carried over.
func (a *Auth) captureAttached(b *Browser) (*State, error) {
	if !b.Attached() {
		return nil, errors.New("auth: attach: true needs --browser-url, the address of the Chrome to take the session from")
	}
	origins := []string{a.origin}
	var allowed []string
	for o := range a.allowed {
		if o != a.origin {
			allowed = append(allowed, o)
		}
	}
	slices.Sort(allowed)
	origins = append(origins, allowed...)
	scopes := make([]cookieScope, len(origins))
	urls := make([]string, len(origins))
	for i, o := range origins {
		path := "/"
		if i == 0 {
			path = a.basePath
		}
		sc, err := scopeOf(o, path)
		if err != nil {
			return nil, fmt.Errorf("auth attach: %w", err)
		}
		scopes[i] = sc
		urls[i] = o + strings.TrimSuffix(sc.path, "/") + "/"
	}

	page, closeTab, err := b.openCaptureTab()
	if err != nil {
		return nil, fmt.Errorf("auth attach: opening a tab in the attached browser: %w", err)
	}
	// Deferred first, so it runs last: the interception below stays on until
	// the tab is gone.
	defer closeTab()
	if captureTabOpened != nil {
		captureTabOpened()
	}
	stopIntercept, err := interceptCaptureTab(page, urls)
	if err != nil {
		return nil, fmt.Errorf("auth attach: %w", err)
	}
	defer stopIntercept()

	st := &State{}
	seen := map[string]bool{}
	keys := 0
	for i, o := range origins {
		items, err := a.readOrigin(page, o, urls[i])
		switch {
		case err != nil && i == 0:
			// Without the app's own page neither its localStorage nor a
			// partitioned cookie can be read, and a scan would start from a
			// partial session that the guard can only call "expired". Say what
			// actually failed instead.
			return nil, fmt.Errorf("auth attach: %w: the session could not be read from the attached browser", err)
		case err != nil:
			a.warnf("auth attach: %v; the session may be missing what %s keeps in localStorage", err, o)
		case len(items) > 0:
			st.origins = append(st.origins, originStorage{origin: o, local: items})
			keys += len(items)
		}
		// Asked from the origin's own page when it loaded, so a first-party
		// partitioned cookie is included; otherwise unpartitioned ones only.
		res, err := proto.NetworkGetCookies{Urls: []string{urls[i]}}.Call(page.Timeout(a.defaults.Timeout.Std()))
		if err != nil {
			return nil, fmt.Errorf("auth attach: reading cookies from the attached browser: %w", err)
		}
		for _, c := range attachedCookies(res.Cookies, scopes[i:i+1]) {
			k := c.Name + "\x00" + c.Domain + "\x00" + c.Path
			if pk := c.PartitionKey; pk != nil {
				k += "\x00" + pk.TopLevelSite
			}
			if !seen[k] {
				seen[k] = true
				st.cookies = append(st.cookies, cookieParam(c))
			}
		}
	}

	a.notef("attach: using the session for %s (%d cookies, %d localStorage keys) from the browser at %s",
		strings.Join(origins, ", "), len(st.cookies), keys, b.Endpoint())
	if len(st.cookies) == 0 && len(st.origins) == 0 {
		a.warnf("auth attach: the attached browser at %s holds no cookies or localStorage for %s, so there is no session to restore; "+
			"sign in to the app in that browser window first (every test will start signed out)", b.Endpoint(), a.origin)
	}
	return st, nil
}

// openCaptureTab opens the background tab the session is read through, in the
// attached browser's own context. Creation and registration happen under the
// registry's lock, which Close takes too, so an interrupt can never fall
// between them and leave an unregistered tab in the user's window. The
// returned function closes the tab; one that could not be closed stays
// registered for Close to try again.
func (b *Browser) openCaptureTab() (*rod.Page, func(), error) {
	id, err := b.owned.createTarget(func() (proto.TargetTargetID, error) {
		target, err := proto.TargetCreateTarget{URL: "about:blank", Background: true}.Call(b.rod.Timeout(connectTimeout))
		if err != nil {
			return "", err
		}
		return target.TargetID, nil
	})
	if err != nil {
		return nil, nil, err
	}
	closeTab := func() {
		if _, err := (proto.TargetCloseTarget{TargetID: id}).Call(b.rod.Timeout(5 * time.Second)); err == nil {
			b.waitGone([]proto.TargetTargetID{id})
			b.owned.dropTarget(id)
		}
	}
	page, err := b.rod.PageFromTarget(id)
	if err != nil {
		closeTab()
		return nil, nil, err
	}
	return page, closeTab, nil
}

// blankDocument is what the capture tab is served in place of the app's page:
// no title, so a history row Chrome keeps for the URL is not renamed.
const blankDocument = "<!DOCTYPE html>"

// interceptCaptureTab turns off service workers for the tab and intercepts
// every request it makes: a document request for one of docs is answered with
// blankDocument, anything else is failed. The returned function stops the
// handler; requests paused after that stay paused until the tab is closed,
// which aborts them. Fetch is never disabled, since that would release a
// paused request to the network.
func interceptCaptureTab(page *rod.Page, docs []string) (func(), error) {
	p := page.Timeout(connectTimeout)
	defer p.CancelTimeout()
	if err := (proto.NetworkEnable{}).Call(p); err != nil {
		return nil, fmt.Errorf("could not prepare the capture tab (%w)", err)
	}
	if err := (proto.NetworkSetBypassServiceWorker{Bypass: true}).Call(p); err != nil {
		return nil, fmt.Errorf("could not turn off service workers for the capture tab (%w)", err)
	}
	if err := (proto.FetchEnable{Patterns: []*proto.FetchRequestPattern{{URLPattern: "*", RequestStage: proto.FetchRequestStageRequest}}}).Call(p); err != nil {
		return nil, fmt.Errorf("could not intercept the capture tab's requests (%w)", err)
	}
	ctx, cancel := context.WithCancel(page.GetContext())
	h := page.Context(ctx)
	// EachEvent subscribes now, before any navigation. (go-rod also enables the
	// Fetch domain for this event type on its own; the explicit enable above
	// says so outright.)
	go h.EachEvent(func(e *proto.FetchRequestPaused) {
		if e.ResourceType == proto.NetworkResourceTypeDocument && slices.Contains(docs, e.Request.URL) {
			_ = proto.FetchFulfillRequest{
				RequestID:       e.RequestID,
				ResponseCode:    http.StatusNotFound, // a hidden history row; see captureAttached
				ResponseHeaders: []*proto.FetchHeaderEntry{{Name: "Content-Type", Value: "text/html; charset=utf-8"}},
				Body:            []byte(blankDocument),
			}.Call(h)
			return
		}
		_ = proto.FetchFailRequest{RequestID: e.RequestID, ErrorReason: proto.NetworkErrorReasonBlockedByClient}.Call(h)
	})()
	return cancel, nil
}

// readOrigin loads url (origin's root, already intercepted) in the capture
// tab and reads the origin's localStorage from the page, but only if the page
// is on that origin: the check runs in the page, so another origin's storage
// never crosses CDP.
func (a *Auth) readOrigin(page *rod.Page, origin, url string) ([][2]string, error) {
	p := page.Timeout(a.defaults.Timeout.Std())
	defer p.CancelTimeout()
	nav, err := proto.PageNavigate{URL: url, TransitionType: proto.PageTransitionTypeLink}.Call(p)
	if err != nil || nav.ErrorText != "" {
		return nil, fmt.Errorf("could not open %s in the capture tab", origin)
	}
	if err := p.WaitLoad(); err != nil {
		return nil, fmt.Errorf("could not open %s in the capture tab", origin)
	}
	res, err := p.Eval(`(o) => {
		if (location.origin !== o) return null;
		try { return Object.keys(localStorage).map(k => [k, localStorage.getItem(k)]) } catch (e) { return [] }
	}`, origin)
	if err != nil {
		return nil, fmt.Errorf("could not read the localStorage of %s", origin)
	}
	if res.Value.Nil() {
		return nil, fmt.Errorf("the capture tab for %s ended on another origin, so its localStorage was not read", origin)
	}
	var items [][2]string
	if err := json.Unmarshal([]byte(res.Value.JSON("", "")), &items); err != nil {
		return nil, fmt.Errorf("could not read the localStorage of %s", origin)
	}
	return items, nil
}
