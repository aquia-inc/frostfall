package discover

import (
	"net/url"
	"reflect"
	"testing"
)

// The browser reports every href on an IDN host in punycode, so a base URL
// written in Unicode must still keep them as same-origin links. The port is
// still part of the host, and an ASCII host is compared as written.
func TestFilterLinksIDNBase(t *testing.T) {
	for _, raw := range []string{"http://café.example:5173", "http://CAFÉ.example:5173", "http://xn--caf-dma.example:5173"} {
		base, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		hrefs := []string{
			"http://xn--caf-dma.example:5173/menu",
			"http://xn--caf-dma.example:5173/",
			"http://xn--caf-dma.example:9999/other-port",
			"http://cafe.example:5173/lookalike",
			"http://xn--caf-dma.example/no-port",
		}
		got := FilterLinks(base, hrefs, nil)
		if want := []string{"/", "/menu"}; !reflect.DeepEqual(got, want) {
			t.Errorf("base %s: got %v, want %v", raw, got, want)
		}
	}
	// ASCII hosts keep the raw comparison they always had.
	base, _ := url.Parse("http://localhost:5173")
	got := FilterLinks(base, []string{"http://localhost:5173/a", "http://LOCALHOST:5173/b"}, nil)
	if want := []string{"/a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ASCII base: got %v, want %v", got, want)
	}
}
