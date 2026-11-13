package secrets

import (
	"strings"
	"testing"
)

func TestTokenLike(t *testing.T) {
	cases := map[string]bool{
		// A JWT, including the base64url characters - and _ in every part.
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4":     true,
		"eyJhbGci-OiJIUzI1_NiJ9.eyJzdWIi-OiIxMjM0.SflKxwRJSM_eKKF2QT4": true,
		"eyJhbGciOiJI.eyJzdWIiOiIx.Sfl-Kxw_RJSM":                       true,
		"a.b.c":                                                        false, // a version number is not a JWT
		"eyJhbGci.eyJzdWIi":                                            false, // two parts
		// 32 hex characters, and the boundary on both sides of it.
		"abcdefabcdefabcdefabcdefabcdefab":  true,  // 32
		"abcdefabcdefabcdefabcdefabcdefa":   false, // 31, letters only: no digit, no mixed case
		"0123456789abcdef0123456789abcdef":  true,
		"0123456789ABCDEF0123456789abcdef0": true,
		// 24+ base64url with a digit or mixed case and no word.
		"dGhpc0lzQVNlY3JldFRva2VuMTIzNDU2":     true,
		"Zm9vYmFyQmF6UXV4MTIzNDU2Nzg5":         true,
		"550e8400-e29b-41d4-a716-446655440000": true, // a UUID: opaque, and flagged on purpose
		// Names and slugs.
		"release-notes-2024-01-15-summary": false,
		"internationalization-guidelines":  false,
		"pricing":                          false,
		"123456":                           false,
		"short-Abc123":                     false,
	}
	for seg, want := range cases {
		if got := TokenLike(seg); got != want {
			t.Errorf("TokenLike(%q) = %v, want %v", seg, got, want)
		}
	}
}

func TestHasCredentialName(t *testing.T) {
	for s, want := range map[string]bool{
		"/dl?Token=abc": true, "x-amz-signature": true, "/a/SessionId": true, "/OTP=1": true,
		"/pricing": false, "/tokens": false, "/keyboard": false,
	} {
		if got := HasCredentialName(s); got != want {
			t.Errorf("HasCredentialName(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestScrubSelector(t *testing.T) {
	cases := map[string]string{
		// Every value goes, whatever the attribute or the value looks like.
		`a[href="/dl?token=Q7xK2pLm9Zw"] > img`:   `a[href] > img`,
		`input[value="Hunter2-Smtp-Pass!"]`:       `input[value]`,
		`button[data-csrf="a1b2c3d4e5f6"]`:        `button[data-csrf]`,
		`span[data-auth-token="abc"]`:             `span[data-auth-token]`,
		`input[value="sk_live_51Habcdefg"]`:       `input[value]`,
		`a[href$="Zm9vYmFyQmF6UXV4MTIzNDU2Nzg5"]`: `a[href]`,
		`a[href="/pricing"]`:                      `a[href]`,
		`[title="Are you sure?"]`:                 `[title]`,
		// Every operator, quote style and flag.
		`[a^="x"] [b$="y"] [c*="z"] [d~="w"] [e|="v"]`: `[a] [b] [c] [d] [e]`,
		`[a='single']`: `[a]`,
		`[a=bare]`:     `[a]`,
		`[a="v" i]`:    `[a]`,
		`[a = "v"]`:    `[a ]`,
		// A CSS-escaped attribute name is kept, and its value is not.
		`button[\@click="dl('/f?token=SECRET')"]`: `button[\@click]`,
		`div[\:class="x"]`:                        `div[\:class]`,
		`div[x-on\:click.prevent="go('/a?t=1')"]`: `div[x-on\:click.prevent]`,
		// Escaped operator and bracket characters in a name.
		`div[a\=b="v"]`:     `div[a\=b]`,
		`div[a\]b="v"] > c`: `div[a\]b] > c`,
		// Quotes and brackets inside a value.
		`a[href="/x\"]?token=SECRET"]`:   `a[href]`,
		`a[data-x="]token=SECRET["] > b`: `a[data-x] > b`,
		`a[data-x='it\'s ]secret'] > b`:  `a[data-x] > b`,
		// The shadow separator and everything outside brackets is untouched.
		`#host >>> input[value="secret"] > span.c:nth-child(2)`: `#host >>> input[value] > span.c:nth-child(2)`,
		`main > a.x#y:not([disabled]) + b`:                      `main > a.x#y:not([disabled]) + b`,
		`#a\[b\]c > d`:                                          `#a\[b\]c > d`,
		`.cls\:x[title="t"]`:                                    `.cls\:x[title]`,
		// Valueless attributes are left alone.
		`input[disabled]`:           `input[disabled]`,
		`input[disabled][required]`: `input[disabled][required]`,
		// Several, mixed.
		`form > input[name="a"][value="b"] ~ button[type="submit"]`: `form > input[name][value] ~ button[type]`,
		// Unterminated: nothing after the name survives.
		`input[value="secret`: `input[value]`,
		`input[value`:         `input[value]`,
		// Nothing to scrub.
		`#main > button.cta`: `#main > button.cta`,
		``:                   ``,
	}
	for in, want := range cases {
		if got := ScrubSelector(in); got != want {
			t.Errorf("ScrubSelector(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestRedactURLFallsBackWithoutEchoingUserinfo(t *testing.T) {
	for _, in := range []string{"http://user:S3cretPW@host/%zz?token=SECRET", "http://user:S3cretPW@host/p%zz", "://bad%zzurl"} {
		got := RedactURL(in)
		for _, secret := range []string{"S3cretPW", "SECRET", "user:"} {
			if strings.Contains(got, secret) {
				t.Errorf("RedactURL(%q) = %q, still carries %q", in, got, secret)
			}
		}
	}
}
