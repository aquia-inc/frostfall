package github

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/aquia-inc/frostfall/internal/runner"
)

const Label = "frostfall"

// GitHub rejects issue bodies and comments over 65536 characters. The
// budgets cap only the element list (counted in bytes, so never optimistic);
// the rest of the body (summary, help link, run link, marker) fits in the
// headroom.
const (
	elementBudget = 50000
	commentBudget = 50000
)

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

// markerRe matches the marker on a line of its own, tolerating the CRLF a
// body edited in the web UI can come back with and stray indentation or
// trailing blanks from a hand edit (a missed marker would orphan the issue
// and file a duplicate). Anchoring keeps text quoted elsewhere in the body (a
// rule summary) from standing in for it.
var markerRe = regexp.MustCompile(`(?m)^[ \t]*<!-- frostfall:v1:rule=([^:\n]+):page=([^\n]+?) -->[ \t]*\r?$`)

// markerLineRe is markerRe for one already-split line.
var markerLineRe = regexp.MustCompile(`^[ \t]*<!-- frostfall:v1:rule=[^:]+:page=.+? -->[ \t]*$`)

// parseMarker extracts (rule, page) from an issue body, ok=false when the
// issue was not filed by frostfall. The generated marker is the last line of
// the body, so the last match wins.
func parseMarker(body string) (rule, page string, ok bool) {
	all := markerRe.FindAllStringSubmatch(body, -1)
	if len(all) == 0 {
		return "", "", false
	}
	m := all[len(all)-1]
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
		el := res.StableTarget
		if res.ScanLabel != "initial" {
			el += " (" + res.ScanLabel + ")"
		}
		el = normElement(el) // after the label: a step label can hold a break too
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
	els := normalize(g.Elements)
	var b strings.Builder
	fmt.Fprintf(&b, "Frostfall detected **%s** (%s) on **%s**.\n\n", g.Rule, g.Impact, g.Page)
	// One line, so the summary cannot start a line of its own that reads as
	// the element header or the marker.
	fmt.Fprintf(&b, "> %s\n\n", oneLine(g.Summary))
	fmt.Fprintf(&b, "%s\n", elementsHeader)
	used, shown := 0, 0
	for _, el := range els {
		line := "- " + codeSpan(el) + "\n"
		if used+len(line) > elementBudget {
			break
		}
		used += len(line)
		shown++
		b.WriteString(line)
	}
	if shown < len(els) {
		// The visible list is a prefix, so the next run cannot learn the
		// full set from it. The hidden record carries the count and a digest
		// of the whole set instead.
		fmt.Fprintf(&b, "\nand %d more not shown.\n%s\n", len(els)-shown, elementsRecord(els))
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

// maxBackticks caps a run of backticks in an element. GitHub's markdown
// renderer (cmark-gfm) gives up on an opening backtick run longer than 80
// (MAXBACKTICKS in inlines.c) and prints it as plain text, which would leave
// the rest of the selector as live markdown, @mentions included. A code span
// fenced one longer than the longest run inside it therefore needs runs well
// under 80. Real selectors never come near this, so a longer run is cut down
// to the cap rather than rendered some other way: both the filed element and
// the one read back go through normElement, so the cut text is what the
// issue's set holds and nothing churns.
const maxBackticks = 16

// normElement makes an element safe to render: one line, backtick runs capped.
func normElement(s string) string {
	s = oneLine(s)
	if !strings.Contains(s, strings.Repeat("`", maxBackticks+1)) {
		return s
	}
	var b strings.Builder
	run := 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > maxBackticks {
				continue
			}
		} else {
			run = 0
		}
		b.WriteRune(r)
	}
	return b.String()
}

// normalize normalizes and deduplicates a list of elements.
func normalize(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = normElement(e)
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// codeSpan renders s as a markdown code span that cannot be broken out of.
// Selectors are page-controlled text and can hold backticks (an attribute
// value like [data-x="a`b"]); a plain single-backtick span would end early
// and leave the rest as live markdown, including @mentions that notify
// people. The fence is one backtick longer than the longest run inside s,
// and s is padded with a space when a backtick at either end, or spaces at
// both ends, would otherwise be swallowed by the fence.
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
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") || stripsOneSpace(s) {
		pad = " "
	}
	return fence + pad + s + pad + fence
}

// stripsOneSpace reports whether a code span reader would drop one space from
// each end of s. CommonMark does that when s starts and ends with a space
// (U+0020 only) and is not all spaces.
func stripsOneSpace(s string) bool {
	return len(s) >= 2 && s[0] == ' ' && s[len(s)-1] == ' ' && strings.Trim(s, " ") != ""
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
	if stripsOneSpace(inner) {
		inner = inner[1 : len(inner)-1]
	}
	return inner, true
}

const elementsHeader = "Affected element(s):"

// elementsRecordRe is the hidden record of a truncated list: how many
// elements the full set has and a digest of it.
var elementsRecordRe = regexp.MustCompile(`^<!-- frostfall:elements:n=(\d+):sha=([0-9a-f]{16}) -->$`)

func elementsRecord(els []string) string {
	return fmt.Sprintf("<!-- frostfall:elements:n=%d:sha=%s -->", len(els), digest(els))
}

// digest is a short fingerprint of an element set, independent of order.
func digest(els []string) string {
	sorted := slices.Clone(els)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// elementList is what an existing issue body says about its elements.
type elementList struct {
	// ok is false when the list is missing or a bullet is not a code span (a
	// human rewrote it).
	ok  bool
	els []string // the visible bullets
	// truncated is set when the body carries the hidden record: the visible
	// bullets are then only a prefix of a set with total elements and the
	// given digest.
	truncated bool
	total     int
	sha       string
	// stale is set when a visible bullet is not rendered the way this version
	// renders that element.
	stale bool
}

// parseElements reads the element list back out of an issue body written by
// Body. The structure is anchored from the marker, which is the last line of
// a generated body: the list is the last header line above it, so text a
// rule summary or a human put earlier cannot stand in for it.
//
// The visible list is the record for untruncated bodies (and for those filed
// by older versions). A second hidden copy of every selector would double the
// body and go stale when someone edits the visible list.
func parseElements(body string) elementList {
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	end := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if markerLineRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	start := -1
	for i := end - 1; i >= 0; i-- {
		if lines[i] == elementsHeader {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return elementList{}
	}
	var out elementList
	i := start
	for ; i < end; i++ {
		l := strings.TrimRight(lines[i], " \t")
		if l == "" {
			break
		}
		rest, found := strings.CutPrefix(l, "- ")
		if !found {
			return elementList{}
		}
		el, good := parseCodeSpan(rest)
		if !good {
			return elementList{}
		}
		el = normElement(el)
		// A bullet written by an older version (a plain single-backtick span
		// around a selector that holds a backtick) reads back as the right
		// element but renders broken. Flag any bullet this version would not
		// write the same way, so the body gets rewritten once.
		if "- "+codeSpan(el) != l {
			out.stale = true
		}
		out.els = append(out.els, el)
	}
	out.ok = true
	for ; i < end; i++ {
		if m := elementsRecordRe.FindStringSubmatch(lines[i]); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return elementList{}
			}
			out.truncated, out.total, out.sha = true, n, m[2]
		}
	}
	return out
}

// change is what a matched issue needs: the elements its body should list,
// which of them the comment announces, and what silently dropped out.
type change struct {
	elements   []string
	added      []string // new elements, nameable (the old list was complete)
	unnamed    int      // growth known only as a count (the old list was truncated)
	removed    int
	rewrite    bool
	unreadable bool // the old list could not be read, so nothing is claimed new
}

// diffElements compares the desired element set with what an issue body
// says. partial means this run could not see every frame of the page: the
// body then grows (existing plus new) and never shrinks, because a missing
// element may be unchecked rather than fixed.
func diffElements(desired []string, cur elementList, partial bool) change {
	want := normalize(desired)
	if !cur.ok {
		if partial {
			// Writing this run's list would put a known-incomplete one in the
			// body, and the next complete scan would announce the missing
			// frame's elements as new. Leave it for a complete scan.
			return change{}
		}
		return change{elements: want, rewrite: true, unreadable: true}
	}
	if cur.truncated {
		if partial {
			// The hidden part of the old list is unknown, so a union cannot
			// be written and nothing can be named. Leave it for a full scan.
			return change{}
		}
		n := len(want)
		// The digest covers the count; n is kept for the growth/shrink split.
		if digest(want) == cur.sha {
			// Same set. A bullet rendered the old way is rewritten quietly.
			return change{elements: want, rewrite: cur.stale}
		}
		c := change{elements: want, rewrite: true}
		if n > cur.total {
			c.unnamed = n - cur.total
		} else {
			c.removed = cur.total - n
		}
		return c
	}
	have := map[string]bool{}
	for _, e := range cur.els {
		have[e] = true
	}
	wantSet := map[string]bool{}
	var added []string
	for _, e := range want {
		wantSet[e] = true
		if !have[e] {
			added = append(added, e)
		}
	}
	removed := 0
	for e := range have {
		if !wantSet[e] {
			removed++
		}
	}
	if partial {
		return change{
			elements: append(normalize(cur.els), added...),
			added:    added,
			rewrite:  len(added) > 0 || cur.stale,
		}
	}
	return change{elements: want, added: added, removed: removed, rewrite: len(added) > 0 || removed > 0 || cur.stale}
}

// Action is one planned issue operation, exposed for dry-run output.
type Action struct {
	Kind   string // create | reopen | update | close
	Title  string
	Marker string // group identity for create/reopen/update; "" for close
	Number int    // 0 for create
	// Set on update and reopen when the issue body must be rewritten.
	Rewrite  bool
	Elements []string // the elements the rewritten body lists
	Added    []string // elements newly failing, named in the comment
	Unnamed  int      // growth known only as a count, announced without names
	Removed  int      // elements that no longer fail, updated silently
	// Unreadable: the old list could not be read, so the body is rewritten
	// and nothing is announced.
	Unreadable bool
}

func (a Action) String() string {
	s := fmt.Sprintf("%s %s", a.Kind, a.Title)
	if a.Number > 0 {
		s = fmt.Sprintf("%s #%d %s", a.Kind, a.Number, a.Title)
	}
	switch {
	case a.Unreadable:
		s += " (rewrite body: element list unreadable)"
	case len(a.Added)+a.Unnamed > 0 || a.Removed > 0:
		s += fmt.Sprintf(" (+%d -%d elements)", len(a.Added)+a.Unnamed, a.Removed)
	case a.Rewrite:
		s += " (rewrite body)"
	}
	return s
}

// Plan reconciles desired groups against existing frostfall issues.
// covered scopes closing: an issue is closed as fixed only when this run
// actually looked for its rule on its page — a filtered run (--id) must not
// close issues for pages it never looked at, nor a keyboard-only test close
// axe issues. partial (nil means never) scopes removals and closing the same
// way: on a page where a scan left a frame out, elements missing from the
// results may be unchecked rather than fixed, so the issue's list only grows
// and the issue stays open. covered
// does not catch that case, since another complete scan of the page still
// covers the rule.
func Plan(groups []Group, existing []Issue, covered, partial func(page, rule string) bool) []Action {
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
			ch := diffElements(g.Elements, parseElements(issue.Body), partial != nil && partial(page, rule))
			base := Action{Title: g.Title(), Marker: marker, Number: issue.Number,
				Rewrite: ch.rewrite, Elements: ch.elements, Added: ch.added,
				Unnamed: ch.unnamed, Removed: ch.removed, Unreadable: ch.unreadable}
			switch {
			case issue.State == "closed":
				base.Kind = "reopen"
				actions = append(actions, base)
			case ch.rewrite:
				base.Kind = "update"
				actions = append(actions, base)
			}
			continue
		}
		// A partial scan can lose the very elements that keep the issue open,
		// so it never closes one.
		if issue.State == "open" && covered(page, rule) && !(partial != nil && partial(page, rule)) {
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
func Sync(c *Client, groups []Group, existing []Issue, covered, partial func(page, rule string) bool, runContext string) ([]Action, error) {
	byMarker := map[string]Group{}
	for _, g := range groups {
		byMarker[g.Marker()] = g
	}
	actions := Plan(groups, existing, covered, partial)
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

// syncExisting applies an update or reopen, body first.
//
// Naming new elements in a comment is only honest once the body lists them,
// and only worth doing once: if the body edit fails (422 on size, a locked
// issue, a 403, a rate limit) and the comment went out anyway, every later
// run would repeat it. So the announcement waits for a successful body edit.
// The cost is the reverse gap: a comment that fails after a good edit is
// never retried, because the next run finds the body already current.
//
// A failed body edit must not keep a recurrence closed, so a reopen still
// posts its plain "Recurred." comment and sets the state to open (as before
// body edits existed), and the body error is returned alongside.
func syncExisting(c *Client, a Action, g Group, runContext string) error {
	var errs []string
	bodyOK := true
	if a.Rewrite {
		g.Elements = a.Elements
		if err := c.SetBody(a.Number, g.Body(runContext)); err != nil {
			bodyOK = false
			errs = append(errs, "edit body: "+err.Error())
		}
	}
	var news string
	if bodyOK {
		news = announcement(a)
	}
	var note string
	switch {
	case a.Kind == "reopen" && news != "":
		note = "Recurred.\n\n" + news + "\n\n" + runContext
	case news != "":
		note = news + "\n\n" + runContext
	case a.Kind == "reopen":
		note = "Recurred. " + runContext
	}
	if note != "" {
		if err := c.Comment(a.Number, note); err != nil {
			errs = append(errs, "comment: "+err.Error())
			return errors.New(strings.Join(errs, "; "))
		}
	}
	if a.Kind == "reopen" {
		if err := c.SetState(a.Number, "open"); err != nil {
			errs = append(errs, "reopen: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// announcement is the comment text for newly failing elements, "" when there
// are none. It names them when it can (within commentBudget) and gives only a
// count when the old list was truncated, since then nothing says which
// elements are new.
func announcement(a Action) string {
	plural := func(n int) string {
		if n == 1 {
			return "element"
		}
		return "elements"
	}
	if len(a.Added) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d new failing %s:\n\n", len(a.Added), plural(len(a.Added)))
		used, shown := 0, 0
		for _, el := range a.Added {
			line := "- " + codeSpan(el) + "\n"
			if used+len(line) > commentBudget {
				break
			}
			used += len(line)
			shown++
			b.WriteString(line)
		}
		if shown < len(a.Added) {
			fmt.Fprintf(&b, "\nand %d more not shown.\n", len(a.Added)-shown)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	if a.Unnamed > 0 {
		return fmt.Sprintf("%d more failing %s (list truncated; see the issue body).", a.Unnamed, plural(a.Unnamed))
	}
	return ""
}
