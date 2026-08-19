package axe

import (
	"testing"

	"github.com/aquia-inc/frostfall/internal/engine"
)

func TestImpactOrDefault(t *testing.T) {
	cases := map[string]engine.Impact{
		"minor":    engine.Minor,
		"moderate": engine.Moderate,
		"serious":  engine.Serious,
		"critical": engine.Critical,
		// Unknown and empty impacts must land AT the default enforcement
		// floor, not below it - axe emitting a value we don't recognize has
		// to fail builds, not silently pass them (issue #7).
		"":         engine.Serious,
		"severe":   engine.Serious,
		"CRITICAL": engine.Serious,
	}
	for in, want := range cases {
		if got := impactOrDefault(in); got != want {
			t.Errorf("impactOrDefault(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestVersionParsedFromBundle(t *testing.T) {
	if v := New().Version(); v == "unknown" || v == "" {
		t.Errorf("embedded axe version not parsed: %q", v)
	}
}
