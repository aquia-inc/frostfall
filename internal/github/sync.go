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
		el := res.StableTarget
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
		fmt.Fprintf(&b, "- `%s`\n", el)
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

// Action is one planned issue operation, exposed for dry-run output.
type Action struct {
	Kind   string // create | reopen | close
	Title  string
	Marker string // group identity for create/reopen; "" for close
	Number int    // 0 for create
}

func (a Action) String() string {
	if a.Number > 0 {
		return fmt.Sprintf("%s #%d %s", a.Kind, a.Number, a.Title)
	}
	return fmt.Sprintf("%s %s", a.Kind, a.Title)
}

// Plan reconciles desired groups against existing frostfall issues.
// testsRun scopes closing: an issue is closed as fixed only when its page was
// actually scanned this run — a filtered run (--id) must not close issues for
// pages it never looked at.
func Plan(groups []Group, existing []Issue, testsRun map[string]bool) []Action {
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
			if issue.State == "closed" {
				actions = append(actions, Action{Kind: "reopen", Title: g.Title(), Marker: marker, Number: issue.Number})
			}
			continue
		}
		if issue.State == "open" && testsRun[page] {
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
func Sync(c *Client, groups []Group, existing []Issue, testsRun map[string]bool, runContext string) ([]Action, error) {
	byMarker := map[string]Group{}
	for _, g := range groups {
		byMarker[g.Marker()] = g
	}
	actions := Plan(groups, existing, testsRun)
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
		case "reopen":
			if err = c.Comment(a.Number, "Recurred. "+runContext); err == nil {
				err = c.SetState(a.Number, "open")
			}
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
