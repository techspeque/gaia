package supervise

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// DefaultGrace is how long Stop waits after SIGTERM before SIGKILL, as the
// launcher contract v1 states.
const DefaultGrace = 10 * time.Second

// Default PTY size when the spec gives none.
const (
	defaultRows = 24
	defaultCols = 80
)

// Spec is what a supervisor runs. It reaches the supervisor as one JSON
// object on stdin and is never written to disk. Env and Secrets may hold
// secret values: a Spec formats with fmt as its session id and mode only.
type Spec struct {
	SessionID core.SessionID   `json:"session_id"`
	Mode      core.SessionMode `json:"mode"`
	Argv      []string         `json:"argv"`
	// WorkDir is the absolute directory the agent starts in.
	WorkDir string `json:"work_dir"`
	// Env is the agent's complete environment, KEY=VALUE. argv[0] is looked
	// up in its PATH. Nothing of the supervisor's own environment is added.
	Env []string `json:"env"`
	// Secrets are values replaced by core.Redacted in the scrollback.
	Secrets []string `json:"secrets"`
	// Rows and Cols are the initial PTY size, interactive mode only;
	// 0 means 24×80.
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
	// GraceMS is the stop grace period in milliseconds; 0 means DefaultGrace.
	GraceMS int64 `json:"grace_ms"`
}

// Format writes the session id and mode only, for every verb, so a Spec in
// a log line never shows its environment or secrets.
func (s Spec) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "supervise.Spec{session_id=%s mode=%s}", s.SessionID, s.Mode)
}

// Validate returns a CodeInvalidArgument error unless s can be run.
func (s Spec) Validate() error {
	bad := func(reason string) error {
		return &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: "invalid supervisor spec: " + reason,
			Details: map[string]string{"session_id": s.SessionID.String()},
		}
	}
	switch {
	case s.SessionID.IsZero():
		return bad("session id is empty")
	case !s.Mode.Valid():
		return bad(fmt.Sprintf("mode %q is not headless or interactive", s.Mode))
	case !filepath.IsAbs(s.WorkDir):
		return bad(fmt.Sprintf("work_dir %q is not an absolute path", s.WorkDir))
	case s.GraceMS < 0:
		return bad("grace_ms is negative")
	}
	if err := core.ValidArgv(s.Argv); err != nil {
		return bad(err.Error())
	}
	return nil
}

func (s Spec) grace() time.Duration {
	if s.GraceMS == 0 {
		return DefaultGrace
	}
	return time.Duration(s.GraceMS) * time.Millisecond
}

func (s Spec) size() (rows, cols uint16) {
	rows, cols = s.Rows, s.Cols
	if rows == 0 {
		rows = defaultRows
	}
	if cols == 0 {
		cols = defaultCols
	}
	return rows, cols
}

// pathEnv returns the PATH entry of env, "" when absent.
func pathEnv(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "PATH="); ok {
			return v
		}
	}
	return ""
}
