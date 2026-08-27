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
	"log"
	"os"
	"time"

	"cfbrowse"
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

func main() {
	headless := flag.Bool("headless", false, "run without a window")
	profile := flag.String("profile", "", "persistent profile dir (keeps cf_clearance)")
	wait := flag.Duration("wait", 90*time.Second, "how long to allow for the challenge")
	settle := flag.Duration("settle", 0, "extra pause after the challenge clears, for SPAs")
	dump := flag.Bool("dump", false, "print the page's tag and class shape")
	eval := flag.String("eval", "", "JavaScript to run once the page is reachable")
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	b, err := cfbrowse.Launch(cfbrowse.Options{UserDataDir: *profile, Headless: *headless})
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()

	if err := b.Navigate(flag.Arg(0)); err != nil {
		log.Fatal(err)
	}
	title, err := b.WaitReady(*wait)
	if err != nil {
		log.Fatalf("%v\n(rerun without -headless and solve the challenge once)", err)
	}
	fmt.Fprintf(os.Stderr, "title: %s\ncf_clearance: %v\n", title, b.HasClearance())

	if *settle > 0 {
		time.Sleep(*settle)
	}
	js := *eval
	if *dump {
		js = shapeJS
	}
	if js == "" {
		return
	}
	out, err := b.EvalString(js)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out)
}
