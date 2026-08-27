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
import "github.com/SP42K/cfbrowse"

b, err := cfbrowse.Launch(cfbrowse.Options{
    UserDataDir: os.ExpandEnv("$HOME/.cfbrowse-profiles/site"), // keeps cf_clearance
    Headless:    true,
})
if err != nil {
    return err
}
defer b.Close()

if err := b.Navigate("https://example.com"); err != nil {
    return err
}
// Solve clicks a challenge if one appears; WaitReady only waits one out.
title, err := b.Solve(90 * time.Second)
if err != nil {
    return err
}
html, err := b.EvalString("document.body.innerHTML")
```

It is a private module, so point Go at it directly:

```sh
go env -w GOPRIVATE=github.com/SP42K/*
go get github.com/SP42K/cfbrowse
```

Or consume it from a checkout without a remote at all:

```sh
go mod edit -replace github.com/SP42K/cfbrowse=/path/to/cfbrowse
```

`Solve` clears an interactive challenge on its own, headless, from an empty
profile. No human, no window, no first run that differs from the rest. Measured
against two sites, both of which serve an interactive challenge: headed and
headless produced identical results — sixteen mouse events, `cf_clearance`
obtained, full content — so the mode is not a variable.

Use a persistent `UserDataDir` anyway. Solving costs a few seconds and the
`cf_clearance` it earns is reused until the site expires it, which on one site
measured took under eleven minutes and on another survived many runs.

`cmd/verify` is a reachability check, not a scraper. It reports the title,
whether a `cf_clearance` cookie was obtained, and how many mouse events were
dispatched; `-eval` runs your JavaScript. Site selectors belong in your project,
not here.

```sh
go run ./cmd/verify -headless -solve -profile ~/.cfbrowse-profiles/site 'https://…'
go run ./cmd/verify -headless -dump 'https://…'                    # page shape
go run ./cmd/verify -headless -eval 'document.title' 'https://…'
go run ./cmd/verify -solve 'https://…'                             # headed, to watch it work
go test ./...
```

## Solving the checkbox

`Solve` clears an interactive challenge without a human. Two things make it
work, and neither costs the invariant:

- **The click is real input.** `Input.dispatchMouseEvent` is injected below
  Blink's event plumbing, so the page sees `isTrusted` events. `element.click()`
  could not work even in principle — the widget is not reachable from script,
  and synthetic DOM events are marked untrusted anyway. The cursor is walked in
  over six steps before the press; arriving instantly is itself a tell.
- **The widget is found through CDP, not JavaScript.** Cloudflare renders it
  inside a closed shadow root, so a page visibly showing a checkbox reports zero
  iframes and zero open shadow roots to `querySelectorAll`. `DOM.getDocument`
  with `pierce: true` walks closed shadow roots and nested documents;
  `DOM.getBoxModel` turns the node into coordinates. Neither needs an enable.

`verify` prints `mouse events sent`, which is not decoration: a challenge that
clears on its own and one that was clicked produce identical output otherwise,
and confusing the two sends you debugging the wrong layer. Eight events is one
click cycle.

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
