// Package engine defines the audit engine contract. Fingerprinting, baselining,
// and formatting consume normalized Violations and know nothing about any
// specific engine; new engines (Equal Access, Lighthouse) slot in behind this
// interface without touching downstream code.
package engine

import (
	"context"
	"encoding/json"
)

// Impact is the normalized severity scale, ordered least to most severe.
type Impact int

const (
	Minor Impact = iota
	Moderate
	Serious
	Critical
)

var impactNames = [...]string{"minor", "moderate", "serious", "critical"}

func (i Impact) String() string {
	if i < Minor || i > Critical {
		return "unknown"
	}
	return impactNames[i]
}

// ParseImpact returns the Impact for its lowercase name.
func ParseImpact(s string) (Impact, bool) {
	for i, n := range impactNames {
		if n == s {
			return Impact(i), true
		}
	}
	return 0, false
}

// ScanOptions carries the per-scan configuration an engine receives.
type ScanOptions struct {
	// Standard is the conformance tag set, e.g. "wcag21aa".
	Standard string
	// Scope is a CSS selector limiting the scan context; empty means document.
	Scope string
	// Rules toggles individual rules by engine rule id.
	Rules map[string]bool
}

// Violation is one engine finding, normalized across engines.
type Violation struct {
	// RuleID is engine-namespaced when the engine is not axe ("equal-access/...").
	RuleID string
	Impact Impact
	// Target is the raw selector from the engine; the fingerprinter normalizes it.
	Target  string
	Summary string
	// Help is the engine's short human remediation text ("Images must have
	// alternative text"), distinct from the Summary description.
	Help    string
	HelpURL string
	// HTML is the offending node snippet, for reports only. Never fingerprinted.
	HTML string
	// Tags are the engine's rule tags (axe: wcag143, section508, cat.aria),
	// used to derive WCAG/508 criterion ids for reports. Never fingerprinted.
	Tags []string
}

// Page is the minimal browser surface an engine needs, implemented by the
// rod session.
type Page interface {
	// Eval evaluates a JS expression in the page and returns its JSON result.
	Eval(js string, args ...any) (json.RawMessage, error)
	// InjectScript evaluates a script source in the page (idempotent injection
	// is the caller's concern).
	InjectScript(src string) error
}

// Engine audits a page that the runner has already navigated to readiness.
type Engine interface {
	Name() string
	// Version is the embedded engine version, e.g. "4.10.3". Recorded in the
	// baseline: fingerprints are only comparable across identical versions.
	Version() string
	// Audit runs the engine in the given page. It must not navigate the page.
	Audit(ctx context.Context, page Page, opts ScanOptions) ([]Violation, error)
}
