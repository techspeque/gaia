package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SessionMode is how a session's agent is attached: headless sessions run
// on pipes with no terminal, interactive sessions on a PTY.
type SessionMode string

// Session modes.
const (
	SessionHeadless    SessionMode = "headless"
	SessionInteractive SessionMode = "interactive"
)

// SessionModes lists every session mode.
var SessionModes = []SessionMode{SessionHeadless, SessionInteractive}

// Valid reports whether m is a known session mode.
func (m SessionMode) Valid() bool { return m == SessionHeadless || m == SessionInteractive }

// SessionsPath is the collection of sessions.
const SessionsPath = APIPrefix + "/sessions"

// SessionPath is the resource of the session with id.
func SessionPath(id SessionID) string { return SessionsPath + "/" + id.String() }

// Session is a session as the API returns it (OVERVIEW §2.2). NodeName is
// the node's name when the host read the session. Profile, Repo, Branch and
// Slice are "" when none. EndedAt is nil until the session reaches a
// terminal state; ExitCode is nil when no exit code is known.
type Session struct {
	ID        SessionID    `json:"id"`
	NodeID    NodeID       `json:"node_id"`
	NodeName  string       `json:"node_name"`
	Profile   string       `json:"profile"`
	Repo      string       `json:"repo"`
	Branch    string       `json:"branch"`
	Slice     string       `json:"slice"`
	Mode      SessionMode  `json:"mode"`
	Argv      []string     `json:"argv"`
	State     SessionState `json:"state"`
	StartedAt time.Time    `json:"started_at"`
	EndedAt   *time.Time   `json:"ended_at"`
	ExitCode  *int         `json:"exit_code"`
}

// ListSessionsResponse is the body of GET /api/v1/sessions, and the shape
// of `gaia ls -o json` (testdata/contract/gaia/api-v1/sessions*.json).
type ListSessionsResponse struct {
	Sessions []Session `json:"sessions"`
}

// StartSessionRequest is the body of POST /api/v1/sessions. Node is the
// node's name. Repo and Branch are "" to use the node's default; Profile is
// "" for none.
type StartSessionRequest struct {
	Node    string      `json:"node"`
	Repo    string      `json:"repo,omitempty"`
	Branch  string      `json:"branch,omitempty"`
	Mode    SessionMode `json:"mode"`
	Argv    []string    `json:"argv"`
	Profile string      `json:"profile,omitempty"`
}

// Validate returns a CodeInvalidArgument error unless r names a valid node
// and mode and carries a valid argv.
func (r StartSessionRequest) Validate() error {
	if err := ValidNodeName(r.Node); err != nil {
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

// StartSessionResponse is the body of a successful POST /api/v1/sessions.
type StartSessionResponse struct {
	Session Session `json:"session"`
}

// StopSessionResponse is the body of a successful stop of a session.
type StopSessionResponse struct {
	Session Session `json:"session"`
}

// ValidArgv returns a CodeInvalidArgument error unless argv has at least
// one element, argv[0] is not empty, and every element is valid UTF-8 with
// no NUL byte. A process argument cannot carry NUL, and JSON would alter
// invalid UTF-8, so every argv this accepts is stored and sent unchanged.
func ValidArgv(argv []string) error {
	bad := func(i int, reason string) error {
		d := map[string]string{}
		if i >= 0 {
			d["index"] = strconv.Itoa(i)
		}
		msg := "invalid argv: " + reason
		if i >= 0 {
			msg = fmt.Sprintf("invalid argv[%d]: %s", i, reason)
		}
		return &Error{Code: CodeInvalidArgument, Message: msg, Details: d}
	}
	if len(argv) == 0 {
		return bad(-1, "empty")
	}
	if argv[0] == "" {
		return bad(0, "program is empty")
	}
	for i, a := range argv {
		if !utf8.ValidString(a) {
			return bad(i, "not valid UTF-8")
		}
		if strings.IndexByte(a, 0) >= 0 {
			return bad(i, "contains a NUL byte")
		}
	}
	return nil
}
