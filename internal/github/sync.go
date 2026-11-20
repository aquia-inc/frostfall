package github

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/aquia-inc/frostfall/internal/runner"
)

const Label = "frostfall"

// Group is one issue-shaped unit: a rule failing on a page. Deliberately
// coarser than the baseline fingerprint — axe reporting the same defect on
// seven grid rows is one bug for one developer, not seven issues.
type Group struct {
	Rule    string
	Page    string // test id
	Impact  string
	Summary string
	HelpURL string
	// Elements are "selector (scan label)" lines, deduplicated.
	Elements []string
}

// Marker returns the hidden dedup comment embedded in the issue body.
func (g Group) Marker() string {
	return fmt.Sprintf("<!-- frostfall:v1:rule=%s:page=%s -->", g.Rule, g.Page)
}

var markerRe = regexp.MustCompile(`<!-- frostfall:v1:rule=([^:]+):page=(.+?) -->`)

// parseMarker extracts (rule, page) from an issue body, ok=false when the
// issue was not filed by frostfall.
func parseMarker(body string) (rule, page string, ok bool) {
	m := markerRe.FindStringSubmatch(body)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// BuildGroups collapses failing results into issue-shaped groups, keyed by
// rule + page, ordered deterministically.
func BuildGroups(results []runner.Result) []Group {
	byKey := map[string]*Group{}
	var order []string
	for _, res := range results {
		key := res.RuleID + "\x00" + res.TestID
		g, ok := byKey[key]
		if !ok {
			g = &Group{
				Rule:    res.RuleID,
				Page:    res.TestID,
				Impact:  res.Impact.String(),
				Summary: res.Summary,
				HelpURL: res.HelpURL,
			}
			byKey[key] = g
			order = append(order, key)
		}
		el := oneLine(res.StableTarget)
		if res.ScanLabel != "initial" {
			el += " (" + res.ScanLabel + ")"
		}
		if !slices.Contains(g.Elements, el) {
			g.Elements = append(g.Elements, el)
		}
	}
	sort.Strings(order)
	out := make([]Group, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out
}

func (g Group) Title() string {
	return fmt.Sprintf("Accessibility: %s on %s", g.Rule, g.Page)
}

func (g Group) Body(runContext string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Frostfall detected **%s** (%s) on **%s**.\n\n", g.Rule, g.Impact, g.Page)
	fmt.Fprintf(&b, "> %s\n\n", g.Summary)
	fmt.Fprintf(&b, "Affected element(s):\n")
	for _, el := range g.Elements {
		fmt.Fprintf(&b, "- %s\n", codeSpan(el))
	}
	if g.HelpURL != "" {
		fmt.Fprintf(&b, "\n[How to fix %s](%s)\n", g.Rule, g.HelpURL)
	}
	if runContext != "" {
		fmt.Fprintf(&b, "\n%s\n", runContext)
	}
	fmt.Fprintf(&b, "\n%s\n", g.Marker())
	return b.String()
}

// oneLine collapses line breaks to spaces. Selectors come from the page under
// test, and a break inside one would split its bullet in the issue body.
func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// codeSpan renders s as a markdown code span that cannot be broken out of.
// Selectors are page-controlled text and can hold backticks (an attribute
// value like [data-x="a`b"]); a plain single-backtick span would end early
// and leave the rest as live markdown, including @mentions that notify
// people. The fence is one backtick longer than the longest run inside s,
// and s is padded with a space when a backtick or surrounding spaces would
// otherwise be swallowed by the fence.
func codeSpan(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	pad := ""
	if s != "" && (strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") ||
		(strings.HasPrefix(s, " ") && strings.HasSuffix(s, " "))) {
		pad = " "
	}
	return fence + pad + s + pad + fence
}

// parseCodeSpan reverses codeSpan for one bullet's text.
func parseCodeSpan(t string) (string, bool) {
	n := 0
	for n < len(t) && t[n] == '`' {
		n++
	}
	if n == 0 {
		return "", false
	}
	if n == len(t) { // only backticks: an empty span is an even-length run
		if n%2 != 0 {
			return "", false
		}
		return "", true
	}
	fence := t[:n]
	if !strings.HasSuffix(t, fence) || len(t) < 2*n {
		return "", false
	}
	inner := t[n : len(t)-n]
	if strings.HasPrefix(inner, "`") || strings.HasSuffix(inner, "`") {
		return "", false // the closing run is longer than the opening one
	}
	if len(inner) >= 2 && strings.HasPrefix(inner, " ") && strings.HasSuffix(inner, " ") &&
		strings.TrimSpace(inner) != "" {
		inner = inner[1 : len(inner)-1]
	}
	return inner, true
}

const elementsHeader = "Affected element(s):"

// parseElements reads the element list back out of an issue body written by
// Body. ok=false when the list is missing or a bullet is not a code span
// (a human rewrote the section), in which case the caller refreshes the body
// without claiming anything is new.
//
// The visible list is the record. A second hidden copy would duplicate every
// selector in the body (GitHub caps bodies at 65536 characters) and would go
// stale the moment someone edits the visible list, so reading what the issue
// actually shows is both simpler and honest about it.
func parseElements(body string) (els []string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == elementsHeader {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, false
	}
	for _, l := range lines[start:] {
		if strings.TrimSpace(l) == "" {
			break
		}
		rest, found := strings.CutPrefix(strings.TrimRight(l, " \t"), "- ")
		if !found {
			return nil, false
		}
		el, good := parseCodeSpan(rest)
		if !good {
			return nil, false
		}
		els = append(els, el)
	}
	return els, true
}

// diffElements compares the desired element set with the one in an issue
// body. added keeps desired order; removed counts elements that no longer
// fail. changed is true when the body needs rewriting. When cur could not be
// read (known=false) it is always true and nothing counts as added.
func diffElements(desired, cur []string, known bool) (added []string, removed int, changed bool) {
	if !known {
		return nil, 0, true
	}
	have := map[string]bool{}
	for _, e := range cur {
		have[e] = true
	}
	want := map[string]bool{}
	for _, e := range desired {
		if want[e] {
			continue
		}
		want[e] = true
		if !have[e] {
			added = append(added, e)
		}
	}
	for e := range have {
		if !want[e] {
			removed++
		}
	}
	return added, removed, len(added) > 0 || removed > 0
}

// Action is one planned issue operation, exposed for dry-run output.
type Action struct {
	Kind   string // create | reopen | update | close
	Title  string
	Marker string // group identity for create/reopen/update; "" for close
	Number int    // 0 for create
	// Set on update and reopen when the issue body lists a different element
	// set than this run found.
	Rewrite bool
	Added   []string // elements newly failing, announced in the comment
	Removed int      // elements that no longer fail, updated silently
}

func (a Action) String() string {
	s := fmt.Sprintf("%s %s", a.Kind, a.Title)
	if a.Number > 0 {
		s = fmt.Sprintf("%s #%d %s", a.Kind, a.Number, a.Title)
	}
	switch {
	case len(a.Added) > 0 || a.Removed > 0:
		s += fmt.Sprintf(" (+%d -%d elements)", len(a.Added), a.Removed)
	case a.Rewrite:
		s += " (refresh element list)"
	}
	return s
}

// Plan reconciles desired groups against existing frostfall issues.
// covered scopes closing: an issue is closed as fixed only when this run
// actually looked for its rule on its page — a filtered run (--id) must not
// close issues for pages it never looked at, nor a keyboard-only test close
// axe issues.
func Plan(groups []Group, existing []Issue, covered func(page, rule string) bool) []Action {
	desired := map[string]Group{}
	for _, g := range groups {
		desired[g.Marker()] = g
	}
	handled := map[string]bool{}
	var actions []Action

	for _, issue := range existing {
		rule, page, ok := parseMarker(issue.Body)
		if !ok {
			continue
		}
		marker := Group{Rule: rule, Page: page}.Marker()
		if g, want := desired[marker]; want {
			handled[marker] = true
			cur, known := parseElements(issue.Body)
			added, removed, changed := diffElements(g.Elements, cur, known)
			switch {
			case issue.State == "closed":
				actions = append(actions, Action{Kind: "reopen", Title: g.Title(), Marker: marker, Number: issue.Number,
					Rewrite: changed, Added: added, Removed: removed})
			case changed:
				actions = append(actions, Action{Kind: "update", Title: g.Title(), Marker: marker, Number: issue.Number,
					Rewrite: true, Added: added, Removed: removed})
			}
			continue
		}
		if issue.State == "open" && covered(page, rule) {
			actions = append(actions, Action{Kind: "close", Title: issue.Title, Number: issue.Number})
		}
	}
	for _, g := range groups {
		if !handled[g.Marker()] {
			actions = append(actions, Action{Kind: "create", Title: g.Title(), Marker: g.Marker()})
		}
	}
	return actions
}

// Sync executes a plan. runContext is a one-line provenance note added to
// bodies and comments (e.g. a link to the CI run). Failures are collected,
// not fatal: filing issues must never break the scan itself.
func Sync(c *Client, groups []Group, existing []Issue, covered func(page, rule string) bool, runContext string) ([]Action, error) {
	byMarker := map[string]Group{}
	for _, g := range groups {
		byMarker[g.Marker()] = g
	}
	actions := Plan(groups, existing, covered)
	var errs []string
	for _, a := range actions {
		var err error
		switch a.Kind {
		case "create":
			g, ok := byMarker[a.Marker]
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: no group for marker", a))
				continue
			}
			_, err = c.CreateIssue(g.Title(), g.Body(runContext), []string{Label, "accessibility"})
		case "update", "reopen":
			g, ok := byMarker[a.Marker]
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: no group for marker", a))
				continue
			}
			err = syncExisting(c, a, g, runContext)
		case "close":
			if err = c.Comment(a.Number, "No longer detected. "+runContext); err == nil {
				err = c.SetState(a.Number, "closed")
			}
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", a, err))
		}
	}
	if len(errs) > 0 {
		return actions, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return actions, nil
}

// syncExisting applies an update or reopen. The comment goes first: if the
// body edit then fails, the next run re-announces the same elements (a
// duplicate comment) rather than losing them (a silent gap). One comment per
// action, so a reopen with new elements folds into "Recurred.".
func syncExisting(c *Client, a Action, g Group, runContext string) error {
	var note string
	if a.Kind == "reopen" {
		note = "Recurred."
	}
	if len(a.Added) > 0 {
		noun := "elements"
		if len(a.Added) == 1 {
			noun = "element"
		}
		var b strings.Builder
		if note != "" {
			b.WriteString(note + "\n\n")
		}
		fmt.Fprintf(&b, "%d new failing %s:\n\n", len(a.Added), noun)
		for _, el := range a.Added {
			fmt.Fprintf(&b, "- %s\n", codeSpan(el))
		}
		fmt.Fprintf(&b, "\n%s", runContext)
		note = b.String()
	} else if note != "" {
		note += " " + runContext
	}
	if note != "" {
		if err := c.Comment(a.Number, note); err != nil {
			return err
		}
	}
	if a.Rewrite {
		if err := c.SetBody(a.Number, g.Body(runContext)); err != nil {
			return err
		}
	}
	if a.Kind == "reopen" {
		return c.SetState(a.Number, "open")
	}
	return nil
}
