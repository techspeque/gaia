package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

func sessionID(b byte) core.SessionID {
	var r [16]byte
	r[0] = b
	return core.NewSessionID(r)
}

func testSession(b byte) Session {
	return Session{
		ID:         sessionID(b),
		NodeID:     nodeID(1),
		Profile:    "",
		RepoRemote: "https://git.example.com/acme/widgets.git",
		Branch:     "main",
		Mode:       core.SessionHeadless,
		Argv:       []string{"claude", "-p", "hello"},
		State:      core.SessionStarting,
		StartedAt:  time.Date(2026, 11, 28, 9, 0, 0, 123456789, time.UTC).Add(time.Duration(b) * time.Second),
	}
}

func intp(v int) *int { return &v }

func sameSession(t *testing.T, got, want Session) {
	t.Helper()
	if got.ID != want.ID || got.NodeID != want.NodeID || got.Profile != want.Profile ||
		got.RepoRemote != want.RepoRemote || got.Branch != want.Branch || got.Slice != want.Slice ||
		got.Mode != want.Mode || !reflect.DeepEqual(got.Argv, want.Argv) || got.State != want.State ||
		!got.StartedAt.Equal(want.StartedAt) || !got.EndedAt.Equal(want.EndedAt) ||
		!reflect.DeepEqual(got.ExitCode, want.ExitCode) {
		t.Errorf("session = %+v, want %+v", got, want)
	}
}

func mustCreate(t *testing.T, s *Store, sess Session) {
	t.Helper()
	if err := s.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("CreateSession(%s): %v", sess.ID, err)
	}
}

func TestSessionCRUD(t *testing.T) {
	ctx := context.Background()
	dir := stateDir(t)
	s := mustOpen(t, dir)

	if l, err := s.ListSessions(ctx); err != nil || l == nil || len(l) != 0 {
		t.Fatalf("empty list = %v, %v; want empty non-nil", l, err)
	}

	later, earlier := testSession(5), testSession(2)
	later.Profile, later.Slice, later.Mode = "work", "phase-1-ws-1.1", core.SessionInteractive
	mustCreate(t, s, later)
	mustCreate(t, s, earlier)

	got, err := s.GetSession(ctx, later.ID)
	if err != nil {
		t.Fatal(err)
	}
	sameSession(t, got, later)

	l, err := s.ListSessions(ctx)
	if err != nil || len(l) != 2 {
		t.Fatalf("list = %v, %v", l, err)
	}
	sameSession(t, l[0], earlier)
	sameSession(t, l[1], later)

	if err := s.UpdateSessionState(ctx, later.ID, core.SessionRunning); err != nil {
		t.Fatal(err)
	}
	end := later.StartedAt.Add(time.Hour)
	if err := s.SetSessionExit(ctx, later.ID, SessionExit{State: core.SessionExited, ExitCode: intp(7), EndedAt: end}); err != nil {
		t.Fatal(err)
	}
	later.State, later.EndedAt, later.ExitCode = core.SessionExited, end, intp(7)
	got, _ = s.GetSession(ctx, later.ID)
	sameSession(t, got, later)

	// Data survives a reopen; the profile column is NULL for "".
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpen(t, dir)
	got, err = s2.GetSession(ctx, later.ID)
	if err != nil {
		t.Fatal(err)
	}
	sameSession(t, got, later)
	var nulls int
	if err := s2.r.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE profile IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Errorf("NULL profiles = %d, %v; want 1", nulls, err)
	}
}

func TestSessionListOrderTieBreak(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	a, b := testSession(9), testSession(3)
	b.StartedAt = a.StartedAt
	mustCreate(t, s, a)
	mustCreate(t, s, b)
	l, err := s.ListSessions(ctx)
	if err != nil || len(l) != 2 {
		t.Fatalf("list = %v, %v", l, err)
	}
	if l[0].ID != b.ID || l[1].ID != a.ID { // same time: by id
		t.Errorf("order %s, %s", l[0].ID, l[1].ID)
	}
}

func TestSessionNotFound(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	id := sessionID(99)
	_, err := s.GetSession(ctx, id)
	wantCode(t, err, core.CodeNotFound)
	wantCode(t, s.UpdateSessionState(ctx, id, core.SessionRunning), core.CodeNotFound)
	wantCode(t, s.SetSessionExit(ctx, id, SessionExit{State: core.SessionFailed, EndedAt: time.Now()}), core.CodeNotFound)
}

func TestSessionDuplicate(t *testing.T) {
	s := mustOpen(t, stateDir(t))
	sess := testSession(1)
	mustCreate(t, s, sess)
	wantCode(t, s.CreateSession(context.Background(), sess), core.CodeConflict)
}

func TestSessionCreateInvalid(t *testing.T) {
	s := mustOpen(t, stateDir(t))
	for name, mut := range map[string]func(*Session){
		"no id":        func(x *Session) { x.ID = core.SessionID{} },
		"no node":      func(x *Session) { x.NodeID = core.NodeID{} },
		"bad mode":     func(x *Session) { x.Mode = "pty" },
		"running":      func(x *Session) { x.State = core.SessionRunning },
		"exited":       func(x *Session) { x.State = core.SessionExited },
		"no start":     func(x *Session) { x.StartedAt = time.Time{} },
		"ended":        func(x *Session) { x.EndedAt = x.StartedAt },
		"exit code":    func(x *Session) { x.ExitCode = intp(0) },
		"no argv":      func(x *Session) { x.Argv = nil },
		"empty prog":   func(x *Session) { x.Argv = []string{""} },
		"NUL in argv":  func(x *Session) { x.Argv = []string{"a", "b\x00"} },
		"invalid utf8": func(x *Session) { x.Argv = []string{"a", "\xff\xfe"} },
	} {
		t.Run(name, func(t *testing.T) {
			sess := testSession(1)
			mut(&sess)
			wantCode(t, s.CreateSession(context.Background(), sess), core.CodeInvalidArgument)
		})
	}
	if l, _ := s.ListSessions(context.Background()); len(l) != 0 {
		t.Errorf("invalid creates stored %d rows", len(l))
	}
}

// TestSessionArgvRoundTrip stores argv values with empty strings, quotes,
// backslashes, control characters and non-ASCII, and reads each back byte
// for byte.
func TestSessionArgvRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := stateDir(t)
	s := mustOpen(t, dir)
	cases := [][]string{
		{"claude"},
		{"claude", ""},
		{"claude", "", "", ""},
		{"sh", `"double"`, `'single'`, "`back`", `\`, `\\n`, `\"`},
		{"echo", "line1\nline2", "cr\r\n", "tab\there", "\x01\x1f\x7f"},
		{"echo", "ünïcödé", "日本語", "emoji 🚀", "  ", "<>&"},
		{"echo", `{"json": [1, 2]}`, "null", "[]", "  lead and trail  "},
	}
	for i, argv := range cases {
		sess := testSession(byte(i + 1))
		sess.Argv = argv
		mustCreate(t, s, sess)
	}
	check := func(s *Store) {
		for i, argv := range cases {
			got, err := s.GetSession(ctx, sessionID(byte(i+1)))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Argv) != len(argv) {
				t.Errorf("case %d: argv %q, want %q", i, got.Argv, argv)
				continue
			}
			for j := range argv {
				if got.Argv[j] != argv[j] {
					t.Errorf("case %d argv[%d] = %q, want %q", i, j, got.Argv[j], argv[j])
				}
			}
		}
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	check(mustOpen(t, dir))
}

// TestSessionTransitionMatrix tries every move between the six states and
// checks the store allows exactly the core table's legal ones.
func TestSessionTransitionMatrix(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	n := byte(0)

	// reach puts a fresh session in state st through legal moves.
	reach := func(st core.SessionState) core.SessionID {
		t.Helper()
		n++
		sess := testSession(n)
		mustCreate(t, s, sess)
		end := SessionExit{State: st, ExitCode: intp(0), EndedAt: sess.StartedAt.Add(time.Minute)}
		switch st {
		case core.SessionStarting:
		case core.SessionRunning, core.SessionLost:
			if err := s.UpdateSessionState(ctx, sess.ID, st); err != nil {
				t.Fatal(err)
			}
		default: // exited is reached only from running
			if err := s.UpdateSessionState(ctx, sess.ID, core.SessionRunning); err != nil {
				t.Fatal(err)
			}
			if err := s.SetSessionExit(ctx, sess.ID, end); err != nil {
				t.Fatal(err)
			}
		}
		return sess.ID
	}
	move := func(id core.SessionID, to core.SessionState) error {
		if to.Terminal() {
			return s.SetSessionExit(ctx, id, SessionExit{State: to, ExitCode: intp(1), EndedAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)})
		}
		return s.UpdateSessionState(ctx, id, to)
	}

	for _, from := range core.SessionStates {
		for _, to := range core.SessionStates {
			id := reach(from)
			before, _ := s.GetSession(ctx, id)
			err := move(id, to)
			after, _ := s.GetSession(ctx, id)
			if from.CanTransition(to) {
				if err != nil {
					t.Errorf("%s -> %s: %v, want allowed", from, to, err)
				}
				if after.State != to {
					t.Errorf("%s -> %s: state now %s", from, to, after.State)
				}
				continue
			}
			ce := wantCode(t, err, core.CodeConflict)
			if ce.Details["from"] != string(from) || ce.Details["to"] != string(to) || ce.Details["id"] != id.String() {
				t.Errorf("%s -> %s: details %v", from, to, ce.Details)
			}
			sameSession(t, after, before)
		}
	}
}

func TestSessionWrongSetter(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	sess := testSession(1)
	mustCreate(t, s, sess)
	for _, st := range []core.SessionState{core.SessionExited, core.SessionStopped, core.SessionFailed, "", "gone"} {
		wantCode(t, s.UpdateSessionState(ctx, sess.ID, st), core.CodeInvalidArgument)
	}
	for _, e := range []SessionExit{
		{State: core.SessionRunning, EndedAt: time.Now()},
		{State: core.SessionLost, EndedAt: time.Now()},
		{State: "gone", EndedAt: time.Now()},
		{State: core.SessionStopped},                     // no end time
		{State: core.SessionExited, EndedAt: time.Now()}, // no exit code
		{State: core.SessionFailed, ExitCode: nil, EndedAt: time.Time{}},
	} {
		wantCode(t, s.SetSessionExit(ctx, sess.ID, e), core.CodeInvalidArgument)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	sameSession(t, got, sess)

	// failed and stopped may end without an exit code.
	if err := s.SetSessionExit(ctx, sess.ID, SessionExit{State: core.SessionFailed, EndedAt: sess.StartedAt}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession(ctx, sess.ID)
	if got.State != core.SessionFailed || got.ExitCode != nil || !got.EndedAt.Equal(sess.StartedAt) {
		t.Errorf("failed session = %+v", got)
	}
}

// TestTerminalSessionFrozenInSQL checks the schema itself refuses to change
// or delete a session in a terminal state, and to delete any session.
func TestTerminalSessionFrozenInSQL(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	for i, st := range []core.SessionState{core.SessionExited, core.SessionStopped, core.SessionFailed} {
		sess := testSession(byte(i + 1))
		mustCreate(t, s, sess)
		if err := s.UpdateSessionState(ctx, sess.ID, core.SessionRunning); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSessionExit(ctx, sess.ID, SessionExit{State: st, ExitCode: intp(0), EndedAt: sess.StartedAt}); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`UPDATE sessions SET state = 'running', ended_at = NULL, exit_code = NULL WHERE id = ?`,
			`UPDATE sessions SET state = 'lost', ended_at = NULL, exit_code = NULL WHERE id = ?`,
			`UPDATE sessions SET state = 'failed' WHERE id = ?`,
			`UPDATE sessions SET exit_code = 99 WHERE id = ?`,
			`UPDATE sessions SET argv = '["x"]' WHERE id = ?`,
			`DELETE FROM sessions WHERE id = ?`,
		} {
			if _, err := s.w.ExecContext(ctx, q, sess.ID.String()); err == nil {
				t.Errorf("%s: %q succeeded", st, q)
			}
		}
		got, err := s.GetSession(ctx, sess.ID)
		if err != nil || got.State != st {
			t.Errorf("%s: after raw SQL %+v, %v", st, got, err)
		}
	}
	live := testSession(9)
	mustCreate(t, s, live)
	if _, err := s.w.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, live.ID.String()); err == nil {
		t.Error("deleting a live session succeeded")
	}
}

// TestSessionSchemaChecks checks the table's CHECK constraints refuse rows
// the store would never write.
func TestSessionSchemaChecks(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	ins := `INSERT INTO sessions (id, node_id, mode, argv, state, started_at, ended_at, exit_code) VALUES (?, 'n', ?, ?, ?, 1, ?, ?)`
	// The same statement with a consistent row succeeds, so each refusal
	// below is the row's fault.
	for _, ok := range [][]any{
		{"ok1", "headless", `["a",""]`, "starting", nil, nil},
		{"ok2", "interactive", `["a"]`, "failed", 2, nil},
		{"ok3", "headless", `["a"]`, "exited", 2, 0},
	} {
		if _, err := s.w.ExecContext(ctx, ins, ok...); err != nil {
			t.Fatalf("consistent row %v refused: %v", ok, err)
		}
	}
	for i, c := range []struct {
		mode, argv, state string
		ended, code       any
	}{
		{"pty", `["a"]`, "starting", nil, nil},
		{"headless", `"a"`, "starting", nil, nil},
		{"headless", `[]`, "starting", nil, nil},
		{"headless", `not json`, "starting", nil, nil},
		{"headless", `["a"]`, "gone", nil, nil},
		{"headless", `["a"]`, "stopped", nil, nil}, // terminal without ended_at
		{"headless", `["a"]`, "running", 2, nil},   // live with ended_at
		{"headless", `["a"]`, "running", nil, 0},   // live with exit code
		{"headless", `["a"]`, "exited", 2, nil},    // exited without exit code
	} {
		if _, err := s.w.ExecContext(ctx, ins, itoa(i), c.mode, c.argv, c.state, c.ended, c.code); err == nil {
			t.Errorf("row %d %+v accepted", i, c)
		}
	}
}

// TestSessionColumnsHoldNoCredential fixes the sessions table's columns and
// checks none is named like a credential (OVERVIEW §3.3).
func TestSessionColumnsHoldNoCredential(t *testing.T) {
	s := mustOpen(t, stateDir(t))
	rows, err := s.r.Query(`SELECT name FROM pragma_table_info('sessions') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	want := []string{"id", "node_id", "profile", "repo_remote", "branch", "slice", "mode", "argv",
		"state", "started_at", "ended_at", "exit_code"}
	if !slices.Equal(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	cred := regexp.MustCompile(`(?i)token|secret|key|pass|cred|auth|env|cookie|bearer|hash`)
	for _, c := range cols {
		if cred.MatchString(c) {
			t.Errorf("column %q looks like it could hold a credential", c)
		}
	}
}

// TestMigrateV2KeepsNodesAddsSessions opens a registry at schema version 2:
// its nodes are kept and the sessions table appears.
func TestMigrateV2KeepsNodesAddsSessions(t *testing.T) {
	ctx := context.Background()
	dir := stateDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	migs, err := builtinMigrations()
	if err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, filepath.Join(dir, dbFileName))
	if err := migrate(ctx, db, migs[:2], false, nil); err != nil {
		t.Fatal(err)
	}
	a := testNode(1, "alpha")
	if _, err := db.Exec(`INSERT INTO nodes (`+nodeColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID.String(), a.Name, a.TailnetIdentity, a.TailnetName, a.OS, a.Arch, a.GaiaVersion, a.APIVersion,
		a.CreatedAt.UnixNano(), a.LastSeen.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := mustOpen(t, dir)
	got, err := s.GetNode(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	sameNode(t, got, a)
	if v, _ := userVersion(ctx, s.r); v != SchemaVersion() || v < 3 {
		t.Errorf("user_version = %d, want %d", v, SchemaVersion())
	}
	if len(backups(t, dir)) != 1 {
		t.Errorf("backups = %v, want one", backups(t, dir))
	}
	mustCreate(t, s, testSession(1))

	// Removing the node keeps its sessions (no foreign key).
	if err := s.RemoveNode(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, sessionID(1)); err != nil {
		t.Errorf("session after node removal: %v", err)
	}
}
