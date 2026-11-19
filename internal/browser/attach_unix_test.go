//go:build unix

package browser

import (
	"syscall"
	"testing"
	"time"
)

// Close never holds an interrupt up for long: with the browser frozen, so
// that nothing it is asked is answered, Close still returns within its
// budget.
func TestCloseIsBoundedWhenTheBrowserHangs(t *testing.T) {
	c := startChrome(t)
	b := connectTo(t, c, c.HTTP())
	if _, _, err := b.openCaptureTab(); err != nil { // a registered tab for Close to work on
		t.Fatal(err)
	}
	if err := c.Process().Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process().Signal(syscall.SIGCONT) }()
	start := time.Now()
	done := make(chan struct{})
	go func() { b.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second): // the budget is 5s; fixed here so a bigger budget fails

		t.Fatalf("Close still running after %s with the browser frozen", time.Since(start))
	}
	if d := time.Since(start); d > 7*time.Second {
		t.Errorf("Close took %s with a 5s budget", d)
	}
}
