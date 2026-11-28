package host

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/store"
)

// NodeSessions is a node's session manager as the host drives it.
// *node.LocalSessions (in-process) and *node.SessionClient (over the
// tailnet) implement it.
//
// StartSession's error is core.ErrNotDelivered (wrapped) when the request
// certainly did not reach the node; a *core.Error the node answered with,
// carrying core.DetailSessionState when the node recorded the session
// before refusing it; or any other error when the outcome is unknown.
type NodeSessions interface {
	StartSession(ctx context.Context, r core.NodeStartRequest) (core.NodeSession, error)
	StopSession(ctx context.Context, id core.SessionID) (core.NodeSession, error)
	// SessionLogs writes the session's scrollback from byte offset off to
	// w: what exists, or with follow everything until the session ends.
	SessionLogs(ctx context.Context, id core.SessionID, off int64, follow bool, w io.Writer) error
}

// NodeRouter finds how to reach a node's sessions: in-process for the
// host's own node, over the tailnet for the others. An error means the
// node cannot be reached from this host.
type NodeRouter interface {
	Route(n core.Node) (NodeSessions, error)
}

// stopTimeout bounds a stop routed to a node: SIGTERM, the 10 s grace
// period, SIGKILL, and the round trip.
const stopTimeout = 30 * time.Second

// recordTimeout bounds recording the outcome of a start the node was
// asked for, which outlives the caller's cancellation.
const recordTimeout = 10 * time.Second

// sessionRoutes adds the session API to mux. Reports from nodes on the
// tailnet are on the tailnet mux only.
func (s *Server) sessionRoutes(mux *http.ServeMux, socket bool) {
	mux.HandleFunc(core.SessionsPath, s.methods(map[string]handler{
		http.MethodGet:  s.listSessionsHTTP,
		http.MethodPost: s.startSessionHTTP,
	}))
	mux.HandleFunc(core.SessionsPath+"/{id}", s.methods(map[string]handler{http.MethodGet: s.getSessionHTTP}))
	mux.HandleFunc(core.SessionsPath+"/{id}/stop", s.methods(map[string]handler{http.MethodPost: s.stopSessionHTTP}))
	mux.HandleFunc(core.SessionsPath+"/{id}/logs", s.logsHTTP)
	if !socket {
		mux.HandleFunc(core.SessionReportsPath, s.methods(map[string]handler{http.MethodPost: s.reportHTTP}))
	}
}

func (s *Server) listSessionsHTTP(r *http.Request) (any, error) {
	l, err := s.ListSessions(r.Context())
	if err != nil {
		return nil, err
	}
	return core.ListSessionsResponse{Sessions: l}, nil
}

func (s *Server) startSessionHTTP(r *http.Request) (any, error) {
	var req core.StartSessionRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	sess, err := s.StartSession(r.Context(), req)
	if err != nil {
		return nil, err
	}
	return core.StartSessionResponse{Session: sess}, nil
}

func (s *Server) getSessionHTTP(r *http.Request) (any, error) {
	id, err := core.ParseSessionID(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	sess, err := s.GetSession(r.Context(), id)
	if err != nil {
		return nil, err
	}
	return core.GetSessionResponse{Session: sess}, nil
}

func (s *Server) stopSessionHTTP(r *http.Request) (any, error) {
	id, err := core.ParseSessionID(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	sess, err := s.StopSession(r.Context(), id)
	if err != nil {
		return nil, err
	}
	return core.StopSessionResponse{Session: sess}, nil
}

// reportHTTP records a report from the calling peer, which must be a
// registered node and the session's node.
func (s *Server) reportHTTP(r *http.Request) (any, error) {
	ident, _, err := peerIdentity(r)
	if err != nil {
		return nil, err
	}
	var req core.ReportSessionRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	n, found, err := s.findByIdentity(r.Context(), ident)
	if err != nil {
		return nil, err
	}
	if !found {
		// Retryable: a node starting up reports before it registers.
		return nil, core.NewError(core.CodeUnavailable, "this node is not registered; register before reporting sessions")
	}
	sess, err := s.reportSession(r.Context(), &n.ID, req.Report)
	if err != nil {
		return nil, err
	}
	return core.ReportSessionResponse{Session: sess}, nil
}

// ListSessions returns every session, oldest first.
func (s *Server) ListSessions(ctx context.Context) ([]core.Session, error) {
	rows, err := s.st.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	names, err := s.nodeNames(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.Session, 0, len(rows))
	for _, row := range rows {
		out = append(out, apiSession(row, names))
	}
	return out, nil
}

// GetSession returns the session with id, or core.CodeNotFound.
func (s *Server) GetSession(ctx context.Context, id core.SessionID) (core.Session, error) {
	row, err := s.st.GetSession(ctx, id)
	if err != nil {
		return core.Session{}, err
	}
	names, err := s.nodeNames(ctx)
	if err != nil {
		return core.Session{}, err
	}
	return apiSession(row, names), nil
}

// StartSession records a new session and starts it on the node req names.
//
// An unknown node is core.CodeNotFound and an offline or unreachable one
// core.CodeUnavailable, both with reason node_unavailable and no session
// recorded. A node that refuses the session leaves it failed (reason
// start_failed). A request whose outcome is unknown leaves it lost, to be
// corrected by the node's next report (reason session_lost).
func (s *Server) StartSession(ctx context.Context, req core.StartSessionRequest) (core.Session, error) {
	if err := req.Validate(); err != nil {
		return core.Session{}, err
	}
	if req.Repo != "" || req.Branch != "" {
		return core.Session{}, core.NewError(core.CodeInvalidArgument,
			"repo and branch need node checkouts, which this version does not have yet; omit them")
	}
	n, ns, err := s.route(ctx, req.Node)
	if err != nil {
		return core.Session{}, err
	}
	id, err := newSessionID()
	if err != nil {
		return core.Session{}, err
	}
	row := store.Session{
		ID: id, NodeID: n.ID, Profile: req.Profile, Mode: req.Mode, Argv: req.Argv,
		State: core.SessionStarting, StartedAt: s.opts.Now().UTC(),
	}
	if err := s.st.CreateSession(ctx, row); err != nil {
		return core.Session{}, err
	}
	s.log.Info("session starting", "id", id.String(), "node", n.Name, "mode", string(req.Mode))
	ns2, err := ns.StartSession(ctx, core.NodeStartRequest{ID: id, Mode: req.Mode, Argv: req.Argv})
	if err == nil {
		if aerr := s.apply(ctx, id, nil, ns2); aerr != nil {
			s.log.Warn("recording a started session", "id", id.String(), "err", aerr)
		}
		return s.GetSession(ctx, id)
	}
	return core.Session{}, s.startFailed(ctx, id, n, err)
}

// startFailed records what a failed start on node n means for the session
// with id, and returns the error for the caller. The record is made even
// when ctx was canceled: the node was asked, so the row must not stay
// starting.
func (s *Server) startFailed(ctx context.Context, id core.SessionID, n core.Node, err error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	var ce *core.Error
	switch {
	case errors.Is(err, core.ErrNotDelivered):
		s.endFailed(ctx, id)
		s.log.Warn("session start not delivered to its node", "id", id.String(), "node", n.Name, "err", err)
		return (&core.Error{
			Code:    core.CodeUnavailable,
			Message: fmt.Sprintf("node %q cannot be reached: %v", n.Name, err),
			Details: map[string]string{"node": n.Name, "id": id.String()},
		}).WithReason(core.ExitNodeUnavailable)
	case errors.As(err, &ce):
		s.endFailed(ctx, id)
		s.log.Warn("node refused a session", "id", id.String(), "node", n.Name, "err", err)
		out := &core.Error{Code: ce.Code, Message: fmt.Sprintf("node %q could not start the session: %s", n.Name, ce.Message),
			Details: map[string]string{"node": n.Name, "id": id.String()}}
		if ce.Code == core.CodeInternal {
			// The node's internal detail stays in its log; the caller
			// still learns that the start failed.
			out.Code = core.CodeUnavailable
		}
		return out.WithReason(core.ExitStartFailed)
	default:
		if uerr := s.st.UpdateSessionState(ctx, id, core.SessionLost); uerr != nil {
			s.log.Warn("marking a session lost", "id", id.String(), "err", uerr)
		}
		s.log.Warn("session start outcome unknown", "id", id.String(), "node", n.Name, "err", err)
		return (&core.Error{
			Code: core.CodeUnavailable,
			Message: fmt.Sprintf("lost contact with node %q while starting session %s (%v); it may be running: `gaia ls` shows it once the node reports",
				n.Name, id, err),
			Details: map[string]string{"node": n.Name, "id": id.String()},
		}).WithReason(core.ExitSessionLost)
	}
}

// endFailed moves a session that cannot be running to failed, unless the
// node already reported an end.
func (s *Server) endFailed(ctx context.Context, id core.SessionID) {
	end := s.opts.Now().UTC()
	err := s.apply(ctx, id, nil, core.NodeSession{ID: id, State: core.SessionFailed, EndedAt: &end})
	if err != nil && !errors.Is(err, core.NewError(core.CodeConflict, "")) {
		s.log.Warn("marking a session failed", "id", id.String(), "err", err)
	}
}

// StopSession stops the session with id on its node and returns it as it
// ended. A session already ended is returned as it is, without asking the
// node. A node that cannot be reached is core.CodeUnavailable with reason
// node_unavailable.
func (s *Server) StopSession(ctx context.Context, id core.SessionID) (core.Session, error) {
	row, err := s.st.GetSession(ctx, id)
	if err != nil {
		return core.Session{}, err
	}
	if row.State.Terminal() {
		return s.GetSession(ctx, id)
	}
	_, ns, err := s.routeID(ctx, row.NodeID)
	if err != nil {
		return core.Session{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	got, err := ns.StopSession(ctx, id)
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) {
			return core.Session{}, ce
		}
		return core.Session{}, (&core.Error{
			Code:    core.CodeUnavailable,
			Message: fmt.Sprintf("the session's node cannot be reached: %v", err),
			Details: map[string]string{"id": id.String()},
		}).WithReason(core.ExitNodeUnavailable)
	}
	if err := s.apply(ctx, id, nil, got); err != nil && !errors.Is(err, core.NewError(core.CodeConflict, "")) {
		return core.Session{}, err
	}
	s.log.Info("session stopped", "id", id.String(), "state", string(got.State))
	return s.GetSession(ctx, id)
}

// logsHTTP streams a session's scrollback from its node: GET with the
// query of core.SessionLogsPath. An error before the first byte is the
// JSON error; after it the stream just ends.
func (s *Server) logsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, core.NewError(core.CodeInvalidArgument, "method "+r.Method+" not allowed"))
		return
	}
	fail := func(err error) {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Code != core.CodeInternal {
			writeError(w, statusFor(ce.Code), ce)
			return
		}
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, core.NewError(core.CodeInternal, "internal error"))
	}
	id, err := core.ParseSessionID(r.PathValue("id"))
	if err != nil {
		fail(err)
		return
	}
	q := r.URL.Query()
	off, follow, err := core.ParseLogsQuery(q.Get("offset"), q.Get("follow"))
	if err != nil {
		fail(err)
		return
	}
	row, err := s.st.GetSession(r.Context(), id)
	if err != nil {
		fail(err)
		return
	}
	_, ns, err := s.routeID(r.Context(), row.NodeID)
	if err != nil {
		fail(err)
		return
	}
	sw := &streamWriter{w: w, rc: http.NewResponseController(w)}
	err = ns.SessionLogs(r.Context(), id, off, follow, sw)
	switch {
	case err != nil && !sw.started:
		var ce *core.Error
		if !errors.As(err, &ce) {
			err = (&core.Error{
				Code:    core.CodeUnavailable,
				Message: fmt.Sprintf("the session's node cannot be reached: %v", err),
				Details: map[string]string{"id": id.String()},
			}).WithReason(core.ExitNodeUnavailable)
		}
		fail(err)
		return
	case err != nil && r.Context().Err() == nil:
		s.log.Warn("streaming a session's scrollback ended early", "id", id.String(), "err", err)
	}
	if !sw.started {
		_, _ = sw.Write(nil)
	}
}

// streamWriter writes a raw byte stream as the response: the headers on
// the first write, a flush after every write.
type streamWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if !s.started {
		s.started = true
		s.w.Header().Set("Content-Type", "application/octet-stream")
		s.w.Header().Set("Cache-Control", "no-store")
		s.w.Header().Set("X-Content-Type-Options", "nosniff")
		s.w.WriteHeader(http.StatusOK)
	}
	n, err := s.w.Write(p)
	if err != nil {
		return n, err
	}
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return n, err
	}
	return n, nil
}

// ReportSession records a report from the host's in-process node.
func (s *Server) ReportSession(ctx context.Context, r core.SessionReport) error {
	_, err := s.reportSession(ctx, nil, r)
	return err
}

// reportSession applies r to its session. A non-nil node must be the
// session's node, else core.CodeForbidden. A report of the state the
// session is in is accepted and changes nothing, so a resent report is
// done; a move the state table forbids is core.CodeConflict.
func (s *Server) reportSession(ctx context.Context, node *core.NodeID, r core.SessionReport) (core.Session, error) {
	if err := r.Validate(); err != nil {
		return core.Session{}, err
	}
	err := s.apply(ctx, r.SessionID, node, core.NodeSession{
		ID: r.SessionID, State: r.State, EndedAt: r.EndedAt, ExitCode: r.ExitCode,
	})
	if err != nil {
		return core.Session{}, err
	}
	return s.GetSession(ctx, r.SessionID)
}

// apply moves the session's row to the state ns reports. See
// reportSession.
func (s *Server) apply(ctx context.Context, id core.SessionID, node *core.NodeID, ns core.NodeSession) error {
	row, err := s.st.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if node != nil && row.NodeID != *node {
		s.log.Warn("session report refused: the caller is not the session's node", "id", id.String(), "caller", node.String())
		return core.NewError(core.CodeForbidden, "permission denied")
	}
	if row.State == ns.State {
		return nil
	}
	if !ns.State.Terminal() {
		err = s.st.UpdateSessionState(ctx, id, ns.State)
	} else {
		end := s.opts.Now().UTC()
		if ns.EndedAt != nil {
			end = *ns.EndedAt
		}
		code := ns.ExitCode
		if ns.State != core.SessionExited && ns.State != core.SessionStopped {
			code = nil
		}
		err = s.st.SetSessionExit(ctx, id, store.SessionExit{State: ns.State, ExitCode: code, EndedAt: end})
	}
	if err == nil {
		s.log.Info("session state", "id", id.String(), "from", string(row.State), "to", string(ns.State))
	}
	return err
}

// route finds the node called name and how to reach its sessions. An
// unknown node is core.CodeNotFound; an offline or unreachable one
// core.CodeUnavailable; both carry reason node_unavailable.
func (s *Server) route(ctx context.Context, name string) (core.Node, NodeSessions, error) {
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return core.Node{}, nil, err
	}
	for _, n := range nodes {
		if n.Name == name {
			return s.routeNode(n)
		}
	}
	return core.Node{}, nil, (&core.Error{
		Code:    core.CodeNotFound,
		Message: fmt.Sprintf("no node named %q is registered; `gaia nodes` lists them", name),
		Details: map[string]string{"node": name},
	}).WithReason(core.ExitNodeUnavailable)
}

// routeID is route for the node with id.
func (s *Server) routeID(ctx context.Context, id core.NodeID) (core.Node, NodeSessions, error) {
	n, err := s.st.GetNode(ctx, id)
	if errors.Is(err, core.NewError(core.CodeNotFound, "")) {
		return core.Node{}, nil, (&core.Error{
			Code:    core.CodeUnavailable,
			Message: "the session's node is no longer registered",
			Details: map[string]string{"node_id": id.String()},
		}).WithReason(core.ExitNodeUnavailable)
	}
	if err != nil {
		return core.Node{}, nil, err
	}
	return s.routeNode(n)
}

func (s *Server) routeNode(sn store.Node) (core.Node, NodeSessions, error) {
	now := s.opts.Now()
	n := s.apiNode(sn, now)
	unavailable := func(msg string) error {
		return (&core.Error{
			Code:    core.CodeUnavailable,
			Message: msg,
			Details: map[string]string{"node": n.Name},
		}).WithReason(core.ExitNodeUnavailable)
	}
	if n.State != core.NodeOnline {
		return n, nil, unavailable(fmt.Sprintf("node %q is offline (last seen %s)", n.Name, n.LastSeen.UTC().Format(time.RFC3339)))
	}
	if s.opts.Nodes == nil {
		return n, nil, unavailable(fmt.Sprintf("this host cannot reach the sessions of node %q", n.Name))
	}
	ns, err := s.opts.Nodes.Route(n)
	if err != nil {
		return n, nil, unavailable(fmt.Sprintf("node %q cannot be reached: %v", n.Name, err))
	}
	return n, ns, nil
}

// nodeNames maps every registered node's id to its name.
func (s *Server) nodeNames(ctx context.Context) (map[core.NodeID]string, error) {
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[core.NodeID]string, len(nodes))
	for _, n := range nodes {
		m[n.ID] = n.Name
	}
	return m, nil
}

// apiSession converts a stored session; names gives the node's name, ""
// for a node no longer registered.
func apiSession(row store.Session, names map[core.NodeID]string) core.Session {
	out := core.Session{
		ID: row.ID, NodeID: row.NodeID, NodeName: names[row.NodeID], Profile: row.Profile,
		Repo: row.RepoRemote, Branch: row.Branch, Slice: row.Slice, Mode: row.Mode,
		Argv: row.Argv, State: row.State, StartedAt: row.StartedAt, ExitCode: row.ExitCode,
	}
	if !row.EndedAt.IsZero() {
		e := row.EndedAt
		out.EndedAt = &e
	}
	if out.Argv == nil {
		out.Argv = []string{}
	}
	return out
}

func newSessionID() (core.SessionID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return core.SessionID{}, err
	}
	return core.NewSessionID(b), nil
}
