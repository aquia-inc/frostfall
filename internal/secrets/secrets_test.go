package secrets

import "testing"

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
		// The value is cut at the first ?, keeping the path.
		`a[href="/dl?token=Q7xK2pLm9Zw"] > img`: `a[href="/dl"] > img`,
		`a[href^="/cb?code=1"]`:                 `a[href^="/cb"]`,
		`img[src$="/x.png?sig=abc"]`:            `img[src$="/x.png"]`,
		`a[href*="/dl?x=1"]`:                    `a[href*="/dl"]`,
		`a[href="/search?q=hello"]`:             `a[href="/search"]`,
		`a[href='/dl?x=1']`:                     `a[href='/dl']`,
		// What is left still names a credential or looks like one: the attribute stays, the value goes.
		`[data-x="token=abc"]`:                          `[data-x]`,
		`[href="/dl/dGhpc0lzQVNlY3JldFRva2VuMTIz?x=1"]`: `[href]`,
		`[href="/api_key/x?y=1"]`:                       `[href]`,
		// Several in one selector, the rest untouched.
		`main > a[href="/a?token=1"] + a[href="/token=2"] > span.c`: `main > a[href="/a"] + a[href] > span.c`,
		// No ? and no = : never touched.
		`a[href="/pricing"]`:            `a[href="/pricing"]`,
		`#main > button.cta`:            `#main > button.cta`,
		`[id="main"]`:                   `[id="main"]`,
		`[class*="release-notes-2024"]`: `[class*="release-notes-2024"]`,
		// = without ? and no credential name: kept.
		`[data-x="a=b"]`: `[data-x="a=b"]`,
	}
	for in, want := range cases {
		if got := ScrubSelector(in); got != want {
			t.Errorf("ScrubSelector(%s) = %s, want %s", in, got, want)
		}
	}
}
