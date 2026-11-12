package screenreader_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/screenreader"
)

const page = `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body>
<nav aria-label="Main"><a href="/">Home</a></nav>
<main><h1>Dashboard</h1><p>Welcome back.</p>
<label><input type="checkbox" checked> Remember me</label>
<button aria-expanded="false">Filters</button><button id="icon"><svg></svg></button>
<input aria-label="Search" value="tie">
<div role="dialog" aria-modal="true" aria-labelledby="t" id="dlg"><h2 id="t">Confirm</h2><button id="ok">OK</button></div>
<div aria-hidden="true">hidden text</div>
</main></body></html>`

func session(t *testing.T) *browser.Session {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(page))
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
	return s
}

func TestTranscript(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	s := session(t)
	got, err := screenreader.Transcript(s, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`navigation "Main"`,
		`  link "Home"`,
		`main`,
		`  heading "Dashboard" (level 1)`,
		`  text "Welcome back."`,
		`  checkbox "Remember me" (checked)`,
		`  button "Filters" (collapsed)`,
		`  button (no accessible name)`,
		`  textbox "Search" (value "tie")`,
		`  dialog "Confirm" (modal)`,
		`    heading "Confirm" (level 2)`,
		`    button "OK"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("transcript:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	scoped, err := screenreader.Transcript(s, "#dlg")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`dialog "Confirm" (modal)`, `  heading "Confirm" (level 2)`, `  button "OK"`}; !slices.Equal(scoped, want) {
		t.Errorf("scoped transcript:\n%s", strings.Join(scoped, "\n"))
	}
}

func TestCheckFocus(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	s := session(t)
	focus := func(sel string) {
		if _, err := s.Eval(`(sel) => document.querySelector(sel).focus()`, sel); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, focus string
		e           screenreader.Expectation
		ok          bool
		actual      string
	}{
		{"nothing focused", "", screenreader.Expectation{Within: "#dlg"}, false, "focus is not on any element"},
		{"within and named", "#ok", screenreader.Expectation{Within: "#dlg", Role: "button", Name: "OK"}, true, ""},
		{"wrong name", "#ok", screenreader.Expectation{Name: "Cancel"}, false, `focus is on button "OK"`},
		{"wrong role", "#ok", screenreader.Expectation{Role: "link"}, false, `focus is on button "OK"`},
		{"outside", "#icon", screenreader.Expectation{Within: "#dlg"}, false, `focus is on button (no accessible name), outside #dlg`},
		{"within matches nothing", "#ok", screenreader.Expectation{Within: "#gone"}, false, "nothing matches #gone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.focus != "" {
				focus(tc.focus)
			}
			ok, actual, err := screenreader.CheckFocus(s, tc.e)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.ok || !strings.Contains(actual, tc.actual) {
				t.Errorf("ok=%v actual=%q, want ok=%v containing %q", ok, actual, tc.ok, tc.actual)
			}
		})
	}
}
