// Package fingerprint computes stable identities for violations so the
// baseline survives DOM churn (hashed class names, sibling insertion,
// regenerated ids) while still distinguishing genuinely new violations.
//
// The scheme is a frozen contract (DESIGN.md §5): changing it invalidates
// every checked-in baseline.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Fingerprint returns the identity hash: first 16 bytes of
// sha256(testID \x00 scanLabel \x00 ruleID \x00 stableTarget), hex-encoded.
func Fingerprint(testID, scanLabel, ruleID, rawTarget string) string {
	h := sha256.New()
	for i, part := range []string{testID, scanLabel, ruleID, StableTarget(rawTarget)} {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

var (
	// Generated-looking ids: a run of 8+ hex chars, a purely numeric suffix
	// of 4+ digits, or a React useId (":r1a:", seen with CSS escaping
	// stripped). CSS-module hashes and React ids regenerate per build/session.
	generatedIDRe     = regexp.MustCompile(`[0-9a-f]{8,}|[0-9]{4,}$|^:r[a-z0-9]+:?$`)
	// Hash-like class names that churn between builds.
	hashClassRes = []*regexp.Regexp{
		// Emotion/MUI: css-10u3qtx and css-10u3qtx-MuiChip-root. The Mui*
		// classes also appear standalone on the same node, so dropping the
		// whole hashed token loses nothing.
		regexp.MustCompile(`^css-[a-z0-9]+(-|$)`),
		regexp.MustCompile(`^sc-[a-zA-Z]+$`),
		regexp.MustCompile(`_[a-z0-9]{5,}$`),
		regexp.MustCompile(`[0-9a-f]{8,}`),
	}
	nthChildRe = regexp.MustCompile(`:nth-child\(\d+\)`)
	// React useId values (":r5n:") land in attribute selectors like
	// [aria-controls=":r5n:"] and regenerate per session; keep the attribute
	// name as identity, drop the volatile value.
	reactIDAttrRe = regexp.MustCompile(`\[([a-zA-Z-]+)="?:r[a-z0-9]+:"?\]`)
)

// StableTarget normalizes a selector path into its fingerprint form:
//
//   - hash-like class names are stripped
//   - :nth-child(n) is dropped when the segment has any other distinguishing
//     feature (id, surviving class, attribute, role)
//   - if a segment within the last 4 carries a non-generated id, the path is
//     truncated to start there (everything above an anchor id is noise)
func StableTarget(raw string) string {
	segments := splitPath(raw)
	for i, seg := range segments {
		segments[i] = normalizeSegment(seg)
	}

	// Anchor at the deepest stable id within the last 4 segments.
	start := 0
	from := max(len(segments)-4, 0)
	for i := len(segments) - 1; i >= from; i-- {
		if hasStableID(segments[i]) {
			start = i
			break
		}
	}
	return strings.Join(segments[start:], " > ")
}

// splitPath splits on combinators while treating descendant and child
// combinators uniformly (axe emits both).
func splitPath(raw string) []string {
	raw = strings.ReplaceAll(raw, " >>> ", " > ") // shadow-DOM boundary
	raw = strings.ReplaceAll(raw, ">", " ")
	fields := strings.Fields(raw)
	return fields
}

func normalizeSegment(seg string) string {
	seg = reactIDAttrRe.ReplaceAllString(seg, "[$1]")
	seg = stripGeneratedID(seg)
	seg = stripHashClasses(seg)
	if nthChildRe.MatchString(seg) && hasOtherFeature(nthChildRe.ReplaceAllString(seg, "")) {
		seg = nthChildRe.ReplaceAllString(seg, "")
	}
	if seg == "" {
		seg = "*"
	}
	return seg
}

// stripGeneratedID removes a #id token when the id looks generated — a
// regenerated id must not fork the violation's identity.
func stripGeneratedID(seg string) string {
	i := strings.IndexByte(seg, '#')
	if i < 0 {
		return seg
	}
	j := scanIDToken(seg, i+1)
	if id := unescapeCSS(seg[i+1 : j]); id != "" && generatedIDRe.MatchString(id) {
		return seg[:i] + seg[j:]
	}
	return seg
}

// scanIDToken advances past an id token starting at from, including
// CSS-escaped characters: axe emits id=":r1a:" as `#\:r1a\:`, and stopping
// at the backslash would leave the volatile id in the fingerprint.
func scanIDToken(seg string, from int) int {
	j := from
	for j < len(seg) {
		if seg[j] == '\\' && j+1 < len(seg) {
			j += 2
			continue
		}
		if isClassChar(seg[j]) || seg[j] == ':' {
			j++
			continue
		}
		break
	}
	return j
}

func unescapeCSS(s string) string {
	return strings.ReplaceAll(s, `\`, "")
}

// emotionSuffixRe matches emotion/MUI classes that carry a stable suffix
// after the hash (css-10u3qtx-MuiChip-root); the suffix survives rebuilds
// even though the hash churns, so it is kept as the class identity.
var emotionSuffixRe = regexp.MustCompile(`^css-[a-z0-9]+-([A-Za-z][A-Za-z0-9_-]*)$`)

// stripHashClasses removes .class tokens matching hash-like patterns,
// rewriting suffixed emotion classes down to their stable suffix so an
// element styled only by a hashed class keeps an identity.
func stripHashClasses(seg string) string {
	var b strings.Builder
	kept := map[string]bool{}
	i := 0
	for i < len(seg) {
		if seg[i] != '.' {
			b.WriteByte(seg[i])
			i++
			continue
		}
		j := i + 1
		for j < len(seg) && isClassChar(seg[j]) {
			j++
		}
		class := seg[i+1 : j]
		if m := emotionSuffixRe.FindStringSubmatch(class); m != nil {
			class = m[1]
		} else if isHashClass(class) {
			i = j
			continue
		}
		if !kept[class] {
			kept[class] = true
			b.WriteByte('.')
			b.WriteString(class)
		}
		i = j
	}
	return b.String()
}

func isClassChar(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isHashClass(class string) bool {
	for _, re := range hashClassRes {
		if re.MatchString(class) {
			return true
		}
	}
	return false
}

// hasOtherFeature reports whether a segment (with :nth-child removed) still
// distinguishes its element beyond bare tag name.
func hasOtherFeature(seg string) bool {
	return strings.ContainsAny(seg, "#.[")
}

func hasStableID(seg string) bool {
	i := strings.IndexByte(seg, '#')
	if i < 0 {
		return false
	}
	j := scanIDToken(seg, i+1)
	id := unescapeCSS(seg[i+1 : j])
	return id != "" && !generatedIDRe.MatchString(id)
}
