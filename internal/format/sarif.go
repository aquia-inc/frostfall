package format

import (
	"encoding/json"
	"io"

	"github.com/aquia-inc/frostfall/internal/engine"
	"github.com/aquia-inc/frostfall/internal/runner"
)

// SARIF 2.1.0 output for GitHub code scanning: one reportingDescriptor per
// axe rule, results located at stable synthetic repo-relative paths
// (frostfall/<testId>) with the DOM selector as the logical location and the
// page URL in the message.
//
// The location choices are load-bearing for GitHub ingestion:
//   - Absolute http(s) URIs are REJECTED at upload (scheme mismatch with the
//     file:// source root codeql-action always sends), and host:port paths
//     would churn on ephemeral --serve ports. Synthetic relative paths are
//     accepted; alerts appear in the Security tab (no inline PR annotations -
//     the honest ceiling for scanning rendered pages rather than source).
//   - region.startLine is required for an alert to render.
//   - primaryLocationLineHash is the only fingerprint key GitHub uses for
//     alert identity, and codeql-action cannot synthesize it for paths not on
//     disk - so it carries the frostfall fingerprint, making alert identity
//     exactly the violation's baseline identity.
//   - baselineState is ignored by GitHub, so baselined violations are OMITTED
//     instead: known debt opens no alerts, and baselining an existing
//     violation closes its alert on the next upload (absent = fixed).

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
	FullDescription  sarifMessage `json:"fullDescription"`
	Help             sarifMessage `json:"help"`
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
	Properties          map[string]any    `json:"properties,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysical  `json:"physicalLocation"`
	LogicalLocations []sarifLogical `json:"logicalLocations,omitempty"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
	Region           sarifRegion   `json:"region"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
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
		if res.Baselined {
			continue
		}
		idx, seen := ruleIndex[res.RuleID]
		if !seen {
			idx = len(rules)
			ruleIndex[res.RuleID] = idx
			// GitHub's required-properties table demands non-empty
			// fullDescription.text and help.text on every rule.
			help := res.HelpURL
			if help == "" {
				help = res.Summary
			}
			rules = append(rules, sarifRule{
				ID:               res.RuleID,
				ShortDescription: sarifMessage{Text: res.Summary},
				FullDescription:  sarifMessage{Text: res.Summary},
				Help:             sarifMessage{Text: help},
				HelpURI:          res.HelpURL,
			})
		}

		results = append(results, sarifResult{
			RuleID:    res.RuleID,
			RuleIndex: idx,
			Level:     sarifLevel(res.Impact, flagged(res)),
			Message: sarifMessage{
				Text: res.Summary + " (page " + res.PageURL + ", test " + res.TestID + ", scan " + res.ScanLabel + ", at " + res.StableTarget + ")",
			},
			Locations: []sarifLocation{{
				PhysicalLocation: sarifPhysical{
					ArtifactLocation: sarifArtifact{URI: "frostfall/" + res.TestID},
					Region:           sarifRegion{StartLine: 1},
				},
				LogicalLocations: []sarifLogical{{
					FullyQualifiedName: res.StableTarget,
					Kind:               "element",
				}},
			}},
			PartialFingerprints: map[string]string{
				"primaryLocationLineHash": res.Fingerprint,
				"frostfallFingerprint/v1": res.Fingerprint,
			},
			Properties: map[string]any{
				"impact":    res.Impact.String(),
				"pageUrl":   res.PageURL,
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
