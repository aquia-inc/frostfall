// Package browser wraps go-rod: browser lifecycle, per-test page sessions,
// readiness waits, and the step vocabulary.
package browser

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/config"
)

// Browser owns one Chromium process; each session gets its own incognito
// context so cookies, storage, and service workers never leak between tests.
type Browser struct {
	rod      *rod.Browser
	launcher *launcher.Launcher
}

// Launch finds a system Chrome (GitHub runners and most dev machines have
// one) or lets rod's launcher download a managed Chromium, then connects.
func Launch(browserPath string) (*Browser, error) {
	l := launcher.New().Headless(true)
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

func (b *Browser) Close() {
	b.rod.Close()
	b.launcher.Cleanup()
}

// Session is one test's page in its own incognito browser context.
// It implements engine.Page.
type Session struct {
	page    *rod.Page
	ctx     *rod.Browser
	timeout time.Duration
}

// NewSession opens a page at url in a fresh incognito context and waits for
// per-page readiness.
func (b *Browser) NewSession(url string, vp config.Viewport, waitFor string, settle, timeout time.Duration) (*Session, error) {
	ctx, err := b.rod.Incognito()
	if err != nil {
		return nil, fmt.Errorf("creating browser context: %w", err)
	}
	page, err := ctx.Page(proto.TargetCreateTarget{URL: ""})
	if err != nil {
		return nil, err
	}
	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: vp.Width, Height: vp.Height, DeviceScaleFactor: 1,
	}); err != nil {
		page.Close()
		return nil, err
	}
	s := &Session{page: page.Timeout(timeout), ctx: ctx, timeout: timeout}
	if err := s.navigate(url, waitFor, settle); err != nil {
		page.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) Close() { s.page.CancelTimeout().Close() }

func (s *Session) navigate(url, waitFor string, settle time.Duration) error {
	if err := s.page.Navigate(url); err != nil {
		return fmt.Errorf("navigating to %s: %w", url, err)
	}
	return s.WaitReady(waitFor, settle)
}

// WaitReady applies the readiness strategy: networkIdle (no in-flight
// requests for 500ms), load, or a CSS selector.
func (s *Session) WaitReady(waitFor string, settle time.Duration) error {
	switch waitFor {
	case "", "networkIdle":
		if err := s.page.WaitLoad(); err != nil {
			return fmt.Errorf("waiting for load: %w", err)
		}
		s.page.WaitRequestIdle(500*time.Millisecond, nil, nil, nil)()
	case "load":
		if err := s.page.WaitLoad(); err != nil {
			return fmt.Errorf("waiting for load: %w", err)
		}
	default: // CSS selector
		if _, err := s.page.Element(waitFor); err != nil {
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
		for sel, val := range st.Fill {
			el, err := s.page.Element(sel)
			if err != nil {
				return fmt.Errorf("fill %q: %w", sel, err)
			}
			if err := el.SelectAllText(); err != nil {
				return err
			}
			if err := el.Input(val); err != nil {
				return fmt.Errorf("fill %q: %w", sel, err)
			}
		}
		return nil
	case st.Press != "":
		return s.page.Keyboard.Press(keyFor(st.Press))
	case st.Hover != "":
		el, err := s.page.Element(st.Hover)
		if err != nil {
			return fmt.Errorf("hover %q: %w", st.Hover, err)
		}
		return el.Hover()
	case len(st.Select) > 0:
		for sel, val := range st.Select {
			el, err := s.page.Element(sel)
			if err != nil {
				return fmt.Errorf("select %q: %w", sel, err)
			}
			if err := el.Select([]string{val}, true, rod.SelectorTypeText); err != nil {
				return fmt.Errorf("select %q: %w", sel, err)
			}
		}
		return nil
	case st.WaitFor != "":
		return s.WaitReady(st.WaitFor, 0)
	case st.Wait != 0:
		time.Sleep(st.Wait.Std())
		return nil
	case st.Goto != "":
		return s.navigate(baseURL+st.Goto, defaultWait, settle)
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

// Eval implements engine.Page.
func (s *Session) Eval(js string, args ...any) (json.RawMessage, error) {
	res, err := s.page.Eval(js, args...)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(res.Value.JSON("", "")), nil
}

// InjectScript implements engine.Page, idempotently: engines guard their own
// globals, and axe re-evaluation is harmless but slow, so skip when present.
func (s *Session) InjectScript(src string) error {
	present, err := s.page.Eval("() => !!window.axe")
	if err == nil && present.Value.Bool() {
		return nil
	}
	_, err = s.page.Eval(src)
	if err != nil {
		// axe.min.js is a bare script, not an expression; evaluate via a
		// function wrapper.
		_, err = s.page.Eval(fmt.Sprintf("() => { %s }", src))
	}
	return err
}
