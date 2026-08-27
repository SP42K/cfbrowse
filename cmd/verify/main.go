// Command verify checks that a URL can be reached through its bot challenge.
//
// It reports the page title and whether a cf_clearance cookie was obtained.
// Extraction is deliberately not part of this tool: pass your own JavaScript
// with -eval and keep site selectors in your own project, where they can rot
// on their own schedule.
//
//	go run ./cmd/verify 'https://example.com'                   # headed, solve once
//	go run ./cmd/verify -headless 'https://example.com'         # thereafter
//	go run ./cmd/verify -headless -dump 'https://example.com'   # page shape
//	go run ./cmd/verify -headless -eval 'document.title' 'https://example.com'
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/SP42K/cfbrowse"
)

// shapeJS is site-agnostic on purpose: it reports what a page is made of so
// you can write selectors elsewhere.
const shapeJS = `(() => {
  const c = {};
  document.querySelectorAll('[class]').forEach(e =>
    e.classList.forEach(k => c[k] = (c[k]||0)+1));
  const tags = {};
  ['article','section','li','h1','h2','h3','a[href]','img'].forEach(sel =>
    tags[sel] = document.querySelectorAll(sel).length);
  return JSON.stringify({
    tags,
    classes: Object.entries(c).filter(([,n]) => n >= 5)
               .sort((a,b) => b[1]-a[1]).slice(0, 20),
  }, null, 1);
})()`

// main does nothing but set the exit code. Every early exit used to go through
// log.Fatal, whose os.Exit skips deferred calls — so a failed run left its
// whole Chrome process tree running and holding the profile lock. Errors have
// to travel back here as values for `defer b.Close()` to ever run.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	headless := flag.Bool("headless", false, "run without a window")
	profile := flag.String("profile", "", "persistent profile dir (keeps cf_clearance)")
	wait := flag.Duration("wait", 90*time.Second, "how long to allow for the challenge")
	settle := flag.Duration("settle", 0, "extra pause after the challenge clears, for SPAs")
	solve := flag.Bool("solve", false, "click the challenge widget instead of waiting for a human")
	dump := flag.Bool("dump", false, "print the page's tag and class shape")
	eval := flag.String("eval", "", "JavaScript to run once the page is reachable")
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	b, err := cfbrowse.Launch(cfbrowse.Options{UserDataDir: *profile, Headless: *headless})
	if err != nil {
		return err
	}
	defer b.Close()

	if err := b.Navigate(flag.Arg(0)); err != nil {
		return err
	}
	reach := b.WaitReady
	if *solve {
		reach = b.Solve
	}
	title, err := reach(*wait)
	if err != nil {
		return fmt.Errorf("%w\n(retry with -solve, or headed so you can click it yourself)", err)
	}
	// Whether the widget was actually clicked is not visible in the outcome:
	// a challenge that clears itself looks identical to one that was solved.
	// Counting the dispatched input is the only way to tell them apart.
	moves := 0
	for _, m := range b.SentMethods() {
		if m == "Input.dispatchMouseEvent" {
			moves++
		}
	}
	fmt.Fprintf(os.Stderr, "title: %s\ncf_clearance: %v\nmouse events sent: %d\n",
		title, b.HasClearance(), moves)

	if *settle > 0 {
		time.Sleep(*settle)
	}
	js := *eval
	if *dump {
		js = shapeJS
	}
	if js == "" {
		return nil
	}
	out, err := b.EvalString(js)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}
