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

// Pinned outputs for targets in the top document, computed with the scheme as
// it stood before frame-qualified targets existed. Frame support must leave
// every one of them byte for byte as it was: these are the fingerprints in
// users' committed baselines. The attribute value holding " |> " must not be
// read as a frame boundary.
func TestTopDocumentTargetsPinned(t *testing.T) {
	cases := []struct{ raw, stable, fp string }{
		{"img", "img", "f784165ebae6dff8c9c1152f80ab8ba8"},
		{"#unlabeled", "#unlabeled", "3f535e9b2cbc24350269f8cb3d4cf1da"},
		{"#app > p:nth-child(4)", "#app > p:nth-child(4)", "dab04d2a8433f047fe789e525d59e05d"},
		{`#app > div#\:r1a\: > button`, "#app > div > button", "fbe96fce05ce42e57ee0b3e11db1fb35"},
		{"html > body > div.wrapper > #sidebar > nav > a", "#sidebar > nav > a", "8be120c00dd1cde496b47b58e7e56ec5"},
		{".MuiChip-label.css-rrn746-MuiChip-label > span", ".MuiChip-label > span", "631f49c6c9ac6a2cb4ef8102eafbed23"},
		{`main > a[href="/dl?token=Q7xK2pLm9Zw"]`, `main > a[href="/dl?token=Q7xK2pLm9Zw"]`, "16f8ce684bcd5e5ebc491c8ef19492c7"},
		{`a[title="x |> y"]`, `a[title="x > | > y"]`, "140b72444f925413a95fc4ce55961642"},
		{"main > a[href]", "main > a[href]", "8533d4825fac091713c177ef00a194ca"},
		{"#list > li:nth-child(2)", "#list > li:nth-child(2)", "b13a77478322cb9d672dd569894091fc"},
		{"my-app >>> button.css-1abc2d", "my-app > button", "1a9c5cd9b80f196497ac114b41fc7dd8"},
	}
	for _, c := range cases {
		if got := StableTarget(c.raw); got != c.stable {
			t.Errorf("StableTarget(%q) = %q, want %q", c.raw, got, c.stable)
		}
		if got := Fingerprint("fixture-initial", "initial", "image-alt", c.raw); got != c.fp {
			t.Errorf("Fingerprint(%q) = %s, want %s", c.raw, got, c.fp)
		}
	}
}

func TestSplitFrames(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"img", []string{"img"}},
		{"#frame |> img", []string{"#frame", "img"}},
		{"#outer |> #inner |> main > p", []string{"#outer", "#inner", "main > p"}},
		// The separator inside a quoted or bracketed value is part of it.
		{`iframe[title="a |> b"] |> img`, []string{`iframe[title="a |> b"]`, "img"}},
		{`iframe[title='a |> b'] |> img`, []string{`iframe[title='a |> b']`, "img"}},
		{`a[title="x |> y"]`, []string{`a[title="x |> y"]`}},
		// An escaped quote does not end the value.
		{`iframe[title="say \" |> "] |> img`, []string{`iframe[title="say \" |> "]`, "img"}},
		{"host >>> iframe |> button", []string{"host >>> iframe", "button"}},
	}
	for _, c := range cases {
		got := SplitFrames(c.in)
		if len(got) != len(c.want) {
			t.Errorf("SplitFrames(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitFrames(%q) = %q, want %q", c.in, got, c.want)
				break
			}
		}
	}
}

func TestFrameTargetNormalizedPerLevel(t *testing.T) {
	cases := map[string]string{
		// Each level normalizes on its own: the hash class goes in both.
		"#app > iframe.css-1abc2d |> main > button.css-9xyz8q": "#app > iframe |> main > button",
		// An id anchor inside the frame truncates that level only; the path
		// to the frame survives.
		"iframe#pay |> html > body > div > #card > input": "iframe#pay |> #card > input",
		// A generated id in the frame path is stripped like anywhere else.
		"iframe#a1b2c3d4e5 |> img": "iframe |> img",
		// Three levels.
		"#outer |> iframe.inner |> p:nth-child(3)": "#outer |> iframe.inner |> p:nth-child(3)",
	}
	for in, want := range cases {
		if got := StableTarget(in); got != want {
			t.Errorf("StableTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFrameTargetFingerprints(t *testing.T) {
	// Hash churn inside the frame does not fork identity.
	if fp("#pay |> button.css-1abc2d") != fp("#pay |> button.css-9xyz8q") {
		t.Errorf("hash class churn inside a frame changed the fingerprint")
	}
	// The same node in two different frames is two violations.
	if fp("#pay |> #card") == fp("#chat |> #card") {
		t.Errorf("same selector in different frames collapsed into one fingerprint")
	}
	// A node in a frame is not the same violation as a top-document node
	// with the same selector.
	if fp("#pay |> #card") == fp("#card") {
		t.Errorf("frame node collapsed into the top-document node")
	}
	// Stable across runs: the same input always gives the same output.
	if fp("#pay |> main > img") != fp("#pay |> main > img") {
		t.Errorf("fingerprint not deterministic")
	}
}
