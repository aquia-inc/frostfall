package main

import (
	"testing"
)

// The auth fixtures through the real entry point: a valid storage state
// reaches the protected page and finds its planted violation (exit 1), and an
// empty one is sent to the login page and must stop with exit 3 instead of
// scanning it. CI runs the same two configs against the built binary.
func TestAuthFixtureExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	// serve: in the fixture configs is relative to the repository root.
	t.Chdir("../..")
	cases := []struct {
		config string
		want   int
	}{
		{"testdata/auth/frostfall.yml", exitViolations},
		{"testdata/auth/frostfall-nosession.yml", exitEnvironment},
	}
	for _, tc := range cases {
		t.Run(tc.config, func(t *testing.T) {
			if got := run([]string{"--config", tc.config, "--no-screenshots", "--baseline", "none"}); got != tc.want {
				t.Errorf("exit code = %d, want %d", got, tc.want)
			}
		})
	}
}
