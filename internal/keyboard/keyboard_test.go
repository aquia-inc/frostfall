package keyboard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/keyboard"
)

// audit serves body as a page, runs the audit, and returns rule id -> targets.
func audit(t *testing.T, body string, opts keyboard.Options) map[string][]string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html lang="en"><head><title>t</title></head><body>` + body + `</body></html>`))
	}))
	t.Cleanup(srv.Close)

	b, err := browser.Launch("")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(b.Close)
	s, err := b.NewSession(srv.URL, config.Viewport{Width: 800, Height: 600}, "load", 0, 20*time.Second)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(s.Close)

	vs, err := keyboard.Audit(context.Background(), s, opts)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	got := map[string][]string{}
	for _, v := range vs {
		got[v.RuleID] = append(got[v.RuleID], v.Target)
	}
	return got
}

func TestKeyboardAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	cases := []struct {
		name string
		body string
		opts keyboard.Options
		want map[string][]string
	}{
		{
			name: "clean page",
			// Focus-trap sentinels are 0x0 tabindex=0 divs that hand focus on
			// at once; a radio group is one tab stop however many members it has.
			body: `<div tabindex="0" data-testid="sentinelStart" onfocus="link.focus()"></div>
				<a href="#x" id="link">link</a> <button id="ok">ok</button>
				<input type="radio" name="r" id="r1"><input type="radio" name="r" id="r2">
				<input type="text" id="txt">`,
			want: map[string][]string{},
		},
		{
			name: "no page tab stops",
			body: `<p>nothing to focus</p>`,
			want: map[string][]string{},
		},
		{
			name: "outline removed",
			body: `<style>#bare:focus { outline: none }</style>
				<button id="ok">ok</button> <button id="bare">bare</button>`,
			want: map[string][]string{keyboard.RuleNotVisible: {"#bare"}},
		},
		{
			// The neighbours' rings fall inside #bare's capture region; they
			// must not pass for a ring of its own.
			name: "outline removed between adjacent controls",
			body: `<style>#bare:focus { outline: none }</style>
				<button id="a">a</button><button id="bare">bare</button><button id="c">c</button>`,
			want: map[string][]string{keyboard.RuleNotVisible: {"#bare"}},
		},
		{
			name: "outline removed, rule off",
			body: `<style>#bare:focus { outline: none }</style><button id="bare">bare</button>`,
			opts: keyboard.Options{Rules: map[string]bool{keyboard.RuleNotVisible: false}},
			want: map[string][]string{},
		},
		{
			name: "focus lands on clipped element",
			body: `<button id="ok">ok</button>
				<a href="#main" id="skip" style="position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0)">skip</a>`,
			want: map[string][]string{keyboard.RuleHidden: {"#skip"}},
		},
		{
			name: "Tab swallowed",
			body: `<button id="ok">ok</button><input id="eat"><button id="after">after</button>
				<script>eat.addEventListener('keydown', e => { if (e.key === 'Tab') e.preventDefault() })</script>`,
			want: map[string][]string{keyboard.RuleTrap: {"#eat"}},
		},
		{
			name: "focus cycles a subset",
			body: `<button id="a">a</button><button id="b">b</button><button id="c">c</button><button id="d">d</button>
				<script>c.addEventListener('keydown', e => { if (e.key === 'Tab') { e.preventDefault(); b.focus() } })</script>`,
			want: map[string][]string{keyboard.RuleTrap: {"#b"}},
		},
		{
			// A cookie banner first in the DOM that wraps Tab between its own
			// buttons: one trap, not a list of everything it shuts out.
			name: "trap includes the first stop",
			body: `<div id="banner"><button id="a">a</button><button id="b">b</button></div>
				<button id="c">c</button><button id="d">d</button>
				<script>b.addEventListener('keydown', e => { if (e.key === 'Tab' && !e.shiftKey) { e.preventDefault(); a.focus() } })</script>`,
			want: map[string][]string{keyboard.RuleTrap: {"#a"}},
		},
		{
			// Script wrapping focus is harmless while it reaches everything.
			name: "script wraps focus over every control",
			body: `<button id="a">a</button><button id="b">b</button>
				<script>b.addEventListener('keydown', e => { if (e.key === 'Tab' && !e.shiftKey) { e.preventDefault(); a.focus() } })</script>`,
			want: map[string][]string{},
		},
		{
			name: "control skipped",
			body: `<button id="a">a</button><button id="b">b</button><button id="c">c</button>
				<script>a.addEventListener('keydown', e => { if (e.key === 'Tab' && !e.shiftKey) { e.preventDefault(); c.focus() } })</script>`,
			want: map[string][]string{keyboard.RuleUnreachable: {"#b"}},
		},
		{
			name: "native modal dialog",
			body: `<button id="behind">behind</button>
				<dialog id="dlg"><button id="d1">one</button><button id="d2">two</button></dialog>
				<script>dlg.showModal()</script>`,
			want: map[string][]string{},
		},
		{
			name: "modal opened without moving focus, and Tab escapes it",
			body: `<button id="behind">behind</button>
				<div role="dialog" aria-modal="true" id="dlg"><button id="d1">one</button></div>`,
			want: map[string][]string{keyboard.RuleDialog: {"#dlg"}},
		},
		{
			name: "scope filters findings",
			body: `<style>button:focus { outline: none }</style>
				<button id="out">out</button><div id="panel"><button id="in">in</button></div>`,
			opts: keyboard.Options{Scope: "#panel"},
			want: map[string][]string{keyboard.RuleNotVisible: {"#in"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := audit(t, tc.body, tc.opts)
			for rule, targets := range got {
				slices.Sort(targets)
				if want := tc.want[rule]; !slices.Equal(targets, want) {
					t.Errorf("%s: got %v, want %v", rule, targets, want)
				}
			}
			for rule, want := range tc.want {
				if _, ok := got[rule]; !ok {
					t.Errorf("%s: got nothing, want %v", rule, want)
				}
			}
		})
	}
}

// TestAuditRestoresState: steps after an audit must run against the state
// the config reached, not wherever Tab traversal left focus and scroll.
func TestAuditRestoresState(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<!DOCTYPE html><html><body><input id="start">
			<div style="height:3000px"></div><button id="far">far</button></body></html>`))
	}))
	defer srv.Close()
	b, err := browser.Launch("")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()
	s, err := b.NewSession(srv.URL, config.Viewport{Width: 800, Height: 600}, "load", 0, 20*time.Second)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()

	if _, err := s.Eval(`() => document.getElementById("start").focus()`); err != nil {
		t.Fatalf("focus: %v", err)
	}
	if _, err := keyboard.Audit(context.Background(), s, keyboard.Options{}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	raw, err := s.Eval(`() => document.activeElement.id + "@" + scrollY`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := strings.Trim(string(raw), `"`); got != "start@0" {
		t.Errorf("after audit: focus@scroll = %s, want start@0", got)
	}
}
