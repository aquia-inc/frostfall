package wcag

import "testing"

func TestFromTags(t *testing.T) {
	criteria, provisions, s508 := FromTags([]string{"cat.color", "wcag2aa", "wcag143", "section508", "section508.22.i"})
	if !s508 {
		t.Errorf("section508 tag not detected")
	}
	if len(provisions) != 1 || provisions[0] != "1194.22(i)" {
		t.Errorf("provision parse: %v", provisions)
	}
	if len(criteria) != 1 || criteria[0].Number != "1.4.3" || criteria[0].Level != "AA" {
		t.Fatalf("got %+v", criteria)
	}
	// Two-digit criterion numbers parse (wcag1410 = 1.4.10).
	criteria, _, _ = FromTags([]string{"wcag1410"})
	if len(criteria) != 1 || criteria[0].Number != "1.4.10" || criteria[0].Name != "Reflow" {
		t.Fatalf("got %+v", criteria)
	}
}

func TestLabel(t *testing.T) {
	got := Label([]string{"wcag2a", "wcag412", "section508", "section508.22.a"})
	want := "4.1.2 Name, Role, Value (WCAG 2.0 A) · Section 508 (E205.4; 1194.22(a))"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if Label([]string{"best-practice", "cat.aria"}) != "" {
		t.Errorf("best-practice rules should have no label")
	}
	// WCAG 2.0 criteria are 508 requirements by incorporation even without a
	// legacy axe provision tag (e.g. aria-required-children, color-contrast).
	got = Label([]string{"cat.aria", "wcag2a", "wcag131"})
	want = "1.3.1 Info and Relationships (WCAG 2.0 A) · Section 508 (E205.4)"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// A 2.1-only criterion is NOT a 508 requirement; no 508 suffix.
	got = Label([]string{"wcag21aa", "wcag1411"})
	want = "1.4.11 Non-text Contrast (WCAG 2.1 AA)"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
