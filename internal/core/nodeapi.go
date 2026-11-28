package core

import (
	"errors"
	"maps"
	"strconv"
	"time"
)

// SessionStopPath is where a session is stopped (POST).
func SessionStopPath(id SessionID) string { return SessionPath(id) + "/stop" }

// SessionLogsPath is where a session's scrollback is read (GET), from the
// byte offset of the query parameter "offset" (default 0), and, with
// "follow=1", until the session ends. The body is the raw bytes.
func SessionLogsPath(id SessionID) string { return SessionPath(id) + "/logs" }

// GetSessionResponse is the body of GET on SessionPath.
type GetSessionResponse struct {
	Session Session `json:"session"`
}

// SessionReportsPath is where a node on the tailnet POSTs its session
// reports (ReportSessionRequest). It is served on the host's tailnet only;
// the in-process node reports directly.
const SessionReportsPath = APIPrefix + "/session-reports"

// ReportSessionRequest is the body of a POST on SessionReportsPath. The
// host takes the reporting node from the caller's tailnet identity, and
// accepts the report only for a session of that node.
type ReportSessionRequest struct {
	Report SessionReport `json:"report"`
}

// ReportSessionResponse is the body of an accepted report.
type ReportSessionResponse struct {
	Session Session `json:"session"`
}

// The node API is served by a node daemon on its own tailnet device, to the
// host only. It is how the host starts, stops and reads the sessions of a
// node on another machine.
const (
	// NodeSessionsPath is where the host starts a session on the node
	// (POST NodeStartRequest).
	NodeSessionsPath = APIPrefix + "/node/sessions"
)

// NodeSessionPath is the node API resource of the session with id.
func NodeSessionPath(id SessionID) string { return NodeSessionsPath + "/" + id.String() }

// NodeSessionStopPath is where the host stops a session on the node (POST).
func NodeSessionStopPath(id SessionID) string { return NodeSessionPath(id) + "/stop" }

// NodeSessionLogsPath is where the host reads a session's scrollback on the
// node (GET), with the query of SessionLogsPath.
func NodeSessionLogsPath(id SessionID) string { return NodeSessionPath(id) + "/logs" }

// NodeStartRequest is the body of a POST on NodeSessionsPath: the session
// the host has recorded, to start under the id the host chose. Repo and
// Branch, both set or both "", name the checkout it runs in.
type NodeStartRequest struct {
	ID     SessionID   `json:"id"`
	Mode   SessionMode `json:"mode"`
	Argv   []string    `json:"argv"`
	Repo   string      `json:"repo,omitempty"`
	Branch string      `json:"branch,omitempty"`
}

// Validate returns a CodeInvalidArgument error unless r carries an id, a
// valid mode, a repo and a branch together or neither, and a valid argv.
func (r NodeStartRequest) Validate() error {
	if r.ID.IsZero() {
		return NewError(CodeInvalidArgument, "start request without a session id")
	}
	if err := validRepoBranch(r.Repo, r.Branch); err != nil {
		return err
	}
	if !r.Mode.Valid() {
		return &Error{
			Code:    CodeInvalidArgument,
			Message: "invalid session mode " + quote(string(r.Mode)) + ": want headless or interactive",
			Details: map[string]string{"mode": string(r.Mode)},
		}
	}
	return ValidArgv(r.Argv)
}

// NodeSession is a session as a node reports it to the host in answer to a
// start or a stop. EndedAt is set exactly for a terminal state; ExitCode
// and Sync as in SessionReport.
type NodeSession struct {
	ID        SessionID    `json:"id"`
	State     SessionState `json:"state"`
	StartedAt time.Time    `json:"started_at"`
	EndedAt   *time.Time   `json:"ended_at"`
	ExitCode  *int         `json:"exit_code"`
	Sync      SyncState    `json:"sync,omitempty"`
}

// NodeSessionResponse is the body of a successful start or stop on the
// node API.
type NodeSessionResponse struct {
	Session NodeSession `json:"session"`
}

// Error detail keys of the session API.
const (
	// DetailReason names, in an error the host answers a session request
	// with, the launcher contract exit code that fits it: its Name, e.g.
	// "node_unavailable". The CLI exits with that code.
	DetailReason = "reason"
	// DetailSessionState is set in a node's refusal of a start when the
	// node did record the session, to the state it recorded (failed).
	DetailSessionState = "session_state"
)

// WithReason returns a copy of e with DetailReason set to c's name.
func (e *Error) WithReason(c ExitCode) *Error {
	d := make(map[string]string, len(e.Details)+1)
	maps.Copy(d, e.Details)
	d[DetailReason] = c.Name()
	return &Error{Code: e.Code, Message: e.Message, Details: d}
}

// ExitCodeByName returns the reserved exit code called name, as Name
// returns it.
func ExitCodeByName(name string) (ExitCode, bool) {
	for c, n := range exitCodeNames {
		if n == name {
			return c, true
		}
	}
	return 0, false
}

// ErrNotDelivered marks a request to a node that certainly did not reach
// it, such as one whose connection could not be made: the node did not act
// on it.
var ErrNotDelivered = errors.New("the request did not reach the node")

// LogsQuery is the query string of a logs request (SessionLogsPath,
// NodeSessionLogsPath) from offset, following the session when follow.
func LogsQuery(offset int64, follow bool) string {
	q := "offset=" + strconv.FormatInt(offset, 10)
	if follow {
		q += "&follow=1"
	}
	return q
}

// ParseLogsQuery reads the "offset" and "follow" parameters of a logs
// request, as given ("" when absent). A bad value is CodeInvalidArgument.
func ParseLogsQuery(offset, follow string) (int64, bool, error) {
	var off int64
	if offset != "" {
		var err error
		off, err = strconv.ParseInt(offset, 10, 64)
		if err != nil || off < 0 {
			return 0, false, NewError(CodeInvalidArgument, "offset must be a byte offset, 0 or more")
		}
	}
	switch follow {
	case "", "0", "false":
		return off, false, nil
	case "1", "true":
		return off, true, nil
	}
	return 0, false, NewError(CodeInvalidArgument, "follow must be 1 or 0")
}
