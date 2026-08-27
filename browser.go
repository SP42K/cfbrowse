// Package cfbrowse drives Chrome over CDP without ever sending Runtime.enable.
//
// Runtime.enable is the command anti-bot vendors probe for: enabling it changes
// observable behaviour inside the page, and no amount of fingerprint patching
// hides it. Everything here evaluates JavaScript through an isolated world
// created with Page.createIsolatedWorld instead, which needs no Runtime domain.
package cfbrowse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// defaultChrome locates the browser. "google-chrome" is a Linux PATH name: on
// Windows the executable is chrome.exe and the installer does not put it on
// PATH, on macOS it lives inside the .app bundle. Falls back to the bare name
// so a miss surfaces as exec's own "executable file not found".
func defaultChrome() string {
	switch runtime.GOOS {
	case "windows":
		if p, err := exec.LookPath("chrome.exe"); err == nil {
			return p
		}
		for _, dir := range []string{
			os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("LocalAppData"),
		} {
			if dir == "" {
				continue
			}
			p := filepath.Join(dir, "Google", "Chrome", "Application", "chrome.exe")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return "chrome.exe"
	case "darwin":
		const p = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "google-chrome"
}

type Options struct {
	UserDataDir string // persistent profile; keeps cf_clearance between runs
	Headless    bool
	ExecPath    string // defaults to the platform's Chrome, see defaultChrome
	UserAgent   string // defaults to the browser's own UA, see resolveUA
}

type Browser struct {
	cmd       *exec.Cmd
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	session   string
	userAgent string
	headless  bool // reported in Solve's timeout, which is the one place it matters

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan result
	sent    []string // every CDP method sent; the test asserts on it
}

type result struct {
	res json.RawMessage
	err error
}

type message struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

// Launch starts Chrome and attaches to its page target.
//
// With no Options.UserAgent it starts Chrome twice: once to ask what it is,
// then for real with a matching --user-agent. See resolveUA for why the extra
// second is not optional.
func Launch(opts Options) (*Browser, error) {
	if opts.UserAgent == "" {
		ua, err := resolveUA(opts)
		if err != nil {
			return nil, err
		}
		opts.UserAgent = ua
	}
	return launch(opts)
}

// resolveUA starts a throwaway Chrome to read its own UA, and returns it in the
// shape a real Chrome sends.
//
// Everything about this is a concession to measurement, against a site that
// clears in sixteen mouse events when the UA is right:
//
//   - The version cannot be hardcoded. Chrome reports its real major version in
//     sec-ch-ua whatever the UA says, and a UA claiming 152 beside a hint saying
//     151 fails the challenge outright. Same for the platform token against
//     sec-ch-ua-platform.
//   - The version cannot be asked for cheaply either. `chrome.exe --version` on
//     Windows prints "Opening in existing browser session." and exits 0.
//   - And the UA cannot be applied after launch. Emulation.setUserAgentOverride
//     with the exact string that passes as a --user-agent flag still fails: the
//     flag suppresses the client hints Chrome would otherwise contradict it
//     with, and the CDP override does not.
//
// Which leaves asking the binary, over CDP, before the run that matters.
func resolveUA(opts Options) (string, error) {
	probe := opts
	probe.Headless = true // never flash a window just to read a string
	probe.UserAgent = "-" // sentinel: launch skips the flag, Chrome answers as itself
	b, err := launch(probe)
	if err != nil {
		return "", err
	}
	var v struct {
		UserAgent string `json:"userAgent"`
	}
	err = b.send("", "Browser.getVersion", map[string]any{}, &v)
	// Close before the real launch: same profile, and Chrome holds a singleton
	// lock on it.
	b.Close()
	if err != nil {
		return "", err
	}
	return frozenUA(v.UserAgent), nil
}

func launch(opts Options) (*Browser, error) {
	if opts.ExecPath == "" {
		opts.ExecPath = defaultChrome()
	}
	if opts.UserDataDir == "" {
		d, err := os.MkdirTemp("", "cfbrowse-")
		if err != nil {
			return nil, err
		}
		opts.UserDataDir = d
	}
	if err := os.MkdirAll(opts.UserDataDir, 0o755); err != nil {
		return nil, err
	}
	// Chrome wants an absolute --user-data-dir. Given a relative one it does not
	// complain: it silently falls back to the default profile, so no
	// DevToolsActivePort ever appears where we poll for it, and if a normal
	// Chrome is already running that fallback also hands the launch off to the
	// existing instance and exits without a word on stderr.
	abs, err := filepath.Abs(opts.UserDataDir)
	if err != nil {
		return nil, err
	}
	opts.UserDataDir = abs
	// Stale port file would make us connect to the previous run.
	portFile := filepath.Join(opts.UserDataDir, "DevToolsActivePort")
	os.Remove(portFile)
	// A Chrome that was killed leaves SingletonLock behind and the next launch
	// aborts outright ("Failed to create a ProcessSingleton"). We own this
	// profile exclusively, so a leftover lock is always stale.
	for _, n := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		os.Remove(filepath.Join(opts.UserDataDir, n))
	}

	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + opts.UserDataDir,
	}
	if opts.UserAgent != "-" {
		args = append(args, "--user-agent="+opts.UserAgent)
	}
	args = append(args,
		// Drops navigator.webdriver without the --enable-automation banner,
		// which is itself a tell.
		"--disable-blink-features=AutomationControlled",
		"--no-first-run",
		"--no-default-browser-check",
		"--no-service-autorun",
		"--password-store=basic",
		"--homepage=about:blank",
		"about:blank",
	)
	if opts.Headless {
		args = append([]string{"--headless=new"}, args...)
	}

	cmd := exec.Command(opts.ExecPath, args...)
	// Chrome's own diagnostics are the only clue when it refuses to start.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch chrome: %w", err)
	}

	wsURL, err := waitForWS(portFile, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("%w\nchrome said: %s", err, strings.TrimSpace(stderr.String()))
	}

	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		cmd.Process.Kill()
		return nil, fmt.Errorf("dial devtools: %w", err)
	}
	conn.SetReadLimit(64 << 20) // pages can be large; the 32KB default truncates

	b := &Browser{cmd: cmd, conn: conn, ctx: ctx, cancel: cancel,
		headless: opts.Headless, userAgent: opts.UserAgent,
		pending: map[int64]chan result{}}
	go b.readLoop()

	if err := b.attachToPage(); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

// waitForWS polls the DevToolsActivePort file Chrome writes once its debug
// socket is listening. Line 1 is the port, line 2 the browser endpoint path.
func waitForWS(portFile string, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		f, err := os.Open(portFile)
		if err == nil {
			sc := bufio.NewScanner(f)
			var lines []string
			for sc.Scan() {
				lines = append(lines, strings.TrimSpace(sc.Text()))
			}
			f.Close()
			if len(lines) >= 2 && lines[0] != "" {
				return fmt.Sprintf("ws://127.0.0.1:%s%s", lines[0], lines[1]), nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", errors.New("timed out waiting for DevToolsActivePort")
}

func (b *Browser) readLoop() {
	for {
		_, data, err := b.conn.Read(b.ctx)
		if err != nil {
			b.mu.Lock()
			for id, ch := range b.pending {
				ch <- result{err: err}
				delete(b.pending, id)
			}
			b.mu.Unlock()
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil || m.ID == 0 {
			continue // an event; we subscribe to none
		}
		b.mu.Lock()
		ch, ok := b.pending[m.ID]
		delete(b.pending, m.ID)
		b.mu.Unlock()
		if !ok {
			continue
		}
		if m.Error != nil {
			ch <- result{err: errors.New(m.Error.Message)}
		} else {
			ch <- result{res: m.Result}
		}
	}
}

func (b *Browser) send(sessionID, method string, params any, out any) error {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan result, 1)
	b.pending[id] = ch
	b.sent = append(b.sent, method)
	b.mu.Unlock()

	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if params == nil {
		raw = []byte("{}")
	}
	payload, err := json.Marshal(message{ID: id, Method: method, Params: raw, SessionID: sessionID})
	if err != nil {
		return err
	}
	if err := b.conn.Write(b.ctx, websocket.MessageText, payload); err != nil {
		return err
	}

	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("%s: %w", method, r.err)
		}
		if out != nil {
			return json.Unmarshal(r.res, out)
		}
		return nil
	case <-time.After(60 * time.Second):
		return fmt.Errorf("%s: timeout", method)
	}
}

func (b *Browser) attachToPage() error {
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := b.send("", "Target.getTargets", map[string]any{}, &targets); err != nil {
		return err
	}
	var pageID string
	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			pageID = t.TargetID
			break
		}
	}
	if pageID == "" {
		return errors.New("no page target")
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	// flatten routes this target's traffic over the one socket via sessionId.
	if err := b.send("", "Target.attachToTarget",
		map[string]any{"targetId": pageID, "flatten": true}, &att); err != nil {
		return err
	}
	b.session = att.SessionID
	// Page is safe to enable; Runtime is the one that leaks. Never enable it.
	return b.send(b.session, "Page.enable", map[string]any{}, nil)
}

// reChromeToken matches the product token, headless or not, with however many
// version components Chrome felt like reporting.
var reChromeToken = regexp.MustCompile(`(?:Headless)?Chrome/(\d+)(?:\.\d+)*`)

// frozenUA rewrites the product token to Chrome's frozen form. It is separate
// from setUserAgent so it can be tested without a browser.
func frozenUA(ua string) string {
	return reChromeToken.ReplaceAllString(ua, "Chrome/$1.0.0.0")
}

// UserAgent reports the UA the page actually sends. Callers that store a
// cf_clearance have to store this alongside it; the cookie is scored against it.
func (b *Browser) UserAgent() string { return b.userAgent }

func (b *Browser) Navigate(url string) error {
	return b.send(b.session, "Page.navigate", map[string]any{"url": url}, nil)
}

func (b *Browser) frameID() (string, error) {
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := b.send(b.session, "Page.getFrameTree", map[string]any{}, &tree); err != nil {
		return "", err
	}
	return tree.FrameTree.Frame.ID, nil
}

// Eval runs js and returns the result as JSON.
//
// The isolated world is recreated per call: it is cheap, and it is invalidated
// by every navigation, so caching the context id only buys stale-handle bugs.
// Isolated worlds see the DOM but not the page's own JS globals — fine for
// scraping, not for reaching into page state.
func (b *Browser) Eval(js string) (json.RawMessage, error) {
	frame, err := b.frameID()
	if err != nil {
		return nil, err
	}
	var world struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	if err := b.send(b.session, "Page.createIsolatedWorld", map[string]any{
		"frameId": frame, "worldName": "cfbrowse", "grantUniveralAccess": true,
	}, &world); err != nil {
		return nil, err
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := b.send(b.session, "Runtime.evaluate", map[string]any{
		"expression":    js,
		"contextId":     world.ExecutionContextID,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &res); err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, errors.New(res.ExceptionDetails.Text)
	}
	return res.Result.Value, nil
}

func (b *Browser) EvalString(js string) (string, error) {
	raw, err := b.Eval(js)
	if err != nil {
		return "", err
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return string(raw), nil
	}
	return s, nil
}

// WaitReady blocks until document.readyState is complete and the Cloudflare
// interstitial title is gone. The interstitial keeps readyState complete while
// it works, so the title check is the part that actually matters.
func (b *Browser) WaitReady(limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	var title, state string
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		// readyState is reported, never gated on: the Cloudflare interstitial
		// holds its connection open, so the page sits at "interactive"
		// indefinitely and a complete-first check waits forever. The title is
		// the signal that actually flips.
		state, _ = b.EvalString("document.readyState")
		var done bool
		done, lastErr = b.cleared()
		if lastErr != nil {
			continue
		}
		if done {
			title, _ = b.EvalString("document.title")
			return title, nil
		}
	}
	// Swallowing the eval error here once cost an hour of chasing the wrong
	// layer; a timeout must always say which of the three states it ended in.
	if lastErr != nil {
		return title, fmt.Errorf("gave up after %s: last eval failed: %w", limit, lastErr)
	}
	return title, fmt.Errorf("still on challenge after %s (readyState=%q title=%q)",
		limit, state, title)
}

// challengeBox is the on-screen rectangle of the Turnstile widget, in CSS
// pixels relative to the viewport.
type challengeBox struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// probeJS reports what the page is actually made of when no widget matched.
// document.querySelectorAll does not pierce shadow roots, so a bare iframe
// count can read "none" while the challenge sits one boundary away; this walks
// open shadow roots too and says which boundaries were closed to it.
const probeJS = `(() => {
  const seen = [], shadows = [];
  const walk = (root, depth) => {
    if (depth > 4) return;
    root.querySelectorAll('*').forEach(el => {
      if (el.tagName === 'IFRAME') {
        const r = el.getBoundingClientRect();
        seen.push('iframe ' + (el.getAttribute('src') || '(no src)').slice(0, 60) +
                  ' [' + Math.round(r.width) + 'x' + Math.round(r.height) + ']');
      }
      if (el.shadowRoot) {
        shadows.push('open:' + el.tagName.toLowerCase() +
                     (el.id ? '#' + el.id : ''));
        walk(el.shadowRoot, depth + 1);
      }
    });
  };
  walk(document, 0);
  const bodyKids = [...(document.body ? document.body.children : [])]
    .slice(0, 8)
    .map(e => e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') +
              (e.className && typeof e.className === 'string'
                 ? '.' + e.className.trim().split(/\\s+/).join('.') : ''));
  return JSON.stringify({
    title: document.title,
    readyState: document.readyState,
    href: location.href.slice(0, 100),
    iframes: seen.length ? seen : 'none',
    openShadowRoots: shadows.length ? shadows : 'none',
    bodyChildren: bodyKids,
  });
})()`

// cleared reports whether the interstitial is gone.
//
// This gates on the title, which was doubted once and should not be again. A
// marker-based check (window._cf_chl_opt, #challenge-running) matched nothing
// on a real interstitial and so declared a still-challenged page ready: it
// returned success with no cf_clearance and no content. A false pass is worse
// than a slow one.
func (b *Browser) cleared() (bool, error) {
	title, err := b.EvalString("document.title")
	if err != nil {
		return false, err
	}
	return title != "" && !strings.Contains(title, "Just a moment"), nil
}

// challengeWidget locates the clickable challenge element through CDP.
//
// JavaScript cannot find it: Cloudflare renders the widget inside a closed
// shadow root, so a page displaying a checkbox on screen reports zero iframes
// and zero open shadow roots to querySelectorAll. DOM.getDocument with
// pierce:true walks closed shadow roots and nested documents, and like
// everything else here it needs no domain enable.
func (b *Browser) challengeWidget() (*challengeBox, error) {
	var doc struct {
		Root domNode `json:"root"`
	}
	if err := b.send(b.session, "DOM.getDocument",
		map[string]any{"depth": -1, "pierce": true}, &doc); err != nil {
		return nil, err
	}
	id := findWidget(&doc.Root)
	if id == 0 {
		return nil, nil
	}
	var bm struct {
		Model struct {
			Content []float64 `json:"content"` // x1,y1 … x4,y4, clockwise
		} `json:"model"`
	}
	if err := b.send(b.session, "DOM.getBoxModel", map[string]any{"nodeId": id}, &bm); err != nil {
		// The node can vanish between the two calls; treat it as not-yet-there.
		return nil, nil
	}
	q := bm.Model.Content
	if len(q) < 8 {
		return nil, nil
	}
	return &challengeBox{X: q[0], Y: q[1], W: q[2] - q[0], H: q[5] - q[1]}, nil
}

type domNode struct {
	NodeID      int       `json:"nodeId"`
	NodeName    string    `json:"nodeName"`
	Attributes  []string  `json:"attributes"` // flat name,value,name,value…
	Children    []domNode `json:"children"`
	ShadowRoots []domNode `json:"shadowRoots"`
	ContentDoc  *domNode  `json:"contentDocument"`
}

func (n *domNode) attr(name string) string {
	for i := 0; i+1 < len(n.Attributes); i += 2 {
		if n.Attributes[i] == name {
			return n.Attributes[i+1]
		}
	}
	return ""
}

// findWidget returns the nodeId of the challenge's clickable element. The
// challenge iframe is preferred; a bare checkbox is the fallback for
// interstitials that render without one.
func findWidget(n *domNode) int {
	if n == nil {
		return 0
	}
	switch n.NodeName {
	case "IFRAME":
		src := n.attr("src")
		if strings.Contains(src, "challenges.cloudflare.com") ||
			strings.Contains(src, "/cdn-cgi/challenge-platform/") ||
			strings.Contains(src, "turnstile") {
			return n.NodeID
		}
	case "INPUT":
		if n.attr("type") == "checkbox" {
			return n.NodeID
		}
	}
	for i := range n.Children {
		if id := findWidget(&n.Children[i]); id != 0 {
			return id
		}
	}
	for i := range n.ShadowRoots {
		if id := findWidget(&n.ShadowRoots[i]); id != 0 {
			return id
		}
	}
	return findWidget(n.ContentDoc)
}

// clickWidget dispatches real mouse input at the widget's checkbox.
//
// Input.dispatchMouseEvent is injected below Blink's event plumbing, so the
// page sees isTrusted events that cross into the challenge iframe like a
// human's would — element.click() cannot reach across the origin boundary at
// all, and synthetic DOM events are marked untrusted anyway. The Input domain
// needs no enable, so this costs nothing against the package invariant.
func (b *Browser) clickWidget(box *challengeBox) error {
	// ponytail: the checkbox's position is assumed, not measured — nothing
	// outside the iframe can see it. Cloudflare puts it at the left edge,
	// vertically centred. If a redesign moves it, this offset is what breaks.
	inset := 30.0
	if w := box.W * 0.15; w < inset {
		inset = w
	}
	tx, ty := box.X+inset, box.Y+box.H/2

	// Arriving instantly at the target is itself a behavioural tell, so walk
	// the cursor in from an offset before pressing.
	const steps = 6
	sx, sy := tx-120, ty-90
	for i := 1; i <= steps; i++ {
		f := float64(i) / steps
		if err := b.dispatchMouse("mouseMoved", sx+(tx-sx)*f, sy+(ty-sy)*f, 0); err != nil {
			return err
		}
		time.Sleep(60 * time.Millisecond)
	}
	if err := b.dispatchMouse("mousePressed", tx, ty, 1); err != nil {
		return err
	}
	time.Sleep(80 * time.Millisecond)
	return b.dispatchMouse("mouseReleased", tx, ty, 1)
}

func (b *Browser) dispatchMouse(kind string, x, y float64, clicks int) error {
	p := map[string]any{"type": kind, "x": x, "y": y, "button": "left", "clickCount": clicks}
	if kind == "mousePressed" {
		p["buttons"] = 1
	}
	return b.send(b.session, "Input.dispatchMouseEvent", p, nil)
}

// Solve gets past an interactive challenge, clicking the Turnstile widget with
// real mouse input if one appears, and returns the page title once through.
//
// A page that was never challenged returns as soon as its title settles, so
// Solve is safe to use in place of WaitReady.
func (b *Browser) Solve(limit time.Duration) (string, error) {
	return b.SolveProgress(limit, nil)
}

// Solve's phases, as reported to SolveProgress's callback.
const (
	// StepWaiting: the page is not readable yet, or the widget has not rendered.
	// It renders a beat after the interstitial, so this is the normal opening.
	StepWaiting = "waiting"
	// StepWidget: the widget was located, but this pass is not clicking it —
	// either the attempt budget is spent or the cooldown has not elapsed.
	StepWidget = "widget"
	// StepClicking: a click cycle is being dispatched. attempt is 1-based.
	StepClicking = "clicking"
	// StepCleared: the challenge is gone. Solve returns right after this.
	StepCleared = "cleared"
)

// SolveProgress is Solve with a progress callback, for callers that put this
// wait in front of a person. It can take a minute and a half, and "still
// waiting for the widget to render" and "clicking it for the third time" are
// different enough that one spinner for all of it reads as a hang.
//
// onStep may be nil. It fires only when the phase actually changes — the loop
// polls at 1 Hz and would otherwise report waiting ninety times — and it runs
// on Solve's own goroutine, so keep it cheap and non-blocking: the poll cadence
// is itself part of what the site scores.
func (b *Browser) SolveProgress(limit time.Duration, onStep func(phase string, attempt int)) (string, error) {
	const maxClicks = 3
	deadline := time.Now().Add(limit)
	var lastClick time.Time
	var lastErr error
	var title string
	clicks := 0
	sawWidget := false

	lastPhase, lastAttempt := "", -1
	step := func(phase string, attempt int) {
		if onStep == nil || (phase == lastPhase && attempt == lastAttempt) {
			return
		}
		lastPhase, lastAttempt = phase, attempt
		onStep(phase, attempt)
	}

	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		var done bool
		done, lastErr = b.cleared()
		if lastErr != nil {
			continue
		}
		if done {
			step(StepCleared, clicks)
			title, _ = b.EvalString("document.title")
			return title, nil
		}
		box, err := b.challengeWidget()
		if err != nil {
			lastErr = err
		}
		if err != nil || box == nil {
			step(StepWaiting, 0) // widget renders a beat after the interstitial
			continue
		}
		sawWidget = true
		// Turnstile sometimes needs a second go, but hammering it is itself a
		// signal; leave it time to run between attempts.
		if clicks >= maxClicks || time.Since(lastClick) < 8*time.Second {
			step(StepWidget, clicks)
			continue
		}
		// Reported before the click, not after: the cycle walks the cursor in
		// over six moves and takes the better part of a second, and that second
		// is exactly the one the caller wants named.
		step(StepClicking, clicks+1)
		if err := b.clickWidget(box); err != nil {
			return "", fmt.Errorf("clicking challenge widget: %w", err)
		}
		clicks++
		lastClick = time.Now()
	}

	switch {
	// Checked before the widget cases on purpose: an eval that never succeeded
	// means the loop never saw a title or a widget, so blaming the widget would
	// point at the wrong layer. WaitReady learned this the expensive way.
	case lastErr != nil && !sawWidget:
		return "", fmt.Errorf("gave up after %s: page never became readable "+
			"(last title=%q); last eval error: %w", limit, title, lastErr)
	case !sawWidget:
		probe, _ := b.EvalString(probeJS)
		return "", fmt.Errorf("no challenge widget appeared within %s; page probe: %s",
			limit, probe)
	case b.headless:
		return "", fmt.Errorf("clicked the widget %d time(s), still challenged after %s "+
			"— headless auto-solve failed; rerun without headless mode", clicks, limit)
	default:
		return "", fmt.Errorf("clicked the widget %d time(s), still challenged after %s",
			clicks, limit)
	}
}

type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

// Cookies uses Storage.getCookies, a browser-level command, so the Network
// domain never has to be enabled.
func (b *Browser) Cookies() ([]Cookie, error) {
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	err := b.send("", "Storage.getCookies", map[string]any{}, &out)
	return out.Cookies, err
}

func (b *Browser) HasClearance() bool {
	cs, err := b.Cookies()
	if err != nil {
		return false
	}
	for _, c := range cs {
		if c.Name == "cf_clearance" {
			return true
		}
	}
	return false
}

// SentMethods returns every CDP method this browser has sent, in order.
func (b *Browser) SentMethods() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

func (b *Browser) Close() error {
	// Browser.close lets Chrome flush cookies to the profile; killing loses them.
	b.send("", "Browser.close", map[string]any{}, nil)
	b.cancel()
	b.conn.CloseNow()
	if b.cmd.Process != nil {
		done := make(chan struct{})
		go func() { b.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			b.cmd.Process.Kill()
		}
	}
	return nil
}
