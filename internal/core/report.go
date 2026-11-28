package core

import (
	"strconv"
	"time"
)

// SessionReport is a node's report of a session's state to the host. A
// node sends one for every state change, in order of Seq, which rises by
// one per report of the same session; it resends a report the host may not
// have recorded, so the host treats a Seq it has already applied as done.
// ExitCode is set for an exited or stopped session when the agent's exit
// code is known (128+N for death by signal N); EndedAt is set exactly for
// a terminal state.
type SessionReport struct {
	SessionID SessionID    `json:"session_id"`
	Seq       uint64       `json:"seq"`
	State     SessionState `json:"state"`
	ExitCode  *int         `json:"exit_code"`
	EndedAt   *time.Time   `json:"ended_at"`
}

// Validate returns a CodeInvalidArgument error unless r names a session,
// has a positive Seq and a known state, an end time exactly when the state
// is terminal, an exit code when exited, and none when not exited or
// stopped.
func (r SessionReport) Validate() error {
	bad := func(reason string) error {
		return &Error{
			Code:    CodeInvalidArgument,
			Message: "invalid session report: " + reason,
			Details: map[string]string{"session_id": r.SessionID.String(), "seq": strconv.FormatUint(r.Seq, 10)},
		}
	}
	switch {
	case r.SessionID.IsZero():
		return bad("session id is empty")
	case r.Seq == 0:
		return bad("seq is 0")
	case !r.State.Valid():
		return bad("state " + quote(string(r.State)) + " is not a session state")
	case r.State.Terminal() != (r.EndedAt != nil):
		return bad("ended_at is set exactly for a terminal state")
	case r.EndedAt != nil && r.EndedAt.IsZero():
		return bad("ended_at is zero")
	case r.State == SessionExited && r.ExitCode == nil:
		return bad("an exited session needs an exit code")
	case r.ExitCode != nil && r.State != SessionExited && r.State != SessionStopped:
		return bad("only an exited or stopped session has an exit code")
	}
	return nil
}
