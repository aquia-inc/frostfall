package keyboard

import (
	"strings"

	"github.com/aquia-inc/frostfall/internal/engine"
)

// Rule is the static metadata for one keyboard finding. Tags use axe's
// format (wcag212) so wcag.Label maps them to criteria like any axe rule.
type Rule struct {
	ID      string
	Impact  engine.Impact
	Summary string
	Help    string
	HelpURL string
	Tags    []string
}

// Rule ids are namespaced so they can never collide with an axe rule, and so
// config validation can tell a typo'd keyboard rule from an axe one.
const (
	Prefix          = "frostfall/"
	RuleTrap        = Prefix + "keyboard-trap"
	RuleUnreachable = Prefix + "focus-unreachable"
	RuleNotVisible  = Prefix + "focus-not-visible"
	RuleHidden      = Prefix + "focus-hidden"
	RuleDialog      = Prefix + "dialog-focus"
	// RuleFocusExpectation is an expectFocus step that did not hold. It is
	// registered here, with the other frostfall/ rules, so it validates and
	// toggles like them.
	RuleFocusExpectation = Prefix + "focus-expectation"
)

var rules = map[string]Rule{
	RuleTrap: {
		ID:      RuleTrap,
		Impact:  engine.Critical,
		Summary: "Pressing Tab must keep moving focus until it leaves the page",
		Help:    "Keyboard focus must not be trapped",
		HelpURL: "https://www.w3.org/WAI/WCAG22/Understanding/no-keyboard-trap.html",
		Tags:    []string{"wcag2a", "wcag212"},
	},
	RuleUnreachable: {
		ID:      RuleUnreachable,
		Impact:  engine.Serious,
		Summary: "Every visible control in the tab order must be reachable by pressing Tab",
		Help:    "Interactive elements must be reachable by keyboard",
		HelpURL: "https://www.w3.org/WAI/WCAG22/Understanding/keyboard.html",
		Tags:    []string{"wcag2a", "wcag211"},
	},
	RuleNotVisible: {
		ID:      RuleNotVisible,
		Impact:  engine.Serious,
		Summary: "An element must look different while it has keyboard focus",
		Help:    "Keyboard focus must be visible",
		HelpURL: "https://www.w3.org/WAI/WCAG22/Understanding/focus-visible.html",
		Tags:    []string{"wcag2aa", "wcag247"},
	},
	RuleHidden: {
		ID:      RuleHidden,
		Impact:  engine.Serious,
		Summary: "Keyboard focus must not land on an element the user cannot see",
		Help:    "Focusable elements must be visible when focused",
		HelpURL: "https://www.w3.org/WAI/WCAG22/Understanding/focus-order.html",
		Tags:    []string{"wcag2a", "wcag243", "wcag2aa", "wcag247"},
	},
	RuleFocusExpectation: {
		ID:      RuleFocusExpectation,
		Impact:  engine.Serious,
		Summary: "Focus must be where the config's expectFocus step says it is",
		Help:    "Focus must move where the interaction should send it",
		HelpURL: "https://www.w3.org/WAI/WCAG22/Understanding/focus-order.html",
		Tags:    []string{"wcag2a", "wcag243"},
	},
	RuleDialog: {
		ID:      RuleDialog,
		Impact:  engine.Serious,
		Summary: "An open modal dialog must hold keyboard focus until it closes",
		Help:    "Focus must move into and stay within an open modal dialog",
		HelpURL: "https://www.w3.org/WAI/WCAG22/Understanding/focus-order.html",
		Tags:    []string{"wcag2a", "wcag243"},
	},
}

// NewViolation builds a finding for one of these rules; summary replaces the
// rule's generic one when set.
func NewViolation(rule, target, html, summary string) engine.Violation {
	r := rules[rule]
	if summary == "" {
		summary = r.Summary
	}
	return engine.Violation{
		RuleID: r.ID, Impact: r.Impact, Target: target, Summary: summary,
		Help: r.Help, HelpURL: r.HelpURL, HTML: html, Tags: r.Tags,
	}
}

// IsRule reports whether id names a keyboard rule (namespaced or not).
func IsRule(id string) bool { return strings.HasPrefix(id, Prefix) }

// Known reports whether id is a keyboard rule this binary implements.
func Known(id string) bool {
	_, ok := rules[id]
	return ok
}
