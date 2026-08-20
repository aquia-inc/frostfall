// Package wcag maps axe rule tags (wcag143) to WCAG success criteria
// (1.4.3 Contrast (Minimum), Level AA) so findings carry the compliance ids
// auditors and VPAT/ACR reports speak in. Section 508 incorporates WCAG 2.0
// A/AA by reference, so criteria at those levels are the 508 ids too.
package wcag

import (
	"regexp"
	"strings"
)

// Criterion is one WCAG success criterion.
type Criterion struct {
	Number  string // "1.4.3"
	Name    string // "Contrast (Minimum)"
	Level   string // "A" or "AA"
	Version string // WCAG version that introduced it: "2.0", "2.1", "2.2"
}

// String renders the display form: "1.4.3 Contrast (Minimum) (WCAG 2.0 AA)".
func (c Criterion) String() string {
	return c.Number + " " + c.Name + " (WCAG " + c.Version + " " + c.Level + ")"
}

// table covers WCAG 2.x Level A and AA - the levels automated tooling and
// Section 508 conformance work against. AAA criteria are intentionally absent.
var table = map[string]Criterion{
	// WCAG 2.0 Level A
	"1.1.1": {"1.1.1", "Non-text Content", "A", "2.0"},
	"1.2.1": {"1.2.1", "Audio-only and Video-only (Prerecorded)", "A", "2.0"},
	"1.2.2": {"1.2.2", "Captions (Prerecorded)", "A", "2.0"},
	"1.2.3": {"1.2.3", "Audio Description or Media Alternative (Prerecorded)", "A", "2.0"},
	"1.3.1": {"1.3.1", "Info and Relationships", "A", "2.0"},
	"1.3.2": {"1.3.2", "Meaningful Sequence", "A", "2.0"},
	"1.3.3": {"1.3.3", "Sensory Characteristics", "A", "2.0"},
	"1.4.1": {"1.4.1", "Use of Color", "A", "2.0"},
	"1.4.2": {"1.4.2", "Audio Control", "A", "2.0"},
	"2.1.1": {"2.1.1", "Keyboard", "A", "2.0"},
	"2.1.2": {"2.1.2", "No Keyboard Trap", "A", "2.0"},
	"2.2.1": {"2.2.1", "Timing Adjustable", "A", "2.0"},
	"2.2.2": {"2.2.2", "Pause, Stop, Hide", "A", "2.0"},
	"2.3.1": {"2.3.1", "Three Flashes or Below Threshold", "A", "2.0"},
	"2.4.1": {"2.4.1", "Bypass Blocks", "A", "2.0"},
	"2.4.2": {"2.4.2", "Page Titled", "A", "2.0"},
	"2.4.3": {"2.4.3", "Focus Order", "A", "2.0"},
	"2.4.4": {"2.4.4", "Link Purpose (In Context)", "A", "2.0"},
	"3.1.1": {"3.1.1", "Language of Page", "A", "2.0"},
	"3.2.1": {"3.2.1", "On Focus", "A", "2.0"},
	"3.2.2": {"3.2.2", "On Input", "A", "2.0"},
	"3.3.1": {"3.3.1", "Error Identification", "A", "2.0"},
	"3.3.2": {"3.3.2", "Labels or Instructions", "A", "2.0"},
	"4.1.1": {"4.1.1", "Parsing", "A", "2.0"},
	"4.1.2": {"4.1.2", "Name, Role, Value", "A", "2.0"},
	// WCAG 2.0 Level AA
	"1.2.4": {"1.2.4", "Captions (Live)", "AA", "2.0"},
	"1.2.5": {"1.2.5", "Audio Description (Prerecorded)", "AA", "2.0"},
	"1.4.3": {"1.4.3", "Contrast (Minimum)", "AA", "2.0"},
	"1.4.4": {"1.4.4", "Resize Text", "AA", "2.0"},
	"1.4.5": {"1.4.5", "Images of Text", "AA", "2.0"},
	"2.4.5": {"2.4.5", "Multiple Ways", "AA", "2.0"},
	"2.4.6": {"2.4.6", "Headings and Labels", "AA", "2.0"},
	"2.4.7": {"2.4.7", "Focus Visible", "AA", "2.0"},
	"3.1.2": {"3.1.2", "Language of Parts", "AA", "2.0"},
	"3.2.3": {"3.2.3", "Consistent Navigation", "AA", "2.0"},
	"3.2.4": {"3.2.4", "Consistent Identification", "AA", "2.0"},
	"3.3.3": {"3.3.3", "Error Suggestion", "AA", "2.0"},
	"3.3.4": {"3.3.4", "Error Prevention (Legal, Financial, Data)", "AA", "2.0"},
	// WCAG 2.1 Level A
	"2.1.4": {"2.1.4", "Character Key Shortcuts", "A", "2.1"},
	"2.5.1": {"2.5.1", "Pointer Gestures", "A", "2.1"},
	"2.5.2": {"2.5.2", "Pointer Cancellation", "A", "2.1"},
	"2.5.3": {"2.5.3", "Label in Name", "A", "2.1"},
	"2.5.4": {"2.5.4", "Motion Actuation", "A", "2.1"},
	// WCAG 2.1 Level AA
	"1.3.4":  {"1.3.4", "Orientation", "AA", "2.1"},
	"1.3.5":  {"1.3.5", "Identify Input Purpose", "AA", "2.1"},
	"1.4.10": {"1.4.10", "Reflow", "AA", "2.1"},
	"1.4.11": {"1.4.11", "Non-text Contrast", "AA", "2.1"},
	"1.4.12": {"1.4.12", "Text Spacing", "AA", "2.1"},
	"1.4.13": {"1.4.13", "Content on Hover or Focus", "AA", "2.1"},
	"4.1.3":  {"4.1.3", "Status Messages", "AA", "2.1"},
	// WCAG 2.2 Level A
	"3.2.6": {"3.2.6", "Consistent Help", "A", "2.2"},
	"3.3.7": {"3.3.7", "Redundant Entry", "A", "2.2"},
	// WCAG 2.2 Level AA
	"2.4.11": {"2.4.11", "Focus Not Obscured (Minimum)", "AA", "2.2"},
	"2.5.7":  {"2.5.7", "Dragging Movements", "AA", "2.2"},
	"2.5.8":  {"2.5.8", "Target Size (Minimum)", "AA", "2.2"},
	"3.3.8":  {"3.3.8", "Accessible Authentication (Minimum)", "AA", "2.2"},
}

// axe criterion tags: wcag143 = 1.4.3, wcag1410 = 1.4.10. The first two
// digits are single-digit principle and guideline; the remainder is the
// criterion number.
var tagRe = regexp.MustCompile(`^wcag(\d)(\d)(\d+)$`)

// section508Re parses axe's provision tags: section508.22.a names the
// original standard's paragraph 1194.22(a). The Revised 508 (2017) supersedes
// these by incorporating WCAG 2.0 A/AA (E205.4), but the paragraph ids remain
// the citation format federal reviewers recognize, so they are surfaced as
// published by the engine.
var section508Re = regexp.MustCompile(`^section508\.(\d+)\.([a-z]+)$`)

// FromTags extracts the success criteria named by a rule's axe tags, in tag
// order, plus any Section 508 provision citations (empty slice with
// section508=true when the rule is tagged 508 without a provision). Level
// tags (wcag2aa), best-practice, and unknown criteria are skipped.
func FromTags(tags []string) (criteria []Criterion, s508 []string, section508 bool) {
	for _, tag := range tags {
		if tag == "section508" {
			section508 = true
			continue
		}
		if m := section508Re.FindStringSubmatch(tag); m != nil {
			section508 = true
			s508 = append(s508, "1194."+m[1]+"("+m[2]+")")
			continue
		}
		m := tagRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		num := m[1] + "." + m[2] + "." + m[3]
		// A miss here is deliberate: axe also emits AAA criterion tags
		// (wcag146, wcag213, ...) that the A/AA table intentionally omits.
		if c, ok := table[num]; ok {
			criteria = append(criteria, c)
		}
	}
	return criteria, s508, section508
}

// Label renders a compact display line for a finding, e.g.
// "WCAG 1.3.1 Info and Relationships (A) · Section 508 1194.22(n)". Empty
// when a rule maps to no criterion (best-practice rules).
func Label(tags []string) string {
	criteria, provisions, s508 := FromTags(tags)
	// Render through String() so the introducing WCAG version is visible:
	// Section 508 incorporates WCAG 2.0 A/AA, so a 2.1-introduced criterion
	// on a 508 report is only legible if the version shows.
	var parts []string
	for _, c := range criteria {
		parts = append(parts, c.String())
	}
	if len(parts) == 0 && !s508 {
		return ""
	}
	out := strings.Join(parts, "; ")

	// The Revised Section 508 (2017) incorporates WCAG 2.0 A/AA wholesale
	// (E205.4), so any 2.0-level criterion IS a 508 requirement even when
	// axe carries no legacy provision tag - axe only tags rules that mapped
	// to the original 1194.22 paragraphs, which had no equivalent for much
	// of WCAG (color contrast, ARIA structure, ...).
	incorporated := false
	for _, c := range criteria {
		if c.Version == "2.0" {
			incorporated = true
			break
		}
	}
	if s508 || incorporated {
		if out != "" {
			out += " · "
		}
		out += "Section 508 (E205.4"
		if len(provisions) > 0 {
			out += "; " + strings.Join(provisions, ", ")
		}
		out += ")"
	}
	return out
}
