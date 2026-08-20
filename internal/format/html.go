package format

import (
	_ "embed"
	"encoding/base64"
	"html/template"
	"io"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/aquia-inc/frostfall/internal/runner"
)

// rowShapeRe strips the per-instance parts of a selector for report-row
// grouping: quoted numbers in attribute values (data-rowindex="7") and
// :nth-child indexes. Deliberately conservative - only digits in those two
// positions - so unrelated selectors do not merge.
var rowShapeRe = regexp.MustCompile(`"(\d+)"|:nth-child\(\d+\)`)

func rowShape(target string) string {
	return rowShapeRe.ReplaceAllString(target, "#")
}

// logoPNG is a downscaled copy of assets/frostfall_no_bg_logo.png, embedded
// so the HTML report is self-contained anywhere the binary runs.
//
//go:embed logo.png
var logoPNG []byte

// RunMeta carries run context into report headers.
type RunMeta struct {
	Date        time.Time
	App         string // app/repo identity, "" when undetectable
	BaseURL     string
	Standard    string
	Profile     string
	ToolVersion string
	AxeVersion  string
	// Mode is the human description of the enforcement posture, e.g.
	// "report only" or "enforcing (serious+)" — computed by the caller from
	// the same inputs as the flagged predicate so the header cannot mislabel.
	Mode string
}

type htmlRow struct {
	Impact     string
	Rule       string
	Test       string
	Scan       string
	Summary    string
	Target     string
	HelpURL    string
	Baselined  bool
	Failing    bool
	Screenshot template.URL // data: URI, empty when no capture
	// MoreTargets holds the selectors of same-shaped sibling violations
	// collapsed into this row (a data grid failing one rule on every row is
	// one bug, not N table rows). Presentation only - counts and fingerprints
	// stay per-violation, and the screenshot shown is the first member's.
	MoreTargets []string
}

type htmlReport struct {
	Meta       RunMeta
	DateHuman  string
	Logo       template.URL
	Rows       []htmlRow
	TestsRun   int
	NewCount   int
	Baselined  int
	StaleCount int
}

// HTML writes a self-contained single-file report: one triage table, styling
// inline, screenshots embedded as data URIs — made to be attached to a ticket
// or handed to a reviewer with nothing else. flagged is the same enforcement
// predicate the exit code uses.
func HTML(w io.Writer, run *runner.Run, meta RunMeta, flagged func(runner.Result) bool) error {
	rep := htmlReport{
		Meta:       meta,
		DateHuman:  meta.Date.Format("January 2, 2006 15:04 MST"),
		Logo:       template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(logoPNG)),
		TestsRun:   run.TestsRun,
		StaleCount: len(run.Stale),
	}

	results := make([]runner.Result, len(run.Results))
	copy(results, run.Results)
	// Triage order: failing first, then by severity; baselined sink to the
	// bottom. Stable, so config order breaks ties.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Baselined != results[j].Baselined {
			return !results[i].Baselined
		}
		return results[i].Impact > results[j].Impact
	})

	// Same-shaped violations of one rule on one page collapse into a single
	// row with an expandable element list.
	rowIndex := map[string]int{}
	for _, res := range results {
		if res.Baselined {
			rep.Baselined++
		} else if flagged(res) {
			rep.NewCount++
		}
		key := res.RuleID + "\x00" + res.TestID + "\x00" + res.ScanLabel + "\x00" +
			boolKey(res.Baselined) + "\x00" + rowShape(res.StableTarget)
		if i, ok := rowIndex[key]; ok {
			rep.Rows[i].MoreTargets = append(rep.Rows[i].MoreTargets, res.StableTarget)
			rep.Rows[i].Failing = rep.Rows[i].Failing || flagged(res)
			continue
		}
		row := htmlRow{
			Impact:    res.Impact.String(),
			Rule:      res.RuleID,
			Test:      res.TestID,
			Scan:      res.ScanLabel,
			Summary:   res.Summary,
			Target:    res.StableTarget,
			HelpURL:   res.HelpURL,
			Baselined: res.Baselined,
			Failing:   flagged(res),
		}
		if res.ScreenshotPath != "" {
			if raw, err := os.ReadFile(res.ScreenshotPath); err == nil {
				row.Screenshot = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(raw))
			}
		}
		rowIndex[key] = len(rep.Rows)
		rep.Rows = append(rep.Rows, row)
	}

	return htmlTmpl.Execute(w, rep)
}

func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

var htmlTmpl = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Meta.App}}{{.Meta.App}} — {{end}}Frostfall Accessibility Report — {{.DateHuman}}</title>
<style>
  :root { color-scheme: light; }
  * { box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
         margin: 0; background: #f4f6f9; color: #1a2333; line-height: 1.5; }
  .wrap { max-width: 1100px; margin: 0 auto; padding: 2.5rem 1.5rem 4rem; }
  header { display: flex; align-items: center; gap: 1.1rem; }
  header img.logo { width: 84px; height: 84px; flex: none; }
  header h1 { font-size: 1.6rem; margin: 0 0 .25rem; }
  header .sub { color: #5a6b85; font-size: .95rem; }
  .metaline { margin-top: 1rem; }
  .meta { display: flex; flex-wrap: wrap; gap: .4rem 1.5rem; margin: 1rem 0 0;
          font-size: .85rem; color: #5a6b85; }
  .meta b { color: #1a2333; font-weight: 600; }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
           gap: .75rem; margin: 1.75rem 0; }
  .card { background: #fff; border: 1px solid #e2e8f2; border-radius: 8px;
          padding: .9rem 1rem; }
  .card .n { font-size: 1.7rem; font-weight: 700; }
  .card .l { font-size: .8rem; color: #5a6b85; }
  .card.bad .n { color: #b3261e; }
  .card.ok .n { color: #1a7f37; }
  .tablewrap { overflow-x: auto; background: #fff; border: 1px solid #e2e8f2;
               border-radius: 8px; }
  table { border-collapse: collapse; width: 100%; font-size: .88rem; }
  th { text-align: left; font-size: .72rem; text-transform: uppercase;
       letter-spacing: .05em; color: #5a6b85; padding: .7rem .9rem;
       border-bottom: 2px solid #e2e8f2; white-space: nowrap; }
  td { padding: .7rem .9rem; border-bottom: 1px solid #eef1f6; vertical-align: top; }
  tr:last-child td { border-bottom: none; }
  tr.baselined { color: #7a8aa3; background: #fafbfd; }
  .badge { font-size: .7rem; font-weight: 700; text-transform: uppercase;
           letter-spacing: .04em; padding: .15rem .5rem; border-radius: 999px;
           color: #fff; white-space: nowrap; }
  .badge.critical { background: #b3261e; } .badge.serious { background: #e4572e; }
  .badge.moderate { background: #d9a406; } .badge.minor { background: #8a97ab; }
  .badge.baselined { background: #eef1f6; color: #5a6b85; }
  .rule a { font-weight: 600; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
            font-size: .85rem; color: #1f5eff; text-decoration: none; }
  .rule a:hover { text-decoration: underline; }
  .rule .desc { display: block; color: #5a6b85; font-size: .8rem; max-width: 34ch; }
  .page { white-space: nowrap; }
  .page .scan { display: block; color: #8a97ab; font-size: .78rem; }
  .target { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .78rem;
            color: #40506b; overflow-wrap: anywhere; max-width: 30ch; display: inline-block; }
  .shot img { max-width: 180px; max-height: 110px; border: 1px solid #e2e8f2;
              border-radius: 4px; display: block; }
  .shot a { font-size: .75rem; color: #8a97ab; }
  details.more summary { cursor: pointer; font-size: .78rem; color: #1f5eff; margin-top: .3rem; }
  footer { margin-top: 3rem; font-size: .8rem; color: #8a97ab; }
  @media print { body { background: #fff; } tr { break-inside: avoid; } }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <img class="logo" src="{{.Logo}}" alt="">
    <div>
      <h1>{{if .Meta.App}}{{.Meta.App}} — {{end}}Accessibility Report</h1>
      <div class="sub">{{.DateHuman}}</div>
    </div>
  </header>
  <div class="meta metaline">
    {{if .Meta.BaseURL}}<span>Target <b>{{.Meta.BaseURL}}</b></span>{{end}}
    <span>Standard <b>{{.Meta.Standard}}</b></span>
    {{if .Meta.Profile}}<span>Profile <b>{{.Meta.Profile}}</b></span>{{end}}
    <span>Mode <b>{{.Meta.Mode}}</b></span>
    <span>frostfall <b>{{.Meta.ToolVersion}}</b></span>
    <span>axe-core <b>{{.Meta.AxeVersion}}</b></span>
  </div>

  <div class="cards">
    <div class="card {{if .NewCount}}bad{{else}}ok{{end}}"><div class="n">{{.NewCount}}</div><div class="l">new violations</div></div>
    <div class="card"><div class="n">{{.Baselined}}</div><div class="l">baselined (known debt)</div></div>
    <div class="card {{if .StaleCount}}ok{{end}}"><div class="n">{{.StaleCount}}</div><div class="l">fixed since baseline</div></div>
    <div class="card"><div class="n">{{.TestsRun}}</div><div class="l">tests run</div></div>
  </div>

  {{if .Rows}}
  <div class="tablewrap">
  <table>
    <thead>
      <tr>
        <th>Severity</th>
        <th>Rule</th>
        <th>Page</th>
        <th>Element</th>
        <th>Evidence</th>
      </tr>
    </thead>
    <tbody>
      {{range .Rows}}
      <tr{{if .Baselined}} class="baselined"{{end}}>
        <td><span class="badge {{.Impact}}">{{.Impact}}</span>
            {{if .Baselined}}<br><span class="badge baselined">baselined</span>{{end}}</td>
        <td class="rule">
          {{if .HelpURL}}<a href="{{.HelpURL}}" title="How to fix">{{.Rule}}</a>{{else}}{{.Rule}}{{end}}
          <span class="desc">{{.Summary}}</span>
        </td>
        <td class="page">{{.Test}}<span class="scan">{{.Scan}}</span></td>
        <td><span class="target">{{.Target}}</span>
          {{if .MoreTargets}}<details class="more"><summary>+{{len .MoreTargets}} more like this</summary>
            {{range .MoreTargets}}<span class="target">{{.}}</span><br>{{end}}
          </details>{{end}}</td>
        <td class="shot">{{if .Screenshot}}<img src="{{.Screenshot}}" alt="Screenshot of the element violating {{.Rule}}">{{else}}—{{end}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  </div>
  {{else}}<p>No violations found.</p>{{end}}

  <footer>Generated by Frostfall. Automated scanning covers a portion of
  accessibility conformance; pair with manual assistive-technology testing.</footer>
</div>
</body>
</html>
`))
