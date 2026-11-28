//go:build linux || darwin

package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// nodeAPI serves a manager's node API over HTTP, admitting callers by
// check, and returns the client the host would use.
func nodeAPI(t *testing.T, check PeerCheck) (*Manager, *SessionClient) {
	t.Helper()
	m := openManager(t, baseConfig(newRoot(t), nil))
	local := &LocalSessions{Manager: m, WorkDir: t.TempDir(), Env: func() []string { return []string{"PATH=/usr/bin:/bin"} }}
	srv := httptest.NewServer(SessionHandler(local, check, nil))
	t.Cleanup(srv.Close)
	return m, &SessionClient{HTTP: srv.Client(), BaseURL: srv.URL}
}

func admitAll(context.Context, string) error { return nil }

func lines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestNodeAPISessionLifecycle(t *testing.T) {
	m, c := nodeAPI(t, admitAll)
	ctx := ctxT(t)

	// Start, then read what exists while it runs, then follow to the end.
	id := newID(t)
	got, err := c.StartSession(ctx, core.NodeStartRequest{ID: id, Mode: core.SessionHeadless, Argv: []string{fakeAgent, "stream:8:100ms", "exit:4"}})
	if err != nil || got.ID != id || got.State != core.SessionRunning {
		t.Fatalf("start: %+v %v", got, err)
	}
	var now bytes.Buffer
	if err := c.SessionLogs(ctx, id, 0, false, &now); err != nil {
		t.Fatalf("logs while running: %v", err)
	}
	if !strings.HasPrefix(lines(8), now.String()) {
		t.Errorf("logs while running %q is not a prefix of the output", now.String())
	}
	var all bytes.Buffer
	if err := c.SessionLogs(ctx, id, 0, true, &all); err != nil || all.String() != lines(8) {
		t.Fatalf("follow: %q %v", all.String(), err)
	}
	info := waitState(t, m, id, core.SessionExited)
	if info.ExitCode == nil || *info.ExitCode != 4 {
		t.Errorf("ended %+v", info)
	}
	// Ended: read from the files, from any offset up to the end.
	var tail bytes.Buffer
	if err := c.SessionLogs(ctx, id, 7, true, &tail); err != nil || tail.String() != lines(8)[7:] {
		t.Errorf("logs from 7 after the end: %q %v", tail.String(), err)
	}
	err = c.SessionLogs(ctx, id, int64(len(lines(8))+1), false, &tail)
	wantCode(t, err, core.CodeInvalidArgument)

	// Stop: stopped with SIGTERM's code; again: unchanged.
	id2 := newID(t)
	if _, err := c.StartSession(ctx, core.NodeStartRequest{ID: id2, Mode: core.SessionHeadless, Argv: []string{fakeAgent, "sleep:1h"}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err = c.StopSession(ctx, id2)
		if err != nil || got.State != core.SessionStopped || got.ExitCode == nil || *got.ExitCode != 143 || got.EndedAt == nil {
			t.Errorf("stop: %+v %v", got, err)
		}
	}

	// Refusals: the same id again (no record of its own), a missing
	// program (recorded failed), unknown sessions.
	_, err = c.StartSession(ctx, core.NodeStartRequest{ID: id2, Mode: core.SessionHeadless, Argv: []string{fakeAgent}})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeConflict || ce.Details[core.DetailSessionState] != "" {
		t.Errorf("duplicate id: %v", err)
	}
	id3 := newID(t)
	_, err = c.StartSession(ctx, core.NodeStartRequest{ID: id3, Mode: core.SessionHeadless, Argv: []string{filepath.Join(t.TempDir(), "missing")}})
	if !errors.As(err, &ce) || ce.Details[core.DetailSessionState] != string(core.SessionFailed) {
		t.Errorf("missing program: %v", err)
	}
	if info, err := m.Get(id3); err != nil || info.State != core.SessionFailed {
		t.Errorf("missing program left %+v %v", info, err)
	}
	unknown := newID(t)
	_, err = c.StopSession(ctx, unknown)
	wantCode(t, err, core.CodeNotFound)
	wantCode(t, c.SessionLogs(ctx, unknown, 0, false, &tail), core.CodeNotFound)
	_, err = c.StartSession(ctx, core.NodeStartRequest{ID: newID(t), Mode: core.SessionHeadless})
	wantCode(t, err, core.CodeInvalidArgument)
}

// A running agent that has gone quiet: logs without follow return what
// exists at once instead of waiting for more output.
func TestNodeAPILogsIdleRunningAgent(t *testing.T) {
	m, c := nodeAPI(t, admitAll)
	id := newID(t)
	if _, err := c.StartSession(ctxT(t), core.NodeStartRequest{ID: id, Mode: core.SessionHeadless, Argv: []string{fakeAgent, "print:ready", "sleep:1h"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Stop(context.Background(), id) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	for out.String() != "ready\n" {
		out.Reset()
		if err := c.SessionLogs(ctx, id, 0, false, &out); err != nil {
			t.Fatalf("logs of an idle running agent: %q %v", out.String(), err)
		}
		if !strings.HasPrefix("ready\n", out.String()) {
			t.Fatalf("logs %q", out.String())
		}
	}
	if info, err := m.Get(id); err != nil || info.State != core.SessionRunning {
		t.Errorf("session %+v %v, want running", info, err)
	}
}

// An offset rotated out of a bounded scrollback is refused with the first
// offset still kept.
func TestNodeAPILogsBeforeStart(t *testing.T) {
	cfg := baseConfig(newRoot(t), nil)
	cfg.MaxScrollback = 100
	m := openManager(t, cfg)
	local := &LocalSessions{Manager: m, WorkDir: t.TempDir(), Env: func() []string { return []string{"PATH=/usr/bin:/bin"} }}
	srv := httptest.NewServer(SessionHandler(local, admitAll, nil))
	t.Cleanup(srv.Close)
	c := &SessionClient{HTTP: srv.Client(), BaseURL: srv.URL}
	ctx := ctxT(t)

	id := newID(t)
	if _, err := c.StartSession(ctx, core.NodeStartRequest{ID: id, Mode: core.SessionHeadless, Argv: []string{fakeAgent, "print:" + strings.Repeat("x", 299), "exit:0"}}); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, id, core.SessionExited)
	err := c.SessionLogs(ctx, id, 0, false, &bytes.Buffer{})
	wantCode(t, err, core.CodeInvalidArgument)
	var ce *core.Error
	errors.As(err, &ce)
	start, perr := strconv.ParseInt(ce.Details["start"], 10, 64)
	if perr != nil || start <= 0 || start >= 300 {
		t.Fatalf("detail start %q, want the first kept offset in (0, 300)", ce.Details["start"])
	}
	var tail bytes.Buffer
	if err := c.SessionLogs(ctx, id, start, false, &tail); err != nil || int64(tail.Len()) != 300-start {
		t.Errorf("logs from start %d: %d bytes, %v", start, tail.Len(), err)
	}
}

func TestNodeAPIRefusesOtherPeers(t *testing.T) {
	m, c := nodeAPI(t, func(context.Context, string) error { return errors.New("not the host") })
	ctx := ctxT(t)
	_, err := c.StartSession(ctx, core.NodeStartRequest{ID: newID(t), Mode: core.SessionHeadless, Argv: []string{fakeAgent}})
	wantCode(t, err, core.CodeForbidden)
	if l := m.List(); len(l) != 0 {
		t.Errorf("a refused caller started %+v", l)
	}
	_, err = c.StopSession(ctx, newID(t))
	wantCode(t, err, core.CodeForbidden)
	wantCode(t, c.SessionLogs(ctx, newID(t), 0, false, &bytes.Buffer{}), core.CodeForbidden)
}

func TestNodeAPIUnknownPath(t *testing.T) {
	_, c := nodeAPI(t, admitAll)
	res, err := c.HTTP.Get(c.BaseURL + core.NodesPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("%s: %d", core.NodesPath, res.StatusCode)
	}
}

// A node that cannot be reached at all: the start was not delivered.
func TestSessionClientNotDelivered(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	c := &SessionClient{HTTP: &http.Client{}, BaseURL: "http://" + addr}
	_, err = c.StartSession(ctxT(t), core.NodeStartRequest{ID: newID(t), Mode: core.SessionHeadless, Argv: []string{"a"}})
	if !errors.Is(err, core.ErrNotDelivered) {
		t.Errorf("start to a closed port: %v, want ErrNotDelivered", err)
	}
}

// A connection that is made, then dropped before any answer: the outcome
// is unknown, so the error is not ErrNotDelivered.
func TestSessionClientOutcomeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	c := &SessionClient{HTTP: srv.Client(), BaseURL: srv.URL}
	_, err := c.StartSession(ctxT(t), core.NodeStartRequest{ID: newID(t), Mode: core.SessionHeadless, Argv: []string{"a"}})
	if err == nil || errors.Is(err, core.ErrNotDelivered) {
		t.Errorf("dropped connection: %v, want an error other than ErrNotDelivered", err)
	}
}
