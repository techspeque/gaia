package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// fakeNode is a node's sessions as a test drives them.
type fakeNode struct {
	start    func(core.NodeStartRequest) (core.NodeSession, error)
	stop     func(core.SessionID) (core.NodeSession, error)
	logs     func(off int64, follow bool, w io.Writer) error
	started  []core.NodeStartRequest
	stopped  []core.SessionID
	routeErr error
}

func (f *fakeNode) StartSession(_ context.Context, r core.NodeStartRequest) (core.NodeSession, error) {
	f.started = append(f.started, r)
	return f.start(r)
}

func (f *fakeNode) StopSession(_ context.Context, id core.SessionID) (core.NodeSession, error) {
	f.stopped = append(f.stopped, id)
	return f.stop(id)
}

func (f *fakeNode) SessionLogs(_ context.Context, _ core.SessionID, off int64, follow bool, w io.Writer) error {
	return f.logs(off, follow, w)
}

// fakeRouter routes every node to node.
type fakeRouter struct{ node *fakeNode }

func (r fakeRouter) Route(core.Node) (NodeSessions, error) {
	if r.node.routeErr != nil {
		return nil, r.node.routeErr
	}
	return r.node, nil
}

// running answers a start with the session running.
func running(r core.NodeStartRequest) (core.NodeSession, error) {
	return core.NodeSession{ID: r.ID, State: core.SessionRunning, StartedAt: time.Now()}, nil
}

// sessionServer is a server with node-a registered over the tailnet
// (addrA) and node-b (addrB), both online, routed to a fake node.
func sessionServer(t *testing.T) (*Server, http.Handler, *fakeNode, *clock) {
	t.Helper()
	s, h, c := registryHandler(t)
	for _, n := range []struct{ name, addr string }{{"node-a", addrA}, {"node-b", addrB}} {
		if code, body := send(t, h, n.addr, http.MethodPost, core.NodesPath, registerReq(n.name)); code != http.StatusOK {
			t.Fatalf("register %s: %d %s", n.name, code, body)
		}
	}
	f := &fakeNode{start: running}
	s.opts.Nodes = fakeRouter{f}
	return s, h, f, c
}

func startReq(node string, argv ...string) core.StartSessionRequest {
	return core.StartSessionRequest{Node: node, Mode: core.SessionHeadless, Argv: argv}
}

func reasonOf(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Details[core.DetailReason]
	}
	return ""
}

func listAll(t *testing.T, s *Server) []core.Session {
	t.Helper()
	l, err := s.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestStartSession(t *testing.T) {
	s, h, f, _ := sessionServer(t)
	code, body := send(t, s.Handler(), "", http.MethodPost, core.SessionsPath, startReq("node-a", "claude", "-p", "hi\nthere"))
	if code != http.StatusOK {
		t.Fatalf("start: %d %s", code, body)
	}
	if len(f.started) != 1 || strings.Join(f.started[0].Argv, "|") != "claude|-p|hi\nthere" || f.started[0].Mode != core.SessionHeadless {
		t.Fatalf("node got %+v", f.started)
	}
	l := listAll(t, s)
	if len(l) != 1 || l[0].ID != f.started[0].ID || l[0].State != core.SessionRunning || l[0].NodeName != "node-a" {
		t.Errorf("sessions %+v", l)
	}
	// Listed on the tailnet too.
	code, body = send(t, h, addrB, http.MethodGet, core.SessionsPath, nil)
	if code != http.StatusOK || !strings.Contains(string(body), l[0].ID.String()) {
		t.Errorf("tailnet list: %d %s", code, body)
	}
	got, err := s.GetSession(context.Background(), l[0].ID)
	if err != nil || got.ID != l[0].ID {
		t.Errorf("get: %+v %v", got, err)
	}
}

func TestStartSessionRefusedBeforeRecording(t *testing.T) {
	s, _, f, c := sessionServer(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		req    core.StartSessionRequest
		code   core.ErrorCode
		reason string
	}{
		"empty argv":   {startReq("node-a"), core.CodeInvalidArgument, ""},
		"empty prog":   {startReq("node-a", ""), core.CodeInvalidArgument, ""},
		"repo":         {core.StartSessionRequest{Node: "node-a", Mode: core.SessionHeadless, Argv: []string{"a"}, Repo: "https://x/y.git"}, core.CodeInvalidArgument, ""},
		"unknown node": {startReq("nosuch", "a"), core.CodeNotFound, "node_unavailable"},
	} {
		_, err := s.StartSession(ctx, c.req)
		if !errors.Is(err, core.NewError(c.code, "")) || reasonOf(err) != c.reason {
			t.Errorf("%s: %v (reason %q)", name, err, reasonOf(err))
		}
	}
	// Unreachable by the router.
	f.routeErr = errors.New("no route")
	if _, err := s.StartSession(ctx, startReq("node-a", "a")); !errors.Is(err, core.NewError(core.CodeUnavailable, "")) || reasonOf(err) != "node_unavailable" {
		t.Errorf("unroutable: %v", err)
	}
	f.routeErr = nil
	// Offline: no heartbeat for longer than the allowance.
	c.add(time.Minute)
	_, err := s.StartSession(ctx, startReq("node-a", "a"))
	if !errors.Is(err, core.NewError(core.CodeUnavailable, "")) || reasonOf(err) != "node_unavailable" || !strings.Contains(err.Error(), "offline") {
		t.Errorf("offline: %v", err)
	}
	if len(f.started) != 0 {
		t.Errorf("the node was asked to start %+v", f.started)
	}
	if l := listAll(t, s); len(l) != 0 {
		t.Errorf("refused starts recorded %+v", l)
	}
	// No router at all.
	s.opts.Nodes = nil
	c.add(-time.Minute)
	if _, err := s.StartSession(ctx, startReq("node-a", "a")); reasonOf(err) != "node_unavailable" {
		t.Errorf("no router: %v", err)
	}
}

func TestStartSessionNodeFailures(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		err    error
		state  core.SessionState
		reason string
	}{
		{"not delivered", fmt.Errorf("%w: dial: refused", core.ErrNotDelivered), core.SessionFailed, "node_unavailable"},
		{"refused with record", &core.Error{Code: core.CodeInvalidArgument, Message: "no such program",
			Details: map[string]string{core.DetailSessionState: "failed"}}, core.SessionFailed, "start_failed"},
		{"refused without record", core.NewError(core.CodeUnavailable, "no room"), core.SessionFailed, "start_failed"},
		{"internal refusal", core.NewError(core.CodeInternal, "disk full"), core.SessionFailed, "start_failed"},
		{"outcome unknown", errors.New("connection reset"), core.SessionLost, "session_lost"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _, f, _ := sessionServer(t)
			f.start = func(core.NodeStartRequest) (core.NodeSession, error) { return core.NodeSession{}, c.err }
			_, err := s.StartSession(ctx, startReq("node-a", "a"))
			if reasonOf(err) != c.reason {
				t.Errorf("error %v, reason %q, want %q", err, reasonOf(err), c.reason)
			}
			var ce *core.Error
			if !errors.As(err, &ce) || ce.Code == core.CodeInternal {
				t.Errorf("error %v: want a typed, non-internal error the caller sees", err)
			}
			l := listAll(t, s)
			if len(l) != 1 || l[0].State != c.state {
				t.Fatalf("sessions %+v, want one %s", l, c.state)
			}
			if c.state.Terminal() && l[0].EndedAt == nil {
				t.Error("a failed session without an end time")
			}
		})
	}
}

// A node that reports the start before answering it: the answer changes
// nothing.
func TestStartSessionReportFirst(t *testing.T) {
	s, _, f, _ := sessionServer(t)
	f.start = func(r core.NodeStartRequest) (core.NodeSession, error) {
		if err := s.ReportSession(context.Background(), core.SessionReport{SessionID: r.ID, Seq: 1, State: core.SessionRunning}); err != nil {
			t.Errorf("report: %v", err)
		}
		return running(r)
	}
	sess, err := s.StartSession(context.Background(), startReq("node-a", "a"))
	if err != nil || sess.State != core.SessionRunning {
		t.Errorf("%+v %v", sess, err)
	}
}

func TestReportSession(t *testing.T) {
	s, h, _, _ := sessionServer(t)
	ctx := context.Background()
	sess, err := s.StartSession(ctx, startReq("node-a", "a"))
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID
	end := time.Date(2026, 11, 27, 12, 5, 0, 0, time.UTC)
	code := 7
	exited := core.SessionReport{SessionID: id, Seq: 2, State: core.SessionExited, ExitCode: &code, EndedAt: &end}

	// From node-b, which is not the session's node: refused.
	st, body := send(t, h, addrB, http.MethodPost, core.SessionReportsPath, core.ReportSessionRequest{Report: exited})
	if st != http.StatusForbidden {
		t.Errorf("report from the wrong node: %d %s", st, body)
	}
	// From an allowlisted peer that is not registered: retryable.
	_, h2, _ := registryHandler(t)
	st, body = send(t, h2, addrA, http.MethodPost, core.SessionReportsPath, core.ReportSessionRequest{Report: exited})
	if st != http.StatusServiceUnavailable {
		t.Errorf("report from an unregistered peer: %d %s", st, body)
	}
	// The socket has no reports endpoint: only nodes on the tailnet report.
	if st, _ := send(t, s.Handler(), "", http.MethodPost, core.SessionReportsPath, core.ReportSessionRequest{Report: exited}); st != http.StatusNotFound {
		t.Errorf("report on the socket: %d", st)
	}
	// From node-a: applied; sent again: done, unchanged.
	for range 2 {
		st, body = send(t, h, addrA, http.MethodPost, core.SessionReportsPath, core.ReportSessionRequest{Report: exited})
		if st != http.StatusOK {
			t.Fatalf("report: %d %s", st, body)
		}
	}
	got, _ := s.GetSession(ctx, id)
	if got.State != core.SessionExited || got.ExitCode == nil || *got.ExitCode != 7 || got.EndedAt == nil || !got.EndedAt.Equal(end) {
		t.Errorf("after report %+v", got)
	}
	// A move the table forbids: conflict, which the node drops.
	st, body = send(t, h, addrA, http.MethodPost, core.SessionReportsPath,
		core.ReportSessionRequest{Report: core.SessionReport{SessionID: id, Seq: 3, State: core.SessionRunning}})
	if st != http.StatusConflict {
		t.Errorf("illegal move: %d %s", st, body)
	}
	// Invalid and unknown reports.
	if err := s.ReportSession(ctx, core.SessionReport{SessionID: id, Seq: 0, State: core.SessionRunning}); !errors.Is(err, core.NewError(core.CodeInvalidArgument, "")) {
		t.Errorf("seq 0: %v", err)
	}
	if err := s.ReportSession(ctx, core.SessionReport{SessionID: core.NewSessionID([16]byte{5}), Seq: 1, State: core.SessionRunning}); !errors.Is(err, core.NewError(core.CodeNotFound, "")) {
		t.Errorf("unknown session: %v", err)
	}
}

func TestStopSession(t *testing.T) {
	s, _, f, _ := sessionServer(t)
	ctx := context.Background()
	sess, err := s.StartSession(ctx, startReq("node-a", "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	code, end := 143, time.Date(2026, 11, 27, 12, 1, 0, 0, time.UTC)
	f.stop = func(id core.SessionID) (core.NodeSession, error) {
		return core.NodeSession{ID: id, State: core.SessionStopped, ExitCode: &code, EndedAt: &end}, nil
	}
	st, body := send(t, s.Handler(), "", http.MethodPost, core.SessionStopPath(sess.ID), nil)
	if st != http.StatusOK || !strings.Contains(string(body), `"state":"stopped"`) || !strings.Contains(string(body), `"exit_code":143`) {
		t.Fatalf("stop: %d %s", st, body)
	}
	// Already ended: answered from the store, the node not asked again.
	if got, err := s.StopSession(ctx, sess.ID); err != nil || got.State != core.SessionStopped || len(f.stopped) != 1 {
		t.Errorf("second stop: %+v %v, node asked %d times", got, err, len(f.stopped))
	}
	// Node unreachable during a stop.
	sess2, err := s.StartSession(ctx, startReq("node-a", "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	f.stop = func(core.SessionID) (core.NodeSession, error) {
		return core.NodeSession{}, errors.New("connection refused")
	}
	if _, err := s.StopSession(ctx, sess2.ID); reasonOf(err) != "node_unavailable" {
		t.Errorf("stop with the node unreachable: %v", err)
	}
	if _, err := s.StopSession(ctx, core.NewSessionID([16]byte{4})); !errors.Is(err, core.NewError(core.CodeNotFound, "")) {
		t.Errorf("stop of an unknown session: %v", err)
	}
}

func TestSessionLogs(t *testing.T) {
	s, _, f, _ := sessionServer(t)
	sess, err := s.StartSession(context.Background(), startReq("node-a", "a"))
	if err != nil {
		t.Fatal(err)
	}
	var gotOff int64
	var gotFollow bool
	f.logs = func(off int64, follow bool, w io.Writer) error {
		gotOff, gotFollow = off, follow
		_, err := io.WriteString(w, "hello\x00\xffworld")
		return err
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, core.SessionLogsPath(sess.ID)+"?"+core.LogsQuery(3, true), nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "hello\x00\xffworld" || !rec.Flushed {
		t.Errorf("logs: %d %q flushed %v", rec.Code, rec.Body.String(), rec.Flushed)
	}
	if rec.Header().Get("Content-Type") != "application/octet-stream" || gotOff != 3 || !gotFollow {
		t.Errorf("type %q offset %d follow %v", rec.Header().Get("Content-Type"), gotOff, gotFollow)
	}
	// An error before the first byte is the JSON error.
	f.logs = func(int64, bool, io.Writer) error {
		return &core.Error{Code: core.CodeInvalidArgument, Message: "rotated out", Details: map[string]string{"start": "10"}}
	}
	res, body := do(t, s.Handler(), http.MethodGet, core.SessionLogsPath(sess.ID))
	if res.StatusCode != http.StatusBadRequest || errorOf(t, body).Details["start"] != "10" {
		t.Errorf("logs refusal: %d %s", res.StatusCode, body)
	}
	f.logs = func(int64, bool, io.Writer) error { return errors.New("connection refused") }
	res, body = do(t, s.Handler(), http.MethodGet, core.SessionLogsPath(sess.ID))
	if res.StatusCode != http.StatusServiceUnavailable || errorOf(t, body).Details[core.DetailReason] != "node_unavailable" {
		t.Errorf("logs, node unreachable: %d %s", res.StatusCode, body)
	}
	// Bad queries and methods.
	for path, want := range map[string]int{
		core.SessionLogsPath(sess.ID) + "?offset=-1":         http.StatusBadRequest,
		core.SessionLogsPath(sess.ID) + "?follow=yes":        http.StatusBadRequest,
		core.SessionsPath + "/nope/logs":                     http.StatusBadRequest,
		core.SessionLogsPath(core.NewSessionID([16]byte{8})): http.StatusNotFound,
	} {
		if res, body := do(t, s.Handler(), http.MethodGet, path); res.StatusCode != want {
			t.Errorf("%s: %d %s, want %d", path, res.StatusCode, body, want)
		}
	}
	if res, _ := do(t, s.Handler(), http.MethodPost, core.SessionLogsPath(sess.ID)); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST logs: %d", res.StatusCode)
	}
}
