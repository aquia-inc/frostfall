package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/engine"
)

// frameTimeout bounds each frame's share of a scan: locating its children,
// injecting the engine, and running it in the frame's document. It is cut
// short by the test's own budget, never extends it. A frame that runs out of
// time is reported and left out; the rest of the page is still scanned.
//
// It only rescues the scan from a frame in another renderer process. A frame
// in the page's own process shares its main thread, so a frame hung there
// hangs the page too, and the test's budget is what ends it. A variable only
// so tests can shorten it.
var frameTimeout = 10 * time.Second

// framePage is one frame's document, reached from a Session. It implements
// engine.FramePage.
type framePage struct {
	s    *Session
	page *rod.Page
}

// Every call into a frame page is guarded (see guard): a frame can leave the
// page between two calls.
func (f *framePage) Eval(js string, args ...any) (res json.RawMessage, err error) {
	defer guard(&err)
	return evalJSON(f.page, js, args...)
}

func (f *framePage) InjectScript(src string) (err error) {
	defer guard(&err)
	return inject(f.page, src)
}

func (f *framePage) Frame(locate string, arg json.RawMessage) (child engine.FramePage, err error) {
	defer guard(&err)
	return f.s.frame(f.page, locate, arg)
}

// errFrameGone is what a call into a frame whose element left the page
// returns in place of rod's panic.
var errFrameGone = errors.New("the frame left the page during the scan")

// guard turns a panic in a call into a frame page into an error, so the frame
// is left out of the scan rather than crashing the run. rod's in-process frame
// page re-reads its owner element's content document whenever it has no
// cached window (first use, and after the frame's context went away) and
// dereferences it unchecked; once the iframe is removed from the page there
// is no content document, and that is a nil dereference.
func guard(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%w (%v)", errFrameGone, r)
	}
}

// Frame implements engine.FramePage for the top document.
func (s *Session) Frame(locate string, arg json.RawMessage) (engine.FramePage, error) {
	return s.frame(s.page, locate, arg)
}

// frame finds a child frame of parent and returns its document, under a
// deadline of its own. Which way the document is reached is decided from the
// owner element before anything is evaluated in the frame: rod's frame page
// for a frame in this process assumes the element has a content document,
// and evaluating in one that has none (an out-of-process frame) panics.
//
//   - A frame in this renderer process: its owner element has a content
//     document, and rod's Element.Frame reaches it.
//   - An out-of-process frame (a cross-site frame under site isolation): no
//     content document here; it is its own CDP target, whose id is the frame
//     id, and it is attached to as one.
func (s *Session) frame(parent *rod.Page, locate string, arg json.RawMessage) (engine.FramePage, error) {
	// Locating the child is the child's work, on a deadline of its own: the
	// parent's started when the parent was opened, and scanning earlier
	// siblings' subtrees may have used it up.
	parent = s.frameDeadline(parent)
	// Evaluate, not ElementByJS: that retries a null until the deadline,
	// and a frame that has left the page will not come back.
	res, err := parent.Evaluate(rod.Eval(locate, string(arg)).ByObject())
	if err != nil {
		return nil, fmt.Errorf("locating the frame: %w", err)
	}
	if res.ObjectID == "" {
		return nil, errors.New("the frame's element is no longer in the page")
	}
	el, err := parent.ElementFromObject(res)
	if err != nil {
		return nil, fmt.Errorf("locating the frame: %w", err)
	}
	node, err := el.Describe(1, false)
	if err != nil {
		return nil, fmt.Errorf("describing the frame: %w", err)
	}
	if node.ContentDocument != nil {
		// rod's frame page keeps the owner element and resolves the frame's
		// window through it whenever it has none cached (first use, or after
		// the frame navigated), so the element carries the test's budget,
		// not this locate's short deadline.
		p, err := el.Context(s.page.GetContext()).Frame()
		if err != nil {
			return nil, err
		}
		return &framePage{s: s, page: s.frameDeadline(p)}, nil
	}
	if node.FrameID == "" {
		return nil, errors.New("the frame has no document")
	}
	p, err := s.attachFrame(proto.TargetTargetID(node.FrameID))
	if err != nil {
		return nil, err
	}
	return &framePage{s: s, page: s.frameDeadline(p)}, nil
}

// frameDeadline puts p under the test's budget, cut to frameTimeout.
func (s *Session) frameDeadline(p *rod.Page) *rod.Page {
	return p.Context(s.page.GetContext()).Timeout(frameTimeout)
}

// attachFrame attaches to an out-of-process frame's target.
//
// rod's PageFromTarget holds a lock shared by every page of the browser
// (Page.Close takes it too) while it enables the Page domain, which waits on
// the frame's renderer. A hung renderer would hold that lock, and with it
// every later Close, for good. So the attach runs on a context of its own,
// cancelled if it outlives the frame's deadline, which releases the lock.
// Otherwise that context lives as long as the session: rod caches the page
// it attached, with a session context derived from it, for later scans of
// the same frame.
//
// rod also emulates its default device on every page it attaches, and Chrome
// refuses device emulation on anything but a top-level target, so the attach
// goes through a copy of the session's browser handle with no default device.
func (s *Session) attachFrame(id proto.TargetTargetID) (*rod.Page, error) {
	deadline, stop := context.WithTimeout(s.page.GetContext(), frameTimeout)
	defer stop()
	attachCtx, cancel := context.WithCancel(s.raw.GetContext())
	type attached struct {
		page *rod.Page
		err  error
	}
	done := make(chan attached, 1)
	go func() {
		p, err := s.ctx.Context(attachCtx).NoDefaultDevice().PageFromTarget(id)
		done <- attached{p, err}
	}()
	select {
	case a := <-done:
		if a.err != nil {
			cancel()
			return nil, fmt.Errorf("attaching to the out-of-process frame: %w", a.err)
		}
		s.detach = append(s.detach, cancel)
		s.outOfProcess++
		return a.page, nil
	case <-deadline.Done():
		cancel()
		return nil, fmt.Errorf("attaching to the out-of-process frame: %w", deadline.Err())
	}
}

// evalJSON evaluates js in p and returns its result as JSON.
func evalJSON(p *rod.Page, js string, args ...any) (json.RawMessage, error) {
	res, err := p.Eval(js, args...)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(res.Value.JSON("", "")), nil
}

// inject evaluates a script source in p, idempotently: engines guard their
// own globals, and axe re-evaluation is harmless but slow, so skip when
// present.
func inject(p *rod.Page, src string) error {
	present, err := p.Eval("() => !!window.axe")
	if err == nil && present.Value.Bool() {
		return nil
	}
	_, err = p.Eval(src)
	if err != nil {
		// axe.min.js is a bare script, not an expression; evaluate via a
		// function wrapper.
		_, err = p.Eval(fmt.Sprintf("() => { %s }", src))
	}
	return err
}
