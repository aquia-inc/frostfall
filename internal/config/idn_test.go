package config

import (
	"errors"
	"net/url"
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
		// Invalid punycode in an ASCII host is left as it was: only non-ASCII
		// hosts are converted.
		{"xn--zz.example", "xn--zz.example"},
		{"under_score.example", "under_score.example"},
		{"café.example", "xn--caf-dma.example"},
		{"CAFÉ.Example", "xn--caf-dma.example"},
		{"faß.de", "xn--fa-hia.de"}, // non-transitional: not fass.de
		{"a_b.café.example", "a_b.xn--caf-dma.example"},
		{"café。example", "xn--caf-dma.example"},
	} {
		got, err := CanonicalHost(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CanonicalHost(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"a‍b.example", "�.Example", "1א.example"} {
		got, err := CanonicalHost(in)
		if !errors.Is(err, ErrInvalidIDN) {
			t.Errorf("CanonicalHost(%q): err = %v, want ErrInvalidIDN", in, err)
		}
		// The fallback is the host as written, lower-cased: never an origin a
		// browser reports, so a comparison against it fails closed.
		if got != strings.ToLower(in) {
			t.Errorf("CanonicalHost(%q) fallback = %q, want %q", in, got, strings.ToLower(in))
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
	if _, err := NormalizeOrigin("http://a‍b.example"); !errors.Is(err, ErrInvalidIDN) {
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
		"  allowedOrigins: [\"https://a‍b.example\"]\n"), "")
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
