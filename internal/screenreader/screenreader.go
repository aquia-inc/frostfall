// Package screenreader renders what a screen reader would announce, read
// from Chrome's computed accessibility tree: the role, name, and state that
// JAWS, NVDA, and VoiceOver get from the browser. It is not a screen reader -
// no speech rules, verbosity settings, or virtual-cursor quirks - but it lets
// a reviewer read what one would say without running one, and lets a config
// assert where focus is.
package screenreader

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-rod/rod/lib/proto"
)

// Page is the browser surface this package needs.
type Page interface {
	Eval(js string, args ...any) (json.RawMessage, error)
	AXTree() ([]*proto.AccessibilityAXNode, error)
	// FocusedAX is the focused element's node, nil when nothing has focus.
	FocusedAX() (*proto.AccessibilityAXNode, error)
	// BackendNodeID resolves a CSS selector to its DOM node, for scoping.
	BackendNodeID(selector string) (proto.DOMBackendNodeID, error)
}

// MaxLines caps a transcript; a data grid can otherwise run to thousands of
// lines. Scope the transcript to the part of the page under review instead.
const MaxLines = 2000

// Roles that announce nothing themselves; their children read in place.
var passThrough = map[string]bool{
	"generic": true, "none": true, "presentation": true, "RootWebArea": true,
	"LineBreak": true, "InlineTextBox": true, "LabelText": true, "paragraph": true,
	"listitem": true, "row": true, "rowgroup": true, "strong": true, "emphasis": true,
	"mark": true, "code": true, "time": true, "Abbr": true, "superscript": true,
	"subscript": true, "deletion": true, "insertion": true, "ListMarker": true,
}

// Roles whose name is their content: once named, their subtree would only
// repeat it.
var leaf = map[string]bool{
	"button": true, "link": true, "heading": true, "menuitem": true,
	"menuitemcheckbox": true, "menuitemradio": true, "tab": true, "option": true,
	"checkbox": true, "radio": true, "switch": true, "treeitem": true, "cell": true,
	"gridcell": true, "columnheader": true, "rowheader": true, "image": true,
	"img": true, "textbox": true, "searchbox": true, "combobox": true,
	"spinbutton": true, "slider": true, "progressbar": true, "meter": true,
}

// Roles a user operates, which are unusable without a name.
var needsName = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true,
	"combobox": true, "checkbox": true, "radio": true, "switch": true,
	"menuitem": true, "menuitemcheckbox": true, "menuitemradio": true, "tab": true,
	"slider": true, "spinbutton": true, "option": true, "treeitem": true,
	"image": true, "img": true, "dialog": true, "alertdialog": true,
}

// states renders a node's announced states in a fixed order.
func states(n *proto.AccessibilityAXNode) []string {
	props := map[string]string{}
	for _, p := range n.Properties {
		if p.Value != nil {
			props[string(p.Name)] = strings.Trim(p.Value.Value.JSON("", ""), `"`)
		}
	}
	var out []string
	if v := props["level"]; v != "" {
		out = append(out, "level "+v)
	}
	switch props["checked"] {
	case "true":
		out = append(out, "checked")
	case "false":
		out = append(out, "not checked")
	case "mixed":
		out = append(out, "partially checked")
	}
	switch props["pressed"] {
	case "true":
		out = append(out, "pressed")
	case "false":
		out = append(out, "not pressed")
	}
	switch props["expanded"] {
	case "true":
		out = append(out, "expanded")
	case "false":
		out = append(out, "collapsed")
	}
	for _, flag := range []struct{ prop, say string }{
		{"selected", "selected"}, {"modal", "modal"}, {"required", "required"},
		{"readonly", "read only"}, {"disabled", "disabled"},
	} {
		if props[flag.prop] == "true" {
			out = append(out, flag.say)
		}
	}
	if v := props["invalid"]; v != "" && v != "false" {
		out = append(out, "invalid")
	}
	return out
}

// norm is a value whitespace-collapsed, as a screen reader reads it.
func norm(v *proto.AccessibilityAXValue) string {
	if v == nil {
		return ""
	}
	return strings.Join(strings.Fields(v.Value.Str()), " ")
}

// str is norm shortened for display.
func str(v *proto.AccessibilityAXValue) string {
	s := norm(v)
	if r := []rune(s); len(r) > 120 {
		s = string(r[:119]) + "…"
	}
	return s
}

// Line renders one node as a screen reader would announce it, e.g.
// `checkbox "Remember me" (not checked)` or `button (no accessible name)`.
func Line(n *proto.AccessibilityAXNode) string {
	role, name := str(n.Role), str(n.Name)
	if role == "StaticText" {
		return fmt.Sprintf("text %q", name)
	}
	var b strings.Builder
	b.WriteString(role)
	if name != "" {
		fmt.Fprintf(&b, " %q", name)
	} else if needsName[role] {
		b.WriteString(" (no accessible name)")
	}
	st := states(n)
	if v := str(n.Value); v != "" && v != name {
		st = append([]string{fmt.Sprintf("value %q", v)}, st...)
	}
	if len(st) > 0 {
		b.WriteString(" (" + strings.Join(st, ", ") + ")")
	}
	return b.String()
}

// render walks the tree from root in reading (DOM) order: pass-through and
// ignored nodes contribute their children in place, named leaves stop the
// walk, everything else announces itself and indents its contents.
func render(nodes []*proto.AccessibilityAXNode, root proto.AccessibilityAXNodeID) []string {
	byID := make(map[proto.AccessibilityAXNodeID]*proto.AccessibilityAXNode, len(nodes))
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	var lines []string
	more := 0
	var walk func(id proto.AccessibilityAXNodeID, depth int)
	walk = func(id proto.AccessibilityAXNodeID, depth int) {
		n := byID[id]
		if n == nil {
			return
		}
		role, name := str(n.Role), str(n.Name)
		emit := !n.Ignored && !passThrough[role] &&
			!(role == "StaticText" && name == "") &&
			!(role == "group" && name == "")
		if emit {
			if len(lines) < MaxLines {
				lines = append(lines, strings.Repeat("  ", depth)+Line(n))
			} else {
				more++
			}
			if role == "StaticText" || (leaf[role] && name != "") {
				return
			}
			depth++
		}
		for _, c := range n.ChildIDs {
			walk(c, depth)
		}
	}
	walk(root, 0)
	if more > 0 {
		lines = append(lines, fmt.Sprintf("… %d more lines (scope the transcript to see them)", more))
	}
	return lines
}

// Transcript returns what a screen reader would announce for the page, or
// for the subtree at scope, in reading order.
func Transcript(p Page, scope string) ([]string, error) {
	nodes, err := p.AXTree()
	if err != nil {
		return nil, fmt.Errorf("reading accessibility tree: %w", err)
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	root := nodes[0].NodeID
	if scope != "" {
		backend, err := p.BackendNodeID(scope)
		if err != nil {
			return nil, fmt.Errorf("transcript scope %q: %w", scope, err)
		}
		found := false
		for _, n := range nodes {
			if n.BackendDOMNodeID == backend {
				root, found = n.NodeID, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("transcript scope %q: element has no accessibility node", scope)
		}
	}
	return render(nodes, root), nil
}

// Expectation is an expectFocus step: every field set must hold.
type Expectation struct {
	Within string
	Role   string
	Name   string
}

// CheckFocus reports whether focus meets e, and when it does not, a sentence
// saying where focus actually is.
func CheckFocus(p Page, e Expectation) (ok bool, actual string, err error) {
	focused, err := p.FocusedAX()
	if err != nil {
		return false, "", fmt.Errorf("reading the focused element: %w", err)
	}
	if focused == nil {
		return false, "focus is not on any element", nil
	}
	actual = "focus is on " + Line(focused)
	if e.Role != "" && str(focused.Role) != e.Role {
		return false, actual, nil
	}
	if e.Name != "" && norm(focused.Name) != strings.Join(strings.Fields(e.Name), " ") {
		return false, actual, nil
	}
	if e.Within != "" {
		raw, err := p.Eval(`(sel) => {
			const root = document.querySelector(sel);
			if (!root) return null;
			let a = document.activeElement;
			while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement;
			for (let n = a; n; n = n.getRootNode().host) if (root.contains(n)) return true;
			return false;
		}`, e.Within)
		if err != nil {
			return false, "", err
		}
		switch string(raw) {
		case "null":
			return false, actual + ", and nothing matches " + e.Within, nil
		case "false":
			return false, actual + ", outside " + e.Within, nil
		}
	}
	return true, actual, nil
}
