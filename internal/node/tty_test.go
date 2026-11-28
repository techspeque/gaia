//go:build linux || darwin

package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/tty"
)

// collector reads a terminal's output in the background.
type collector struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	err  error
	done chan struct{}
}

func collect(term tty.Terminal) *collector {
	c := &collector{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		b := make([]byte, 4096)
		for {
			n, err := term.Read(b)
			c.mu.Lock()
			c.buf.Write(b[:n])
			if err != nil {
				c.err = err
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *collector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// waitFor waits until the output contains s.
func (c *collector) waitFor(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(c.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("output %q does not contain %q", c.String(), s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// end waits for the output to end and returns its error.
func (c *collector) end(t *testing.T) error {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the output did not end")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func TestAttachInteractiveThroughNodeAPI(t *testing.T) {
	m, c := nodeAPI(t, admitAll)
	ctx := ctxT(t)
	id := newID(t)
	argv := []string{fakeAgent, "print:ready", "echo", "echo", "wait-winch", "exit:5"}
	if _, err := c.StartSession(ctx, core.NodeStartRequest{ID: id, Mode: core.SessionInteractive, Argv: argv}); err != nil {
		t.Fatal(err)
	}
	a, err := c.AttachSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Mode() != core.SessionInteractive || a.Offset() != 0 {
		t.Errorf("mode %s offset %d", a.Mode(), a.Offset())
	}
	outA := collect(a)
	outA.waitFor(t, "ready")

	// A second client: it replays what was written, and both take input.
	b, err := c.AttachSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	outB := collect(b)
	outB.waitFor(t, "ready")
	if err := a.Input(ctx, []byte("from-a\r")); err != nil {
		t.Fatal(err)
	}
	outA.waitFor(t, "got:from-a")
	outB.waitFor(t, "got:from-a")
	if err := b.Input(ctx, []byte("from-b\r")); err != nil {
		t.Fatal(err)
	}
	outA.waitFor(t, "got:from-b")
	outB.waitFor(t, "got:from-b")

	outA.waitFor(t, "waiting for winch")
	if err := b.Resize(ctx, 31, 101); err != nil {
		t.Fatal(err)
	}
	outA.waitFor(t, "size=31x101")
	if err := outA.end(t); !errors.Is(err, io.EOF) {
		t.Errorf("end of output: %v, want io.EOF", err)
	}
	if err := outB.end(t); !errors.Is(err, io.EOF) {
		t.Errorf("end of the second output: %v, want io.EOF", err)
	}
	info := waitState(t, m, id, core.SessionExited)
	if info.ExitCode == nil || *info.ExitCode != 5 {
		t.Errorf("ended %+v", info)
	}

	// Ended: the replay, then the end at once.
	e, err := c.AttachSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	outE := collect(e)
	if err := outE.end(t); !errors.Is(err, io.EOF) || !strings.Contains(outE.String(), "got:from-b") {
		t.Errorf("attach after the end: %q %v", outE.String(), err)
	}
}

func TestAttachReplaysTail(t *testing.T) {
	m, c := nodeAPI(t, admitAll)
	ctx := ctxT(t)
	id := newID(t)
	// About 290 KB of output, more than ReplayBytes.
	if _, err := c.StartSession(ctx, core.NodeStartRequest{ID: id, Mode: core.SessionHeadless, Argv: []string{fakeAgent, "stream:30000:0s", "exit:0"}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, id, core.SessionExited)
	all := lines(30000)
	a, err := c.AttachSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if want := int64(len(all) - ReplayBytes); a.Offset() != want {
		t.Errorf("offset %d, want %d", a.Offset(), want)
	}
	got, err := io.ReadAll(a)
	if err != nil || string(got) != all[len(all)-ReplayBytes:] {
		t.Errorf("replay: %d bytes, %v; want the last %d", len(got), err, ReplayBytes)
	}

	// Running: what it wrote so far, then live.
	id2 := newID(t)
	if _, err := c.StartSession(ctx, core.NodeStartRequest{ID: id2, Mode: core.SessionHeadless, Argv: []string{fakeAgent, "print:before", "sleep:500ms", "print:after"}}); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, id2, "before\n")
	b, err := c.AttachSession(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, err = io.ReadAll(b)
	if err != nil || string(got) != "before\nafter\n" {
		t.Errorf("running replay: %q %v", got, err)
	}
}

// waitOutput waits until the session's scrollback reads want.
func waitOutput(t *testing.T, m *Manager, id core.SessionID, want string) {
	t.Helper()
	l := &LocalSessions{Manager: m}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var b bytes.Buffer
		_ = l.SessionLogs(context.Background(), id, 0, false, &b)
		if b.String() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("scrollback %q, want %q", b.String(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAttachHeadlessTakesNoInput(t *testing.T) {
	m := openManager(t, baseConfig(newRoot(t), nil))
	l := &LocalSessions{Manager: m, WorkDir: t.TempDir()}
	ctx := ctxT(t)
	id := newID(t)
	if _, err := l.StartSession(ctx, core.NodeStartRequest{ID: id, Mode: core.SessionHeadless, Argv: []string{fakeAgent, "print:hi", "sleep:1h"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Stop(context.Background(), id) })
	term, err := l.AttachSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if term.Mode() != core.SessionHeadless {
		t.Errorf("mode %s", term.Mode())
	}
	collect(term).waitFor(t, "hi")
	wantCode(t, term.Input(ctx, []byte("x")), core.CodeInvalidArgument)
	wantCode(t, term.Resize(ctx, 10, 10), core.CodeInvalidArgument)
	_, err = l.AttachSession(ctx, newID(t))
	wantCode(t, err, core.CodeNotFound)
}

func TestNodeAPITTYRefusals(t *testing.T) {
	_, c := nodeAPI(t, admitAll)
	ctx := ctxT(t)
	id := newID(t)
	if _, err := c.StartSession(ctx, core.NodeStartRequest{ID: id, Mode: core.SessionInteractive, Argv: []string{fakeAgent, "sleep:5s"}}); err != nil {
		t.Fatal(err)
	}
	conn, res, err := websocket.Dial(ctx, c.BaseURL+core.NodeSessionTTYPath(id), &websocket.DialOptions{
		HTTPClient: c.HTTP,
		HTTPHeader: http.Header{"Origin": {"https://evil.example"}},
	})
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("a foreign origin was admitted")
	}
	if ce := tty.ResponseError(res); res.StatusCode != http.StatusForbidden || ce == nil || ce.Code != core.CodeForbidden {
		t.Errorf("foreign origin: %d %v", res.StatusCode, ce)
	}
	_, err = c.AttachSession(ctx, newID(t))
	wantCode(t, err, core.CodeNotFound)

	_, other := nodeAPI(t, func(context.Context, string) error { return errors.New("not the host") })
	_, err = other.AttachSession(ctx, id)
	wantCode(t, err, core.CodeForbidden)
}

// The forwarder of a node's local socket passes a terminal upgrade through
// to the host, with the link ready and while it is not.
func TestForwarderPassesUpgrade(t *testing.T) {
	_, c := nodeAPI(t, admitAll)
	ctx := ctxT(t)
	id := newID(t)
	if _, err := c.StartSession(ctx, core.NodeStartRequest{ID: id, Mode: core.SessionInteractive, Argv: []string{fakeAgent, "print:hello", "echo"}}); err != nil {
		t.Fatal(err)
	}
	// The node API stands in for the host here: the forwarder does not
	// care what is behind it.
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	notReady := &Link{}
	notReady.Set(PhaseRegistering)
	for _, link := range []*Link{nil, notReady} {
		fwd := httptest.NewServer(Forwarder(base, http.DefaultTransport, "test-host", link, nil))
		term, err := tty.Dial(ctx, http.DefaultClient, fwd.URL+core.NodeSessionTTYPath(id))
		if err != nil {
			fwd.Close()
			t.Fatalf("link %v: %v", link, err)
		}
		out := collect(term)
		out.waitFor(t, "hello")
		_ = term.Close()
		fwd.Close()
	}
}
