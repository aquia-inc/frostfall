package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runCaptured runs the CLI and returns its exit code and what it wrote to
// stderr, which is where every failure message goes.
func runCaptured(t *testing.T, args ...string) (int, string) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := run(args)
	os.Stderr = orig
	w.Close()
	return code, <-done
}

// The auth fixtures through the real entry point: a valid storage state
// reaches the protected page and finds its planted violation (exit 1), and an
// empty one is sent to the login page and must stop with exit 3 and say so,
// instead of scanning it. CI runs the same two configs against the built
// binary.
func TestAuthFixtureExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	// serve: in the fixture configs is relative to the repository root.
	t.Chdir("../..")
	cases := []struct {
		config    string
		want      int
		stderrHas string
	}{
		{"testdata/auth/frostfall.yml", exitViolations, ""},
		{"testdata/auth/frostfall-nosession.yml", exitEnvironment, "landed on the login page"},
	}
	for _, tc := range cases {
		t.Run(tc.config, func(t *testing.T) {
			code, stderr := runCaptured(t, "--config", tc.config, "--no-screenshots", "--baseline", "none")
			if code != tc.want {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tc.want, stderr)
			}
			if !strings.Contains(stderr, tc.stderrHas) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.stderrHas)
			}
		})
	}
}

// A trailing slash on --base-url must not put a double slash in every URL
// built from it.
func TestBaseURLTrailingSlashIsTrimmed(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a browser")
	}
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><main>page</main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	cfg := filepath.Join(t.TempDir(), "frostfall.yml")
	if err := os.WriteFile(cfg, []byte("version: 1\ndefaults:\n  waitFor: load\ntests:\n  - id: p\n    path: /p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if code, stderr := runCaptured(t, "--config", cfg, "--base-url", srv.URL+"/", "--no-screenshots", "--baseline", "none"); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if strings.HasPrefix(p, "//") {
			t.Errorf("request for %q: the trailing slash of the base URL was kept", p)
		}
	}
	if len(paths) == 0 || !strings.Contains(strings.Join(paths, " "), "/p") {
		t.Errorf("paths = %v, want /p requested", paths)
	}
}
