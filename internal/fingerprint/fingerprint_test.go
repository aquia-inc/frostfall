package fingerprint

import "testing"

// The stability suite: DOM mutations that must NOT change a fingerprint, and
// real changes that MUST. These cases are the executable form of the frozen
// contract in DESIGN.md §5 — a failing case here means checked-in baselines
// break.

func fp(target string) string {
	return Fingerprint("test", "initial", "color-contrast", target)
}

func TestStableAcrossHashClassChurn(t *testing.T) {
	a := fp("#app > nav > button.css-1abc2d")
	b := fp("#app > nav > button.css-9xyz8q")
	if a != b {
		t.Errorf("emotion class churn changed fingerprint: %s vs %s", a, b)
	}
}

func TestStableAcrossMUIEmotionHash(t *testing.T) {
	// Found dogfooding against ztmf-ui: MUI emits css-<hash>-MuiChip-root,
	// where the hash is short and non-hex; the suffixed form must be stripped
	// like the bare emotion hash.
	a := fp(".MuiChip-label.css-rrn746-MuiChip-label > span")
	b := fp(".MuiChip-label.css-zz91xq-MuiChip-label > span")
	if a != b {
		t.Errorf("MUI emotion hash churn changed fingerprint")
	}
	if got := StableTarget(".MuiChip-label.css-rrn746-MuiChip-label"); got != ".MuiChip-label" {
		t.Errorf("stableTarget kept hashed class: %q", got)
	}
}

func TestEmotionOnlyClassKeepsSuffixIdentity(t *testing.T) {
	// An element styled ONLY by a suffixed emotion class must not collapse to
	// "*": the stable suffix is the identity (found dogfooding ztmf-ui).
	if got := StableTarget(".css-133jsql-MuiTypography-root"); got != ".MuiTypography-root" {
		t.Errorf("got %q, want .MuiTypography-root", got)
	}
	a := fp(".css-133jsql-MuiTypography-root")
	b := fp(".css-9k1abc-MuiTypography-root")
	if a != b {
		t.Errorf("emotion hash churn changed fingerprint for suffix-only class")
	}
	c := fp(".css-133jsql-MuiAlert-root")
	if a == c {
		t.Errorf("different suffixes collapsed into one fingerprint")
	}
}

func TestStableAcrossCSSModuleHash(t *testing.T) {
	a := fp("div.button_a1b2c3 > span")
	b := fp("div.button_z9y8x7 > span")
	if a != b {
		t.Errorf("CSS-module hash churn changed fingerprint")
	}
}

func TestNthChildDroppedWhenOtherFeaturePresent(t *testing.T) {
	a := fp("#list > li.item:nth-child(2)")
	b := fp("#list > li.item:nth-child(5)")
	if a != b {
		t.Errorf("sibling insertion changed fingerprint despite surviving class")
	}
}

func TestNthChildKeptAsLastResort(t *testing.T) {
	a := fp("#list > li:nth-child(2)")
	b := fp("#list > li:nth-child(5)")
	if a == b {
		t.Errorf("positional-only siblings collapsed into one fingerprint")
	}
}

func TestGeneratedIDNotUsedAsAnchor(t *testing.T) {
	// A regenerated hex id must not fork identity; the stable ancestor anchors.
	a := StableTarget("#sidebar > div#a1b2c3d4e5 > button")
	b := StableTarget("#sidebar > div#f6e5d4c3b2 > button")
	if a == b {
		return
	}
	t.Errorf("generated ids forked stableTarget: %q vs %q", a, b)
}

func TestAnchorsAtStableID(t *testing.T) {
	// Everything above a stable id is noise.
	a := fp("html > body > div > main > #sidebar > nav > a")
	b := fp("html > body > div.wrapper > #sidebar > nav > a")
	if a != b {
		t.Errorf("ancestor churn above stable id changed fingerprint")
	}
}

func TestStableAcrossEscapedReactID(t *testing.T) {
	// Codex review finding: axe emits id=":r1a:" CSS-escaped as #\:r1a\:.
	// The volatile id must be stripped despite the backslash escaping.
	a := fp(`#app > div#\:r1a\: > button`)
	b := fp(`#app > div#\:r5n\: > button`)
	if a != b {
		t.Errorf("escaped React useId churn changed fingerprint")
	}
	// And an escaped React id must not anchor the path.
	if got := StableTarget(`#app > div#\:r1a\: > button`); got != "#app > div > button" {
		t.Errorf("got %q, want %q", got, "#app > div > button")
	}
}

func TestStableAcrossReactUseIDAttr(t *testing.T) {
	// Found dogfooding the ztmf-ui pillar scores modal: MUI Select renders
	// [aria-controls=":r5n:"] where the value is a per-session React useId.
	a := fp(`div[aria-controls=":r5n:"]`)
	b := fp(`div[aria-controls=":r2a:"]`)
	if a != b {
		t.Errorf("React useId churn changed fingerprint")
	}
	if got := StableTarget(`div[aria-controls=":r5n:"]`); got != "div[aria-controls]" {
		t.Errorf("got %q, want div[aria-controls]", got)
	}
}

func TestDifferentRuleDiffers(t *testing.T) {
	a := Fingerprint("test", "initial", "color-contrast", "#x > button")
	b := Fingerprint("test", "initial", "button-name", "#x > button")
	if a == b {
		t.Errorf("different rules produced identical fingerprints")
	}
}

func TestDifferentScanLabelDiffers(t *testing.T) {
	a := Fingerprint("test", "initial", "color-contrast", "#x")
	b := Fingerprint("test", "modal-open", "color-contrast", "#x")
	if a == b {
		t.Errorf("different scan points produced identical fingerprints")
	}
}

func TestFieldBoundariesNotAmbiguous(t *testing.T) {
	// Joining with \x00 must prevent "ab"+"c" == "a"+"bc" collisions.
	a := Fingerprint("ab", "c", "r", "#x")
	b := Fingerprint("a", "bc", "r", "#x")
	if a == b {
		t.Errorf("field boundary collision")
	}
}
