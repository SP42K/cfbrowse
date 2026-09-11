# cfbrowse

[![Go Reference](https://pkg.go.dev/badge/github.com/SP42K/cfbrowse.svg)](https://pkg.go.dev/github.com/SP42K/cfbrowse)
[![Go Report Card](https://goreportcard.com/badge/github.com/SP42K/cfbrowse)](https://goreportcard.com/report/github.com/SP42K/cfbrowse)
[![MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Minimal Go CDP driver that clears a Cloudflare interactive challenge headless,
from an empty profile, with no human.

## What actually decides it

Measured, not asserted. Target was a host on a zone I own, behind a WAF custom
rule set to `managed_challenge`, with my own origin logging request headers
behind it. Same mac, same Chrome 152 binary, same UA, a fresh profile every run,
arms interleaved. Each arm carried its own `?arm=` marker, so the origin's log
is an independent record of which arm got past the edge rather than an
inference from the driver's own report.

| arm | cleared | clicks | seconds | reached origin |
|---|---|---|---|---|
| `go-rod` + `go-rod/stealth` | 0/3 | 3 | 60s timeout | no |
| `go-rod`, defaults | 0/3 | 3 | 60s timeout | no |
| `go-rod`, minus `enable-automation` | 0/3 | 3 | 60s timeout | no |
| `go-rod`, launch flags identical to this | 0/3 | 3 | 60s timeout | no |
| **`go-rod` + `NoDefaultDevice()`** | **3/3** | 1 | 7–11s | yes |
| `go-rod`, device with a current UA | 3/3 | 2–3 | 16–26s | yes |
| this | 3/3 | 1 | 6–10s | yes |

The click layer was the same code in every `go-rod` arm, so the arms differ only
where the table says they do.

**The tell is `rod.New()`'s default device emulation, not the control channel.**
`go-rod` emulates `devices.LaptopWithMDPIScreen` on every new page, which sends
`Network.setUserAgentOverride`. Reading the headers that actually go out (over
`localhost`, which is a secure context, so Chrome sends the hints):

| | `User-Agent` | `Sec-CH-UA` | `Accept-Language` |
|---|---|---|---|
| `go-rod` defaults | `Chrome/114` | **absent** | `en` |
| device with `UserAgent` + `AcceptLanguage` set | `Chrome/152` | **absent** | `zh-TW` |
| `NoDefaultDevice()` | `Chrome/152` | `"Chromium";v="152", …` | the real list |

Three tells at once: the device's UA is hardcoded at `Chrome/114.0.0.0`, 38
releases stale, and it silently overrides the `--user-agent` launch flag; the
override carries no `userAgentMetadata`, so Chrome stops sending `Sec-CH-UA`
entirely — a browser claiming to be Chrome 114 with no client hints at all,
which no real Chrome since 89 has been; and `navigator.languages` collapses to
`["en"]`. Setting the device's `UserAgent` fixes the first and not the second,
which is the row that clears but takes two to three clicks.

One line fixes it for `go-rod` users:

```go
browser := rod.New().ControlURL(u).NoDefaultDevice()
```

Reported upstream on [go-rod/rod#1208][1].

### What this package is, given that

Not faster and not stronger: `go-rod` with that one line matches it. What this
has is that it never calls the override path in the first place — a UA here is
a launch flag, because `Emulation.setUserAgentOverride` was measured to fail
with the exact string that passes as `--user-agent`, and the flag also
suppresses the client hints that would otherwise contradict it.

The rest is properties, not advantages:

- **No `Runtime.enable`, ever.** `browser_test.go` asserts it as an allowlist —
  any `*.enable` other than `Page.enable` fails the test. `chromedp` does send
  it on every target attach ([chromedp.go:445][2]), to work out whether the
  target is a worker. `go-rod` does not, and deserves the credit: it never needs
  an `executionContextId`, so it takes an object id from a bare
  `Runtime.evaluate` and calls through `Runtime.callFunctionOn` instead. On the
  evidence above, this was not what any of the arms turned on.
- **JS evaluates in an isolated world**, via `Page.createIsolatedWorld` +
  `Runtime.evaluate` with an explicit `contextId`, so nothing injected is
  visible to the page's own script. The `NoDefaultDevice()` arm evaluates in the
  main world and cleared 3/3, so this is a property worth having, not the
  reason anything passed.
- **The widget is found through CDP, not JavaScript.** `DOM.getDocument` with
  `pierce: true` walks closed shadow roots. This is not exclusive either —
  it is plain CDP and works the same from `go-rod`.

[1]: https://github.com/go-rod/rod/issues/1208
[2]: https://github.com/chromedp/chromedp/blob/master/chromedp.go#L445

## Scope

This is one layer, and a narrow one: the CDP control channel. It does not touch
your TLS fingerprint, your IP, or your request rate, and it deliberately ships
no proxy rotation, no concurrency pool, no CAPTCHA-service client, and no
per-site selectors. Those are the parts that turn a driver into scraping
infrastructure, and they belong in whatever is calling this, if anywhere.

It exists because "which layer actually fails the challenge" was worth pinning
down. The answer turned out to be the UA override path, which is as useful to
whoever writes the check as to whoever fails it. Point it at origins you are
allowed to automate.

## Intended use

Point this at origins you own, or ones whose operator has authorised you to
automate — your own staging and production sites, a customer's, a pentest or
bug-bounty target inside its scope, an anti-bot vendor's own test pages.
Automating someone else's site against its terms of service is on you, and
several jurisdictions treat circumventing an access control as more than a
contract problem. The detector side is a first-class use too: a missing
`Sec-CH-UA` behind a stale UA string is as useful to whoever writes the check
as to whoever fails it.

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

```sh
go get github.com/SP42K/cfbrowse
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

`SolveProgress` is `Solve` with a callback, for callers that put this wait in
front of a person. It reports `waiting` / `widget` / `clicking` / `cleared` and
fires only on a change, so a UI can say "clicking it for the third time" instead
of spinning for ninety seconds:

```go
title, err := b.SolveProgress(90*time.Second, func(phase string, attempt int) {
    log.Printf("solve: %s (attempt %d)", phase, attempt)
})
```

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
