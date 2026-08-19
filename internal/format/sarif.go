package format

import (
	"encoding/json"
	"io"

	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/runner"
)

// SARIF 2.1.0 output for GitHub code scanning: one reportingDescriptor per
// axe rule, results located by page URL with the DOM selector as the logical
// location, the frostfall fingerprint as a partial fingerprint (stable alert
// identity across uploads), and baselined violations marked "unchanged" so
// known debt never opens new alerts.

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string         `json:"name"`
	Version        string         `json:"version,omitempty"`
	InformationURI string         `json:"informationUri,omitempty"`
	Rules          []sarifRule    `json:"rules"`
	Properties     map[string]any `json:"properties,omitempty"`
}

type sarifRule struct {
	ID               string       `json:"id"`
	ShortDescription sarifMessage `json:"shortDescription"`
	HelpURI          string       `json:"helpUri,omitempty"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           int               `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             sarifMessage      `json:"message"`
	Locations           []sarifLocation   `json:"locations"`
	PartialFingerprints map[string]string `json:"partialFingerprints,omitempty"`
	BaselineState       string            `json:"baselineState,omitempty"`
	Properties          map[string]any    `json:"properties,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysical  `json:"physicalLocation"`
	LogicalLocations []sarifLogical `json:"logicalLocations,omitempty"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
}

type sarifArtifact struct {
	URI string `json:"uri"`
}

type sarifLogical struct {
	FullyQualifiedName string `json:"fullyQualifiedName"`
	Kind               string `json:"kind"`
}

// sarifLevel maps axe impact to SARIF level. flagged results present as
// errors regardless of impact so enforced rules below the floor still show
// with weight.
func sarifLevel(impact engine.Impact, failing bool) string {
	switch {
	case failing, impact >= engine.Serious:
		return "error"
	case impact == engine.Moderate:
		return "warning"
	default:
		return "note"
	}
}

// SARIF writes the run as SARIF 2.1.0. flagged is the shared enforcement
// predicate.
func SARIF(w io.Writer, run *runner.Run, meta RunMeta, flagged func(runner.Result) bool) error {
	ruleIndex := map[string]int{}
	var rules []sarifRule
	results := make([]sarifResult, 0, len(run.Results))

	for _, res := range run.Results {
		idx, seen := ruleIndex[res.RuleID]
		if !seen {
			idx = len(rules)
			ruleIndex[res.RuleID] = idx
			rules = append(rules, sarifRule{
				ID:               res.RuleID,
				ShortDescription: sarifMessage{Text: res.Summary},
				HelpURI:          res.HelpURL,
			})
		}

		baselineState := "new"
		if res.Baselined {
			baselineState = "unchanged"
		}
		uri := res.PageURL
		if uri == "" {
			uri = res.TestID
		}
		results = append(results, sarifResult{
			RuleID:    res.RuleID,
			RuleIndex: idx,
			Level:     sarifLevel(res.Impact, flagged(res)),
			Message: sarifMessage{
				Text: res.Summary + " (test " + res.TestID + ", scan " + res.ScanLabel + ", at " + res.StableTarget + ")",
			},
			Locations: []sarifLocation{{
				PhysicalLocation: sarifPhysical{ArtifactLocation: sarifArtifact{URI: uri}},
				LogicalLocations: []sarifLogical{{
					FullyQualifiedName: res.StableTarget,
					Kind:               "element",
				}},
			}},
			PartialFingerprints: map[string]string{"frostfallFingerprint/v1": res.Fingerprint},
			BaselineState:       baselineState,
			Properties: map[string]any{
				"impact":    res.Impact.String(),
				"testId":    res.TestID,
				"scanLabel": res.ScanLabel,
			},
		})
	}
	if rules == nil {
		rules = []sarifRule{}
	}

	log := sarifLog{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           "Frostfall",
				Version:        meta.ToolVersion,
				InformationURI: "https://github.com/aquia-inc/frostfall",
				Rules:          rules,
				Properties: map[string]any{
					"axeVersion": meta.AxeVersion,
					"standard":   meta.Standard,
					"mode":       meta.Mode,
				},
			}},
			Results: results,
		}},
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(log)
}
