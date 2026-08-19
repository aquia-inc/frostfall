// Package server provides the three ways Frostfall reaches an app: attaching
// to a running server, spawning one, or serving a static build itself.
package server

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Static serves a build directory from an embedded file server — the
// documented CI default, since it scans the production bundle rather than a
// dev build with its extra dev-only DOM.
type Static struct {
	Dir         string
	SPAFallback bool

	listener net.Listener
	srv      *http.Server
}

// Start binds an ephemeral port and begins serving. Returns the base URL.
func (s *Static) Start() (string, error) {
	if info, err := os.Stat(s.Dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("serve directory %q is not a directory", s.Dir)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	s.listener = ln
	s.srv = &http.Server{Handler: s.handler()}
	go s.srv.Serve(ln) //nolint:errcheck // Shutdown error surfaces via Stop
	return fmt.Sprintf("http://%s", ln.Addr().String()), nil
}

func (s *Static) Stop() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

func (s *Static) handler() http.Handler {
	fs := http.FileServer(http.Dir(s.Dir))
	if !s.SPAFallback {
		return fs
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SPA routing: extensionless paths that don't exist on disk fall back
		// to index.html so deep links resolve like they do behind a real host.
		p := filepath.Join(s.Dir, filepath.Clean(r.URL.Path))
		if _, err := os.Stat(p); os.IsNotExist(err) && !strings.Contains(filepath.Base(r.URL.Path), ".") {
			http.ServeFile(w, r, filepath.Join(s.Dir, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
}
