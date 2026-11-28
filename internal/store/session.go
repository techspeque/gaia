package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Session is a session as stored (OVERVIEW §2.2). Profile, RepoRemote,
// Branch and Slice are "" when none. EndedAt is zero until the session
// reaches a terminal state; ExitCode is nil when no exit code is known.
type Session struct {
	ID         core.SessionID
	NodeID     core.NodeID
	Profile    string
	RepoRemote string
	Branch     string
	Slice      string
	Mode       core.SessionMode
	Argv       []string
	State      core.SessionState
	StartedAt  time.Time
	EndedAt    time.Time
	ExitCode   *int
}

// SessionExit is how a session ended: a terminal state, the time, and the
// agent's exit code when one is known.
type SessionExit struct {
	State    core.SessionState
	ExitCode *int
	EndedAt  time.Time
}

const sessionColumns = `id, node_id, profile, repo_remote, branch, slice, mode, argv, state, started_at, ended_at, exit_code`

// CreateSession inserts s, which must be in state starting with no end time
// or exit code. A duplicate id is a core.CodeConflict error.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	if err := validateNewSession(sess); err != nil {
		return err
	}
	argv, err := json.Marshal(sess.Argv)
	if err != nil {
		return err
	}
	var profile any
	if sess.Profile != "" {
		profile = sess.Profile
	}
	_, err = s.w.ExecContext(ctx,
		`INSERT INTO sessions (id, node_id, profile, repo_remote, branch, slice, mode, argv, state, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID.String(), sess.NodeID.String(), profile, sess.RepoRemote, sess.Branch, sess.Slice,
		string(sess.Mode), string(argv), string(sess.State), sess.StartedAt.UnixNano())
	if isUniqueViolation(err) || isDuplicateSession(err) {
		return &core.Error{
			Code:    core.CodeConflict,
			Message: fmt.Sprintf("a session with id %s already exists", sess.ID),
			Details: map[string]string{"id": sess.ID.String()},
		}
	}
	return err
}

// GetSession returns the session with id, or a core.CodeNotFound error.
func (s *Store) GetSession(ctx context.Context, id core.SessionID) (Session, error) {
	row := s.r.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id.String())
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, sessionNotFound(id)
	}
	return sess, err
}

// ListSessions returns every session, oldest first: by started_at, then id.
func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// UpdateSessionState moves the session with id to the non-terminal state
// to. A move the state table forbids is a core.CodeConflict error and
// changes nothing; a missing session is core.CodeNotFound. A terminal state
// is reached only through SetSessionExit.
func (s *Store) UpdateSessionState(ctx context.Context, id core.SessionID, to core.SessionState) error {
	if !to.Valid() || to.Terminal() {
		return invalidSession(fmt.Sprintf("state %q is not a live session state; use SetSessionExit for an end", to))
	}
	return s.transitionSession(ctx, id, to, `UPDATE sessions SET state = ? WHERE id = ? AND state = ?`,
		func(from core.SessionState) []any { return []any{string(to), id.String(), string(from)} })
}

// SetSessionExit records the end of the session with id: its terminal
// state, end time and exit code, in one write. A move the state table
// forbids is a core.CodeConflict error and changes nothing; a missing
// session is core.CodeNotFound.
func (s *Store) SetSessionExit(ctx context.Context, id core.SessionID, e SessionExit) error {
	switch {
	case !e.State.Terminal():
		return invalidSession(fmt.Sprintf("state %q is not a terminal session state", e.State))
	case e.EndedAt.IsZero():
		return invalidSession("ended_at is zero")
	case e.State == core.SessionExited && e.ExitCode == nil:
		return invalidSession("an exited session needs an exit code")
	}
	var code any
	if e.ExitCode != nil {
		code = *e.ExitCode
	}
	return s.transitionSession(ctx, id, e.State,
		`UPDATE sessions SET state = ?, ended_at = ?, exit_code = ? WHERE id = ? AND state = ?`,
		func(from core.SessionState) []any {
			return []any{string(e.State), e.EndedAt.UnixNano(), code, id.String(), string(from)}
		})
}

// transitionSession reads the session's state and, if the state table
// allows the move to to, runs update with args(from) in the same
// transaction. The writer's transactions take the write lock at BEGIN, so
// no other write lands between the read and the update.
func (s *Store) transitionSession(ctx context.Context, id core.SessionID, to core.SessionState,
	update string, args func(from core.SessionState) []any) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var from string
	err = tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id = ?`, id.String()).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionNotFound(id)
	}
	if err != nil {
		return err
	}
	if _, err := core.SessionState(from).Transition(to); err != nil {
		cerr := err.(*core.Error)
		cerr.Details["id"] = id.String()
		return cerr
	}
	res, err := tx.ExecContext(ctx, update, args(core.SessionState(from))...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("session %s: state changed during transition", id)
	}
	return tx.Commit()
}

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var (
		sess           Session
		id, node, argv string
		mode, state    string
		profile        sql.NullString
		started        int64
		ended, code    sql.NullInt64
	)
	if err := row.Scan(&id, &node, &profile, &sess.RepoRemote, &sess.Branch, &sess.Slice,
		&mode, &argv, &state, &started, &ended, &code); err != nil {
		return Session{}, err
	}
	var err error
	if sess.ID, err = core.ParseSessionID(id); err != nil {
		return Session{}, fmt.Errorf("stored session id: %w", err)
	}
	if sess.NodeID, err = core.ParseNodeID(node); err != nil {
		return Session{}, fmt.Errorf("stored session %s node id: %w", id, err)
	}
	if err := json.Unmarshal([]byte(argv), &sess.Argv); err != nil {
		return Session{}, fmt.Errorf("stored session %s argv: %w", id, err)
	}
	sess.Profile = profile.String
	sess.Mode = core.SessionMode(mode)
	sess.State = core.SessionState(state)
	sess.StartedAt = time.Unix(0, started).UTC()
	if ended.Valid {
		sess.EndedAt = time.Unix(0, ended.Int64).UTC()
	}
	if code.Valid {
		c := int(code.Int64)
		sess.ExitCode = &c
	}
	return sess, nil
}

func validateNewSession(sess Session) error {
	switch {
	case sess.ID.IsZero():
		return invalidSession("id is empty")
	case sess.NodeID.IsZero():
		return invalidSession("node id is empty")
	case !sess.Mode.Valid():
		return invalidSession(fmt.Sprintf("mode %q is not headless or interactive", sess.Mode))
	case sess.State != core.SessionStarting:
		return invalidSession(fmt.Sprintf("a new session is %q, not %q", core.SessionStarting, sess.State))
	case sess.StartedAt.IsZero():
		return invalidSession("started_at is zero")
	case !sess.EndedAt.IsZero() || sess.ExitCode != nil:
		return invalidSession("a new session has no end time or exit code")
	}
	return core.ValidArgv(sess.Argv)
}

func invalidSession(reason string) *core.Error {
	return core.NewError(core.CodeInvalidArgument, "invalid session: "+reason)
}

func sessionNotFound(id core.SessionID) *core.Error {
	return &core.Error{
		Code:    core.CodeNotFound,
		Message: fmt.Sprintf("session %s not found", id),
		Details: map[string]string{"id": id.String()},
	}
}

// isDuplicateSession reports whether err is the sessions_no_replace trigger
// refusing an existing id, which it does before the primary key is checked.
func isDuplicateSession(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_TRIGGER &&
		strings.Contains(se.Error(), "session id already exists")
}
