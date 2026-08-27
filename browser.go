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
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// realUA has no "HeadlessChrome" token; Chrome puts one there in headless mode
// and cf_clearance is scored against the UA that earned it.
const realUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) " +
	"Chrome/152.0.0.0 Safari/537.36"

type Options struct {
	UserDataDir string // persistent profile; keeps cf_clearance between runs
	Headless    bool
	ExecPath    string // defaults to google-chrome
	UserAgent   string // defaults to realUA
}

type Browser struct {
	cmd     *exec.Cmd
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	session string

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

func Launch(opts Options) (*Browser, error) {
	if opts.ExecPath == "" {
		opts.ExecPath = "google-chrome"
	}
	if opts.UserAgent == "" {
		opts.UserAgent = realUA
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
		"--user-agent=" + opts.UserAgent,
		// Drops navigator.webdriver without the --enable-automation banner,
		// which is itself a tell.
		"--disable-blink-features=AutomationControlled",
		"--no-first-run",
		"--no-default-browser-check",
		"--no-service-autorun",
		"--password-store=basic",
		"--homepage=about:blank",
		"about:blank",
	}
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
		title, lastErr = b.EvalString("document.title")
		if lastErr != nil {
			continue
		}
		if title != "" && !strings.Contains(title, "Just a moment") {
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
