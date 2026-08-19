package discover

import (
	"net/url"
	"reflect"
	"regexp"
	"testing"
)

func TestFilterLinks(t *testing.T) {
	base, _ := url.Parse("http://localhost:5173")
	hrefs := []string{
		"http://localhost:5173/pricing",
		"http://localhost:5173/pricing/",        // trailing slash duplicate
		"http://localhost:5173/pricing#plans",   // fragment duplicate
		"http://localhost:5173/pricing?ref=nav", // query duplicate
		"http://localhost:5173/",
		"http://localhost:5173/admin/users", // excluded below
		"https://example.com/external",      // cross-origin
		"mailto:hi@example.com",
		"http://localhost:9999/other-port", // different host:port
	}
	excludes := []*regexp.Regexp{regexp.MustCompile(`^/admin/`)}
	got := FilterLinks(base, hrefs, excludes)
	want := []string{"/", "/pricing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilterLinksHashRouter(t *testing.T) {
	// Found dogfooding ztmf-ui: HashRouter apps put the route in the
	// fragment. Dropping fragments collapses the whole app into "/".
	base, _ := url.Parse("http://localhost:5174")
	hrefs := []string{
		"http://localhost:5174/#/users",
		"http://localhost:5174/#/systems/1",
		"http://localhost:5174/#/",        // hash root == root
		"http://localhost:5174/#plans",    // plain anchor, not a route
		"http://localhost:5174/#/users/",  // trailing slash duplicate
	}
	got := FilterLinks(base, hrefs, nil)
	want := []string{"/", "/#/systems/1", "/#/users"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestShapeKey(t *testing.T) {
	cases := map[string]string{
		"/#/systems/1":     "/#/systems/:n",
		"/#/systems/1383":  "/#/systems/:n",
		"/users":           "/users",
		"/orgs/3/repos/44": "/orgs/:n/repos/:n",
	}
	for in, want := range cases {
		if got := ShapeKey(in); got != want {
			t.Errorf("ShapeKey(%q) = %q, want %q", in, got, want)
		}
	}
	if ShapeKey("/#/systems/1") == ShapeKey("/#/users") {
		t.Errorf("distinct routes collapsed")
	}
}
