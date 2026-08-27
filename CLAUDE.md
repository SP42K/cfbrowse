# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go build ./...
go test ./...                    # needs Chrome (see defaultChrome); skips if absent
go test -run TestNeverEnablesRuntime -v .
go vet ./...

go run ./cmd/verify -headless -solve -profile ~/.cfbrowse-profiles/site 'https://…'
go run ./cmd/verify -headless -dump 'https://…'                    # tag/class shape of a page
go run ./cmd/verify -headless -eval 'document.title' 'https://…'
go run ./cmd/verify -solve 'https://…'                             # headed, to watch it work
```

No linter config, no CI, one dependency (`github.com/coder/websocket`). Two files carry the whole project: `browser.go` and `cmd/verify/main.go`.

The module path is `github.com/SP42K/cfbrowse` and the repo is private, so consumers need `GOPRIVATE=github.com/SP42K/*` or a `go mod edit -replace` pointing at a local checkout. `browser.go` is the library; `cmd/verify` is a demo and a test harness, and nothing in it should ever become a dependency of the package.

**`Solve` handles interactive challenges unattended, headless, from an empty profile.** Measured across two sites that both serve interactive challenges: headed and headless gave identical results (sixteen mouse events, `cf_clearance` obtained, full content). Do not reintroduce the claim that a human is needed for the first run — it was true of an earlier design and is not true now.

## The invariant

The package exists for exactly one property: **`Runtime.enable` is never sent.** Anti-bot vendors probe the control channel, and enabling the Runtime domain is observable from inside the page regardless of fingerprint patching — that is why chromedp/go-rod/stealth variants fail Cloudflare.

`Browser.send` records every CDP method in `b.sent`; `TestNeverEnablesRuntime` asserts `Runtime.enable` never appears after a full navigate + evaluate cycle. If a change makes that test fail, the package has no reason to exist. Treat it as the acceptance gate for anything touching CDP.

Consequences to respect when adding features:

- JS evaluation goes through `Page.createIsolatedWorld` → `Runtime.evaluate` with an explicit `contextId`. The world is recreated per `Eval` call on purpose (navigation invalidates it; caching only buys stale-handle bugs).
- Cookies use `Storage.getCookies` (browser-level) so the **Network** domain also stays disabled. Prefer browser-level or `Page.*` commands over anything requiring a domain enable.
- `Page.enable` is the only domain enabled. No CDP events are subscribed to — `readLoop` drops every message without an `id`. Waiting is therefore polling (`WaitReady` polls at 1 Hz), not event-driven.
- `Solve` reaches for `Input.dispatchMouseEvent`, `DOM.getDocument` and `DOM.getBoxModel`. None of the three requires enabling its domain, which is why they are allowed. The test now asserts an **allowlist** (`Page.enable` only) rather than a `Runtime.enable` denylist, so a future feature that reaches for `Network.enable` or `DOM.enable` trips the same gate.

## Solving challenges

`Solve` clicks the Turnstile checkbox. Two findings, both established by measurement and both easy to get wrong again:

- **The widget is invisible to JavaScript.** Cloudflare renders it in a *closed* shadow root: a page displaying a checkbox on screen reports zero iframes and zero open shadow roots to `querySelectorAll`. Four rounds were lost to selector-widening before this was measured. Locate it with `DOM.getDocument {pierce: true}` (walks closed shadow roots and nested documents) and `DOM.getBoxModel`; never with `document.querySelector`.
- **The click must be `Input.dispatchMouseEvent`.** It is injected below Blink's event plumbing so the page sees `isTrusted` events. `element.click()` cannot reach the node at all, and synthetic DOM events are untrusted regardless.

`SolveProgress` is the same loop with a change-only callback (`waiting` /
`widget` / `clicking` / `cleared`); `Solve` is now a nil-callback wrapper around
it. The dedup lives in the library, not the caller — the loop polls at 1 Hz and
every consumer would otherwise write the same "did the phase change" guard. The
callback runs on Solve's goroutine, so a slow one drags the poll cadence, which
is itself scored.

`cmd/verify` prints `mouse events sent` because a self-clearing challenge and a solved one are otherwise indistinguishable in the output — a success was misread as a failure, and later a failure as a success, before this counter existed. Eight events is one click cycle (six `mouseMoved`, then press and release).

Two mistakes worth not repeating, both already made in this file's history:

- **Do not gate readiness on challenge markers.** `window._cf_chl_opt` and `#challenge-running` matched nothing on a real interstitial, so the gate declared a still-challenged page ready and returned with no `cf_clearance` and no content. The title gate is the correct one.
- **Never swallow an eval error in a polling loop.** `WaitReady` was fixed for this, then `Solve` was written with the same bug, and its timeout blamed the widget for what was an evaluation failure. Report `lastErr` ahead of any other diagnosis.

## Architecture

`browser.go` is a hand-rolled CDP client: launch Chrome with `--remote-debugging-port=0`, poll the `DevToolsActivePort` file in the profile dir for the ws endpoint, dial it, attach to the single page target with `flatten: true` so all traffic rides one socket keyed by `sessionId`. `send()` is request/response over an id→channel map with a 60s timeout; `readLoop` fans replies back.

One page target, no tab management. `Browser` is not designed for concurrent use beyond the mutex around `pending`/`sent`.

Session/anti-detection details that are load-bearing, not incidental:

- **The UA is measured, never written down.** `Launch` starts Chrome twice: `resolveUA` reads the browser's own UA over CDP, `frozenUA` rewrites the product token to Chrome's frozen `MAJOR.0.0.0` shape, and the real launch passes it as `--user-agent`. Three things were established the expensive way against a live interactive challenge, and each looks like a safe simplification: a hardcoded version fails as soon as the installed Chrome differs by one major (`sec-ch-ua` reports the real one and the mismatch is the tell); `chrome.exe --version` on Windows prints "Opening in existing browser session." and exits 0, so the version cannot be read cheaply; and `Emulation.setUserAgentOverride` fails **with the exact string that passes as a flag**, because the flag suppresses the client hints that would contradict it and the CDP override does not. The extra second is the price of not guessing. `cf_clearance` is scored against the UA that earned it, so a Chrome update invalidates existing profiles — as it would for a real browser.
- `defaultChrome` follows `runtime.GOOS`: `google-chrome` is a Linux PATH name and finds nothing on Windows (where the installer never touches PATH) or macOS (where it lives in the .app bundle).
- `--disable-blink-features=AutomationControlled` drops `navigator.webdriver` without the `--enable-automation` banner, which is itself a tell.
- `Launch` removes stale `DevToolsActivePort` and `Singleton*` files; a killed Chrome otherwise blocks the next launch outright. It also resolves `UserDataDir` to an absolute path — Chrome ignores a relative one silently, falling back to the default profile, so the port file never appears where `waitForWS` polls.
- `Close` sends `Browser.close` rather than killing, so Chrome flushes cookies to the profile. Killing loses `cf_clearance`.
- `WaitReady` gates on the title losing "Just a moment", **not** on `readyState` — the Cloudflare interstitial holds the connection open and sits at `interactive` forever. `readyState` is reported in the timeout error only.

## Scope

`cmd/verify` is a reachability check, not a scraper: it prints the title and whether `cf_clearance` was obtained. **Site-specific selectors and extractors do not belong in this repo** — they rot on the target site's schedule, not this one's. Pass JavaScript with `-eval` from the calling project instead.

Known limits, already accepted: isolated worlds see the DOM but not page JS globals; no event subscriptions; on WSL `WEBGL_debug_renderer_info` returns nothing (no GPU), which a strict checker can score against.
