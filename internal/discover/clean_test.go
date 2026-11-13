package discover

import "testing"

func TestCleanAuthPath(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"/plain", "/plain", true},
		{"/a;jsessionid=Q", "/a", true},
		{"/a;x=1/b;y=2", "/a/b", true},
		{"/#/users", "/#/users", true},
		{"/b#/home#access_token=X", "/b#/home", true},
		{"/#/x?y=1&z=2", "/#/x", true},
		{"/#/cb&access_token=X", "/#/cb", true},
		{"/#/route=1", "/#/route", true},
		{"/;x=1", "/", true},
		{"/cb/token=abc", "", false},
		{"/cb/TOKEN=abc", "", false},
		{"/x/access_token", "", false},
		{"/sig=1f", "", false},
		{"/a/code=9", "", false},
		{"/#/access_token=X&token_type=bearer", "", false}, // the route itself is the token's name
		{"/#/ok", "/#/ok", true},
		// A percent-decoded ? in the path is a query like any other.
		{"/login?token=abc", "/login", true},
		{"/x?y=1#/r", "/x#/r", true},
		// More credential names, any case.
		{"/a/password=1", "", false},
		{"/a/PASSWD=1", "", false},
		{"/a/key=1", "", false},
		{"/a/api_key", "", false},
		{"/a/apikey", "", false},
		{"/a/session=1", "", false},
		{"/a/SessionId", "", false},
		{"/a/ticket=1", "", false},
		{"/a/signature", "", false},
		{"/a/X-Amz-Credential", "", false},
		{"/a/auth=1", "", false},
		{"/a/otp=1", "", false},
		// By shape: a JWT, 32+ hex characters, a long opaque string.
		{"/cb/eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpM", "", false},
		{"/o/0123456789abcdef0123456789abcdef", "", false},
		{"/o/0123456789abcdef0123456789abcde", "", false}, // 31 hex is still a long opaque string
		{"/s/dGhpc0lzQVNlY3JldFRva2VuMTIzNDU2", "", false},
		{"/s/Zm9vYmFyQmF6UXV4MTIzNDU2Nzg5", "", false},
		{"/#/cb/dGhpc0lzQVNlY3JldFRva2VuMTIzNDU2", "", false},
		// A JWT whose parts use - and _, and a token right after a # that is not a route.
		{"/cb/eyJhbGci-OiJIUzI1_NiJ9.eyJzdWIi-OiIxMjM0.SflKxwRJSM_eKKF2QT4", "", false},
		{"/a#dGhpc0lzQVNlY3JldFRva2VuMTIzNDU2", "", false},
		// 32 hex characters is a secret, 31 letters-only is a name.
		{"/h/abcdefabcdefabcdefabcdefabcdefab", "", false},
		{"/h/abcdefabcdefabcdefabcdefabcdefa", "/h/abcdefabcdefabcdefabcdefabcdefa", true},
		// Names and slugs are kept.
		{"/docs/release-notes-2024-01-15-summary", "/docs/release-notes-2024-01-15-summary", true},
		{"/docs/internationalization-guidelines", "/docs/internationalization-guidelines", true},
		{"/items/1.2.3", "/items/1.2.3", true},
		{"/pricing", "/pricing", true},
	}
	for _, tc := range cases {
		got, ok := cleanAuthPath(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("cleanAuthPath(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
