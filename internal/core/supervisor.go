package core

import (
	"strconv"
	"time"
)

// SupervisorStateVersion is the format version of the supervisor state file.
const SupervisorStateVersion = 1

// SupervisorState is the supervisor's state file (ADR-0008): what a node
// daemon reads to rediscover a session after a restart. The supervisor
// writes it on start and again on exit. ExitCode, Signal and EndedAt are
// unset while the child runs. Signal is the name of the signal that killed
// the child ("SIGKILL"), "" when it exited by itself. It holds no secret:
// never the environment.
type SupervisorState struct {
	Version   int         `json:"version"`
	SessionID SessionID   `json:"session_id"`
	PID       int         `json:"pid"`
	Mode      SessionMode `json:"mode"`
	Argv      []string    `json:"argv"`
	StartedAt time.Time   `json:"started_at"`
	ExitCode  *int        `json:"exit_code"`
	Signal    string      `json:"signal"`
	EndedAt   *time.Time  `json:"ended_at"`
}

// Ended reports whether the state records the child's end.
func (s SupervisorState) Ended() bool { return s.EndedAt != nil }

// Validate returns a CodeInvalidArgument error unless s is a complete state
// of this format version: a session id, a positive pid, a valid mode and
// argv, a start time, and either no outcome (running) or an end time with
// an exit code or a signal.
func (s SupervisorState) Validate() error {
	bad := func(reason string) error {
		return &Error{
			Code:    CodeInvalidArgument,
			Message: "invalid supervisor state: " + reason,
			Details: map[string]string{"session_id": s.SessionID.String()},
		}
	}
	switch {
	case s.Version != SupervisorStateVersion:
		return bad("version " + strconv.Itoa(s.Version) + ", want " + strconv.Itoa(SupervisorStateVersion))
	case s.SessionID.IsZero():
		return bad("session id is empty")
	case s.PID <= 0:
		return bad("pid " + strconv.Itoa(s.PID) + " is not positive")
	case !s.Mode.Valid():
		return bad("mode " + quote(string(s.Mode)) + " is not headless or interactive")
	case s.StartedAt.IsZero():
		return bad("started_at is zero")
	}
	if err := ValidArgv(s.Argv); err != nil {
		return bad(err.Error())
	}
	if s.EndedAt == nil {
		if s.ExitCode != nil || s.Signal != "" {
			return bad("exit code or signal without ended_at")
		}
		return nil
	}
	switch {
	case s.EndedAt.IsZero():
		return bad("ended_at is zero")
	case s.EndedAt.Before(s.StartedAt):
		return bad("ended_at is before started_at")
	case s.ExitCode == nil && s.Signal == "":
		return bad("ended without an exit code or a signal")
	}
	return nil
}
