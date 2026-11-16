package browser

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		time.Sleep(2 * time.Second)
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
