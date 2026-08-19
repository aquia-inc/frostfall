package browser

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aquia-inc/frostfall/internal/config"
)

// TestNetworkIdleSeesInFlightRequests reproduces issue #8: a fetch started
// before the load event and still in flight when it fires. A monitor that
// subscribes after load never sees it, declares idle ~500ms after load, and
// readiness completes before the page's real content exists. The monitor now
// subscribes before navigation, so readiness must wait for the slow fetch.
func TestNetworkIdleSeesInFlightRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><head><script>
			fetch('/slow').then(() => { document.title = 'hydrated' });
		</script><title>skeleton</title></head><body>hi</body></html>`))
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b, err := Launch("")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()

	session, err := b.NewSession(srv.URL, config.Viewport{Width: 800, Height: 600},
		"networkIdle", 0, 20*time.Second)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer session.Close()

	raw, err := session.Eval("() => document.title")
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := string(raw); got != `"hydrated"` {
		t.Errorf("readiness completed before the in-flight fetch: title = %s", got)
	}
}
