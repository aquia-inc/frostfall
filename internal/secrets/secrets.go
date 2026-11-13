// Package secrets holds the heuristics that decide whether a piece of a URL
// or selector looks like a credential. They are shared by discovery (which
// refuses to turn such a link into a test id), URL redaction, and selector
// scrubbing, so the three cannot disagree about what a secret looks like.
package secrets

import (
	"regexp"
	"strings"
)

// CredentialNames in a string say it carries a secret. Matched
// case-insensitively as substrings, so they are blunt on purpose: a page
// named /signature-pad matches too, and is added as an explicit test.
var CredentialNames = []string{
	"token=", "access_token", "sig=", "code=",
	"password=", "passwd=", "key=", "api_key", "apikey", "session=", "sessionid",
	"ticket=", "signature", "x-amz-", "auth=", "otp=",
}

// HasCredentialName reports whether s contains one of CredentialNames.
func HasCredentialName(s string) bool {
	lower := strings.ToLower(s)
	for _, name := range CredentialNames {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
}

var (
	jwtSegment = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}$`)
	hexSegment = regexp.MustCompile(`^[0-9a-fA-F]{32,}$`)
	b64Segment = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}$`)
	wordChunk  = regexp.MustCompile(`^[a-z]{4,}$`)
)

// TokenLike reports whether a single path segment looks like a secret rather
// than a name: a JWT (three base64url parts joined by dots), 32 or more hex
// characters, or 24 or more base64url characters that carry a digit or mixed
// case and no recognizable word. A slug like release-notes-2024-summary has
// words between its hyphens and is kept; a random string has none. The
// heuristic is deliberately simple: it will miss a short or all-lowercase
// token, and flag a long opaque id (a UUID, an ObjectId) that is not one,
// which is the safer error for something that becomes a test id or is printed.
func TokenLike(seg string) bool {
	if jwtSegment.MatchString(seg) || hexSegment.MatchString(seg) {
		return true
	}
	if !b64Segment.MatchString(seg) {
		return false
	}
	hasDigit, hasUpper, hasLower := false, false, false
	for _, r := range seg {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		}
	}
	if !hasDigit && !(hasUpper && hasLower) {
		return false
	}
	for _, chunk := range strings.FieldsFunc(seg, func(r rune) bool { return r == '-' || r == '_' }) {
		if wordChunk.MatchString(chunk) && strings.ContainsAny(chunk, "aeiou") {
			return false // a word between separators: a slug, not a secret
		}
	}
	return true
}

var attrSelector = regexp.MustCompile(`\[([A-Za-z_:][-A-Za-z0-9_:.]*)([\^$*]?)=(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)')\]`)

// ScrubSelector removes secrets axe leaves in an attribute selector. axe
// anchors on a full href or src value when it is short enough, and a
// download or callback link carries its token there:
// a[href="/dl?token=Q7xK2pLm9Zw"] > img. For each [attr="v"] (also ^=, $=,
// *=) whose value contains '?' or '=', the value is cut at the first '?', so
// the path stays and the query goes; if what is left still names a
// credential or is token-shaped, the whole test becomes [attr]. Every other
// selector is returned as it was.
func ScrubSelector(sel string) string {
	return attrSelector.ReplaceAllStringFunc(sel, func(m string) string {
		g := attrSelector.FindStringSubmatch(m)
		value, quote := g[3], `"`
		if g[4] != "" || strings.HasSuffix(m, `']`) {
			value, quote = g[4], "'"
		}
		if !strings.ContainsAny(value, "?=") {
			return m
		}
		if i := strings.IndexByte(value, '?'); i >= 0 {
			value = value[:i]
		}
		if HasCredentialName(value) || tokenInPath(value) {
			return "[" + g[1] + "]"
		}
		return "[" + g[1] + g[2] + "=" + quote + value + quote + "]"
	})
}

func tokenInPath(v string) bool {
	for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '#' }) {
		if TokenLike(seg) {
			return true
		}
	}
	return false
}
