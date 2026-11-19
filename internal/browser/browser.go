// Package browser wraps go-rod: browser lifecycle, per-test page sessions,
// readiness waits, and the step vocabulary.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/config"
)

// Browser owns one Chromium process, or is attached to one the user started
// (Connect); each session gets its own incognito context so cookies,
// storage, and service workers never leak between tests.
type Browser struct {
	rod      *rod.Browser
	launcher *launcher.Launcher

	// ws is the DevTools connection of an attached browser, nil for a launched
	// one. Closing it disconnects without quitting the user's Chrome.
	ws *cdp.WebSocket
	// endpoint is the attached browser's DevTools endpoint as it may be
	// printed: scheme://host:port, never the path (a browser GUID).
	endpoint string
	// owned is what Frostfall created in an attached browser and must remove
	// again: its browser contexts and the one tab it opens in the user's own
	// context. Close disposes whatever is still there.
	owned     ownedTargets
	closeOnce sync.Once

	// Warn, when set, receives what Close could not finish in time (a closed
	// tab still listed). Nothing is printed without it.
	Warn func(format string, args ...any)
}

// LaunchOptions adjusts how the browser is started.
type LaunchOptions struct {
	// SiteIsolation runs cross-site frames in renderer processes of their
	// own, as desktop Chrome does for its users; each is then a separate CDP
	// target (an out-of-process frame). rod's launcher turns site isolation
	// off by default, and Launch keeps that: frames of every origin then
	// share the page's process. Scans reach frames either way.
	SiteIsolation bool
}

// ownedTargets tracks the contexts and tabs Frostfall created in an attached
// browser. The signed-out probe opens contexts in parallel, hence the lock.
// Once Close has taken what is there the registry is closed: an add then
// reports false, and the caller removes what it just created itself, since
// nothing else would.
type ownedTargets struct {
	mu       sync.Mutex
	closed   bool
	contexts map[proto.BrowserBrowserContextID]bool
	targets  map[proto.TargetTargetID]bool
}

func (o *ownedTargets) addContext(id proto.BrowserBrowserContextID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	if o.contexts == nil {
		o.contexts = map[proto.BrowserBrowserContextID]bool{}
	}
	o.contexts[id] = true
	return true
}

func (o *ownedTargets) dropContext(id proto.BrowserBrowserContextID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.contexts, id)
}

func (o *ownedTargets) addTarget(id proto.TargetTargetID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	if o.targets == nil {
		o.targets = map[proto.TargetTargetID]bool{}
	}
	o.targets[id] = true
	return true
}

// createTarget runs create and registers what it made, all under the lock
// take also holds: Close either runs before (and nothing is created) or
// after (and finds it registered), never between the two.
func (o *ownedTargets) createTarget(create func() (proto.TargetTargetID, error)) (proto.TargetTargetID, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return "", errors.New("the browser connection is closing")
	}
	id, err := create()
	if err != nil {
		return "", err
	}
	if o.targets == nil {
		o.targets = map[proto.TargetTargetID]bool{}
	}
	o.targets[id] = true
	return id, nil
}

func (o *ownedTargets) dropTarget(id proto.TargetTargetID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.targets, id)
}

// take empties and closes the registry and returns what was in it.
func (o *ownedTargets) take() ([]proto.BrowserBrowserContextID, []proto.TargetTargetID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	var cs []proto.BrowserBrowserContextID
	for id := range o.contexts {
		cs = append(cs, id)
	}
	var ts []proto.TargetTargetID
	for id := range o.targets {
		ts = append(ts, id)
	}
	o.contexts, o.targets = nil, nil
	return cs, ts
}

// Attached reports whether b is a browser the user started, reached with
// Connect, rather than one Frostfall launched.
func (b *Browser) Attached() bool { return b.ws != nil }

// Endpoint is an attached browser's DevTools endpoint as scheme://host:port,
// safe to print; "" for a launched browser.
func (b *Browser) Endpoint() string { return b.endpoint }

// Launch finds a system Chrome (GitHub runners and most dev machines have
// one) or lets rod's launcher download a managed Chromium, then connects.
func Launch(browserPath string) (*Browser, error) {
	return LaunchWith(browserPath, LaunchOptions{})
}

// LaunchWith is Launch with options.
func LaunchWith(browserPath string, o LaunchOptions) (*Browser, error) {
	// disable-dev-shm-usage: containers default /dev/shm to 64MB and Chromium
	// crashes tabs when it fills on large pages; writing shared memory to
	// /tmp instead is the standard CI mitigation and harmless elsewhere.
	l := launcher.New().Headless(true).Set("disable-dev-shm-usage")
	if o.SiteIsolation {
		// rod's defaults disable site-per-process and TranslateUI in one
		// flag; keep the second.
		l = l.Set("disable-features", "TranslateUI").Delete("disable-site-isolation-trials")
	}
	if browserPath != "" {
		l = l.Bin(browserPath)
	} else if path, ok := launcher.LookPath(); ok {
		l = l.Bin(path)
	}
	u, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launching browser: %w", err)
	}
	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		l.Kill()
		l.Cleanup()
		return nil, fmt.Errorf("connecting to browser: %w", err)
	}
	return &Browser{rod: b, launcher: l}, nil
}

// Close ends the run's use of the browser. A launched browser is quit and its
// profile removed. An attached one is never quit: Browser.close would end the
// user's Chrome, every window and tab in it. Instead the contexts and the tab
// Frostfall created that are still open are removed, and the connection is
// dropped. Safe to call more than once (a signal and the normal exit can both
// reach it).
func (b *Browser) Close() {
	b.closeOnce.Do(func() {
		if b.ws == nil {
			b.rod.Close()
			b.launcher.Cleanup()
			return
		}
		// One budget for all of it, so an interrupt is never held up long by a
		// browser that has stopped answering.
		deadline := time.Now().Add(closeBudget)
		contexts, targets := b.owned.take()
		for _, id := range targets {
			b.scrubTab(id, deadline)
		}
		r := b.rod.Timeout(time.Until(deadline))
		for _, id := range targets {
			_, _ = proto.TargetCloseTarget{TargetID: id}.Call(r)
		}
		for _, id := range contexts {
			_ = proto.TargetDisposeBrowserContext{BrowserContextID: id}.Call(r)
		}
		r.CancelTimeout()
		if !b.waitGone(targets, deadline) && b.Warn != nil {
			b.Warn("attach: a tab Frostfall closed was still listed by the browser after %s; it should go away on its own", closeBudget)
		}
		_ = b.ws.Close()
	})
}

// closeBudget bounds Browser.Close in attach mode, waiting included.
const closeBudget = 5 * time.Second

// waitGone waits, until deadline, for none of ids to be among the browser's
// targets, and reports whether that happened. Target.closeTarget answers once
// the close has started, and on a busy machine the tab is still listed for a
// moment afterwards (seen on a CI runner): Close and the capture tab's own
// close wait it out, so "closed" means gone when they return. A browser that
// stops answering counts as gone: there is nothing left to wait on.
func (b *Browser) waitGone(ids []proto.TargetTargetID, deadline time.Time) bool {
	if len(ids) == 0 {
		return true
	}
	for {
		res, err := proto.TargetGetTargets{}.Call(b.rod.Timeout(time.Second))
		if err != nil {
			return true
		}
		if !slices.ContainsFunc(res.TargetInfos, func(ti *proto.TargetTargetInfo) bool { return slices.Contains(ids, ti.TargetID) }) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Session is one test's page in its own incognito browser context.
// It implements engine.Page.
type Session struct {
	// raw is the page with no deadline; page is raw under the current
	// budget. They are kept apart so the budget can be restarted (a login
	// flow runs under its own, then the test's starts fresh).
	raw     *rod.Page
	page    *rod.Page
	ctx     *rod.Browser
	owner   *Browser
	timeout time.Duration
	// seed unregisters the auth storage script; nil when there is none.
	seed func() error
	// auth lets a step re-check that the session is still signed in; nil
	// for an unauthenticated session, and for an auth: false test's.
	auth *Auth
	// redact prints URLs in this session's errors without their query and
	// other secrets; set for authenticated runs only, so a config without
	// auth reports exactly what it always did.
	redact bool
	// detach releases the contexts of out-of-process frames this session
	// attached to (see attachFrame); outOfProcess counts those attaches.
	detach       []context.CancelFunc
	outOfProcess int
}

// setTimeout restarts the session's time budget at d.
func (s *Session) setTimeout(d time.Duration) {
	s.page.CancelTimeout()
	s.timeout = d
	s.page = s.raw.Timeout(d)
}

// NewSession opens a page at url in a fresh incognito context and waits for
// per-page readiness.
func (b *Browser) NewSession(url string, vp config.Viewport, waitFor string, settle, timeout time.Duration) (*Session, error) {
	return b.NewAuthSession(nil, url, vp, waitFor, settle, timeout)
}

// open creates a session in a fresh incognito context, seeded with state
// (nil for none) but not yet navigated anywhere. Cookies go in on the
// context and storage through a script that runs before any page script of
// the first load, so the page sees its session from its very first request.
func (b *Browser) open(vp config.Viewport, timeout time.Duration, state *State) (*Session, error) {
	ctx, err := b.incognito()
	if err != nil {
		return nil, fmt.Errorf("creating browser context: %w", err)
	}
	if state != nil && len(state.cookies) > 0 {
		// SetCookies(nil) would clear instead; the guard above keeps it non-nil.
		if err := ctx.SetCookies(state.cookies); err != nil {
			ctx.Close()
			return nil, fmt.Errorf("injecting auth cookies (the browser rejected one: check each cookie's name, domain and path): %w", err)
		}
	}
	page, err := ctx.Page(proto.TargetCreateTarget{URL: ""})
	if err != nil {
		ctx.Close()
		return nil, err
	}
	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: vp.Width, Height: vp.Height, DeviceScaleFactor: 1,
	}); err != nil {
		page.Close()
		ctx.Close()
		return nil, err
	}
	if b.Attached() {
		// The user's Chrome is headed, and a test's window is often not the
		// focused one (it opens behind the user's own, and only one window has
		// focus at a time). An unfocused page reports document.hasFocus() false
		// and draws no focus ring, which the keyboard audit and the screen
		// reader transcript read. Focus emulation makes the page behave as
		// focused whatever the window manager does, as a headless page is.
		if err := (proto.EmulationSetFocusEmulationEnabled{Enabled: true}).Call(page); err != nil {
			page.Close()
			ctx.Close()
			return nil, fmt.Errorf("enabling focus emulation: %w", err)
		}
	}
	s := &Session{raw: page, page: page.Timeout(timeout), ctx: ctx, owner: b, timeout: timeout}
	if state != nil && len(state.origins) > 0 {
		src, err := state.seedScript()
		if err == nil {
			s.seed, err = page.EvalOnNewDocument(src)
		}
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("injecting auth storage: %w", err)
		}
	}
	return s, nil
}

// incognito creates a fresh browser context. In an attached browser it is
// created with disposeOnDetach, so Chrome removes it, signed-in cookies and
// all, the moment the connection drops: a crash or a Ctrl-C that skips Close
// leaves nothing behind in the user's browser. It is recorded so Close can
// remove it on an ordinary exit too.
func (b *Browser) incognito() (*rod.Browser, error) {
	res, err := proto.TargetCreateBrowserContext{DisposeOnDetach: b.Attached()}.Call(b.rod)
	if err != nil {
		return nil, err
	}
	if res.BrowserContextID == "" {
		// A context handle with no id is the browser itself, and closing it
		// would quit the browser.
		return nil, errors.New("the browser returned no context id")
	}
	ctx := b.rod.Context(b.rod.GetContext())
	ctx.BrowserContextID = res.BrowserContextID
	if b.Attached() && !b.owned.addContext(res.BrowserContextID) {
		// Close has already run, so nothing would dispose of this one later.
		_ = ctx.Close()
		return nil, errors.New("the browser connection is closing")
	}
	return ctx, nil
}

// Close closes the page and disposes its context, so a signed-in context's
// cookies do not outlive its test.
func (s *Session) Close() {
	s.page.CancelTimeout().Close()
	// A handle with no context id is the browser itself, and Close on it is
	// Browser.close. A session's never has one, but the cost of being wrong in
	// an attached browser is the user's whole Chrome, so it is checked.
	if id := s.ctx.BrowserContextID; id != "" {
		// One that could not be disposed stays registered for Browser.Close.
		if err := s.ctx.Close(); err == nil && s.owner != nil {
			s.owner.owned.dropContext(id)
		}
	}
	for _, cancel := range s.detach {
		cancel()
	}
}

// dropSeed stops the auth storage script from running on later documents.
// Best effort: the page is closing soon either way.
func (s *Session) dropSeed() {
	if s.seed != nil {
		_ = s.seed()
		s.seed = nil
	}
}

// idleWindow is how long the network must be quiet before networkIdle
// readiness completes. Shared by the navigation and step paths so they
// cannot drift.
const idleWindow = 500 * time.Millisecond

// newIdleWaiter subscribes the request monitor immediately (rod enables the
// Network domain and subscribes at call time; events are buffered) and
// returns the function that blocks until the network has been idle.
func (s *Session) newIdleWaiter() func() {
	return s.page.WaitRequestIdle(idleWindow, nil, nil, nil)
}

// finishIdle waits for the load event, then for the already-subscribed idle
// monitor, then settles.
func (s *Session) finishIdle(idle func(), settle time.Duration) error {
	if err := s.waitLoad(); err != nil {
		return fmt.Errorf("waiting for load: %w", err)
	}
	idle()
	if settle > 0 {
		time.Sleep(settle)
	}
	return nil
}

func (s *Session) navigate(url, waitFor string, settle time.Duration) error {
	// For networkIdle readiness the idle monitor must subscribe BEFORE
	// navigation: requests already in flight at the load event (an SPA's
	// initial data fetches, streamed SSR chunks) are invisible to a monitor
	// that subscribes afterwards, which lets a scan run against skeleton DOM.
	// Trade-off: a pre-load request that never completes (long-poll or
	// streaming XHR in place of SSE) now holds readiness until the test
	// timeout and fails loudly, where it previously slipped through ~500ms
	// after load. Pages doing that need a selector waitFor.
	var idle func()
	if waitFor == "" || waitFor == "networkIdle" {
		idle = s.newIdleWaiter()
	}
	if err := s.page.Navigate(url); err != nil {
		if s.redact {
			url = RedactURL(url)
		}
		return fmt.Errorf("navigating to %s: %w", url, err)
	}
	if idle != nil {
		return s.finishIdle(idle, settle)
	}
	return s.WaitReady(waitFor, settle)
}

// followNavigation runs a readiness wait (the load event, a selector),
// starting it over when the page navigates itself mid-wait. A script redirect
// (an auth guard's location.replace, a login form's submit) destroys the
// context the wait runs in, which CDP reports as an error rather than as the
// new page loading; whether the wait began before the navigation is a race
// that slow CI runners lose. The page's own timeout still bounds the whole
// wait, a target that closed or crashed fails at once, and a page that keeps
// navigating itself (a redirect loop, where a wait hangs rather than errors)
// fails once it passes maxSelfNavigations.
func (s *Session) followNavigation(wait func(p *rod.Page) error) error {
	ctx, cancel := context.WithCancel(s.page.GetContext())
	defer cancel()
	p := s.page.Context(ctx)
	var looped atomic.Bool
	hops := 0
	go p.EachEvent(func(e *proto.PageFrameNavigated) bool {
		if e.Frame.ParentID != "" {
			return false
		}
		if hops++; hops > maxSelfNavigations {
			looped.Store(true)
			cancel()
			return true
		}
		return false
	})()
	for {
		err := wait(p)
		if looped.Load() {
			return fmt.Errorf("the page navigated more than %d times on its own while waiting; is it redirecting in a loop?", maxSelfNavigations)
		}
		var cdpErr *cdp.Error
		if err == nil || !errors.As(err, &cdpErr) || !navigatedAway(cdpErr.Message) {
			return err
		}
		// CDP's message is the same for a target that closed or crashed: only
		// one that still exists navigated, and only that is worth waiting on.
		if _, infoErr := (proto.TargetGetTargetInfo{TargetID: s.page.TargetID}).Call(s.page); infoErr != nil {
			return err
		}
		select {
		case <-ctx.Done():
			if looped.Load() {
				return fmt.Errorf("the page navigated more than %d times on its own while waiting; is it redirecting in a loop?", maxSelfNavigations)
			}
			return err
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// maxSelfNavigations bounds followNavigation: well above an SSO flow's hops.
const maxSelfNavigations = 20

func (s *Session) waitLoad() error {
	return s.followNavigation(func(p *rod.Page) error { return p.WaitLoad() })
}

func navigatedAway(msg string) bool {
	for _, m := range []string{"navigated or closed", "Execution context was destroyed", "Cannot find context"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// WaitReady applies the readiness strategy: networkIdle (no in-flight
// requests for 500ms), load, or a CSS selector. When called after an action
// (a waitFor step), the idle monitor can only observe requests still open or
// started after this call — page loads go through navigate, which subscribes
// before navigation.
func (s *Session) WaitReady(waitFor string, settle time.Duration) error {
	switch waitFor {
	case "", "networkIdle":
		if err := s.finishIdle(s.newIdleWaiter(), 0); err != nil {
			return err
		}
	case "load":
		if err := s.waitLoad(); err != nil {
			return fmt.Errorf("waiting for load: %w", err)
		}
	default: // CSS selector
		if err := s.followNavigation(func(p *rod.Page) error {
			_, err := p.Element(waitFor)
			return err
		}); err != nil {
			return fmt.Errorf("waiting for selector %q: %w", waitFor, err)
		}
	}
	if settle > 0 {
		time.Sleep(settle)
	}
	return nil
}

// Step executes one step of the vocabulary. Failures are environment errors
// (exit 3), not violations.
func (s *Session) Step(st config.Step, baseURL, defaultWait string, settle time.Duration) error {
	switch {
	case st.Click != "":
		el, err := s.page.Element(st.Click)
		if err != nil {
			return fmt.Errorf("click %q: %w", st.Click, err)
		}
		return el.Click(proto.InputMouseButtonLeft, 1)
	case len(st.Fill) > 0:
		for _, fv := range st.Fill {
			el, err := s.page.Element(fv.Selector)
			if err != nil {
				return fmt.Errorf("fill %q: %w", fv.Selector, err)
			}
			if err := el.SelectAllText(); err != nil {
				return err
			}
			if err := el.Input(fv.Value); err != nil {
				return fmt.Errorf("fill %q: %w", fv.Selector, err)
			}
		}
		return nil
	case st.Press != "":
		key, err := keyFor(st.Press)
		if err != nil {
			return err
		}
		// Type, not Press: rod's Press sends only keydown, leaving the key
		// held and keyup handlers unfired.
		return s.page.Keyboard.Type(key)
	case st.Hover != "":
		el, err := s.page.Element(st.Hover)
		if err != nil {
			return fmt.Errorf("hover %q: %w", st.Hover, err)
		}
		return el.Hover()
	case len(st.Select) > 0:
		for _, fv := range st.Select {
			sel, val := fv.Selector, fv.Value
			el, err := s.page.Element(sel)
			if err != nil {
				return fmt.Errorf("select %q: %w", sel, err)
			}
			// Match the option's value attribute first (what a config most
			// plausibly specifies), falling back to the visible label. The
			// probe checks getAttribute, not the value property: the property
			// falls back to the option's text when the attribute is absent,
			// which would make the probe and the CSS [value=...] selector
			// disagree on <option>Alaska</option>. Probe via JS so a miss
			// doesn't burn the wait timeout, and fall back to label matching
			// if the CSS select still finds nothing.
			hasValue, err := el.Eval(`(v) => [...this.options].some(o => o.getAttribute('value') === v)`, val)
			if err == nil && hasValue.Value.Bool() {
				err = el.Select([]string{fmt.Sprintf(`[value=%q]`, val)}, true, rod.SelectorTypeCSSSector)
			} else {
				err = fmt.Errorf("no value match")
			}
			if err != nil {
				if err = el.Select([]string{val}, true, rod.SelectorTypeText); err != nil {
					return fmt.Errorf("select %q: %w", sel, err)
				}
			}
		}
		return nil
	case st.WaitFor != "":
		return s.WaitReady(st.WaitFor, 0)
	case st.Wait != 0:
		time.Sleep(st.Wait.Std())
		return nil
	case st.Goto != "":
		return s.navigate(config.JoinURL(baseURL, st.Goto), defaultWait, settle)
	}
	return fmt.Errorf("unhandled step")
}

// CaptureElement screenshots the element at sel, cropped to its bounds, as
// PNG. A short timeout keeps a stale selector from stalling the run; callers
// treat failure as "no screenshot", never as a test failure.
func (s *Session) CaptureElement(sel string) ([]byte, error) {
	page := s.page.Timeout(3 * time.Second)
	el, err := page.Element(sel)
	if err != nil {
		return nil, fmt.Errorf("locating %q: %w", sel, err)
	}
	if err := el.ScrollIntoView(); err != nil {
		return nil, err
	}
	return el.Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
}

// PressTab implements keyboard.Page with real CDP key events, which move
// focus the way a user's Tab does - a synthetic JS event would not. Type
// sends keydown and keyup; rod's Press alone would leave Tab held.
func (s *Session) PressTab() error { return s.page.Keyboard.Type(input.Tab) }

// CaptureRegion implements keyboard.Page: a PNG of a rectangle in document
// coordinates, inside the viewport.
func (s *Session) CaptureRegion(x, y, w, h float64) ([]byte, error) {
	res, err := proto.PageCaptureScreenshot{
		Format: proto.PageCaptureScreenshotFormatPng,
		Clip:   &proto.PageViewport{X: x, Y: y, Width: w, Height: h, Scale: 1},
	}.Call(s.page)
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// AXTree implements screenreader.Page: Chrome's computed accessibility
// tree for the page, the same roles and names assistive technology reads.
func (s *Session) AXTree() ([]*proto.AccessibilityAXNode, error) {
	res, err := proto.AccessibilityGetFullAXTree{}.Call(s.page)
	if err != nil {
		return nil, err
	}
	return res.Nodes, nil
}

// FocusedAX implements screenreader.Page: the accessibility node of the
// element with focus (descending into shadow roots), or nil when focus is on
// no element.
func (s *Session) FocusedAX() (*proto.AccessibilityAXNode, error) {
	res, err := s.page.Evaluate(rod.Eval(`() => {
		let a = document.activeElement;
		while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement;
		return !document.hasFocus() || !a || a === document.body || a === document.documentElement ? null : a;
	}`).ByObject())
	if err != nil {
		return nil, err
	}
	if res.ObjectID == "" {
		return nil, nil
	}
	node, err := proto.DOMDescribeNode{ObjectID: res.ObjectID}.Call(s.page)
	if err != nil {
		return nil, err
	}
	tree, err := proto.AccessibilityGetPartialAXTree{BackendNodeID: node.Node.BackendNodeID}.Call(s.page)
	if err != nil {
		return nil, err
	}
	for _, n := range tree.Nodes {
		if n.BackendDOMNodeID == node.Node.BackendNodeID {
			return n, nil
		}
	}
	return nil, nil
}

// BackendNodeID implements screenreader.Page. A short timeout: a scope that
// matches nothing is a config error to report, not something to wait on.
func (s *Session) BackendNodeID(selector string) (proto.DOMBackendNodeID, error) {
	el, err := s.page.Timeout(3 * time.Second).Element(selector)
	if err != nil {
		return 0, err
	}
	node, err := el.Describe(0, false)
	if err != nil {
		return 0, err
	}
	return node.BackendNodeID, nil
}

// Eval implements engine.Page.
func (s *Session) Eval(js string, args ...any) (json.RawMessage, error) {
	return evalJSON(s.page, js, args...)
}

// InjectScript implements engine.Page, idempotently: engines guard their own
// globals, and axe re-evaluation is harmless but slow, so skip when present.
func (s *Session) InjectScript(src string) error { return inject(s.page, src) }
