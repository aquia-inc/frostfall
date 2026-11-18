package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aquia-inc/frostfall/internal/baseline"
	"github.com/aquia-inc/frostfall/internal/browser"
	"github.com/aquia-inc/frostfall/internal/config"
)

// A scan that had to leave a frame out did not look for the frame's
// violations: a baselined entry is unchecked, not fixed, and must not go
// stale (or have its issue closed). A full scan at another point of the same
// test still covers it.
func TestPartialScanDoesNotCover(t *testing.T) {
	entry := baseline.Violation{Fingerprint: "framed", TestID: "page", RuleID: "label"}
	r := &Runner{Baseline: &baseline.File{Violations: []baseline.Violation{entry}}}

	cov := newCoverage()
	cov.recordPartial(checkAxe)
	run := &Run{ran: map[string]*coverage{"page": cov}}
	r.classify(run)
	if len(run.Stale) != 0 {
		t.Errorf("stale = %+v after a partial scan, want none", run.Stale)
	}
	if got := run.Checks("page"); len(got) != 1 || got[0] != checkAxe {
		t.Errorf("checks = %q: a partial scan still ran axe", got)
	}

	cov.record(checkAxe, nil)
	run = &Run{ran: map[string]*coverage{"page": cov}}
	r.classify(run)
	if len(run.Stale) != 1 {
		t.Errorf("stale = %+v with a full scan too, want the entry", run.Stale)
	}
}

// A frame that could not be scanned is a warning, printed whether or not the
// run is verbose. A signed-in test names it with attribute values dropped and
// leaves out the error, whose text can carry the frame's URL; a test without
// auth prints both as they are.
func TestFrameWarning(t *testing.T) {
	const frame = `main > iframe[src="https://idp.example/embed?session=FRAMETOKEN9"]`
	cause := errors.New("navigating to https://idp.example/embed?session=FRAMETOKEN9: failed")
	var got []string
	warn := func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) }

	signedIn := &Runner{Auth: browser.RedactingAuth(), Warn: warn}
	signedIn.frameWarning(config.Test{ID: "account"}, "initial", frame, cause)
	signedIn.frameWarning(config.Test{ID: "account"}, "initial", frame, fmt.Errorf("attaching: %w", context.DeadlineExceeded))
	plain := &Runner{Warn: warn}
	plain.frameWarning(config.Test{ID: "home"}, "initial", frame, cause)

	if len(got) != 3 {
		t.Fatalf("warnings = %q, want 3", got)
	}
	for _, w := range got[:2] {
		if strings.Contains(w, "FRAMETOKEN9") || strings.Contains(w, "idp.example") {
			t.Errorf("signed-in warning leaks the frame's URL: %s", w)
		}
		if !strings.Contains(w, "main > iframe[src]") || !strings.Contains(w, `test "account": scan "initial"`) {
			t.Errorf("signed-in warning lacks the scrubbed frame: %s", w)
		}
	}
	if !strings.Contains(got[1], "did not respond in time") {
		t.Errorf("a deadline is not reported as one: %s", got[1])
	}
	if !strings.Contains(got[2], frame) || !strings.Contains(got[2], cause.Error()) {
		t.Errorf("warning without auth lost the frame or the error: %s", got[2])
	}
}
