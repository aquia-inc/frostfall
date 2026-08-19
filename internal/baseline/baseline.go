// Package baseline implements the accept-current, fail-on-new mechanism that
// makes Frostfall adoptable in codebases with existing violations.
package baseline

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type File struct {
	Version    int         `json:"version"`
	AxeVersion string      `json:"axeVersion"`
	Created    time.Time   `json:"created"`
	Violations []Violation `json:"violations"`
}

type Violation struct {
	Fingerprint  string `json:"fingerprint"`
	TestID       string `json:"testId"`
	ScanLabel    string `json:"scanLabel"`
	RuleID       string `json:"ruleId"`
	StableTarget string `json:"stableTarget"`
	// Note is human context, preserved across --update-baseline.
	Note string `json:"note,omitempty"`
}

// Load reads a baseline file; a missing file returns an empty baseline, since
// "no baseline yet" is the normal day-one state, not an error.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &File{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported baseline version %d", path, f.Version)
	}
	return &f, nil
}

// Save writes atomically (temp file + rename): the baseline is a committed
// file, and an interrupted write must not corrupt it.
func (f *File) Save(path string) error {
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Index returns fingerprint -> entry for matching current results.
func (f *File) Index() map[string]Violation {
	idx := make(map[string]Violation, len(f.Violations))
	for _, v := range f.Violations {
		idx[v.Fingerprint] = v
	}
	return idx
}
