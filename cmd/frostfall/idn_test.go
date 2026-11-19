package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The reachability probe for a base URL dials the host the browser will:
// punycode, even for a label idna.Lookup refuses but Chrome loads (an
// underscore, a leading hyphen), for which Go's client would dial the raw
// UTF-8 host. Every dial lands on the test server, so the address dialed is
// the only thing that differs.
func TestProbeDialsThePunycodeHost(t *testing.T) {
	var mu sync.Mutex
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host)
		mu.Unlock()
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	var dialed []string
	probeTransport = &http.Transport{DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, a)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
	t.Cleanup(func() { probeTransport = nil })

	for _, tc := range []struct{ base, want string }{
		{"http://a_b.café.example:" + port + "/app", "a_b.xn--caf-dma.example:" + port},
		{"http://-café.example:" + port, "xn---caf-epa.example:" + port},
		{"http://CAFÉ.example:" + port, "xn--caf-dma.example:" + port},
		{"http://localhost:" + port + "/x", "localhost:" + port},
	} {
		mu.Lock()
		dialed, hosts = nil, nil
		mu.Unlock()
		got, _, code, msg := probeAttach(tc.base, func() {}, false)
		if code != exitOK || got != tc.base {
			t.Errorf("%s: probe = %q, %d, %q; want the base URL as written and exit 0", tc.base, got, code, msg)
		}
		mu.Lock()
		if len(dialed) != 1 || dialed[0] != tc.want {
			t.Errorf("%s: dialed %v, want %s", tc.base, dialed, tc.want)
		}
		if len(hosts) != 1 || hosts[0] != tc.want {
			t.Errorf("%s: Host header %v, want %s", tc.base, hosts, tc.want)
		}
		mu.Unlock()
	}
}
