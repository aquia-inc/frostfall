package format

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/runner"
)

func sarifFixtureRun() *runner.Run {
	return &runner.Run{
		TestsRun: 2,
		Results: []runner.Result{
			{
				Violation:    engine.Violation{RuleID: "image-alt", Impact: engine.Critical, Summary: "Images need alt text", HelpURL: "https://example.com/image-alt"},
				TestID:       "home",
				ScanLabel:    "initial",
				Fingerprint:  "aaaa",
				StableTarget: "#hero > img",
				PageURL:      "http://localhost:5173/",
			},
			{
				Violation:    engine.Violation{RuleID: "image-alt", Impact: engine.Critical, Summary: "Images need alt text", HelpURL: "https://example.com/image-alt"},
				TestID:       "about",
				ScanLabel:    "initial",
				Fingerprint:  "bbbb",
				StableTarget: "#footer > img",
				PageURL:      "http://localhost:5173/#/about",
				Baselined:    true,
			},
			{
				Violation:    engine.Violation{RuleID: "color-contrast", Impact: engine.Minor, Summary: "Contrast too low", HelpURL: "https://example.com/contrast"},
				TestID:       "home",
				ScanLabel:    "modal",
				Fingerprint:  "cccc",
				StableTarget: ".chip",
				PageURL:      "http://localhost:5173/",
			},
		},
	}
}

func TestSARIFOutput(t *testing.T) {
	run := sarifFixtureRun()
	// Enforced-rule semantics: color-contrast is flagged despite minor impact.
	flagged := func(r runner.Result) bool {
		return !r.Baselined && (r.Impact >= engine.Serious || r.RuleID == "color-contrast")
	}

	var buf bytes.Buffer
	if err := SARIF(&buf, run, RunMeta{ToolVersion: "test", AxeVersion: "4.10.3", Standard: "section508", Mode: "report only"}, flagged); err != nil {
		t.Fatal(err)
	}

	var log struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name  string `json:"name"`
					Rules []struct {
						ID      string `json:"id"`
						HelpURI string `json:"helpUri"`
					} `json:"rules"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleID              string            `json:"ruleId"`
				RuleIndex           int               `json:"ruleIndex"`
				Level               string            `json:"level"`
				PartialFingerprints map[string]string `json:"partialFingerprints"`
				Locations           []struct {
					PhysicalLocation struct {
						ArtifactLocation struct {
							URI string `json:"uri"`
						} `json:"artifactLocation"`
						Region struct {
							StartLine int `json:"startLine"`
						} `json:"region"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if log.Version != "2.1.0" || len(log.Runs) != 1 {
		t.Fatalf("bad log shape: version=%s runs=%d", log.Version, len(log.Runs))
	}
	r := log.Runs[0]
	// Rules dedupe: image-alt appears twice in results, once in rules.
	if len(r.Tool.Driver.Rules) != 2 {
		t.Errorf("want 2 deduped rules, got %d", len(r.Tool.Driver.Rules))
	}
	// Baselined results are omitted: GitHub ignores baselineState, so known
	// debt must not open alerts, and its absence closes existing ones.
	if len(r.Results) != 2 {
		t.Fatalf("want 2 results (baselined omitted), got %d", len(r.Results))
	}
	// ruleIndex must point at the right rule.
	for _, res := range r.Results {
		if r.Tool.Driver.Rules[res.RuleIndex].ID != res.RuleID {
			t.Errorf("ruleIndex mismatch for %s", res.RuleID)
		}
	}
	// Flagged minor is promoted to error.
	if r.Results[0].Level != "error" || r.Results[1].Level != "error" {
		t.Errorf("levels wrong: critical=%s, flagged minor=%s", r.Results[0].Level, r.Results[1].Level)
	}
	// primaryLocationLineHash is the only key GitHub uses for alert identity;
	// it must carry the frostfall fingerprint.
	if r.Results[0].PartialFingerprints["primaryLocationLineHash"] != "aaaa" {
		t.Errorf("primaryLocationLineHash missing/wrong: %v", r.Results[0].PartialFingerprints)
	}
	// Locations are stable synthetic relative paths with a region - absolute
	// http URIs are rejected at upload, and startLine is required to render.
	loc := r.Results[0].Locations[0].PhysicalLocation
	if loc.ArtifactLocation.URI != "frostfall/home" {
		t.Errorf("location uri = %s", loc.ArtifactLocation.URI)
	}
	if loc.Region.StartLine != 1 {
		t.Errorf("region.startLine = %d", loc.Region.StartLine)
	}
}

func TestSARIFEmptyRun(t *testing.T) {
	var buf bytes.Buffer
	if err := SARIF(&buf, &runner.Run{}, RunMeta{}, func(runner.Result) bool { return false }); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("empty run must still be valid SARIF: %v", err)
	}
}
