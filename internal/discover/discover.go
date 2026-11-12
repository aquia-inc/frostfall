// Package discover crawls same-origin links to synthesize page-coverage
// tests. It complements, never replaces, explicit tests: a crawler finds
// pages nobody thought to list, but only hand-written steps reach the states
// (modals, menus, error forms) where most violations live.
package discover

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
)

type Crawler struct {
	Browser  *browser.Browser
	BaseURL  string
	Options  config.Discover
	Defaults config.Defaults
	// Auth seeds every crawled page's context; nil crawls signed out. With
	// auth.reuse: false each page loaded runs the login flow itself, up to
	// MaxPages times.
	Auth *browser.Auth
	Log  func(format string, args ...any)
}

// Run crawls breadth-first from the root path and returns discovered paths in
// visit order, root first. A page that fails to load is not fatal and is not
// expanded, but it stays in the result: its failure is then reported when it
// is scanned, rather than the page going unmentioned. Signed in, a page that
// turns out to be the login page (or an unallowed origin) is dropped from the
// result instead, and the crawl fails if that is the root.
func (c *Crawler) Run() ([]string, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base url: %w", err)
	}
	excludes := make([]*regexp.Regexp, 0, len(c.Options.Exclude))
	for _, pat := range c.Options.Exclude {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("discover.exclude %q: %w", pat, err)
		}
		excludes = append(excludes, re)
	}
	if c.Auth != nil {
		// A crawl that follows a logout link signs itself out and reads every
		// later page as the login page; the login page itself is no page to
		// scan. Neither is configurable away, both are always wrong.
		excludes = append(excludes, c.Auth.DiscoverExcludes()...)
	}

	type node struct {
		path  string
		depth int
	}
	queue := []node{{path: "/", depth: 0}}
	seen := map[string]bool{"/": true}
	seenShape := map[string]bool{ShapeKey("/"): true}
	var found []string
	dropped := 0

	for len(queue) > 0 && len(found) < c.Options.MaxPages {
		n := queue[0]
		queue = queue[1:]
		// Signed in, pages at the depth limit are still loaded once: their
		// links are not followed, but a page that redirects to login must not
		// become a test that fails the run.
		var links []string
		var err error
		if n.depth < c.Options.MaxDepth || c.Auth != nil {
			links, err = c.collectLinks(n.path)
		}
		if errors.Is(err, browser.ErrLoginRedirect) {
			// A root that redirects to the login page means the session was not
			// applied: every link on it would be the login page's, and the crawl
			// would quietly cover nothing. Deeper pages that do it are dropped,
			// never recorded as pages.
			if n.depth == 0 {
				return nil, err
			}
			dropped++
			if c.Log != nil {
				c.Log("discover: dropping %s: %s (a public page belongs in the config as an explicit test with auth: false; "+
					"a signed-in page that does not render the signedIn marker needs a marker every signed-in page renders)", c.shown(n.path), err)
			}
			continue
		}
		found = append(found, n.path)
		if n.depth >= c.Options.MaxDepth {
			continue
		}
		if err != nil {
			if c.Log != nil {
				c.Log("discover: not following links of %s: %v", c.shown(n.path), err)
			}
			continue
		}
		for _, p := range FilterLinks(base, links, excludes) {
			if c.Auth != nil {
				// These paths become test ids in reports, baselines and issues,
				// so a signed-in crawl does not let a token in a link reach them.
				var ok bool
				if p, ok = cleanAuthPath(p); !ok {
					continue
				}
			}
			shape := ShapeKey(p)
			if !seen[p] && !seenShape[shape] {
				seen[p] = true
				seenShape[shape] = true
				queue = append(queue, node{path: p, depth: n.depth + 1})
			}
		}
	}
	if c.Auth != nil && c.Log != nil {
		// Always said, zero included: a crawl that dropped nothing and one
		// that was not told are different things to a reader.
		c.Log("discover: dropped %d of %d pages (not signed in)", dropped, dropped+len(found))
	}
	return found, nil
}

var pathParams = regexp.MustCompile(`;[^/]*`)

// credentialMarks in a path say the link carries a secret in the path itself.
var credentialMarks = []string{"token=", "access_token", "sig=", "code="}

// cleanAuthPath makes a discovered path safe to use as a test id when signed
// in: ;params are stripped, a hash route keeps only the route (its query and
// any nested fragment are dropped, as in browser.RedactURL), and a path that
// still looks like it carries a credential is rejected. Without auth nothing
// is changed: discovery output stays as it always was.
func cleanAuthPath(p string) (string, bool) {
	route, hash, hasHash := strings.Cut(p, "#")
	route = pathParams.ReplaceAllString(route, "")
	if route == "" {
		route = "/"
	}
	if hasHash {
		if i := strings.IndexAny(hash, "#?&="); i >= 0 {
			hash = hash[:i]
		}
		if hash = pathParams.ReplaceAllString(hash, ""); hash != "" {
			route += "#" + hash
		}
	}
	lower := strings.ToLower(route)
	for _, mark := range credentialMarks {
		if strings.Contains(lower, mark) {
			return "", false
		}
	}
	return route, true
}

// shown is path as it may be printed: redacted when signed in, unchanged
// otherwise so an unauthenticated crawl logs exactly what it always did.
func (c *Crawler) shown(path string) string {
	if c.Auth != nil {
		return browser.RedactURL(path)
	}
	return path
}

func (c *Crawler) collectLinks(path string) ([]string, error) {
	// The 15s is the crawl's per-page budget; a login flow (reuse: false)
	// runs under its own, not inside it.
	session, err := c.Browser.NewAuthSession(c.Auth, c.BaseURL+path, c.Defaults.Viewport,
		c.Defaults.WaitFor, c.Defaults.SettleTime.Std(), 15*time.Second)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	raw, err := session.Eval(`() => [...document.querySelectorAll('a[href]')].map(a => a.href)`)
	if err != nil {
		return nil, err
	}
	var hrefs []string
	if err := unmarshalStrings(raw, &hrefs); err != nil {
		return nil, err
	}
	return hrefs, nil
}

// FilterLinks reduces raw hrefs to normalized same-origin paths. Hash-router
// routes (fragments starting with "/", e.g. /#/users) keep the fragment as
// part of the path identity — dropping it would collapse a whole HashRouter
// app into "/". Plain fragments and queries are dropped, trailing slashes
// trimmed (except root). Sorted for deterministic queueing.
func FilterLinks(base *url.URL, hrefs []string, excludes []*regexp.Regexp) []string {
	seen := map[string]bool{}
	var out []string
	for _, href := range hrefs {
		u, err := url.Parse(href)
		if err != nil {
			continue
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			continue
		}
		if u.Host != base.Host {
			continue
		}
		p := browser.RouteKey(u)
		if seen[p] || excluded(p, excludes) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

var numericSegRe = regexp.MustCompile(`/\d+(/|$)`)

// ShapeKey collapses numeric path segments (/systems/42 -> /systems/:n) so a
// crawl visits one representative per route shape instead of every record a
// data table links to.
func ShapeKey(path string) string {
	prev := ""
	for prev != path {
		prev = path
		path = numericSegRe.ReplaceAllString(path, "/:n$1")
	}
	return path
}

func excluded(path string, excludes []*regexp.Regexp) bool {
	for _, re := range excludes {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}
