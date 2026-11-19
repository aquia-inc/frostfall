package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A base URL on a non-ASCII host the browser refuses is a config error before
// anything is requested, even where Go's client would reach something (it maps
// a fullwidth 127.0.0.1 to the loopback address).
func TestProbeRefusesAnInvalidIDNHost(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	probeTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		requests++
		mu.Unlock()
		return nil, errors.New("the probe sent a request")
	})
	t.Cleanup(func() { probeTransport = nil })
	for _, base := range []string{
		"http://１２７.0.0.1:8080/",
		"https://user:S3cretBasicPW@xn--café-.example/?token=SECRETTOK123",
		"http://a‍b.example",
	} {
		_, _, code, msg := probeAttach(base, func() {}, false)
		if code != exitBadConfig || !strings.Contains(msg, "internationalized domain name") || !strings.HasPrefix(msg, "config error: base url ") {
			t.Errorf("%q: code %d, msg %q; want exit %d and the IDN reason", base, code, msg, exitBadConfig)
		}
		if strings.Contains(msg, "S3cretBasicPW") || strings.Contains(msg, "SECRETTOK123") {
			t.Errorf("%q: the message printed a credential: %q", base, msg)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Errorf("the transport saw %d requests, want none", requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
