// Package axe adapts the embedded axe-core scanner to the engine interface.
package axe

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/aquia-inc/frostfall/internal/engine"
)

//go:embed axe.min.js
var axeSource string

var versionRe = regexp.MustCompile(`axe v(\d+\.\d+\.\d+)`)

// Engine runs axe-core inside the page over CDP.
type Engine struct{}

func New() *Engine { return &Engine{} }

func (e *Engine) Name() string { return "axe" }

// Version is parsed from the embedded bundle's banner so it can never drift
// from what actually runs.
func (e *Engine) Version() string {
	if m := versionRe.FindStringSubmatch(axeSource[:200]); m != nil {
		return m[1]
	}
	return "unknown"
}

// standardTags expands a conformance target into axe's cumulative tag sets.
// axe tags rules with only the level that introduced them, so "wcag21aa"
// alone would skip every WCAG 2.0 rule — the tag set must be cumulative.
// section508 pairs the dedicated axe tag with WCAG 2.0 AA, which is what the
// 508 refresh incorporates by reference.
func standardTags(standard string) []string {
	switch standard {
	case "wcag2a":
		return []string{"wcag2a"}
	case "wcag2aa":
		return []string{"wcag2a", "wcag2aa"}
	case "wcag21a":
		return []string{"wcag2a", "wcag21a"}
	case "wcag21aa":
		return []string{"wcag2a", "wcag2aa", "wcag21a", "wcag21aa"}
	case "wcag22aa":
		return []string{"wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"}
	case "section508":
		return []string{"section508", "wcag2a", "wcag2aa"}
	case "best-practice":
		return []string{"best-practice"}
	}
	return nil
}

// axeResult mirrors the subset of axe.run() output we consume.
type axeResult struct {
	Violations []struct {
		ID          string `json:"id"`
		Impact      string `json:"impact"`
		Description string `json:"description"`
		HelpURL     string `json:"helpUrl"`
		Nodes       []struct {
			Target []json.RawMessage `json:"target"`
			HTML   string            `json:"html"`
		} `json:"nodes"`
	} `json:"violations"`
}

func (e *Engine) Audit(ctx context.Context, page engine.Page, opts engine.ScanOptions) ([]engine.Violation, error) {
	if err := page.InjectScript(axeSource); err != nil {
		return nil, fmt.Errorf("injecting axe: %w", err)
	}

	runOpts := map[string]any{
		"resultTypes": []string{"violations"},
	}
	if tags := standardTags(opts.Standard); tags != nil {
		runOpts["runOnly"] = map[string]any{"type": "tag", "values": tags}
	}
	if len(opts.Rules) > 0 {
		rules := map[string]any{}
		for id, enabled := range opts.Rules {
			rules[id] = map[string]bool{"enabled": enabled}
		}
		runOpts["rules"] = rules
	}
	optsJSON, err := json.Marshal(runOpts)
	if err != nil {
		return nil, err
	}

	context := "document"
	if opts.Scope != "" {
		scopeJSON, _ := json.Marshal(opts.Scope)
		context = string(scopeJSON)
	}

	js := fmt.Sprintf("() => window.axe.run(%s, %s)", context, optsJSON)
	raw, err := page.Eval(js)
	if err != nil {
		return nil, fmt.Errorf("running axe: %w", err)
	}

	var res axeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parsing axe results: %w", err)
	}

	var out []engine.Violation
	for _, v := range res.Violations {
		impact := impactOrDefault(v.Impact)
		for _, n := range v.Nodes {
			out = append(out, engine.Violation{
				RuleID:  v.ID,
				Impact:  impact,
				Target:  flattenTarget(n.Target),
				Summary: v.Description,
				HelpURL: v.HelpURL,
				HTML:    n.HTML,
			})
		}
	}
	return out, nil
}

// impactOrDefault maps an axe impact string to the normalized scale. An
// unknown or null impact defaults to Serious, never below the default
// enforcement floor: if the parser and axe ever disagree, violations must
// fail loud rather than silently slip under the gate.
func impactOrDefault(s string) engine.Impact {
	if impact, ok := engine.ParseImpact(s); ok {
		return impact
	}
	return engine.Serious
}

// flattenTarget joins axe's target array (selectors, possibly nested for
// iframes/shadow DOM) into a single selector path string.
func flattenTarget(parts []json.RawMessage) string {
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		var s string
		if err := json.Unmarshal(p, &s); err == nil {
			segs = append(segs, s)
			continue
		}
		var nested []string
		if err := json.Unmarshal(p, &nested); err == nil {
			segs = append(segs, strings.Join(nested, " >>> "))
		}
	}
	return strings.Join(segs, " ")
}
