//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/techspeque/gaia/internal/core"
)

// fakeTerm is the caller's terminal in a test.
type fakeTerm struct {
	mu         sync.Mutex
	raw        bool
	raws       int
	restores   int
	rows, cols uint16
	resized    chan struct{}
}

func newFakeTerm() *fakeTerm { return &fakeTerm{rows: 24, cols: 80, resized: make(chan struct{}, 1)} }

func (f *fakeTerm) MakeRaw() (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw = true
	f.raws++
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.raw = false
		f.restores++
		return nil
	}, nil
}

func (f *fakeTerm) Size() (uint16, uint16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, f.cols, nil
}

func (f *fakeTerm) Resized() (<-chan struct{}, func()) { return f.resized, func() {} }

func (f *fakeTerm) resize(rows, cols uint16) {
	f.mu.Lock()
	f.rows, f.cols = rows, cols
	f.mu.Unlock()
	f.resized <- struct{}{}
}

// checkRestored fails unless the terminal went raw `raws` times and was
// restored each time.
func (f *fakeTerm) checkRestored(t *testing.T, raws int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.raw || f.raws != raws || f.restores != raws {
		t.Errorf("terminal: raw=%v, made raw %d times, restored %d; want %d and restored", f.raw, f.raws, f.restores, raws)
	}
}

// ttyPeer is the daemon's end of a terminal WebSocket in a test: it
// records what the CLI sends.
type ttyPeer struct {
	mu     sync.Mutex
	input  bytes.Buffer
	sizes  [][2]uint16
	update chan struct{}
}

func newTTYPeer() *ttyPeer { return &ttyPeer{update: make(chan struct{}, 100)} }

func (p *ttyPeer) state() (string, [][2]uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.String(), append([][2]uint16(nil), p.sizes...)
}

func (p *ttyPeer) waitFor(t *testing.T, what string, ok func(string, [][2]uint16) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if ok(p.state()) {
			return
		}
		select {
		case <-p.update:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			in, sizes := p.state()
			t.Fatalf("%s: not reached; input %q, sizes %v", what, in, sizes)
		}
	}
}

// handler serves the terminal: hello with mode, then script, while
// recording everything the CLI sends.
func (p *ttyPeer) handler(mode core.SessionMode, script func(ctx context.Context, c *websocket.Conn)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		hello, _ := json.Marshal(core.TTYMessage{Type: core.TTYHello, Mode: mode})
		if c.Write(ctx, websocket.MessageText, hello) != nil {
			return
		}
		go func() {
			for {
				typ, b, err := c.Read(ctx)
				if err != nil {
					return
				}
				p.mu.Lock()
				if typ == websocket.MessageBinary {
					p.input.Write(b)
				} else {
					var m core.TTYMessage
					if json.Unmarshal(b, &m) == nil && m.Type == core.TTYResize {
						p.sizes = append(p.sizes, [2]uint16{m.Rows, m.Cols})
					}
				}
				p.mu.Unlock()
				p.update <- struct{}{}
			}
		}()
		script(ctx, c)
	}
}

var ttyID = core.NewSessionID([16]byte{5})

// attachDaemon serves the terminal of ttyID with h, and GET on the session
// with sess.
func attachDaemon(t *testing.T, h http.HandlerFunc, sess core.Session) *Client {
	t.Helper()
	d := newSessionDaemon()
	d.mux.HandleFunc("GET "+core.SessionTTYPath(ttyID), h)
	d.mux.HandleFunc("GET "+core.SessionPath(ttyID), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.GetSessionResponse{Session: sess})
	})
	return &Client{Dir: fakeDaemon(t, d)}
}

// runAttach runs `gaia attach ttyID` with stdin and term.
func runAttach(c *Client, stdin io.Reader, term Terminal) (int, string, string) {
	var out, errOut lockedBuf
	code := Attach([]string{ttyID.String()}, Env{Stdout: &out, Stderr: &errOut, Stdin: stdin, Term: term, Client: c})
	return code, out.String(), errOut.String()
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func exited(code int) core.Session {
	return core.Session{ID: ttyID, NodeID: testNode, State: core.SessionExited, Mode: core.SessionInteractive, ExitCode: &code}
}

func waitCtx(ctx context.Context) { <-ctx.Done() }

func TestAttachDetachKey(t *testing.T) {
	p := newTTYPeer()
	c := attachDaemon(t, p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageBinary, []byte("screen"))
		waitCtx(ctx)
	}), exited(0))
	term := newFakeTerm()
	inR, inW := io.Pipe()
	defer inW.Close()
	done := make(chan [3]any, 1)
	go func() {
		code, out, errOut := runAttach(c, inR, term)
		done <- [3]any{code, out, errOut}
	}()
	// The size at once, then on every resize.
	p.waitFor(t, "initial size", func(_ string, s [][2]uint16) bool { return len(s) == 1 && s[0] == [2]uint16{24, 80} })
	term.resize(50, 132)
	p.waitFor(t, "resize", func(_ string, s [][2]uint16) bool { return len(s) == 2 && s[1] == [2]uint16{50, 132} })
	_, _ = inW.Write([]byte("ab"))
	p.waitFor(t, "input", func(in string, _ [][2]uint16) bool { return in == "ab" })
	// The bytes before the detach key are sent; the key and what follows
	// are not.
	_, _ = inW.Write([]byte{'c', DetachKey, 'z', 'z'})
	var r [3]any
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("attach did not return after the detach key")
	}
	code, out, errOut := r[0].(int), r[1].(string), r[2].(string)
	if code != 0 || out != "screen" || !strings.Contains(errOut, "detached from session "+ttyID.String()) {
		t.Errorf("exit %d %q %q", code, out, errOut)
	}
	time.Sleep(100 * time.Millisecond)
	if in, _ := p.state(); in != "abc" {
		t.Errorf("daemon got input %q, want abc", in)
	}
	term.checkRestored(t, 1)
}

func TestAttachSessionEnds(t *testing.T) {
	p := newTTYPeer()
	c := attachDaemon(t, p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageBinary, []byte("bye\r\n"))
		_ = c.Close(websocket.StatusNormalClosure, "the session has ended")
	}), exited(7))
	term := newFakeTerm()
	inR, inW := io.Pipe()
	defer inW.Close()
	code, out, errOut := runAttach(c, inR, term)
	if code != 7 || out != "bye\r\n" || !strings.Contains(errOut, "exited (exit code 7)") {
		t.Errorf("exit %d %q %q", code, out, errOut)
	}
	term.checkRestored(t, 1)

	// Ended without an exit code: 1.
	failed := core.Session{ID: ttyID, NodeID: testNode, State: core.SessionFailed, Mode: core.SessionInteractive}
	c = attachDaemon(t, p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Close(websocket.StatusNormalClosure, "the session has ended")
	}), failed)
	if code, _, errOut := runAttach(c, nil, nil); code != 1 || !strings.Contains(errOut, "failed") {
		t.Errorf("failed session: exit %d %q", code, errOut)
	}
}

// A host slow to answer, past endWait, gets the fallback 0 once endWait
// expires, not its late answer.
func TestAttachSessionEndsSlowHost(t *testing.T) {
	p := newTTYPeer()
	d := newSessionDaemon()
	d.mux.HandleFunc("GET "+core.SessionTTYPath(ttyID), p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Close(websocket.StatusNormalClosure, "the session has ended")
	}))
	d.mux.HandleFunc("GET "+core.SessionPath(ttyID), func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(endWait + 2*time.Second):
		case <-r.Context().Done():
			return
		}
		writeJSON(w, http.StatusOK, core.GetSessionResponse{Session: exited(7)})
	})
	c := &Client{Dir: fakeDaemon(t, d)}
	start := time.Now()
	code, _, errOut := runAttach(c, nil, nil)
	elapsed := time.Since(start)
	if code != 0 || !strings.Contains(errOut, "has not recorded its exit yet") {
		t.Errorf("exit %d %q", code, errOut)
	}
	if elapsed > endWait+time.Second {
		t.Errorf("waited %v; want about %v", elapsed, endWait)
	}
}

func TestAttachConnectionDrops(t *testing.T) {
	p := newTTYPeer()
	c := attachDaemon(t, p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageBinary, []byte("part"))
		time.Sleep(50 * time.Millisecond)
		_ = c.CloseNow()
	}), exited(0))
	term := newFakeTerm()
	inR, inW := io.Pipe()
	defer inW.Close()
	code, out, errOut := runAttach(c, inR, term)
	if code != int(core.ExitSessionLost) || out != "part" || !strings.Contains(errOut, "may still be running") {
		t.Errorf("exit %d %q %q", code, out, errOut)
	}
	term.checkRestored(t, 1)

	// An error close from the daemon is a drop too.
	c = attachDaemon(t, p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Close(websocket.StatusInternalError, "cut")
	}), exited(0))
	if code, _, _ := runAttach(c, nil, term); code != int(core.ExitSessionLost) {
		t.Errorf("error close: exit %d", code)
	}
	term.checkRestored(t, 2)
}

func TestAttachSignalDetaches(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	old := notifyInterrupt
	notifyInterrupt = func() (<-chan os.Signal, func()) { return sigs, func() {} }
	t.Cleanup(func() { notifyInterrupt = old })

	p := newTTYPeer()
	c := attachDaemon(t, p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) { waitCtx(ctx) }), exited(0))
	term := newFakeTerm()
	inR, inW := io.Pipe()
	defer inW.Close()
	done := make(chan int, 1)
	go func() { code, _, _ := runAttach(c, inR, term); done <- code }()
	p.waitFor(t, "attached", func(_ string, s [][2]uint16) bool { return len(s) == 1 })
	sigs <- os.Interrupt
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("attach did not return on the signal")
	}
	term.checkRestored(t, 1)
}

func TestAttachHeadlessSendsNothing(t *testing.T) {
	p := newTTYPeer()
	c := attachDaemon(t, p.handler(core.SessionHeadless, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageBinary, []byte("log line\n"))
		time.Sleep(300 * time.Millisecond)
		_ = c.Close(websocket.StatusNormalClosure, "the session has ended")
	}), exited(0))
	term := newFakeTerm()
	code, out, errOut := runAttach(c, strings.NewReader("rm -rf /\n"), term)
	if code != 0 || out != "log line\n" || !strings.Contains(errOut, "input is not accepted") {
		t.Errorf("exit %d %q %q", code, out, errOut)
	}
	if in, sizes := p.state(); in != "" || len(sizes) != 0 {
		t.Errorf("a headless attach sent input %q, sizes %v", in, sizes)
	}
	term.checkRestored(t, 0)

	// The detach key still detaches.
	c = attachDaemon(t, p.handler(core.SessionHeadless, func(ctx context.Context, c *websocket.Conn) { waitCtx(ctx) }), exited(0))
	if code, _, errOut := runAttach(c, bytes.NewReader([]byte{'x', DetachKey}), nil); code != 0 || !strings.Contains(errOut, "detached") {
		t.Errorf("headless detach: exit %d %q", code, errOut)
	}
	if in, _ := p.state(); in != "" {
		t.Errorf("a headless attach sent input %q", in)
	}
}

func TestAttachNotice(t *testing.T) {
	p := newTTYPeer()
	c := attachDaemon(t, p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) {
		n, _ := json.Marshal(core.TTYMessage{Type: core.TTYNotice, Message: "input not delivered: gone"})
		_ = c.Write(ctx, websocket.MessageText, n)
		_ = c.Close(websocket.StatusNormalClosure, "")
	}), exited(0))
	if _, out, errOut := runAttach(c, nil, nil); out != "" || !strings.Contains(errOut, "gaia: input not delivered: gone") {
		t.Errorf("notice: %q %q", out, errOut)
	}
}

func TestAttachRefusals(t *testing.T) {
	d := newSessionDaemon()
	d.mux.HandleFunc("GET "+core.SessionTTYPath(ttyID), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, core.ErrorResponse{Error: core.NewError(core.CodeNotFound, "no session")})
	})
	other := core.NewSessionID([16]byte{6})
	d.mux.HandleFunc("GET "+core.SessionTTYPath(other), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, core.ErrorResponse{Error: core.NewError(core.CodeUnavailable, "node-b is offline").WithReason(core.ExitNodeUnavailable)})
	})
	c := &Client{Dir: fakeDaemon(t, d)}
	if code, _, errOut := runCmd(Attach, c, ttyID.String()); code != 1 || !strings.Contains(errOut, "no session") {
		t.Errorf("unknown session: exit %d %q", code, errOut)
	}
	if code, _, errOut := runCmd(Attach, c, other.String()); code != int(core.ExitNodeUnavailable) || !strings.Contains(errOut, "offline") {
		t.Errorf("node offline: exit %d %q", code, errOut)
	}
	for _, args := range [][]string{{}, {"nope"}, {ttyID.String(), "x"}} {
		if code, _, _ := runCmd(Attach, c, args...); code != int(core.ExitUsage) {
			t.Errorf("%q: exit %d, want usage", args, code)
		}
	}
	if code, _, errOut := runCmd(Attach, c, "--help"); code != 0 || !strings.Contains(errOut, attachUsage) {
		t.Errorf("--help: %d %q", code, errOut)
	}
	if c, _ := (&Client{Dir: shortDir(t)}).Attach(context.Background(), ttyID); c != nil {
		t.Error("attached with no daemon")
	}
}

func TestRunInteractive(t *testing.T) {
	p := newTTYPeer()
	d := newSessionDaemon()
	var got core.StartSessionRequest
	d.mux.HandleFunc("POST "+core.SessionsPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, core.StartSessionResponse{Session: core.Session{ID: ttyID, NodeID: testNode, State: core.SessionRunning}})
	})
	d.mux.HandleFunc("GET "+core.SessionTTYPath(ttyID), p.handler(core.SessionInteractive, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageBinary, []byte("$ "))
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	d.mux.HandleFunc("GET "+core.SessionPath(ttyID), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.GetSessionResponse{Session: exited(3)})
	})
	c := &Client{Dir: fakeDaemon(t, d)}
	var out, errOut lockedBuf
	term := newFakeTerm()
	code := Run([]string{"--node", "node-b", "--interactive", "--", "claude"}, Env{Stdout: &out, Stderr: &errOut, Term: term, Client: c})
	if code != 3 || out.String() != "$ " || got.Mode != core.SessionInteractive || got.Node != "node-b" {
		t.Errorf("exit %d %q %q; request %+v", code, out.String(), errOut.String(), got)
	}
	term.checkRestored(t, 1)

	for _, args := range [][]string{
		{"--node", "n", "--detach", "--interactive", "--", "claude"},
		{"--node", "n", "--interactive", "--"},
		{"--node", "n", "--interactive", "--slice", "s", "--", "claude"},
	} {
		if code, _, errOut := runCmd(Run, c, args...); code != int(core.ExitUsage) || !strings.Contains(errOut, "usage: gaia run") {
			t.Errorf("%q: exit %d %q", args, code, errOut)
		}
	}
}
