// Package keyboard audits keyboard operability by pressing Tab through a page
// and recording where focus goes - the class of WCAG failure axe cannot see
// because it needs interaction. Findings are engine.Violations under the
// frostfall/ rule namespace, so fingerprinting, baselines, SARIF, and
// screenshots treat them exactly like axe results.
package keyboard

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"maps"
	"math"
	"slices"

	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/fingerprint"
)

//go:embed keyboard.js
var pageScript string

// DefaultMaxTabs bounds a traversal. A page that has not cycled back to its
// first tab stop by then is an error rather than a silent partial audit.
const DefaultMaxTabs = 300

// focusPad is how far past an element's box a focus indicator is looked for:
// outlines and rings are commonly drawn outside the border box.
const focusPad = 8

// Page is the browser surface the audit needs: script evaluation plus real
// (CDP-dispatched) key presses and region screenshots.
type Page interface {
	Eval(js string, args ...any) (json.RawMessage, error)
	PressTab() error
	// CaptureRegion returns a PNG of a rectangle in document coordinates
	// that lies inside the viewport.
	CaptureRegion(x, y, w, h float64) ([]byte, error)
}

// Options configures one audit.
type Options struct {
	// Scope limits findings to a subtree; traversal still covers the page,
	// since focus order is a whole-page property.
	Scope string
	// MaxTabs caps the traversal; 0 means DefaultMaxTabs.
	MaxTabs int
	// Rules toggles frostfall/ rules by id; absent means enabled.
	Rules map[string]bool
}

type initResult struct {
	Modal *struct {
		Sel  string `json:"sel"`
		HTML string `json:"html"`
	} `json:"modal"`
	ActiveInModal bool `json:"activeInModal"`
}

type element struct {
	Sel  string `json:"sel"`
	HTML string `json:"html"`
}

type stop struct {
	Out    bool       `json:"out"`
	View   [4]float64 `json:"view"`
	ID     int        `json:"id"`
	Frame  bool       `json:"frame"`
	Sel    string     `json:"sel"`
	HTML   string     `json:"html"`
	Hidden string     `json:"hidden"`
	// Labelled: a hidden stop whose label is visible, so Box is the label's.
	Labelled bool       `json:"labelled"`
	InModal  bool       `json:"inModal"`
	InScope  bool       `json:"inScope"`
	Box      [4]float64 `json:"box"`
}

type audit struct {
	page  Page
	opts  Options
	out   []engine.Violation
	found map[string]bool
}

func (a *audit) enabled(rule string) bool {
	on, set := a.opts.Rules[rule]
	return !set || on
}

func (a *audit) report(rule, target, html, summary string) {
	if !a.enabled(rule) || a.found[rule+"\x00"+target] {
		return
	}
	a.found[rule+"\x00"+target] = true
	a.out = append(a.out, NewViolation(rule, target, html, summary))
}

func (a *audit) call(fn string, out any, args ...any) error {
	raw, err := a.page.Eval(fmt.Sprintf("(...args) => window.__ffkb.%s(...args)", fn), args...)
	if err != nil {
		return fmt.Errorf("%s: %w", fn, err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// region returns a stop's padded rectangle in document pixels, clipped to
// the viewport. It is snapped to whole pixels: Chrome rounds a fractional
// clip differently at the edge of a capture than inside one, and the two
// captures of a stop must crop alike. Nothing outside the viewport is
// captured, because captureBeyondViewport stretches the viewport to the
// page, which moves position: fixed elements out of their own region.
func region(box, view [4]float64) image.Rectangle {
	r := image.Rect(int(math.Floor(box[0]-focusPad)), int(math.Floor(box[1]-focusPad)),
		int(math.Ceil(box[0]+box[2]+focusPad)), int(math.Ceil(box[1]+box[3]+focusPad)))
	return r.Intersect(viewport(view))
}

func viewport(view [4]float64) image.Rectangle {
	return image.Rect(int(math.Ceil(view[0])), int(math.Ceil(view[1])),
		int(math.Floor(view[0]+view[2])), int(math.Floor(view[1]+view[3])))
}

// frame captures at most once per Tab press, so the stop that lost focus and
// the one that gained it are cut from one image: each capture waits on a
// fresh compositor frame, which dominates a traversal's cost. The clip is
// the union of the regions requested, not the whole viewport, which would
// cost more to encode than the second capture it saves.
type frame struct {
	page  Page
	union image.Rectangle
	img   image.Image
}

// edgeMargin keeps every compared pixel off the edge of a capture: Chrome
// renders a clip's edge pixels differently from the same pixels inside a
// larger clip, and a region is edge in one frame and interior in the next.
const edgeMargin = 2

func newFrame(page Page, view [4]float64, regions ...image.Rectangle) *frame {
	f := &frame{page: page}
	for _, r := range regions {
		f.union = f.union.Union(r)
	}
	f.union = f.union.Inset(-edgeMargin).Intersect(viewport(view))
	return f
}

// pixels returns one of the regions newFrame was given.
func (f *frame) pixels(r image.Rectangle) (*image.RGBA, error) {
	if f.img == nil {
		u := f.union
		data, err := f.page.CaptureRegion(float64(u.Min.X), float64(u.Min.Y), float64(u.Dx()), float64(u.Dy()))
		if err != nil {
			return nil, fmt.Errorf("capturing focus region: %w", err)
		}
		if f.img, err = png.Decode(bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("decoding focus region: %w", err)
		}
	}
	r = r.Sub(f.union.Min)
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(dst, dst.Bounds(), f.img, r.Min, draw.Src)
	return dst, nil
}

// A focus indicator is visible when more than noise pixels change by more
// than faint levels in some channel. The count absorbs anti-aliasing that
// re-rasterizes a few pixels between frames (a rounded corner); the level
// absorbs changes nobody can see, like MUI's white Switch ripple over its
// pale track, which moves channels by under 8. A real indicator, even a
// ripple partway into its fade-in, changes dozens of pixels by tens.
const (
	noise = 10
	faint = 12
)

// sameOutside reports whether two captures of one region look the same
// outside mask (in region coordinates).
func sameOutside(a, b *image.RGBA, mask image.Rectangle) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	changed := 0
	for y := range a.Bounds().Dy() {
		for x := range a.Bounds().Dx() {
			if image.Pt(x, y).In(mask) {
				continue
			}
			i := a.PixOffset(x, y)
			for c := range 3 {
				if d := int(a.Pix[i+c]) - int(b.Pix[i+c]); d > faint || d < -faint {
					changed++
					break
				}
			}
			if changed > noise {
				return false
			}
		}
	}
	return true
}

// Result is an audit's outcome.
type Result struct {
	Violations []engine.Violation
	// Unchecked lists rules the audit could not fully evaluate, so a missing
	// finding is not a fixed one: everything but the trap when a trap cut the
	// traversal short, focus-not-visible when a suspect could not be
	// re-checked. Coverage treats them like rules switched off.
	Unchecked []string
}

// Audit presses Tab until focus cycles back to the first element it reached,
// then reports traps, unreachable controls, invisible or hidden focus, and
// modal dialogs that do not hold focus. It leaves focus and scroll where it
// found them.
func Audit(ctx context.Context, page Page, opts Options) (*Result, error) {
	if opts.MaxTabs == 0 {
		opts.MaxTabs = DefaultMaxTabs
	}
	a := &audit{page: page, opts: opts, found: map[string]bool{}}
	if _, err := page.Eval(fmt.Sprintf("() => { %s }", pageScript)); err != nil {
		return nil, fmt.Errorf("injecting keyboard audit: %w", err)
	}
	var init initResult
	if err := a.call("init", &init, opts.Scope, fingerprint.GeneratedIDPattern); err != nil {
		return nil, err
	}
	// Deferred in this order so reenter runs first: finish restores focus
	// with element.focus(), which cannot pull focus back from the browser
	// UI a traversal may have ended on.
	defer a.call("finish", nil)
	defer a.reenter()

	if init.Modal != nil && !init.ActiveInModal {
		a.report(RuleDialog, init.Modal.Sel, init.Modal.HTML,
			"Focus was outside the open modal dialog when the audit began; opening a dialog must move focus into it")
	}
	var modalStops int
	if err := a.call("modalStops", &modalStops); err != nil {
		return nil, err
	}

	// A labelled hidden stop is judged by its label's pixels, like any stop.
	checkVisible := a.enabled(RuleNotVisible)
	checkHidden := a.enabled(RuleHidden)
	reached := map[int]bool{}
	var (
		first int
		// pend is the last stop captured while focused, awaiting its
		// comparison once something else has focus.
		pend       *stop
		pendRegion image.Rectangle
		pendShot   *image.RGBA
		// suspects failed the fast comparison; confirm re-checks them.
		suspects   = map[int]stop{}
		last       stop
		firstStop  stop
		lastID     int // previous in-page stop, in scope or not
		leftPage   bool
		outs       int
		cycleEnded bool
		trapped    bool
	)
	for range opts.MaxTabs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := page.PressTab(); err != nil {
			return nil, fmt.Errorf("pressing Tab: %w", err)
		}
		if checkVisible || checkHidden {
			if err := a.call("settle", nil); err != nil {
				return nil, err
			}
		}
		var cur stop
		if err := a.call("state", &cur); err != nil {
			return nil, err
		}
		// The pending stop is compared only if Tab left it fully in view;
		// one scrolled away cannot be judged, which can only miss a finding.
		comparing := pend != nil && !cur.Out && cur.ID != pend.ID &&
			pendRegion.In(viewport(cur.View))
		var curRegion image.Rectangle
		if !cur.Out {
			curRegion = region(cur.Box, cur.View)
		}
		var regions []image.Rectangle
		if comparing {
			regions = append(regions, pendRegion)
		}
		regions = append(regions, curRegion)
		shots := newFrame(page, cur.View, regions...)

		// The pending stop has now lost focus: if it looks the same as it
		// did while focused, nothing showed the user where focus was. The
		// newly focused element's region is masked out, or an adjacent
		// control's ring would read as this one's. Not compared while focus
		// is outside the page, since Chrome does not promptly repaint a page
		// that lost focus to the browser, nor when Tab did not move focus.
		if comparing {
			now, err := shots.pixels(pendRegion)
			if err != nil {
				return nil, err
			}
			mask := curRegion.Intersect(pendRegion).Sub(pendRegion.Min)
			if sameOutside(pendShot, now, mask) {
				suspects[pend.ID] = *pend
			}
		}
		// Tab came straight back to the pending stop (a page or modal with one
		// tab stop): there was never a moment to compare against, so the
		// confirming lap, which blurs instead, judges it.
		if pend != nil && !cur.Out && cur.ID == pend.ID {
			suspects[pend.ID] = *pend
		}
		if pend != nil && !cur.Out {
			pend, pendShot = nil, nil
		}

		if cur.Out {
			// Leaving the page twice in a row means it has no tab stops.
			if outs++; outs == 2 {
				cycleEnded = true
				break
			}
			lastID, leftPage = 0, true
			continue
		}
		outs = 0
		// Tabbing inside an iframe keeps the iframe as the top document's
		// active element; its contents are out of scope.
		if cur.Frame {
			reached[cur.ID] = true
			continue
		}
		last = cur
		if cur.ID == lastID {
			// A modal with a single tab stop legitimately keeps it.
			if init.Modal != nil && cur.InModal && modalStops <= 1 {
				cycleEnded = true
				break
			}
			trapped = true
			if cur.InScope {
				a.report(RuleTrap, cur.Sel, cur.HTML, "Pressing Tab did not move focus off this element")
			}
			break
		}
		lastID = cur.ID
		if first == 0 {
			first, firstStop = cur.ID, cur
		} else if cur.ID == first {
			cycleEnded = true
			break
		} else if reached[cur.ID] {
			trapped = true
			if cur.InScope {
				a.report(RuleTrap, cur.Sel, cur.HTML,
					"Focus cycles through a subset of the page and never returns to where Tab navigation began")
			}
			break
		}
		reached[cur.ID] = true

		if init.Modal != nil && !cur.InModal {
			a.report(RuleDialog, init.Modal.Sel, init.Modal.HTML,
				"Pressing Tab moved focus out of the open modal dialog to "+cur.Sel)
		}
		if !cur.InScope {
			continue
		}
		// A label off screen cannot show focus either, and has no pixels to judge.
		if cur.Hidden != "" && (!cur.Labelled || curRegion.Empty()) {
			a.report(RuleHidden, cur.Sel, cur.HTML, "Focus landed on this element, but "+cur.Hidden)
			continue
		}
		judge := checkVisible
		if cur.Hidden != "" {
			judge = checkHidden
		}
		if judge && !curRegion.Empty() {
			shot, err := shots.pixels(curRegion)
			if err != nil {
				return nil, err
			}
			focused := cur
			pend, pendRegion, pendShot = &focused, curRegion, shot
		}
	}
	if !cycleEnded && !trapped {
		return nil, fmt.Errorf("focus did not cycle back to its first stop within %d Tab presses; raise maxTabs", opts.MaxTabs)
	}

	// Unreachable is only meaningful after a complete cycle: after a trap,
	// everything past it is unreachable as a consequence, not a separate bug.
	if cycleEnded {
		ids := make([]int, 0, len(reached))
		for id := range reached {
			ids = append(ids, id)
		}
		var missed []element
		if err := a.call("unreached", &missed, ids); err != nil {
			return nil, err
		}
		// Focus came back to the first stop without ever leaving the page:
		// script is wrapping it. Harmless while it reaches everything; with
		// controls left out it is a trap, and they are its consequence.
		if init.Modal == nil && !leftPage && first != 0 && len(missed) > 0 {
			if firstStop.InScope {
				a.report(RuleTrap, firstStop.Sel, firstStop.HTML,
					"Focus cycles through a subset of the page and never returns to where Tab navigation began")
			}
			missed = nil
		}
		for _, m := range missed {
			a.report(RuleUnreachable, m.Sel, m.HTML, "")
		}
	}
	res := &Result{Violations: a.out}
	if !cycleEnded {
		// The traversal stopped at a trap: stops past it were never
		// visited and the suspects before it go unconfirmed.
		for id := range rules {
			if id != RuleTrap && id != RuleFocusExpectation {
				res.Unchecked = append(res.Unchecked, id)
			}
		}
		return res, nil
	}
	// Last: the confirming lap can re-render what it scrolls past (a
	// virtualized grid's rows), which would make reached elements look new.
	if len(suspects) > 0 {
		unjudged, err := a.confirm(ctx, last, suspects)
		if err != nil {
			return nil, err
		}
		res.Violations = a.out
		res.Unchecked = append(res.Unchecked, unjudged...)
	}
	return res, nil
}

// confirmDelay is how long confirm gives a suspect to show, and then drop,
// its indicator. The fast pass waits only for effects already under way;
// some start later (MUI's DataGrid moves focus into a row asynchronously,
// so the button's ripple mounts after settle has seen a quiet frame).
const confirmDelay = 400

// confirm makes a second lap from cur, the first stop, and re-compares each
// suspect focused and then blurred, with confirmDelay on both sides, reporting
// only those that still look unchanged. A suspect the lap cannot judge (a
// virtualized row re-rendered, focus moved on or was pulled back by itself) is
// dropped, since a gate must not fail on a guess, and its rule is returned as
// unchecked: not fully evaluated, so a missing finding must not read as fixed.
func (a *audit) confirm(ctx context.Context, cur stop, suspects map[int]stop) (unchecked []string, err error) {
	first := cur.ID
	unjudged := map[string]bool{}
	skip := func(s stop) { unjudged[ruleOf(s)] = true }
	done := func() []string {
		for _, s := range suspects {
			skip(s)
		}
		return slices.Sorted(maps.Keys(unjudged))
	}
	// next presses Tab, without settling: getting from one suspect to the next
	// needs none.
	next := func() (stop, error) {
		var st stop
		if err := a.page.PressTab(); err != nil {
			return st, fmt.Errorf("pressing Tab: %w", err)
		}
		return st, a.call("state", &st)
	}
	for range a.opts.MaxTabs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s, ok := suspects[cur.ID]
		if !ok || cur.Out {
			if cur, err = next(); err != nil {
				return nil, err
			}
			// Back at the start with suspects left: the lap could not find
			// them again.
			if !cur.Out && cur.ID == first {
				return done(), nil
			}
			continue
		}
		delete(suspects, cur.ID)
		if err := a.call("settle", nil, confirmDelay); err != nil {
			return nil, err
		}
		if err := a.call("state", &cur); err != nil {
			return nil, err
		}
		// Focus may have moved on by itself during the wait (a grid moving
		// it into a row): then this capture is not the suspect's to judge.
		if cur.Out || cur.ID != s.ID {
			skip(s)
			continue
		}
		before := region(cur.Box, cur.View)
		focused, err := newFrame(a.page, cur.View, before).pixels(before)
		if err != nil {
			return nil, err
		}
		// Blur rather than Tab to the next stop: the neighbour's ring, and the
		// mask that hides it, can cover most of a tiny element's region (an 8px
		// chip in a zoomed-out diagram). The page keeps focus, so it repaints.
		if err := a.call("blurActive", nil); err != nil {
			return nil, err
		}
		if err := a.call("settle", nil, confirmDelay); err != nil {
			return nil, err
		}
		var after stop
		if err := a.call("state", &after); err != nil {
			return nil, err
		}
		// Focus is on an element again: a focus trap (MUI's, focus-trap's)
		// pulled it back, so the capture would show a ring either way.
		if !after.Out || !before.In(viewport(after.View)) {
			skip(s)
		} else {
			blurred, err := newFrame(a.page, after.View, before).pixels(before)
			if err != nil {
				return nil, err
			}
			if sameOutside(focused, blurred, image.Rectangle{}) {
				if s.Hidden != "" {
					a.report(RuleHidden, s.Sel, s.HTML,
						"Focus landed on this element, but "+s.Hidden+", and its label does not show focus either")
				} else {
					a.report(RuleNotVisible, s.Sel, s.HTML, "")
				}
			}
		}
		// Tab from where the blurred stop was continues the lap.
		if cur, err = next(); err != nil {
			return nil, err
		}
		if len(suspects) == 0 || (!cur.Out && cur.ID == first) {
			return done(), nil
		}
	}
	return done(), nil
}

// ruleOf is the visibility rule a suspect is judged under.
func ruleOf(s stop) string {
	if s.Hidden != "" {
		return RuleHidden
	}
	return RuleNotVisible
}

// reenter gives the page focus back if the audit left it in the browser UI
// (a page with no tab stops, a lap that ended off the page). Tab cannot be
// relied on to do it: on Linux Chrome a page with no tab stops never gets
// focus back from Tab, and CDP's Target.activateTarget does not restore it
// either; window.focus() does on every platform.
func (a *audit) reenter() {
	a.page.Eval(`() => { if (!document.hasFocus()) window.focus() }`)
}
