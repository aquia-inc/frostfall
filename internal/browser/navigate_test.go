package browser

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"

	"github.com/aquia-inc/frostfall/internal/config"
)

// A page that navigates itself while frostfall waits for its load event (an
// auth guard's location.replace) destroys the context the wait runs in. CDP
// reports that as "Inspected target navigated or closed"; readiness must
// follow the page to where it went instead of failing. A slow image holds the
// first page's load open so the redirect always lands mid-wait.
func TestLoadFollowsSelfRedirect(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/guarded", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><head><title>guarded</title>
			<script>setTimeout(() => location.replace('/login'), 300)</script></head>
			<body><img src="/slow.png"></body></html>`))
	})
	mux.HandleFunc("/slow.png", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><head><title>login</title></head><body>sign in</body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b, err := Launch("")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()
	for _, waitFor := range []string{"load", "networkIdle"} {
		s, err := b.NewSession(srv.URL+"/guarded", config.Viewport{Width: 800, Height: 600}, waitFor, 0, 20*time.Second)
		if err != nil {
			t.Fatalf("%s: session: %v", waitFor, err)
		}
		raw, err := s.Eval(`() => location.pathname`)
		s.Close()
		if err != nil || !strings.Contains(string(raw), "/login") {
			t.Errorf("%s: landed on %s (%v), want /login", waitFor, raw, err)
		}
	}
}

// The retry itself, deterministically: a selector wait is too brief to stage
// inside the redirect race, so the navigation error is injected. A navigation
// is waited through; any other error returns at once.
func TestFollowNavigationRetriesOnlyNavigations(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<!DOCTYPE html><html><body>ok</body></html>`))
	}))
	defer srv.Close()
	b, err := Launch("")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()
	s, err := b.NewSession(srv.URL, config.Viewport{Width: 800, Height: 600}, "load", 0, 20*time.Second)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()

	navigated := &cdp.Error{Code: -32000, Message: "Inspected target navigated or closed"}
	calls := 0
	err = s.followNavigation(func(*rod.Page) error {
		if calls++; calls == 1 {
			return navigated
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("after a navigation: err = %v after %d calls, want nil after 2", err, calls)
	}

	other := errors.New("selector never appeared")
	calls = 0
	err = s.followNavigation(func(*rod.Page) error { calls++; return other })
	if !errors.Is(err, other) || calls != 1 {
		t.Errorf("another error: err = %v after %d calls, want it returned after 1", err, calls)
	}
}

// A page that redirects itself forever fails fast and says so, rather than
// running out the page's whole timeout.
func TestSelfRedirectLoopFailsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><head><script>location.replace('/again?' + Date.now())</script></head><body><img src="/slow"></body></html>`))
	}))
	defer srv.Close()
	b, err := Launch("")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()
	start := time.Now()
	_, err = b.NewSession(srv.URL, config.Viewport{Width: 800, Height: 600}, "load", 0, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "redirecting in a loop") {
		t.Fatalf("err = %v, want the redirect loop named", err)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("took %v, want well under the 30s timeout", took)
	}
}
