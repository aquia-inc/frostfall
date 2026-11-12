package browser

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/aquia-inc/frostfall/internal/config"
)

func TestRouteKey(t *testing.T) {
	cases := map[string]string{
		"http://x":                "/",
		"http://x/":               "/",
		"http://x/login/":         "/login",
		"http://x/login?next=/a":  "/login",
		"http://x/login#section":  "/login",
		"http://x/#/login":        "/#/login",
		"http://x/#/login/":       "/#/login",
		"http://x/app#/users?x=1": "/app#/users?x=1", // the query inside a hash route is the route's own
		"http://x/#/":             "/",
		"/#/login":                "/#/login",
		"/spa#/login":             "/spa#/login",
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := RouteKey(u); got != want {
			t.Errorf("RouteKey(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestOriginOf(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8080/x":  "http://127.0.0.1:8080",
		"https://example.com:443/": "https://example.com",
		"http://example.com:80":    "http://example.com",
		"https://Example.com":      "https://example.com",
		"http://[::1]:3000/":       "http://[::1]:3000",
	}
	for raw, want := range cases {
		got, err := originOf(raw)
		// url.Parse keeps host case; browsers lower-case it. Compare folded.
		if err != nil || !strings.EqualFold(got, want) {
			t.Errorf("originOf(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := originOf("example.com"); err == nil {
		t.Error("a bare host is not an origin")
	}
}

func TestStateFromFileMapsCookies(t *testing.T) {
	future := float64(time.Now().Add(time.Hour).Unix())
	st, err := stateFromFile(&config.StorageState{Cookies: []config.StateCookie{
		{Name: "session", Value: "a", Domain: "h", Path: "/", Expires: -1, SameSite: "Strict", HTTPOnly: true},
		{Name: "persist", Value: "b", Domain: "h", Expires: future, SameSite: "Lax", Secure: true},
		{Name: "loose", Value: "c", Domain: "h", Path: "/x", Expires: 0, SameSite: "None"},
		{Name: "unset", Value: "d", Domain: "h", Path: "/"},
		{Name: "stale", Value: "e", Domain: "h", Path: "/", Expires: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*proto.NetworkCookieParam{}
	for _, c := range st.cookies {
		got[c.Name] = c
	}
	if _, ok := got["stale"]; ok || len(got) != 4 {
		t.Errorf("expired cookie kept or others dropped: %d cookies", len(got))
	}
	if c := got["session"]; c.Expires != 0 || c.SameSite != proto.NetworkCookieSameSiteStrict || !c.HTTPOnly {
		t.Errorf("session cookie: %+v (expires -1 must leave Expires unset)", c)
	}
	if c := got["persist"]; float64(c.Expires) != future || c.SameSite != proto.NetworkCookieSameSiteLax || !c.Secure || c.Path != "/" {
		t.Errorf("persistent cookie: %+v", c)
	}
	if c := got["loose"]; c.Expires != 0 || c.SameSite != proto.NetworkCookieSameSiteNone || c.Path != "/x" {
		t.Errorf("expires 0 / None cookie: %+v", c)
	}
	if c := got["unset"]; c.SameSite != "" {
		t.Errorf("absent sameSite must stay unset: %+v", c)
	}
}

func TestSeedScriptGuardsEveryOrigin(t *testing.T) {
	st, err := stateFromFile(&config.StorageState{Origins: []config.StateOrigin{
		{Origin: "https://app.example.com:443", LocalStorage: []config.StateItem{{Name: "a", Value: `x"</script>`}}},
		{Origin: "http://localhost:3000", LocalStorage: []config.StateItem{{Name: "b", Value: "2"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	src, err := st.seedScript()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`location.origin === "https://app.example.com"`, `location.origin === "http://localhost:3000"`} {
		if !strings.Contains(src, want) {
			t.Errorf("script lacks %q:\n%s", want, src)
		}
	}
	// Every setItem sits inside a guard: the text before the first guard
	// holds none.
	if i := strings.Index(src, "location.origin"); i < 0 || strings.Contains(src[:i], "setItem") {
		t.Errorf("storage written outside an origin guard:\n%s", src)
	}
	if strings.Count(src, "location.origin") != 2 {
		t.Errorf("want one guard per origin:\n%s", src)
	}
}
