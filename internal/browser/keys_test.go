package browser

import (
	"testing"

	"github.com/go-rod/rod/lib/input"
)

func TestKeyForNamedAndSingle(t *testing.T) {
	if k, err := keyFor("Tab"); err != nil || k != input.Tab {
		t.Errorf("Tab: %v %v", k, err)
	}
	if k, err := keyFor("a"); err != nil || k != input.Key('a') {
		t.Errorf("single char: %v %v", k, err)
	}
}

func TestKeyForUnknownNameErrors(t *testing.T) {
	// Issue #5: "Ctrl+A" must error, not silently press C.
	for _, name := range []string{"Ctrl+A", "F13", "Return"} {
		if _, err := keyFor(name); err == nil {
			t.Errorf("keyFor(%q) should error", name)
		}
	}
}
