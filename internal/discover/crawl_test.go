package discover_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
	"github.com/aquia-inc/frostfall/internal/discover"
)

// A signed-out crawl of a base URL with a trailing slash finds the same pages
// as one without, and never asks the server for a "//" path. Before the URL
// join was fixed (see config.JoinURL) the page links, which are relative the
// way a static site writes them, resolved against "http://host//" and every
// level of the crawl added slashes: each page was found again under a longer
// path until maxPages. The crawl runs under a deadline so that a regression
// fails the test instead of hanging it.
func TestSignedOutCrawlOfATrailingSlashBaseMatchesTheBareBase(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	var mu sync.Mutex
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested = append(requested, r.URL.Path)
		mu.Unlock()
		// A static site's pages: relative links, directories with an index.
		links := map[string][]string{
			"/":           {"about.html", "docs/"},
			"/about.html": {"index.html", "./"},
			"/docs/":      {"intro.html", "../"},
		}[r.URL.Path]
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>`)
		for _, l := range links {
			fmt.Fprintf(w, `<a href=%q>%s</a>`, l, l)
		}
		fmt.Fprint(w, `</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	b, err := browser.Launch("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	crawl := func(base string) []string {
		t.Helper()
		c := &discover.Crawler{
			Browser: b, BaseURL: base,
			Options:  config.Discover{MaxDepth: 3, MaxPages: 20},
			Defaults: config.Defaults{Viewport: config.Viewport{Width: 800, Height: 600}, WaitFor: "load"},
		}
		type result struct {
			paths []string
			err   error
		}
		done := make(chan result, 1)
		go func() {
			paths, err := c.Run()
			done <- result{paths, err}
		}()
		select {
		case res := <-done:
			if res.err != nil {
				t.Fatalf("crawl of %s: %v", base, res.err)
			}
			slices.Sort(res.paths)
			return res.paths
		case <-time.After(90 * time.Second):
			t.Fatalf("crawl of %s did not finish", base)
			return nil
		}
	}

	bare := crawl(srv.URL)
	slashed := crawl(srv.URL + "/")
	if !slices.Equal(bare, slashed) {
		t.Errorf("trailing slash changed what was found: %v without, %v with", bare, slashed)
	}
	for _, want := range []string{"/", "/about.html", "/docs"} {
		if !slices.Contains(slashed, want) {
			t.Errorf("crawl missed %s: %v", want, slashed)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range requested {
		if strings.HasPrefix(p, "//") {
			t.Errorf("request for %q: the trailing slash of the base URL was kept", p)
		}
	}
}
