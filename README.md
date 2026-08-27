# cfbrowse

Minimal Go CDP driver that never sends `Runtime.enable`.

## Why

Cloudflare's bot scoring reads the **control channel**, not just the page.
`chromedp`, `go-rod`, `go-rod/stealth` and `chromedp-undetected` all patch the
fingerprint layer (`navigator.webdriver`, UA, `window.chrome`) but still enable
the Runtime domain on attach, and that alone is enough to fail a challenge.

This package evaluates JavaScript through `Page.createIsolatedWorld` +
`Runtime.evaluate` with an explicit `contextId`, so the Runtime domain is never
enabled. `browser_test.go` asserts the invariant.

## Use

```go
b, _ := cfbrowse.Launch(cfbrowse.Options{
    UserDataDir: os.ExpandEnv("$HOME/.cfbrowse-profiles/site"), // keeps cf_clearance
    Headless:    true,
})
defer b.Close()

b.Navigate("https://example.com")
title, _ := b.WaitReady(90 * time.Second)
html, _ := b.EvalString("document.body.innerHTML")
```

Interactive challenges still need a human once. Run headed, solve the checkbox,
and the `cf_clearance` cookie persists in the profile; every later run can be
headless.

`cmd/verify` is a reachability check, not a scraper. It reports the title and
whether a `cf_clearance` cookie was obtained; `-eval` runs your JavaScript.
Site selectors belong in your project, not here.

```sh
go run ./cmd/verify -profile ~/.cfbrowse-profiles/site 'https://…'            # solve once
go run ./cmd/verify -headless -profile ~/.cfbrowse-profiles/site 'https://…'  # thereafter
go run ./cmd/verify -headless -dump 'https://…'                              # page shape
go run ./cmd/verify -headless -eval 'document.title' 'https://…'
go test ./...
```

## Limits

- Isolated worlds see the DOM but not the page's own JS globals.
- No event subscriptions; waiting is done by polling.
- One page target, no tab management.
- Clearance lifetime varies wildly by site and is the thing that will bite you.
  One site tested had its clearance rejected eleven minutes after a solve;
  another's survived across many runs. Headless is not the discriminator — a
  headless run fifty seconds after a fresh solve passed in nine. Expect to
  re-solve, and treat a returning challenge as expiry rather than as a
  fingerprinting problem.
- Fingerprint hardening stops at UA + `AutomationControlled`. On WSL,
  `WEBGL_debug_renderer_info` returns nothing (no GPU), which a strict checker
  can score against.
