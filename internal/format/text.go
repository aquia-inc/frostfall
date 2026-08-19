// Package format renders run results. v1 formats: text (human, stdout);
// json/sarif/junit land behind the same interface.
package format

import (
	"fmt"
	"io"
	"sort"

	"github.com/charmbracelet/lipgloss"

	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/runner"
)

// Styles render via lipgloss, which detects terminal capability per writer:
// full color on a TTY, plain text when piped to a file or CI log, and it
// honors NO_COLOR. One formatter serves both audiences.
var (
	styleTestID    = lipgloss.NewStyle().Bold(true)
	styleScanLabel = lipgloss.NewStyle().Faint(true)
	styleMarker    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleSummary   = lipgloss.NewStyle().Faint(true)
	styleTarget    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleMeta      = lipgloss.NewStyle().Faint(true)
	styleBaselined = lipgloss.NewStyle().Faint(true)
	styleGood      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleNote      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleTotals    = lipgloss.NewStyle().Bold(true)

	impactStyles = map[engine.Impact]lipgloss.Style{
		engine.Critical: lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true),
		engine.Serious:  lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		engine.Moderate: lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		engine.Minor:    lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	}
)

func badge(i engine.Impact) string {
	return impactStyles[i].Render(fmt.Sprintf("[%s]", i))
}

// Text writes the human-readable report: grouped by test and scan point,
// impact-sorted, baselined violations collapsed to a count. flagged is the
// same predicate the exit code uses (severity floor plus enforced rules), so
// the summary count and the exit code can never disagree; enforcing tells
// the summary whether flagged violations fail the build or are report-only.
func Text(w io.Writer, run *runner.Run, flagged func(runner.Result) bool, label string, enforcing bool) {
	type key struct{ test, scan string }
	groups := map[key][]runner.Result{}
	var order []key
	for _, res := range run.Results {
		k := key{res.TestID, res.ScanLabel}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], res)
	}

	newTotal, baselinedTotal := 0, 0
	for _, k := range order {
		results := groups[k]
		var fresh []runner.Result
		baselined := 0
		for _, res := range results {
			if res.Baselined {
				baselined++
				baselinedTotal++
			} else {
				fresh = append(fresh, res)
			}
		}
		if len(fresh) == 0 && baselined == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s %s\n", styleTestID.Render(k.test), styleScanLabel.Render("("+k.scan+")"))
		sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Impact > fresh[j].Impact })
		for _, res := range fresh {
			marker := "  "
			if flagged(res) {
				marker = styleMarker.Render("✗") + " "
				newTotal++
			}
			fmt.Fprintf(w, "  %s%s %s\n", marker, badge(res.Impact), res.RuleID)
			fmt.Fprintf(w, "      %s\n", styleSummary.Render(res.Summary))
			fmt.Fprintf(w, "      %s %s\n", styleMeta.Render("at"), styleTarget.Render(res.StableTarget))
			if res.HelpURL != "" {
				fmt.Fprintf(w, "      %s\n", styleMeta.Render(res.HelpURL))
			}
			if res.ScreenshotPath != "" {
				fmt.Fprintf(w, "      %s\n", styleMeta.Render("screenshot: "+res.ScreenshotPath))
			}
		}
		if baselined > 0 {
			fmt.Fprintf(w, "    %s\n", styleBaselined.Render(fmt.Sprintf("(%d baselined)", baselined)))
		}
	}

	fmt.Fprintf(w, "\n%s\n", styleTotals.Render(fmt.Sprintf(
		"%d tests, %d new violation(s) %s, %d baselined",
		run.TestsRun, newTotal, label, baselinedTotal)))
	if len(run.Stale) > 0 {
		fmt.Fprintf(w, "%s\n", styleGood.Render(fmt.Sprintf(
			"%d baselined violation(s) no longer occur — run --update-baseline to prune", len(run.Stale))))
	}
	if !enforcing && newTotal > 0 {
		fmt.Fprintf(w, "%s\n", styleNote.Render(
			"report mode: the build is not failed — set expect: in the config to enforce"))
	}
}
