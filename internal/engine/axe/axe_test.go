package axe

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aquia-inc/frostfall/internal/engine"
)

func TestImpactOrDefault(t *testing.T) {
	cases := map[string]engine.Impact{
		"minor":    engine.Minor,
		"moderate": engine.Moderate,
		"serious":  engine.Serious,
		"critical": engine.Critical,
		// Unknown and empty impacts must land AT the default enforcement
		// floor, not below it - axe emitting a value we don't recognize has
		// to fail builds, not silently pass them (issue #7).
		"":         engine.Serious,
		"severe":   engine.Serious,
		"CRITICAL": engine.Serious,
	}
	for in, want := range cases {
		if got := impactOrDefault(in); got != want {
			t.Errorf("impactOrDefault(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestVersionParsedFromBundle(t *testing.T) {
	if v := New().Version(); v == "unknown" || v == "" {
		t.Errorf("embedded axe version not parsed: %q", v)
	}
}

func TestFlattenTarget(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// One level, the top document: unchanged from before frames.
		{`["#app > img"]`, "#app > img"},
		{`[["my-app", "button"]]`, "my-app >>> button"},
		// Frame levels join with the frame separator.
		{`["#pay", "#card > input"]`, "#pay |> #card > input"},
		{`["#outer", "iframe.inner", "img"]`, "#outer |> iframe.inner |> img"},
		// Shadow DOM inside a frame level keeps its own separator.
		{`[["my-app", "iframe"], "main > img"]`, "my-app >>> iframe |> main > img"},
	}
	for _, c := range cases {
		var parts []json.RawMessage
		if err := json.Unmarshal([]byte(c.in), &parts); err != nil {
			t.Fatal(err)
		}
		if got := flattenTarget(parts); got != c.want {
			t.Errorf("flattenTarget(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeFramePage answers the engine's scripts by what they call, records the
// partials handed to finishRun, and has frames that may fail to open.
type fakeFramePage struct {
	frames    string // getFrameContexts result
	partial   string // runPartial result
	frameErr  error  // Frame fails with this when set
	onFrame   func() // runs on each Frame call
	child     *fakeFramePage
	runCalled bool
	finished  string // the partials finishRun received
}

func (f *fakeFramePage) Eval(js string, args ...any) (json.RawMessage, error) {
	switch {
	case strings.Contains(js, "getFrameContexts"):
		return json.RawMessage(f.frames), nil
	case strings.Contains(js, "runPartial"):
		return json.RawMessage(f.partial), nil
	case strings.Contains(js, "finishRun"):
		f.finished = args[0].(string)
		return json.RawMessage(`{"violations":[{"id":"image-alt","impact":"critical","nodes":[{"target":["#f","img"],"html":"<img>"}]}]}`), nil
	case strings.Contains(js, "axe.run("):
		f.runCalled = true
		return json.RawMessage(`{"violations":[]}`), nil
	}
	return nil, errors.New("unexpected script: " + js)
}

func (f *fakeFramePage) InjectScript(string) error { return nil }

func (f *fakeFramePage) Frame(string, json.RawMessage) (engine.FramePage, error) {
	if f.onFrame != nil {
		f.onFrame()
	}
	if f.frameErr != nil {
		return nil, f.frameErr
	}
	return f.child, nil
}

func TestUnreachableFrameIsOneNullPartial(t *testing.T) {
	top := &fakeFramePage{
		frames:   `[{"frameSelector":"#f[src=\"https://idp.example/x?t=1\"]","frameContext":{"include":[],"exclude":[]}},{"frameSelector":"#ok","frameContext":{"include":[],"exclude":[]}}]`,
		partial:  `{"top":true,"frames":[{},{}]}`,
		frameErr: errors.New("frame gone"),
	}
	var warned []string
	vs, err := New().Audit(context.Background(), top, engine.ScanOptions{
		Frames: true,
		Warn:   func(frame string, err error) { warned = append(warned, frame+": "+err.Error()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Each of the two frames failed to open: one null each, after the top's
	// own partial, and one warning naming each frame.
	if top.finished != `[{"top":true,"frames":[{},{}]},null,null]` {
		t.Errorf("finishRun got %s, want the top partial then one null per frame", top.finished)
	}
	if len(warned) != 2 || !strings.HasPrefix(warned[0], `#f[src="https://idp.example/x?t=1"]: frame gone`) {
		t.Errorf("warnings = %q", warned)
	}
	if len(vs) != 1 || vs[0].Target != "#f |> img" {
		t.Errorf("violations = %+v, want one with a frame-qualified target", vs)
	}
}

func TestNestedFramesDepthFirst(t *testing.T) {
	grandchild := &fakeFramePage{partial: `{"gc":1,"frames":[{}]}`}
	child := &fakeFramePage{
		frames:  `[{"frameSelector":"#gc","frameContext":{}},{"frameSelector":"#gone","frameContext":{}}]`,
		partial: `{"child":1,"frames":[{},{}]}`,
		child:   grandchild,
	}
	// The child fake hands out the same grandchild page for both of its
	// frames, and that page's own frame (#deep) cannot be opened. Each
	// grandchild visit therefore yields its partial and then one null for
	// #deep, in depth-first order.
	top := &fakeFramePage{frames: `[{"frameSelector":"#c","frameContext":{}}]`, partial: `{"top":1,"frames":[{}]}`, child: child}
	grandchild.frames = `[{"frameSelector":"#deep","frameContext":{}}]`
	grandchild.frameErr = errors.New("unreachable")
	var warned []string
	if _, err := New().Audit(context.Background(), top, engine.ScanOptions{
		Frames: true,
		Warn:   func(frame string, err error) { warned = append(warned, frame) },
	}); err != nil {
		t.Fatal(err)
	}
	want := `[{"top":1,"frames":[{}]},{"child":1,"frames":[{},{}]},{"gc":1,"frames":[{}]},null,{"gc":1,"frames":[{}]},null]`
	if top.finished != want {
		t.Errorf("finishRun got %s, want %s", top.finished, want)
	}
	if len(warned) != 2 || warned[0] != "#c |> #gc |> #deep" || warned[1] != "#c |> #gone |> #deep" {
		t.Errorf("warnings = %q, want the frame paths", warned)
	}
}

func TestNoFramesTakesPlainRun(t *testing.T) {
	for _, frames := range []bool{true, false} {
		top := &fakeFramePage{frames: `[]`}
		if _, err := New().Audit(context.Background(), top, engine.ScanOptions{Frames: frames}); err != nil {
			t.Fatal(err)
		}
		if !top.runCalled || top.finished != "" {
			t.Errorf("frames=%v: a page with no frames must take the plain axe.run path", frames)
		}
	}
	// Frames off never asks about frames, even when there are some.
	top := &fakeFramePage{frames: `[{"frameSelector":"#f","frameContext":{}}]`}
	if _, err := New().Audit(context.Background(), top, engine.ScanOptions{Frames: false}); err != nil {
		t.Fatal(err)
	}
	if !top.runCalled || top.finished != "" {
		t.Errorf("frames: false must take the plain axe.run path")
	}
}

// If a document's frames change between getFrameContexts and runPartial, the
// two lists no longer line up, and walking one under the other would file
// partials under the wrong frames. In a frame, that frame becomes one null
// and a warning; in the top document, the scan falls back to the plain run
// and every frame is reported as left out.
func TestFramesChangedDuringScan(t *testing.T) {
	// A child lists two frames but its partial recorded one.
	child := &fakeFramePage{
		frames:  `[{"frameSelector":"#a","frameContext":{}},{"frameSelector":"#b","frameContext":{}}]`,
		partial: `{"child":1,"frames":[{}]}`,
	}
	top := &fakeFramePage{frames: `[{"frameSelector":"#c","frameContext":{}}]`, partial: `{"top":1,"frames":[{}]}`, child: child}
	var warned []string
	if _, err := New().Audit(context.Background(), top, engine.ScanOptions{
		Frames: true,
		Warn:   func(frame string, err error) { warned = append(warned, frame+": "+err.Error()) },
	}); err != nil {
		t.Fatal(err)
	}
	if top.finished != `[{"top":1,"frames":[{}]},null]` {
		t.Errorf("finishRun got %s, want the changed frame as one null", top.finished)
	}
	if len(warned) != 1 || warned[0] != "#c: frames changed during the scan" {
		t.Errorf("warnings = %q", warned)
	}

	// The top document lists two frames but its partial recorded three.
	top = &fakeFramePage{
		frames:  `[{"frameSelector":"#x","frameContext":{}},{"frameSelector":"#y","frameContext":{}}]`,
		partial: `{"top":1,"frames":[{},{},{}]}`,
		child:   child,
	}
	warned = nil
	if _, err := New().Audit(context.Background(), top, engine.ScanOptions{
		Frames: true,
		Warn:   func(frame string, err error) { warned = append(warned, frame+": "+err.Error()) },
	}); err != nil {
		t.Fatal(err)
	}
	if !top.runCalled || top.finished != "" {
		t.Errorf("a changed top document must fall back to the plain run (ran=%v, finished=%q)", top.runCalled, top.finished)
	}
	if !slices.Equal(warned, []string{"#x: frames changed during the scan", "#y: frames changed during the scan"}) {
		t.Errorf("warnings = %q, want every frame", warned)
	}
}

// When the scan's context ends partway, the frames not yet walked are nulls,
// and each is warned about, so the caller counts the scan as partial.
func TestCancelledScanWarnsForRemainingFrames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	grandchild := &fakeFramePage{frames: `[]`, partial: `{"gc":1,"frames":[]}`}
	child := &fakeFramePage{
		frames:  `[{"frameSelector":"#first","frameContext":{}},{"frameSelector":"#second","frameContext":{}}]`,
		partial: `{"child":1,"frames":[{},{}]}`,
		child:   grandchild,
		onFrame: cancel, // the context ends while #first is opened
	}
	top := &fakeFramePage{frames: `[{"frameSelector":"#c","frameContext":{}}]`, partial: `{"top":1,"frames":[{}]}`, child: child}
	var warned []string
	_, _ = New().Audit(ctx, top, engine.ScanOptions{
		Frames: true,
		Warn:   func(frame string, err error) { warned = append(warned, frame) },
	})
	if !slices.Equal(warned, []string{"#c |> #second"}) {
		t.Errorf("warnings = %q, want the frame skipped after the cancel", warned)
	}
}
