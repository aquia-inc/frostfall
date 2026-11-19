package config

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// An ASCII host is only lower-cased and bracketed, as before IDN support; a
// non-ASCII one is written in punycode, as the browser reports it.
func TestCanonicalHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"example.com", "example.com"},
		{"App.Example.COM", "app.example.com"},
		{"127.0.0.1", "127.0.0.1"},
		{"::1", "[::1]"},
		{"FE80::1", "[fe80::1]"},
		{"xn--caf-dma.example", "xn--caf-dma.example"},
		{"XN--CAF-DMA.Example", "xn--caf-dma.example"},
		// None of the IDN checks apply to an ASCII host: invalid punycode, an
		// xn-- label with nothing after it, empty labels and a numeric last
		// label are left as they were.
		{"xn--zz.example", "xn--zz.example"},
		{"a.xn--.example", "a.xn--.example"},
		{"a..b", "a..b"},
		{"a.1.2.3", "a.1.2.3"},
		{"under_score.example", "under_score.example"},
		{"café.example", "xn--caf-dma.example"},
		{"CAFÉ.Example", "xn--caf-dma.example"},
		{"faß.de", "xn--fa-hia.de"}, // non-transitional: not fass.de
		{"a_b.café.example", "a_b.xn--caf-dma.example"},
		{"café。example", "xn--caf-dma.example"},
		{"\u212a.café.example", "k.xn--caf-dma.example"}, // the Kelvin sign maps to k
		// Labels empty as written are kept, as the browser keeps them.
		{"é..example", "xn--9ca..example"},
		{"é.example.", "xn--9ca.example."},
		{"é.example..", "xn--9ca.example.."},
		// A last label that only looks numeric is a name.
		{"é.0x1g", "xn--9ca.0x1g"},
		{"é.1a", "xn--9ca.1a"},
	} {
		got, err := CanonicalHost(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CanonicalHost(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"a\u200db.example", "\ufffd.Example", "1א.example",
		// Bidi after mapping: ℵ is Hebrew alef once mapped.
		"xℵ.example",
		// Refused hosts whose lower-cased form would be pure ASCII.
		"\u212a.xn--zz.example", "\u0130.xn--zz.example",
		// Forbidden domain code points after mapping.
		"：：1", "evil.com＠app.example", "a／b.example", "a％b.example", "a＃b.example",
		"a＜b.example", "a＾b.example", "a｜b.example", "a︓b.example", "a﹕b.example",
		"a℀b.example", "a﹟b.example", "a﹤b.example", "a﹪b.example", "a﹨b.example",
		// An xn-- label that decodes to nothing.
		"é.xn--.example",
		// A last label that is a number, with one trailing dot or none.
		"é.1.2.3", "é.0x1F", "é.0X", "1.2.3.4。", "１２７.0.0.1", "é.123.",
		// Nothing left after mapping.
		"\u00ad",
	} {
		got, err := CanonicalHost(in)
		if !errors.Is(err, ErrInvalidIDN) {
			t.Errorf("CanonicalHost(%q) = %q, %v; want ErrInvalidIDN", in, got, err)
		}
		// The fallback is the host exactly as written: non-ASCII, so never an
		// origin a browser reports, and a comparison against it fails closed.
		if got != in {
			t.Errorf("CanonicalHost(%q) fallback = %q, want it unchanged", in, got)
		}
	}
}

// Every origin a config names is compared with what the browser reports, so
// an IDN origin is written in punycode, its port and case handled as for any
// other.
func TestNormalizeOriginIDN(t *testing.T) {
	for in, want := range map[string]string{
		"http://café.example":           "http://xn--caf-dma.example",
		"HTTP://CAFÉ.example:80/":       "http://xn--caf-dma.example",
		"https://café.example:8443":     "https://xn--caf-dma.example:8443",
		"http://xn--caf-dma.example":    "http://xn--caf-dma.example",
		"https://Login.Example.com:443": "https://login.example.com",
		"http://[::1]:8080":             "http://[::1]:8080",
	} {
		got, err := NormalizeOrigin(in)
		if err != nil || got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeOrigin("http://a\u200db.example"); !errors.Is(err, ErrInvalidIDN) {
		t.Errorf("an invalid IDN origin: err = %v, want ErrInvalidIDN", err)
	}
	u, _ := url.Parse("http://café.example:8080/x?y=1")
	if got := OriginOfURL(u); got != "http://xn--caf-dma.example:8080" {
		t.Errorf("OriginOfURL = %q", got)
	}
}

// An IDN allowedOrigins entry is stored in the form the browser reports, so it
// matches the provider's origin, and an invalid one is a config error.
func TestAllowedOriginsIDN(t *testing.T) {
	cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+signed+
		"  allowedOrigins: [\"https://Login.Café.example\", \"http://xn--caf-dma.example:8080\"]\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://login.xn--caf-dma.example", "http://xn--caf-dma.example:8080"}
	if !slices.Equal(cfg.Auth.AllowedOrigins, want) {
		t.Errorf("allowedOrigins = %v, want %v", cfg.Auth.AllowedOrigins, want)
	}
	_, err = Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+signed+
		"  allowedOrigins: [\"https://a\u200db.example\"]\n"), "")
	if err == nil || !strings.Contains(err.Error(), "auth.allowedOrigins[0]") || !strings.Contains(err.Error(), "internationalized domain name") {
		t.Errorf("err = %v, want the invalid IDN origin refused", err)
	}
}

// A test URL on the IDN base URL's origin, written either way, is the app's
// origin; another IDN host is not.
func TestCheckTestOriginsIDN(t *testing.T) {
	cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+signed), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://xn--caf-dma.example:8080/x", "http://CAFÉ.example:8080/y", "http://café.example:8080"} {
		if err := cfg.CheckTestOrigins("http://café.example:8080", []Test{{ID: "t", URL: u}}); err != nil {
			t.Errorf("url %s: %v", u, err)
		}
	}
	if err := cfg.CheckTestOrigins("http://café.example:8080", []Test{{ID: "t", URL: "http://cafe.example:8080/"}}); err == nil {
		t.Error("cafe.example is another host than café.example")
	}
}

// A test URL whose host the browser refuses is a config error, not compared:
// lower-cased, İ.xn--zz.example would be the ASCII host i.xn--zz.example.
func TestCheckTestOriginsRefusesInvalidIDN(t *testing.T) {
	cfg, err := Load(writeConfigIn(t, t.TempDir(), authPrefix+validSetup+signed), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ base, url string }{
		{"https://i.xn--zz.example", "https://\u0130.xn--zz.example/x?token=SECRETTOK123"},
		{"https://k.xn--zz.example", "https://\u212a.xn--zz.example/x"},
	} {
		err := cfg.CheckTestOrigins(tc.base, []Test{{ID: "t", URL: tc.url}})
		if !errors.Is(err, ErrInvalidIDN) || !strings.Contains(err.Error(), `test "t"`) {
			t.Errorf("base %s, url %s: err = %v, want ErrInvalidIDN naming the test", tc.base, tc.url, err)
		}
		if err != nil && strings.Contains(err.Error(), "SECRETTOK123") {
			t.Errorf("the error printed the query: %v", err)
		}
	}
	// auth: false takes the test out of the check, as for any other host.
	f := false
	if err := cfg.CheckTestOrigins("https://i.xn--zz.example", []Test{{ID: "t", URL: "https://\u0130.xn--zz.example/", Auth: &f}}); err != nil {
		t.Errorf("auth: false: %v", err)
	}
}

// A host that maps to a forbidden code point is no origin: a fullwidth colon
// is not an IPv6 literal, a fullwidth @ not userinfo.
func TestNormalizeOriginForbiddenAfterMapping(t *testing.T) {
	for _, raw := range []string{"http://：：1", "http://evil.com＠app.example", "http://a／b.example"} {
		if got, err := NormalizeOrigin(raw); !errors.Is(err, ErrInvalidIDN) {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want ErrInvalidIDN", raw, got, err)
		}
	}
}

// A storage-state origin on a host the browser refuses says so, without the
// value.
func TestStorageStateInvalidIDNOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"cookies":[],"origins":[{"origin":"http://a` + "\u200d" + `b.example/","localStorage":[{"name":"token","value":"t0k"}]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadStorageState(path)
	if !errors.Is(err, ErrInvalidIDN) || !strings.Contains(err.Error(), "origins[0]") || !strings.Contains(err.Error(), "internationalized domain name") {
		t.Errorf("err = %v, want origins[0] and the IDN reason", err)
	}
	if err != nil && strings.Contains(err.Error(), "example") {
		t.Errorf("the error printed the origin: %v", err)
	}
}
