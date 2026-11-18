package browser

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/engine/axe"
	"github.com/aquia-inc/frostfall/internal/server"
)

func targets(vs []engine.Violation) []string {
	out := []string{}
	for _, v := range vs {
		out = append(out, v.RuleID+" at "+v.Target)
	}
	slices.Sort(out)
	return out
}

// The static fixture's frames page: a planted violation inside a same-origin
// frame and another inside a cross-site one, both reported with
// frame-qualified targets. Under default launch flags (rod turns site
// isolation off) both frames share the page's process. With site isolation on,
// as desktop Chrome runs for its users, the cross-site frame is a separate
// CDP target, and the scan must reach it through the out-of-process branch:
// the counter proves that branch ran, not just the in-process one.
func TestFixtureFramesInAndOutOfProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := &server.Static{Dir: "../../testdata/static"}
	base, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	want := []string{
		"image-alt at #same-frame |> img",
		"label at #cross-frame |> #cross-unlabeled",
	}
	for _, isolate := range []bool{false, true} {
		t.Run(fmt.Sprintf("siteIsolation=%v", isolate), func(t *testing.T) {
			b, err := LaunchWith("", LaunchOptions{SiteIsolation: isolate})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			// load, not networkIdle: under site isolation the request for an
			// out-of-process frame's document starts in the page's target
			// but finishes in the frame's, which nothing here is attached
			// to, so the page never looks idle and the wait runs out the
			// budget. Default launches have no out-of-process frames.
			s, err := b.NewSession(base+"/frames.html", config.Viewport{Width: 1280, Height: 800}, "load", 0, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			// frames: false first, while no frame has axe in it, is the scan
			// as it was before frames: the top document is clean.
			top, err := axe.New().Audit(t.Context(), s, engine.ScanOptions{Standard: "wcag21aa"})
			if err != nil {
				t.Fatal(err)
			}
			if len(top) != 0 {
				t.Errorf("frames off: violations = %q, want none", targets(top))
			}

			var warned []string
			vs, err := axe.New().Audit(t.Context(), s, engine.ScanOptions{
				Standard: "wcag21aa",
				Frames:   true,
				Warn:     func(frame string, err error) { warned = append(warned, frame+": "+err.Error()) },
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := targets(vs); !slices.Equal(got, want) {
				t.Errorf("violations = %q, want %q", got, want)
			}
			if len(warned) > 0 {
				t.Errorf("warnings: %q", warned)
			}
			wantOOP := 0
			if isolate {
				wantOOP = 1
			}
			if s.outOfProcess != wantOOP {
				t.Errorf("out-of-process attaches = %d, want %d", s.outOfProcess, wantOOP)
			}

			// scope is a top-document selector; frames inside it are
			// scanned in full.
			scoped, err := axe.New().Audit(t.Context(), s, engine.ScanOptions{Standard: "wcag21aa", Scope: "#frames", Frames: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := targets(scoped); !slices.Equal(got, want) {
				t.Errorf("scope #frames: violations = %q, want %q", got, want)
			}

			// A second scan of the same page reuses the attached frame and
			// finds the same thing.
			again, err := axe.New().Audit(t.Context(), s, engine.ScanOptions{Standard: "wcag21aa", Frames: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := targets(again); !slices.Equal(got, want) {
				t.Errorf("second scan: violations = %q, want %q", got, want)
			}
		})
	}
}

// Under site isolation a cross-site frame whose renderer hangs must cost the
// scan one frame deadline, a warning and a null partial, not the test's whole
// budget; the healthy frames are still reported, a frame nested in a frame
// included, and the session still closes. The page is served on 127.0.0.1,
// the stuck frame on localhost and the nested frame on [::1]: three sites.
func TestHungOutOfProcessFrameIsSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	defer func(d time.Duration) { frameTimeout = d }(frameTimeout)
	frameTimeout = 2 * time.Second

	page := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><title>t</title></head><body><main>%s</main></body></html>`, body)
	}
	// The nested frame comes from a third site, [::1], so it does not share
	// the stuck frame's renderer.
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback for the third site: %v", err)
	}
	third := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page(w, `<input type="text" id="nested-unlabeled">`)
	}))
	third.Listener.Close()
	third.Listener = ln
	third.Start()
	defer third.Close()

	var port string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		page(w, `<h1>top</h1>
			<iframe id="outer" title="outer" src="/outer"></iframe>
			<iframe id="stuck" title="stuck" src="http://localhost:`+port+`/stuck"></iframe>`)
	})
	// Same origin as the top, with a cross-site frame of its own.
	mux.HandleFunc("/outer", func(w http.ResponseWriter, r *http.Request) {
		page(w, `<img src="a.png"><iframe id="inner" title="inner" src="`+third.URL+`/inner"></iframe>`)
	})
	mux.HandleFunc("/stuck", func(w http.ResponseWriter, r *http.Request) {
		page(w, `<input type="text"><script>addEventListener('load', () => setTimeout(() => { for (;;) {} }, 100))</script>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port = srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	b, err := LaunchWith("", LaunchOptions{SiteIsolation: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s, err := b.NewSession("http://127.0.0.1:"+port+"/", config.Viewport{Width: 1280, Height: 800}, "load", 500*time.Millisecond, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var warned []string
	start := time.Now()
	vs, err := axe.New().Audit(t.Context(), s, engine.ScanOptions{
		Standard: "wcag21aa",
		Frames:   true,
		Warn:     func(frame string, err error) { warned = append(warned, frame) },
	})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"image-alt at #outer |> img",
		"label at #outer |> #inner |> #nested-unlabeled",
	}
	if got := targets(vs); !slices.Equal(got, want) {
		t.Errorf("violations = %q, want %q", got, want)
	}
	// Frames are named by axe's frameSelector, a path from the root.
	if !slices.Equal(warned, []string{"html > body > main > iframe:nth-child(3)"}) {
		t.Errorf("warnings = %q, want the stuck frame alone", warned)
	}
	if took > 20*time.Second {
		t.Errorf("scan took %v: the stuck frame cost more than its deadline", took)
	}
	if s.outOfProcess < 1 {
		t.Errorf("the nested cross-site frame was not reached out of process")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("Session.Close did not return after a frame attach timed out")
	}
}

// Each frame's deadline covers its own work. Locating a child is the child's
// work: a parent frame whose own deadline has passed (scanning earlier
// siblings' subtrees took it) must still be able to hand out its next child.
func TestChildLocatedOnItsOwnDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	defer func(d time.Duration) { frameTimeout = d }(frameTimeout)
	frameTimeout = time.Second

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><iframe id="outer" src="/outer"></iframe></body></html>`)
	})
	mux.HandleFunc("/outer", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><iframe id="inner" src="/inner"></iframe></body></html>`)
	})
	mux.HandleFunc("/inner", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><p id="deep">deep</p></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b, err := Launch("")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s, err := b.NewSession(srv.URL+"/", config.Viewport{Width: 800, Height: 600}, "load", 0, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const locate = `(s) => document.querySelector(JSON.parse(s))`
	outer, err := s.Frame(locate, []byte(`"#outer"`))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(frameTimeout + 500*time.Millisecond)
	inner, err := outer.Frame(locate, []byte(`"#inner"`))
	if err != nil {
		t.Fatalf("locating a child after the parent's deadline: %v", err)
	}
	raw, err := inner.Eval(`() => document.getElementById('deep').textContent`)
	if err != nil || string(raw) != `"deep"` {
		t.Errorf("inner frame eval = %s, %v", raw, err)
	}
}
