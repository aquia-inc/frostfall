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
	}
	for _, tc := range cases {
		got, ok := cleanAuthPath(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("cleanAuthPath(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
