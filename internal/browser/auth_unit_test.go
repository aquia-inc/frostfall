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
		"https://APP.example.com/": "https://app.example.com",
		"HTTP://Host:8080":         "http://host:8080",
		"http://[::1]:3000/":       "http://[::1]:3000",
	}
	for raw, want := range cases {
		got, err := originOf(raw)
		// Browsers report location.origin lower-cased, so the comparisons
		// that capture, seed and guard on it must be exact on a lower-cased
		// form: an upper-case host in a config must still match.
		if err != nil || got != want {
			t.Errorf("originOf(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := originOf("example.com"); err == nil {
		t.Error("a bare host is not an origin")
	}
}

func TestGuardKeyIgnoresHashRouteQuery(t *testing.T) {
	cases := map[string]string{
		"http://x/#/login?redirect=/x": "/#/login",
		"http://x/#/login?a=1&b=2":     "/#/login",
		"http://x/app#/login?next=%2F": "/app#/login",
		"http://x/login?next=/a":       "/login",
		"http://x/#/login/":            "/#/login",
		"http://x/#section":            "/",
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		if got := guardKey(u); got != want {
			t.Errorf("guardKey(%q) = %q, want %q", raw, got, want)
		}
	}
	// Discovery's identity is untouched: two hash queries are two pages.
	u, _ := url.Parse("http://x/#/login?redirect=/x")
	if got := RouteKey(u); got != "/#/login?redirect=/x" {
		t.Errorf("RouteKey changed: %q", got)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"/login?token=abc":                       "/login",
		"/#/login?token=abc":                     "/#/login",
		"http://h/app?x=1#/y":                    "http://h/app#/y", // a hash route is page identity
		"http://h/app?x=1#section":               "http://h/app",
		"/plain":                                 "/plain",
		"http://user:pa55@h:8080/p?q=1#frag":     "http://h:8080/p",
		"https://u@h/p":                          "https://h/p",
		"/login;jsessionid=ABC123":               "/login",
		"/a;x=1/b;y=2?z":                         "/a/b",
		"http://h/login;jsessionid=ABC?x#/r?t=1": "http://h/login#/r",
		"":                                       "",
		// OAuth implicit flows put the token after the route, in these places.
		"http://h/a#/home#access_token=X":             "http://h/a#/home",
		"http://h/#/access_token=X&token_type=bearer": "http://h/#/access_token",
		"http://h/#/cb&access_token=X":                "http://h/#/cb",
		"http://h/#/route=1":                          "http://h/#/route",
		"/login%3Ftoken=X":                            "/login",
		"/x%23frag":                                   "/x",
		"/login%3Ftoken=X;jsessionid=Y%23z":           "/login",
		// A path segment that looks like a secret is masked; names are kept.
		"http://h/dl/dGhpc0lzQVNlY3JldFRva2VuMTIzNDU2/x":     "http://h/dl/[redacted]/x",
		"/o/0123456789abcdef0123456789abcdef":                "/o/[redacted]",
		"/cb/eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0.SflKxwRJ": "/cb/[redacted]",
		"/#/cb/dGhpc0lzQVNlY3JldFRva2VuMTIzNDU2":             "/#/cb/[redacted]",
		"/docs/release-notes-2024-01-15-summary":             "/docs/release-notes-2024-01-15-summary",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RedactURL("http://[bad"); strings.Contains(got, "bad") {
		t.Errorf("an unparsable URL was echoed: %q", got)
	}
	if got := describeStep(config.Step{Goto: "/next?token=abc;k=v"}); strings.Contains(got, "abc") || strings.Contains(got, "k=v") {
		t.Errorf("describeStep leaks a query: %q", got)
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
