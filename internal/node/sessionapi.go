package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/supervise"
)

// LocalSessions is a node's session manager as the host drives it: start,
// stop and read the scrollback of sessions by the core wire types. The
// host's in-process node uses it directly; a node on the tailnet serves it
// to the host with SessionHandler.
type LocalSessions struct {
	// Manager runs the sessions.
	Manager *Manager
	// WorkDir is the directory an agent started without a repository starts
	// in (the node user's home). One with a repository starts in the node's
	// checkout of it.
	WorkDir string
	// Env returns the agent's whole environment, KEY=VALUE.
	Env func() []string
}

// nodeSession converts a manager's SessionInfo for the host.
func nodeSession(info SessionInfo) core.NodeSession {
	return core.NodeSession{
		ID: info.ID, State: info.State, StartedAt: info.StartedAt,
		EndedAt: info.EndedAt, ExitCode: info.ExitCode, Sync: info.Sync,
	}
}

// StartSession starts the session r describes. A refusal after the node
// recorded the session (a supervisor that could not start the agent) is a
// *core.Error carrying core.DetailSessionState, the state recorded.
func (l *LocalSessions) StartSession(ctx context.Context, r core.NodeStartRequest) (core.NodeSession, error) {
	if err := r.Validate(); err != nil {
		return core.NodeSession{}, err
	}
	var env []string
	if l.Env != nil {
		env = l.Env()
	}
	info, err := l.Manager.Start(ctx, StartSpec{
		ID: r.ID, Mode: r.Mode, Argv: r.Argv, WorkDir: l.WorkDir, Env: env,
		Repo: r.Repo, Branch: r.Branch,
	})
	if err == nil {
		return nodeSession(info), nil
	}
	if info.ID.IsZero() {
		return core.NodeSession{}, err
	}
	ce := &core.Error{Code: core.CodeInternal, Message: err.Error()}
	var src *core.Error
	if errors.As(err, &src) {
		ce.Code, ce.Message = src.Code, src.Message
		ce.Details = maps.Clone(src.Details)
	}
	if ce.Details == nil {
		ce.Details = map[string]string{}
	}
	ce.Details[core.DetailSessionState] = string(info.State)
	return nodeSession(info), ce
}

// StopSession stops the session with id and returns it as it ended; a
// session already ended is returned as it is.
func (l *LocalSessions) StopSession(ctx context.Context, id core.SessionID) (core.NodeSession, error) {
	info, err := l.Manager.Stop(ctx, id)
	if info.ID.IsZero() {
		return core.NodeSession{}, err
	}
	return nodeSession(info), err
}

// streamRetryWait is the pause before following a session's output again
// after a stream was cut.
const streamRetryWait = 100 * time.Millisecond

// errLimit ends a stream that has written what was asked.
var errLimit = errors.New("limit reached")

// limitWriter passes at most n bytes to w and fails with errLimit as soon
// as the n-th byte is written.
type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errLimit
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	k, err := l.w.Write(p)
	l.n -= int64(k)
	if err == nil && l.n <= 0 {
		err = errLimit
	}
	return k, err
}

// SessionLogs writes the scrollback of the session with id to w from the
// logical byte offset off: what exists now, or, with follow, everything
// until the session ends. An ended session is read from its scrollback
// files; a running one from its supervisor. An offset before the first
// byte kept is core.CodeInvalidArgument with the detail "start"; a
// scrollback dropped to keep the node's bound is core.CodeNotFound.
func (l *LocalSessions) SessionLogs(ctx context.Context, id core.SessionID, off int64, follow bool, w io.Writer) error {
	if off < 0 {
		return core.NewError(core.CodeInvalidArgument, "the offset is negative")
	}
	if _, err := l.Manager.Get(id); err != nil {
		return err
	}
	dir := l.Manager.SessionDir(id)
	w = markWrites{w}
	failedCalls := 0
	for {
		sb, err := supervise.OpenScrollback(dir, off)
		if err == nil {
			_, err = io.Copy(w, sb)
			_ = sb.Close()
			return err
		}
		if !errors.Is(err, core.NewError(core.CodeConflict, "")) {
			return err
		}
		// Running: its supervisor has the output.
		c := supervise.NewClient(dir)
		var n int64
		if follow {
			n, err = c.Stream(ctx, off, w)
		} else {
			var st supervise.Status
			if st, err = c.Status(ctx); err == nil {
				if off > st.Size {
					return &core.Error{
						Code:    core.CodeInvalidArgument,
						Message: fmt.Sprintf("offset %d is past the end of the scrollback, %d", off, st.Size),
						Details: map[string]string{"end": strconv.FormatInt(st.Size, 10)},
					}
				}
				if off == st.Size {
					return nil
				}
				n, err = c.Stream(ctx, off, &limitWriter{w: w, n: st.Size - off})
				if errors.Is(err, errLimit) {
					return nil
				}
			}
		}
		off += n
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, core.NewError(core.CodeInvalidArgument, "")):
			return err
		}
		var se sinkError
		if errors.As(err, &se) {
			return se.error
		}
		// Cut, or the supervisor is between answering and exiting: the
		// state file says which on the next round. A supervisor that
		// keeps failing without a byte is gone.
		if n == 0 {
			failedCalls++
		} else {
			failedCalls = 0
		}
		if failedCalls >= 3 {
			return err
		}
		if !sleep(ctx, streamRetryWait) {
			return ctx.Err()
		}
	}
}

// sinkError marks a failure of the caller's writer, which ends a stream
// at once.
type sinkError struct{ error }

func (e sinkError) Unwrap() error { return e.error }

// markWrites wraps w so that its failures are sinkErrors.
type markWrites struct{ w io.Writer }

func (m markWrites) Write(p []byte) (int, error) {
	n, err := m.w.Write(p)
	if err != nil {
		return n, sinkError{err}
	}
	return n, nil
}

// PeerCheck returns nil when the tailnet peer at remoteAddr is the host,
// the only caller the node API admits.
type PeerCheck func(ctx context.Context, remoteAddr string) error

// SessionHandler serves the node API (core.NodeSessionsPath and below) of
// s to the host on the node's tailnet listener. Every request is first
// checked with check; a refusal is 403 with code forbidden and a warn log
// line. Anything else is 404.
func SessionHandler(s *LocalSessions, check PeerCheck, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+core.NodeSessionsPath, func(w http.ResponseWriter, r *http.Request) {
		var req core.NodeStartRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxNodeRequest)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, core.NewError(core.CodeInvalidArgument, "the request body is not a start request"))
			return
		}
		sess, err := s.StartSession(r.Context(), req)
		if err != nil {
			writeNodeError(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, core.NodeSessionResponse{Session: sess})
	})
	mux.HandleFunc("POST "+core.NodeSessionsPath+"/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id, err := core.ParseSessionID(r.PathValue("id"))
		if err != nil {
			writeNodeError(w, err, log)
			return
		}
		sess, err := s.StopSession(r.Context(), id)
		if err != nil {
			writeNodeError(w, err, log)
			return
		}
		writeJSON(w, http.StatusOK, core.NodeSessionResponse{Session: sess})
	})
	mux.HandleFunc("GET "+core.NodeSessionsPath+"/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id, err := core.ParseSessionID(r.PathValue("id"))
		if err != nil {
			writeNodeError(w, err, log)
			return
		}
		q := r.URL.Query()
		off, follow, err := core.ParseLogsQuery(q.Get("offset"), q.Get("follow"))
		if err != nil {
			writeNodeError(w, err, log)
			return
		}
		fw := NewStreamWriter(w)
		err = s.SessionLogs(r.Context(), id, off, follow, fw)
		if err != nil && !fw.Started() {
			writeNodeError(w, err, log)
			return
		}
		if err != nil && r.Context().Err() == nil {
			log.Warn("streaming a session's scrollback to the host ended early", "session", id.String(), "err", err)
		}
		fw.Finish()
	})
	mux.HandleFunc("GET "+core.NodeSessionsPath+"/{id}/tty", serveTTY(s, log))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, core.NewError(core.CodeNotFound, "no such endpoint"))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := check(r.Context(), r.RemoteAddr); err != nil {
			log.Warn("node API request refused: the caller is not the host", "remote_addr", r.RemoteAddr, "err", err)
			writeError(w, http.StatusForbidden, core.NewError(core.CodeForbidden, "permission denied"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// maxNodeRequest bounds a node API request body.
const maxNodeRequest = 64 << 10

// writeNodeError answers with err: a *core.Error with its status, anything
// else as a generic internal error, logged.
func writeNodeError(w http.ResponseWriter, err error, log *slog.Logger) {
	var ce *core.Error
	if errors.As(err, &ce) && ce.Code != core.CodeInternal {
		writeError(w, statusFor(ce.Code), ce)
		return
	}
	log.Error("node API request failed", "err", err)
	e := core.NewError(core.CodeInternal, "internal error")
	if ce != nil && ce.Details[core.DetailSessionState] != "" {
		e.Details = map[string]string{core.DetailSessionState: ce.Details[core.DetailSessionState]}
	}
	writeError(w, http.StatusInternalServerError, e)
}

// statusFor maps an error code to its HTTP status.
func statusFor(c core.ErrorCode) int {
	switch c {
	case core.CodeInvalidArgument:
		return http.StatusBadRequest
	case core.CodeUnauthorized:
		return http.StatusUnauthorized
	case core.CodeForbidden:
		return http.StatusForbidden
	case core.CodeNotFound:
		return http.StatusNotFound
	case core.CodeConflict, core.CodeVersionMismatch:
		return http.StatusConflict
	case core.CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// StreamWriter writes a raw byte stream as an HTTP response: the headers
// on the first write, and a flush after every write, so the reader gets
// the bytes as they are produced.
type StreamWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

// NewStreamWriter returns a StreamWriter on w.
func NewStreamWriter(w http.ResponseWriter) *StreamWriter {
	return &StreamWriter{w: w, rc: http.NewResponseController(w)}
}

// Started reports whether the response has begun: an error can no longer
// be sent as a status.
func (s *StreamWriter) Started() bool { return s.started }

func (s *StreamWriter) Write(p []byte) (int, error) {
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

// Finish starts an empty response if nothing was written.
func (s *StreamWriter) Finish() {
	if !s.started {
		_, _ = s.Write(nil)
	}
}

// SessionClient is the node API as the host calls it over the tailnet.
type SessionClient struct {
	// HTTP carries the requests; its transport reaches the node. Its
	// Timeout must be zero, or it would cut a followed stream.
	HTTP *http.Client
	// BaseURL is the node's API origin, e.g. https://node-b.
	BaseURL string
	// Timeout bounds a start or a stop. Default: 30 s.
	Timeout time.Duration
	// RepoStartTimeout bounds a start with a repository, which includes
	// waiting for the checkout and readying it. Default: RepoStartTimeout.
	RepoStartTimeout time.Duration
}

// RepoStartTimeout is the default bound of a start with a repository: a
// clone on first use, and the wait for a session holding the checkout.
const RepoStartTimeout = time.Hour

// StartSession asks the node to start r.
func (c *SessionClient) StartSession(ctx context.Context, r core.NodeStartRequest) (core.NodeSession, error) {
	var res core.NodeSessionResponse
	timeout := c.Timeout
	if r.Repo != "" {
		timeout = c.RepoStartTimeout
		if timeout <= 0 {
			timeout = RepoStartTimeout
		}
	}
	err := c.call(ctx, timeout, core.NodeSessionsPath, r, &res)
	return res.Session, err
}

// StopSession asks the node to stop the session with id.
func (c *SessionClient) StopSession(ctx context.Context, id core.SessionID) (core.NodeSession, error) {
	var res core.NodeSessionResponse
	err := c.call(ctx, c.Timeout, core.NodeSessionStopPath(id), struct{}{}, &res)
	return res.Session, err
}

// SessionLogs copies the session's scrollback from the node to w, as
// LocalSessions.SessionLogs describes.
func (c *SessionClient) SessionLogs(ctx context.Context, id core.SessionID, off int64, follow bool, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+core.NodeSessionLogsPath(id)+"?"+core.LogsQuery(off, follow), nil)
	if err != nil {
		return err
	}
	res, err := c.do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return responseError(res, core.NodeSessionLogsPath(id))
	}
	_, err = io.Copy(w, res.Body)
	return err
}

// call POSTs in as JSON to path, within timeout (30 s when 0), and decodes
// a 2xx answer into out.
func (c *SessionClient) call(ctx context.Context, timeout time.Duration, path string, in, out any) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return responseError(res, path)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("reading the node's answer to %s: %w", path, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("the node's answer to %s is not valid: %w", path, err)
	}
	return nil
}

// do sends req. A failure before a connection to the node was made wraps
// core.ErrNotDelivered: the node never saw the request.
func (c *SessionClient) do(req *http.Request) (*http.Response, error) {
	connected := false
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected = true }}
	res, err := c.HTTP.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil && !connected {
		return nil, fmt.Errorf("%w: %w", core.ErrNotDelivered, err)
	}
	return res, err
}

// responseError is the node's *core.Error in a non-2xx answer.
func responseError(res *http.Response, path string) error {
	b, _ := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	var er core.ErrorResponse
	if json.Unmarshal(b, &er) == nil && er.Error != nil && er.Error.Code != "" {
		return er.Error
	}
	return fmt.Errorf("the node answered %s with HTTP %d", path, res.StatusCode)
}
