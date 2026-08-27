package cfbrowse

import (
	"strings"
	"testing"
	"time"
)

// The entire reason this package exists: a full navigate + evaluate cycle must
// complete without the Runtime domain ever being enabled. If this fails, the
// package has no advantage over chromedp and should be deleted.
func TestNeverEnablesRuntime(t *testing.T) {
	b, err := Launch(Options{Headless: true})
	if err != nil {
		t.Skipf("chrome unavailable: %v", err)
	}
	defer b.Close()

	if err := b.Navigate("https://example.com"); err != nil {
		t.Fatal(err)
	}
	title, err := b.WaitReady(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(title, "Example") {
		t.Fatalf("title = %q, want it to mention Example", title)
	}
	if v, err := b.EvalString("document.querySelector('h1').textContent"); err != nil || v == "" {
		t.Fatalf("DOM unreachable from the isolated world: %q %v", v, err)
	}
	for _, m := range b.SentMethods() {
		if m == "Runtime.enable" {
			t.Fatal("Runtime.enable was sent — the leak this package exists to avoid")
		}
	}
}
