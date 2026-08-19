// Package discover crawls same-origin links to synthesize page-coverage
// tests. It complements, never replaces, explicit tests: a crawler finds
// pages nobody thought to list, but only hand-written steps reach the states
// (modals, menus, error forms) where most violations live.
package discover

import (
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
	Log      func(format string, args ...any)
}

// Run crawls breadth-first from the root path and returns discovered paths in
// visit order, root first. Pages that fail to load are skipped, not fatal:
// partial discovery beats none.
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

	type node struct {
		path  string
		depth int
	}
	queue := []node{{path: "/", depth: 0}}
	seen := map[string]bool{"/": true}
	seenShape := map[string]bool{ShapeKey("/"): true}
	var found []string

	for len(queue) > 0 && len(found) < c.Options.MaxPages {
		n := queue[0]
		queue = queue[1:]
		found = append(found, n.path)
		if n.depth >= c.Options.MaxDepth {
			continue
		}

		links, err := c.collectLinks(n.path)
		if err != nil {
			if c.Log != nil {
				c.Log("discover: skipping %s: %v", n.path, err)
			}
			continue
		}
		for _, p := range FilterLinks(base, links, excludes) {
			shape := ShapeKey(p)
			if !seen[p] && !seenShape[shape] {
				seen[p] = true
				seenShape[shape] = true
				queue = append(queue, node{path: p, depth: n.depth + 1})
			}
		}
	}
	return found, nil
}

func (c *Crawler) collectLinks(path string) ([]string, error) {
	session, err := c.Browser.NewSession(c.BaseURL+path, c.Defaults.Viewport,
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
		p := u.Path
		if p == "" {
			p = "/"
		}
		if p != "/" {
			p = strings.TrimSuffix(p, "/")
		}
		if frag := u.Fragment; strings.HasPrefix(frag, "/") && frag != "/" {
			p += "#" + strings.TrimSuffix(frag, "/")
		}
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
