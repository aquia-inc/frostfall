// Package secrets holds the heuristics that decide whether a piece of a URL
// or selector looks like a credential. They are shared by discovery (which
// refuses to turn such a link into a test id), URL redaction, and selector
// scrubbing, so the three cannot disagree about what a secret looks like.
package secrets

import (
	"net/url"
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

// ScrubSelector drops the value of every attribute selector in a target:
// [anything<op>"..."] or '...' or an unquoted value, for any operator (=, ^=,
// $=, *=, ~=, |=) and any flag (i, s), becomes [anything]. axe puts values in
// its selectors (a download link's href, a prefilled field's value, a data-*
// token, an Alpine @click handler), and there is no list of attribute names or
// value shapes that catches every secret, so no value is kept. The selector is
// parsed, not pattern-matched: quoted values may contain ']' and escaped
// quotes, attribute names may be CSS-escaped (\@click), and text outside
// attribute brackets (tags, ids, classes, combinators, the " >>> " shadow
// separator) is returned exactly as it was. An attribute with no value
// ([disabled]) is left alone. The accepted cost is that elements differing
// only in an attribute value share a selector, and so a fingerprint.
func ScrubSelector(sel string) string {
	var out strings.Builder
	for i := 0; i < len(sel); {
		switch sel[i] {
		case '\\':
			// An escape outside brackets: keep it and the escaped byte as they are.
			end := min(i+2, len(sel))
			out.WriteString(sel[i:end])
			i = end
		case '[':
			end, text := scrubBracket(sel, i)
			out.WriteString(text)
			i = end
		default:
			out.WriteByte(sel[i])
			i++
		}
	}
	return out.String()
}

// scrubBracket handles the attribute selector that starts at sel[start] ('[').
// It returns where scanning resumes and the text to emit.
func scrubBracket(sel string, start int) (int, string) {
	i := start + 1
	nameStart := i
	// The name runs to an operator or the closing bracket; a backslash escapes
	// the next byte, which may be one of those (\] \= \@).
	for i < len(sel) {
		c := sel[i]
		if c == '\\' {
			i = min(i+2, len(sel))
			continue
		}
		if c == ']' || c == '=' || ((c == '^' || c == '$' || c == '*' || c == '~' || c == '|') && i+1 < len(sel) && sel[i+1] == '=') {
			break
		}
		i++
	}
	name := sel[nameStart:i]
	if i >= len(sel) {
		return len(sel), "[" + name + "]" // unterminated: nothing after the name is kept
	}
	if sel[i] == ']' {
		return i + 1, sel[start : i+1] // no operator, no value: nothing to scrub
	}
	// An operator: skip it, then the value, then the flags, to the closing bracket.
	if sel[i] != '=' {
		i++
	}
	i++ // the '='
	for i < len(sel) && sel[i] == ' ' {
		i++
	}
	if i < len(sel) && (sel[i] == '"' || sel[i] == '\'') {
		quote := sel[i]
		i++
		for i < len(sel) && sel[i] != quote {
			if sel[i] == '\\' {
				i++
			}
			i++
		}
		i++ // the closing quote
	}
	for i < len(sel) && sel[i] != ']' {
		if sel[i] == '\\' {
			i++
		}
		i++
	}
	return min(i+1, len(sel)), "[" + name + "]"
}

var pathParamRe = regexp.MustCompile(`;[^/]*`)

// userinfoRe finds user:password@ in a URL that did not parse.
var userinfoRe = regexp.MustCompile(`(?i)(://)[^/?#@]*@`)

// MaskSegments replaces each path segment that looks like a secret (TokenLike)
// with [redacted]. Explicit paths are otherwise printed as written; a token in
// the path itself, as opposed to its query, is what this catches.
func MaskSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if TokenLike(seg) {
			segs[i] = "[redacted]"
		}
	}
	return strings.Join(segs, "/")
}

// RedactURL reduces a URL to where it points, safe to print: userinfo, the
// query, ;params on path segments (/login;jsessionid=...) and the fragment
// are dropped. A hash route (#/users) is the page's identity in a hash-routed
// app and is kept, but only up to the first '#', '?', '&' or '=' after it:
// OAuth implicit flows put the token in exactly those places
// (#/home#access_token=..., #/cb&access_token=...). A percent-encoded '?' or
// '#' in the path is decoded by the parse, so it is cut the same way and
// cannot smuggle a query past it. Path segments that look like secrets are
// masked. Setup paths, base URLs and navigation targets are printed in errors,
// logs and reports, and any of those parts can carry a token or credential.
// A URL that does not parse is cut at its first '?', '#' or ';' and stripped
// of user:password@, which is the most that can be promised of it.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return userinfoRe.ReplaceAllString(cutAtAny(raw, "?#;"), "$1")
	}
	path := u.Path
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	out := MaskSegments(pathParamRe.ReplaceAllString(path, ""))
	if u.Host != "" {
		out = u.Scheme + "://" + u.Host + out
	}
	if frag := u.Fragment; strings.HasPrefix(frag, "/") {
		if i := strings.IndexAny(frag, "#?&="); i >= 0 {
			frag = frag[:i]
		}
		out += "#" + MaskSegments(pathParamRe.ReplaceAllString(frag, ""))
	}
	return out
}

func cutAtAny(s, chars string) string {
	if i := strings.IndexAny(s, chars); i >= 0 {
		return s[:i]
	}
	return s
}
