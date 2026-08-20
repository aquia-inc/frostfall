package format

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/runner"
)

func gridResult(target string) runner.Result {
	return runner.Result{
		Violation:    engine.Violation{RuleID: "aria-required-children", Impact: engine.Critical, Summary: "grid roles"},
		TestID:       "users",
		ScanLabel:    "initial",
		Fingerprint:  target,
		StableTarget: target,
		PageURL:      "http://localhost/",
	}
}

func TestHTMLReportCollapsesSameShapedRows(t *testing.T) {
	run := &runner.Run{
		TestsRun: 1,
		Results: []runner.Result{
			gridResult(`div[data-rowindex="0"] > .cell`),
			gridResult(`div[data-rowindex="1"] > .cell`),
			gridResult(`div[data-rowindex="2"] > .cell`),
			gridResult(`#unrelated > img`), // different shape, own row
		},
	}
	var buf bytes.Buffer
	flagged := func(r runner.Result) bool { return true }
	if err := HTML(&buf, run, RunMeta{Mode: "report only"}, flagged); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "+2 more like this") {
		t.Errorf("same-shaped rows not collapsed")
	}
	// Counts stay per-violation: 4 new violations despite 2 table rows.
	if !strings.Contains(out, `<div class="n">4</div>`) {
		t.Errorf("violation count should remain per-violation")
	}
	if strings.Count(out, "aria-required-children") > 3 {
		t.Errorf("collapsed rows still render the rule repeatedly")
	}
}
