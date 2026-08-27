package cfbrowse

import (
	"fmt"
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
	// Stated as an allowlist rather than a Runtime.enable denylist: enabling
	// any other domain is the same class of mistake, and this way a future
	// feature that reaches for Network.enable or DOM.enable trips the gate too.
	for _, m := range b.SentMethods() {
		if strings.HasSuffix(m, ".enable") && m != "Page.enable" {
			t.Fatalf("%s was sent; Page.enable is the only domain this package may enable", m)
		}
	}
}

// defaultChrome is the one place this package guesses about the host, and the
// guess is silent when wrong: a bad exec path only shows up as Chrome not
// starting. The UA is no longer guessed at all — see TestUserAgentIsNotHeadless.
func TestDefaultChrome(t *testing.T) {
	if defaultChrome() == "" {
		t.Fatal("defaultChrome returned an empty path; exec would report a useless error")
	}
}

// The UA rewrite is what two failed challenge runs came down to, and it is the
// one piece testable without a browser.
func TestFrozenUA(t *testing.T) {
	const win = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) %s Safari/537.36"
	want := fmt.Sprintf(win, "Chrome/151.0.0.0")

	for _, token := range []string{
		"HeadlessChrome/151.0.7922.175", // what headless Chrome actually reports
		"Chrome/151.0.7922.175",         // headed, full build number
		"Chrome/151.0.0.0",              // already frozen; must survive untouched
	} {
		if got := frozenUA(fmt.Sprintf(win, token)); got != want {
			t.Errorf("frozenUA(%s) = %q, want %q", token, got, want)
		}
	}
}

// cf_clearance is scored against the UA that earned it, and an interactive
// challenge rejects a UA whose version or platform disagrees with the client
// hints Chrome sends alongside it. Taking the browser's own UA and removing only
// the headless token is what keeps the two consistent.
func TestUserAgentIsNotHeadless(t *testing.T) {
	b, err := Launch(Options{Headless: true})
	if err != nil {
		t.Skipf("chrome unavailable: %v", err)
	}
	defer b.Close()

	if strings.Contains(b.UserAgent(), "Headless") {
		t.Fatalf("UA still says headless: %q", b.UserAgent())
	}
	if err := b.Navigate("https://example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WaitReady(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	// The override has to reach the renderer, not just our own bookkeeping.
	seen, err := b.EvalString("navigator.userAgent")
	if err != nil {
		t.Fatal(err)
	}
	if seen != b.UserAgent() {
		t.Fatalf("page sends %q, UserAgent() reports %q", seen, b.UserAgent())
	}
}
