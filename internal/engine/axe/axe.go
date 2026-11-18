// Package axe adapts the embedded axe-core scanner to the engine interface.
package axe

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/aquia-inc/frostfall/internal/engine"
)

//go:embed axe.min.js
var axeSource string

var versionRe = regexp.MustCompile(`axe v(\d+\.\d+\.\d+)`)

// Engine runs axe-core inside the page over CDP.
type Engine struct{}

func New() *Engine { return &Engine{} }

func (e *Engine) Name() string { return "axe" }

// Version is parsed from the embedded bundle's banner so it can never drift
// from what actually runs.
func (e *Engine) Version() string {
	if m := versionRe.FindStringSubmatch(axeSource[:200]); m != nil {
		return m[1]
	}
	return "unknown"
}

// standardTags expands a conformance target into axe's cumulative tag sets.
// axe tags rules with only the level that introduced them, so "wcag21aa"
// alone would skip every WCAG 2.0 rule — the tag set must be cumulative.
// section508 pairs the dedicated axe tag with WCAG 2.0 AA, which is what the
// 508 refresh incorporates by reference.
func standardTags(standard string) []string {
	switch standard {
	case "wcag2a":
		return []string{"wcag2a"}
	case "wcag2aa":
		return []string{"wcag2a", "wcag2aa"}
	case "wcag21a":
		return []string{"wcag2a", "wcag21a"}
	case "wcag21aa":
		return []string{"wcag2a", "wcag2aa", "wcag21a", "wcag21aa"}
	case "wcag22aa":
		return []string{"wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"}
	case "section508":
		return []string{"section508", "wcag2a", "wcag2aa"}
	case "best-practice":
		return []string{"best-practice"}
	}
	return nil
}

// axeResult mirrors the subset of axe.run() output we consume.
type axeResult struct {
	Violations []struct {
		ID          string   `json:"id"`
		Impact      string   `json:"impact"`
		Description string   `json:"description"`
		Help        string   `json:"help"`
		HelpURL     string   `json:"helpUrl"`
		Tags        []string `json:"tags"`
		Nodes       []struct {
			Target []json.RawMessage `json:"target"`
			HTML   string            `json:"html"`
		} `json:"nodes"`
	} `json:"violations"`
}

func (e *Engine) Audit(ctx context.Context, page engine.Page, opts engine.ScanOptions) ([]engine.Violation, error) {
	if err := page.InjectScript(axeSource); err != nil {
		return nil, fmt.Errorf("injecting axe: %w", err)
	}

	runOpts := map[string]any{
		"resultTypes": []string{"violations"},
	}
	if tags := standardTags(opts.Standard); tags != nil {
		runOpts["runOnly"] = map[string]any{"type": "tag", "values": tags}
	}
	if len(opts.Rules) > 0 {
		rules := map[string]any{}
		for id, enabled := range opts.Rules {
			rules[id] = map[string]bool{"enabled": enabled}
		}
		runOpts["rules"] = rules
	}
	optsJSON, err := json.Marshal(runOpts)
	if err != nil {
		return nil, err
	}

	scope := "document"
	if opts.Scope != "" {
		scopeJSON, _ := json.Marshal(opts.Scope)
		scope = string(scopeJSON)
	}

	var raw json.RawMessage
	fp, canFrame := page.(engine.FramePage)
	if opts.Frames && canFrame {
		raw, err = e.frameRun(ctx, fp, scope, string(optsJSON), opts.Warn)
	} else {
		raw, err = page.Eval(fmt.Sprintf("() => window.axe.run(%s, %s)", scope, optsJSON))
	}
	if err != nil {
		return nil, fmt.Errorf("running axe: %w", err)
	}

	var res axeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parsing axe results: %w", err)
	}

	var out []engine.Violation
	for _, v := range res.Violations {
		impact := impactOrDefault(v.Impact)
		for _, n := range v.Nodes {
			out = append(out, engine.Violation{
				RuleID:  v.ID,
				Impact:  impact,
				Target:  flattenTarget(n.Target),
				Summary: v.Description,
				Help:    v.Help,
				HelpURL: v.HelpURL,
				HTML:    n.HTML,
				Tags:    v.Tags,
			})
		}
	}
	return out, nil
}

// frameContext is one entry of axe.utils.getFrameContexts: the selector of a
// frame's owner element in its parent, and the axe context to scan the
// frame's document with.
type frameContext struct {
	FrameSelector json.RawMessage `json:"frameSelector"`
	FrameContext  json.RawMessage `json:"frameContext"`
}

// locateFrame resolves an axe frameSelector (a CSS selector, or an array of
// them for a frame inside shadow DOM) in the parent document. The selector
// arrives as a JSON string, so no value is spliced into the script source.
const locateFrame = `(s) => { const el = window.axe.utils.shadowSelect(JSON.parse(s)); return el && /^i?frame$/i.test(el.nodeName) ? el : null }`

// nullPartial stands in for a frame that could not be scanned: finishRun
// skips it, and the frames below it with it.
var nullPartial = json.RawMessage("null")

// frameRun scans the top document and every frame below it with axe's
// partial-run API, the flow @axe-core/playwright uses: runPartial in each
// document, depth first in the order getFrameContexts lists the frames, then
// finishRun in the top document to merge the partials into one result whose
// targets carry the frame path. A page with no frames in scope takes the
// plain axe.run path instead, so its results are exactly what they were
// before frames were supported.
func (e *Engine) frameRun(ctx context.Context, page engine.FramePage, scope, opts string, warn func(string, error)) (json.RawMessage, error) {
	raw, err := page.Eval(fmt.Sprintf("() => window.axe.utils.getFrameContexts(%s, %s)", scope, opts))
	if err != nil {
		return nil, err
	}
	var frames []frameContext
	if err := json.Unmarshal(raw, &frames); err != nil {
		return nil, fmt.Errorf("parsing frame contexts: %w", err)
	}
	if len(frames) == 0 {
		return page.Eval(fmt.Sprintf("() => window.axe.run(%s, %s)", scope, opts))
	}
	top, err := page.Eval(fmt.Sprintf("() => window.axe.runPartial(%s, %s)", scope, opts))
	if err != nil {
		return nil, err
	}
	if !framesMatch(top, frames) {
		// The page's frames changed between the two calls, so the frame
		// list cannot be matched to the frames finishRun will expect. Scan
		// the top document as the plain run does, and report every frame
		// as left out.
		if warn != nil {
			for _, f := range frames {
				warn(flattenTarget([]json.RawMessage{f.FrameSelector}), errFramesChanged)
			}
		}
		return page.Eval(fmt.Sprintf("() => window.axe.run(%s, %s)", scope, opts))
	}
	partials := []json.RawMessage{top}
	for _, f := range frames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		partials = append(partials, e.framePartials(ctx, page, f, nil, opts, warn)...)
	}
	all, err := json.Marshal(partials)
	if err != nil {
		return nil, err
	}
	// The partials go in as one string and are parsed in the page: they can
	// be large, and a string argument is passed through exactly as it is.
	return page.Eval(fmt.Sprintf("(p) => window.axe.finishRun(JSON.parse(p), %s)", opts), string(all))
}

// framePartials returns the partial results for one frame and the frames
// nested in it, in the order finishRun expects: the frame's own partial, then
// each child's, depth first. A frame that cannot be reached or scanned
// contributes exactly one null, which stands for it and everything below it.
// path is the frame selectors above this frame, for the warning.
func (e *Engine) framePartials(ctx context.Context, parent engine.FramePage, f frameContext, path []json.RawMessage, opts string, warn func(string, error)) []json.RawMessage {
	path = append(slices.Clone(path), f.FrameSelector)
	fail := func(err error) []json.RawMessage {
		if warn != nil {
			warn(flattenTarget(path), err)
		}
		return []json.RawMessage{nullPartial}
	}
	frame, err := parent.Frame(locateFrame, f.FrameSelector)
	if err != nil {
		return fail(err)
	}
	if err := frame.InjectScript(axeSource); err != nil {
		return fail(fmt.Errorf("injecting axe: %w", err))
	}
	// The same context and options for both calls, so the frames runPartial
	// records match the order getFrameContexts lists them in.
	context := string(f.FrameContext)
	raw, err := frame.Eval(fmt.Sprintf("(c) => window.axe.utils.getFrameContexts(JSON.parse(c), %s)", opts), context)
	if err != nil {
		return fail(err)
	}
	var children []frameContext
	if err := json.Unmarshal(raw, &children); err != nil {
		return fail(fmt.Errorf("parsing frame contexts: %w", err))
	}
	partial, err := frame.Eval(fmt.Sprintf("(c) => window.axe.runPartial(JSON.parse(c), %s)", opts), context)
	if err != nil {
		return fail(err)
	}
	if !framesMatch(partial, children) {
		return fail(errFramesChanged)
	}
	out := []json.RawMessage{partial}
	for _, c := range children {
		if err := ctx.Err(); err != nil {
			if warn != nil {
				warn(flattenTarget(append(slices.Clone(path), c.FrameSelector)), err)
			}
			out = append(out, nullPartial)
			continue
		}
		out = append(out, e.framePartials(ctx, frame, c, path, opts, warn)...)
	}
	return out
}

// errFramesChanged is why a frame is left out when its document's frames
// changed between getFrameContexts and runPartial.
var errFramesChanged = errors.New("frames changed during the scan")

// framesMatch reports whether a runPartial result recorded as many frames as
// getFrameContexts listed. finishRun matches the partials that follow to the
// partial's own frames list, in order; if the document gained or lost a frame
// between the two calls, the children walked from the other list would be
// filed under the wrong frames.
func framesMatch(partial json.RawMessage, listed []frameContext) bool {
	var p struct {
		Frames []json.RawMessage `json:"frames"`
	}
	if err := json.Unmarshal(partial, &p); err != nil {
		return false
	}
	return len(p.Frames) == len(listed)
}

// impactOrDefault maps an axe impact string to the normalized scale. An
// unknown or null impact defaults to Serious, never below the default
// enforcement floor: if the parser and axe ever disagree, violations must
// fail loud rather than silently slip under the gate.
func impactOrDefault(s string) engine.Impact {
	if impact, ok := engine.ParseImpact(s); ok {
		return impact
	}
	return engine.Serious
}

// flattenTarget joins axe's target array into a single selector path
// string. The array has one element per frame level, the top document first;
// each is a selector, or an array of selectors for a node inside shadow DOM,
// joined with " >>> ". Frame levels are joined with engine.FrameSeparator. A
// node in the top document has one level, and its string is exactly what it
// was before frames were supported.
func flattenTarget(parts []json.RawMessage) string {
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		var s string
		if err := json.Unmarshal(p, &s); err == nil {
			segs = append(segs, s)
			continue
		}
		var nested []string
		if err := json.Unmarshal(p, &nested); err == nil {
			segs = append(segs, strings.Join(nested, " >>> "))
		}
	}
	return strings.Join(segs, engine.FrameSeparator)
}
