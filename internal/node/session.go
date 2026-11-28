package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/supervise"
)

// Scrollback bounds a Manager applies when its config names none.
const (
	DefaultMaxScrollback      int64 = 64 << 20
	DefaultMaxScrollbackTotal int64 = 1 << 30
)

// Files in a session's directory under the manager root.
const (
	recordFile    = "session.json"
	supervisorLog = "supervisor.log"
	supervisorDir = "sv"
	recordVersion = 1
)

// startWait is how long after its record was written a session with no
// supervisor state yet may still be starting. A variable for tests.
var startWait = 10 * time.Second

const (
	// readyTimeout bounds the wait for a new supervisor's ready line.
	readyTimeout = 30 * time.Second
	// statusTimeout bounds one status request to a supervisor.
	statusTimeout = 5 * time.Second
	// rewatchDelay is the pause before a watcher whose stream was cut, while
	// its supervisor still answers, streams again.
	rewatchDelay = 200 * time.Millisecond
)

// testHook, when set by a test, is called at named steps of a session's
// life, so a test can kill the daemon exactly there.
var testHook func(step string)

func hook(step string) {
	if testHook != nil {
		testHook(step)
	}
}

// Reporter is the host as a node's session manager reports to it.
// ReportSession returns nil once the host has recorded r. A *core.Error
// with code invalid_argument, not_found or conflict is a final refusal of
// r; any other error means the host may not have it, and r is sent again.
type Reporter interface {
	ReportSession(ctx context.Context, r core.SessionReport) error
}

// ManagerConfig configures a Manager. Zero limits and backoffs take their
// defaults.
type ManagerConfig struct {
	// Dir is the root of the session directories, created at 0700.
	Dir string
	// Supervisor is the command that runs `gaia supervise`, e.g. the gaia
	// binary and "supervise"; the manager adds --session-dir.
	Supervisor []string
	// SupervisorEnv is the supervisor's whole environment. The agent's own
	// comes from its StartSpec.
	SupervisorEnv []string
	// Reporter receives every state change. Nil keeps reports queued.
	Reporter Reporter
	// MaxScrollback bounds each session's scrollback on disk, and
	// MaxScrollbackTotal the sum over every session; it is at least
	// MaxScrollback.
	MaxScrollback, MaxScrollbackTotal int64
	// MinBackoff and MaxBackoff bound the wait between report attempts
	// while the host is unreachable.
	MinBackoff, MaxBackoff time.Duration
	// Logger receives progress and failures. Default: discard.
	Logger *slog.Logger
}

// StartSpec is a session to start. Env and Secrets may hold secret values:
// they reach the supervisor on its stdin only, and a StartSpec formats
// without them.
type StartSpec struct {
	// ID is the session's id, chosen by the host.
	ID   core.SessionID
	Mode core.SessionMode
	Argv []string
	// WorkDir is the absolute directory the agent starts in.
	WorkDir string
	// Env is the agent's whole environment, KEY=VALUE.
	Env []string
	// Secrets are values redacted from the scrollback.
	Secrets []string
	// Rows and Cols are the initial terminal size of an interactive session.
	Rows, Cols uint16
	// Grace is how long a stop waits after SIGTERM; 0 means the default.
	Grace time.Duration
}

// Format writes the session id and mode only.
func (s StartSpec) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "node.StartSpec{id=%s mode=%s}", s.ID, s.Mode)
}

// SessionInfo is a session as the node knows it. Argv is redacted of the
// session's secrets. EndedAt is nil until the session is terminal;
// ExitCode is the agent's exit code, 128+N for death by signal N, when
// known; Signal names that signal. PID is the agent's, SupervisorPID its
// supervisor's; either is 0 while unknown.
type SessionInfo struct {
	ID            core.SessionID
	Mode          core.SessionMode
	Argv          []string
	State         core.SessionState
	PID           int
	SupervisorPID int
	StartedAt     time.Time
	EndedAt       *time.Time
	ExitCode      *int
	Signal        string
}

// record is a session's session.json: everything the manager knows about
// a session, so a new daemon continues where the last one stopped.
type record struct {
	Version       int               `json:"version"`
	SessionID     core.SessionID    `json:"session_id"`
	CreatedAt     time.Time         `json:"created_at"`
	Mode          core.SessionMode  `json:"mode"`
	Argv          []string          `json:"argv"`
	State         core.SessionState `json:"state"`
	StopRequested bool              `json:"stop_requested"`
	PID           int               `json:"pid"`
	SupervisorPID int               `json:"supervisor_pid"`
	StartedAt     time.Time         `json:"started_at"`
	EndedAt       *time.Time        `json:"ended_at"`
	ExitCode      *int              `json:"exit_code"`
	Signal        string            `json:"signal"`
	// Seq is the seq of the last report queued.
	Seq uint64 `json:"seq"`
	// Pending are the reports the host has not acknowledged, oldest first.
	Pending []core.SessionReport `json:"pending"`
	// ScrollbackDropped is set once the scrollback was deleted to keep the
	// total bound.
	ScrollbackDropped bool `json:"scrollback_dropped"`
}

func (r *record) info() SessionInfo {
	return SessionInfo{
		ID: r.SessionID, Mode: r.Mode, Argv: slices.Clone(r.Argv), State: r.State,
		PID: r.PID, SupervisorPID: r.SupervisorPID, StartedAt: r.StartedAt,
		EndedAt: r.EndedAt, ExitCode: r.ExitCode, Signal: r.Signal,
	}
}

// Manager runs a node's sessions: it starts each under its own detached
// supervisor (ADR-0008), keeps every session's record on disk, finds the
// supervisors again after a restart, reports state changes to the host and
// bounds the scrollback. Safe for concurrent use.
type Manager struct {
	cfg ManagerConfig
	log *slog.Logger

	mu       sync.Mutex
	recs     map[core.SessionID]*record
	watching map[core.SessionID]bool

	kick   chan struct{} // wakes Run when a report is queued
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// OpenManager opens the manager root, finds every session in it and brings
// each up to date: a session whose supervisor answers is running and
// watched again, one whose supervisor recorded the end is terminal, and one
// whose supervisor is gone without recording an end is lost (ADR-0008 rule
// 4). Every session that is not terminal queues a report of its state, so
// the host learns of it again. Close releases the manager.
func OpenManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if len(cfg.Supervisor) == 0 || cfg.Supervisor[0] == "" {
		return nil, core.NewError(core.CodeInvalidArgument, "session manager: no supervisor command")
	}
	if !filepath.IsAbs(cfg.Dir) {
		return nil, core.NewError(core.CodeInvalidArgument, fmt.Sprintf("session manager: directory %q is not absolute", cfg.Dir))
	}
	if cfg.MaxScrollback == 0 {
		cfg.MaxScrollback = DefaultMaxScrollback
	}
	if cfg.MaxScrollbackTotal == 0 {
		cfg.MaxScrollbackTotal = max(DefaultMaxScrollbackTotal, cfg.MaxScrollback)
	}
	if cfg.MaxScrollback < 2 || cfg.MaxScrollbackTotal < cfg.MaxScrollback {
		return nil, core.NewError(core.CodeInvalidArgument, fmt.Sprintf(
			"session manager: scrollback bounds %d per session, %d in total: want at least 2 per session and a total no smaller",
			cfg.MaxScrollback, cfg.MaxScrollbackTotal))
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(cfg.MinBackoff, DefaultMaxBackoff)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := ensurePrivateDir(cfg.Dir); err != nil {
		return nil, err
	}
	mctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg: cfg, log: log,
		recs:     map[core.SessionID]*record{},
		watching: map[core.SessionID]bool{},
		kick:     make(chan struct{}, 1),
		ctx:      mctx, cancel: cancel,
	}
	if err := m.load(); err != nil {
		cancel()
		return nil, err
	}
	for _, id := range m.ids() {
		m.reconcile(ctx, id)
		m.mu.Lock()
		rec := m.recs[id]
		live := rec != nil && !rec.State.Terminal()
		stop := live && rec.StopRequested && rec.State == core.SessionRunning
		if live {
			m.reregister(rec)
		}
		m.mu.Unlock()
		if live {
			m.watch(id)
		}
		if stop {
			// The last daemon recorded a stop it may not have sent.
			m.wg.Go(func() {
				if _, err := m.Stop(m.ctx, id); err != nil && m.ctx.Err() == nil {
					m.log.Warn("completing a requested stop failed", "session", id.String(), "err", err)
				}
			})
		}
	}
	m.mu.Lock()
	if err := m.makeRoom(0); err != nil {
		log.Warn("scrollback over its total bound with only live sessions left", "err", err)
	}
	m.mu.Unlock()
	return m, nil
}

// load reads every session record under the root.
func (m *Manager) load() error {
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		id, err := core.ParseSessionID(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		dir := filepath.Join(m.cfg.Dir, e.Name())
		rec, err := readRecord(dir)
		if errors.Is(err, fs.ErrNotExist) {
			// A start that died before writing the record: nothing ran.
			if rmErr := os.Remove(dir); rmErr != nil {
				m.log.Warn("session directory without a record", "session", id.String(), "err", rmErr)
			}
			continue
		}
		if err != nil {
			m.log.Warn("unreadable session record; skipped", "session", id.String(), "err", err)
			continue
		}
		if rec.SessionID != id {
			m.log.Warn("session record names another session; skipped", "session", id.String())
			continue
		}
		m.recs[id] = rec
	}
	return nil
}

// ids returns the ids of every session, oldest first.
func (m *Manager) ids() []core.SessionID {
	m.mu.Lock()
	defer m.mu.Unlock()
	recs := make([]*record, 0, len(m.recs))
	for _, r := range m.recs {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool {
		if !recs[i].CreatedAt.Equal(recs[j].CreatedAt) {
			return recs[i].CreatedAt.Before(recs[j].CreatedAt)
		}
		return recs[i].SessionID.String() < recs[j].SessionID.String()
	})
	ids := make([]core.SessionID, len(recs))
	for i, r := range recs {
		ids[i] = r.SessionID
	}
	return ids
}

// Close stops watching the sessions and returns once every watcher has.
// Sessions keep running under their supervisors.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

// SessionDir is the supervisor directory of the session with id, for
// supervise.NewClient.
func (m *Manager) SessionDir(id core.SessionID) string {
	return filepath.Join(m.cfg.Dir, id.String(), supervisorDir)
}

func (m *Manager) dir(id core.SessionID) string { return filepath.Join(m.cfg.Dir, id.String()) }

// List returns every session the node keeps, oldest first.
func (m *Manager) List() []SessionInfo {
	ids := m.ids()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(ids))
	for _, id := range ids {
		if r := m.recs[id]; r != nil {
			out = append(out, r.info())
		}
	}
	return out
}

// Get returns the session with id, or a core.CodeNotFound error.
func (m *Manager) Get(id core.SessionID) (SessionInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.recs[id]
	if r == nil {
		return SessionInfo{}, sessionNotFound(id)
	}
	return r.info(), nil
}

func sessionNotFound(id core.SessionID) *core.Error {
	return &core.Error{
		Code:    core.CodeNotFound,
		Message: fmt.Sprintf("session %s not found on this node", id),
		Details: map[string]string{"id": id.String()},
	}
}

// Start starts the session spec describes under a new detached supervisor
// and returns it running. A supervisor that refuses the session leaves it
// failed and returns the refusal. An id the node already has is a
// core.CodeConflict error; a start the total scrollback bound has no room
// for is core.CodeUnavailable.
func (m *Manager) Start(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	sspec := supervise.Spec{
		SessionID:     spec.ID,
		Mode:          spec.Mode,
		Argv:          spec.Argv,
		WorkDir:       spec.WorkDir,
		Env:           append([]string{}, spec.Env...),
		Secrets:       spec.Secrets,
		Rows:          spec.Rows,
		Cols:          spec.Cols,
		GraceMS:       spec.Grace.Milliseconds(),
		MaxScrollback: m.cfg.MaxScrollback,
	}
	if err := sspec.Validate(); err != nil {
		return SessionInfo{}, err
	}
	payload, err := json.Marshal(sspec)
	if err != nil {
		return SessionInfo{}, err
	}
	now := time.Now().UTC().Round(0)
	rec := &record{
		Version: recordVersion, SessionID: spec.ID, CreatedAt: now, Mode: spec.Mode,
		Argv: redactArgv(spec.Argv, spec.Secrets), State: core.SessionStarting, StartedAt: now,
	}
	dir := m.dir(spec.ID)

	m.mu.Lock()
	if _, ok := m.recs[spec.ID]; ok {
		m.mu.Unlock()
		return SessionInfo{}, &core.Error{
			Code:    core.CodeConflict,
			Message: fmt.Sprintf("session %s already exists on this node", spec.ID),
			Details: map[string]string{"id": spec.ID.String()},
		}
	}
	if err := m.makeRoom(1); err != nil {
		m.mu.Unlock()
		return SessionInfo{}, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		m.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("creating the session directory: %w", err)
	}
	if err := writeRecord(dir, rec); err != nil {
		m.mu.Unlock()
		_ = os.RemoveAll(dir)
		return SessionInfo{}, err
	}
	m.recs[spec.ID] = rec
	m.mu.Unlock()
	hook("record-written")

	pid, ready, err := m.spawn(dir, payload)
	if err == nil && !ready.OK {
		err = ready.Error
		if ready.Error == nil {
			err = core.NewError(core.CodeInternal, "the supervisor refused the session without a reason")
		}
	}
	if err != nil {
		end := time.Now().UTC().Round(0)
		m.mu.Lock()
		rec.SupervisorPID = pid
		m.transition(rec, core.SessionFailed, nil, "", &end)
		m.mu.Unlock()
		return m.mustGet(spec.ID), err
	}
	hook("ready")
	m.mu.Lock()
	rec.SupervisorPID = pid
	rec.PID = ready.PID
	if st, err := supervise.ReadState(m.SessionDir(spec.ID)); err == nil {
		rec.StartedAt = st.StartedAt
	}
	m.transition(rec, core.SessionRunning, nil, "", nil)
	m.mu.Unlock()
	m.watch(spec.ID)
	return m.mustGet(spec.ID), nil
}

func (m *Manager) mustGet(id core.SessionID) SessionInfo {
	info, _ := m.Get(id)
	return info
}

// spawn starts a detached supervisor for the session in dir with the spec
// payload on its stdin and returns its pid and ready line. The supervisor
// leads a new session and process group, so it outlives the daemon.
func (m *Manager) spawn(dir string, payload []byte) (int, supervise.Response, error) {
	logf, err := os.OpenFile(filepath.Join(dir, supervisorLog), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, supervise.Response{}, err
	}
	defer logf.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return 0, supervise.Response{}, err
	}
	defer r.Close()
	inR, inW, err := os.Pipe()
	if err != nil {
		_ = w.Close()
		return 0, supervise.Response{}, err
	}
	defer inW.Close()
	args := append(slices.Clone(m.cfg.Supervisor[1:]), "--session-dir", filepath.Join(dir, supervisorDir))
	cmd := exec.Command(m.cfg.Supervisor[0], args...)
	cmd.Env = append([]string{}, m.cfg.SupervisorEnv...) // never the daemon's own
	cmd.Stdin = inR
	cmd.Stdout = w
	cmd.Stderr = logf
	cmd.Dir = "/"
	detach(cmd)
	err = cmd.Start()
	_ = w.Close()
	_ = inR.Close()
	if err != nil {
		return 0, supervise.Response{}, fmt.Errorf("starting the supervisor: %w", err)
	}
	pid := cmd.Process.Pid
	// Reap it while this daemon lives; after, it is init's.
	go func() { _ = cmd.Wait() }()
	// The spec is written in full before anything else: a daemon that dies
	// before it is delivered leaves a supervisor that refuses, and no agent.
	_ = inW.SetWriteDeadline(time.Now().Add(readyTimeout))
	_, werr := inW.Write(payload)
	if cerr := inW.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = cmd.Process.Kill()
		return pid, supervise.Response{}, fmt.Errorf("sending the spec to the supervisor: %w", werr)
	}
	hook("spawned")

	type result struct {
		line []byte
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadBytes('\n')
		got <- result{line, err}
	}()
	var res result
	select {
	case res = <-got:
	case <-time.After(readyTimeout):
		_ = cmd.Process.Kill()
		return pid, supervise.Response{}, core.NewError(core.CodeInternal,
			fmt.Sprintf("the supervisor wrote no ready line within %s", readyTimeout))
	}
	if res.err != nil {
		return pid, supervise.Response{}, core.NewError(core.CodeInternal,
			"the supervisor exited without a ready line; see "+filepath.Join(dir, supervisorLog))
	}
	var ready supervise.Response
	if err := json.Unmarshal(res.line, &ready); err != nil {
		return pid, supervise.Response{}, fmt.Errorf("decoding the supervisor's ready line: %w", err)
	}
	return pid, ready, nil
}

// Stop stops the session with id and returns it once it has ended. A
// session already terminal is returned as it is. A session whose
// supervisor cannot be reached is returned lost with the error.
func (m *Manager) Stop(ctx context.Context, id core.SessionID) (SessionInfo, error) {
	if _, err := m.Get(id); err != nil {
		return SessionInfo{}, err
	}
	m.reconcile(ctx, id)
	m.mu.Lock()
	rec := m.recs[id]
	if rec == nil || rec.State.Terminal() {
		m.mu.Unlock()
		return m.Get(id)
	}
	if !rec.StopRequested {
		rec.StopRequested = true
		if err := writeRecord(m.dir(id), rec); err != nil {
			rec.StopRequested = false
			m.mu.Unlock()
			return rec.info(), err
		}
	}
	m.mu.Unlock()
	hook("stop-recorded")
	_, stopErr := supervise.NewClient(m.SessionDir(id)).Stop(ctx)
	m.reconcile(ctx, id)
	info, err := m.Get(id)
	if err != nil {
		return info, err
	}
	if !info.State.Terminal() && stopErr != nil {
		return info, stopErr
	}
	return info, nil
}

// observation is what reconcile learned of a supervisor.
type observation struct {
	state     *core.SupervisorState // its state, nil when it has none
	alive     bool                  // it answered
	gone      bool                  // it is not running
	undecided bool                  // it may be running but did not answer in time
}

// observe looks at the supervisor of a session whose record was written at
// created.
func (m *Manager) observe(ctx context.Context, id core.SessionID, created time.Time) observation {
	dir := m.SessionDir(id)
	st, err := supervise.ReadState(dir)
	for errors.Is(err, core.NewError(core.CodeNotFound, "")) && time.Since(created) < startWait {
		// The supervisor may still be starting: it writes its state
		// before its ready line.
		if !sleep(ctx, 50*time.Millisecond) {
			return observation{undecided: true}
		}
		st, err = supervise.ReadState(dir)
	}
	if err != nil {
		if !errors.Is(err, core.NewError(core.CodeNotFound, "")) {
			m.log.Warn("unreadable supervisor state", "session", id.String(), "err", err)
		}
		return observation{gone: true}
	}
	if st.Ended() {
		return observation{state: &st}
	}
	sctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	status, err := supervise.NewClient(dir).Status(sctx)
	if err == nil {
		return observation{state: &status.State, alive: true}
	}
	if ctx.Err() != nil || !errors.Is(err, core.NewError(core.CodeUnavailable, "")) {
		// The caller gave up, which says nothing of the supervisor.
		return observation{undecided: true}
	}
	// Unreachable: it may have just recorded the end and removed its socket.
	if again, err := supervise.ReadState(dir); err == nil && again.Ended() {
		return observation{state: &again}
	}
	return observation{state: &st, gone: true}
}

// reconcile brings the session's record up to date with its supervisor.
func (m *Manager) reconcile(ctx context.Context, id core.SessionID) {
	m.mu.Lock()
	rec := m.recs[id]
	if rec == nil || rec.State.Terminal() {
		m.mu.Unlock()
		return
	}
	created := rec.CreatedAt
	m.mu.Unlock()

	obs := m.observe(ctx, id, created)
	if obs.undecided {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec.State.Terminal() {
		return
	}
	if st := obs.state; st != nil {
		rec.PID, rec.StartedAt = st.PID, st.StartedAt
	}
	switch {
	case obs.state != nil && obs.state.Ended():
		to, code := classify(rec.StopRequested, *obs.state)
		if !rec.State.CanTransition(to) && rec.State.CanTransition(core.SessionRunning) {
			// A daemon that died after the ready line and before
			// recording running: the agent did run, so it passes
			// through running to its end.
			m.transition(rec, core.SessionRunning, nil, "", nil)
		}
		m.transition(rec, to, code, obs.state.Signal, obs.state.EndedAt)
	case obs.alive:
		m.transition(rec, core.SessionRunning, nil, "", nil)
	case obs.state != nil:
		m.transition(rec, core.SessionLost, nil, "", nil)
	case rec.State == core.SessionStarting:
		// No supervisor state, long after the start: it never ran.
		end := time.Now().UTC().Round(0)
		m.transition(rec, core.SessionFailed, nil, "", &end)
	default:
		m.transition(rec, core.SessionLost, nil, "", nil)
	}
}

// classify maps an ended supervisor state to the session's terminal state
// and exit code: stopped when a stop was requested, else exited; the
// agent's exit code, or 128+N for death by signal N (launcher contract
// v1).
func classify(stopRequested bool, st core.SupervisorState) (core.SessionState, *int) {
	to := core.SessionExited
	if stopRequested {
		to = core.SessionStopped
	}
	code := st.ExitCode
	if code == nil {
		if n, ok := signalNumber(st.Signal); ok {
			c := 128 + n
			code = &c
		}
	}
	if code == nil && to == core.SessionExited {
		return core.SessionFailed, nil
	}
	return to, code
}

// transition moves rec to state to, if the state table allows it, records
// the outcome, queues a report and persists the record. Callers hold mu.
func (m *Manager) transition(rec *record, to core.SessionState, code *int, signal string, ended *time.Time) {
	if rec.State == to {
		return
	}
	if !rec.State.CanTransition(to) {
		m.log.Warn("session state change refused", "session", rec.SessionID.String(), "from", string(rec.State), "to", string(to))
		return
	}
	rec.State = to
	if to.Terminal() {
		rec.ExitCode, rec.Signal, rec.EndedAt = code, signal, ended
	}
	m.queue(rec)
	if err := writeRecord(m.dir(rec.SessionID), rec); err != nil {
		m.log.Error("cannot persist the session record", "session", rec.SessionID.String(), "err", err)
	}
	m.log.Info("session state", "session", rec.SessionID.String(), "state", string(to))
	if to.Terminal() {
		hook("ended")
		if err := m.makeRoom(0); err != nil {
			m.log.Warn("scrollback over its total bound with only live sessions left", "err", err)
		}
	}
}

// queue appends a report of rec's current state and wakes Run. Callers
// hold mu and persist rec.
func (m *Manager) queue(rec *record) {
	rec.Seq++
	r := core.SessionReport{SessionID: rec.SessionID, Seq: rec.Seq, State: rec.State}
	if rec.State.Terminal() {
		r.EndedAt = rec.EndedAt
		if rec.State == core.SessionExited || rec.State == core.SessionStopped {
			r.ExitCode = rec.ExitCode
		}
	}
	rec.Pending = append(rec.Pending, r)
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// reregister queues a report of a live session's current state unless the
// last report queued already says it. Callers hold mu.
func (m *Manager) reregister(rec *record) {
	if n := len(rec.Pending); n > 0 && rec.Pending[n-1].State == rec.State {
		return
	}
	m.queue(rec)
	if err := writeRecord(m.dir(rec.SessionID), rec); err != nil {
		m.log.Error("cannot persist the session record", "session", rec.SessionID.String(), "err", err)
	}
}

// watch follows a live session until it is no longer running: it streams
// the session's output to nowhere, so the end of the stream says at once
// that the session ended or its supervisor died, and reconciles.
func (m *Manager) watch(id core.SessionID) {
	m.mu.Lock()
	if m.watching[id] || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	m.watching[id] = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.watching, id)
			m.mu.Unlock()
		}()
		c := supervise.NewClient(m.SessionDir(id))
		for {
			sctx, cancel := context.WithTimeout(m.ctx, statusTimeout)
			st, err := c.Status(sctx)
			cancel()
			if err == nil && !st.State.Ended() {
				_, _ = c.Stream(m.ctx, st.Size, io.Discard)
			}
			if m.ctx.Err() != nil {
				return
			}
			m.reconcile(m.ctx, id)
			m.mu.Lock()
			rec := m.recs[id]
			running := rec != nil && rec.State == core.SessionRunning
			m.mu.Unlock()
			if !running {
				return
			}
			if !sleep(m.ctx, rewatchDelay) {
				return
			}
		}
	}()
}

// Run sends queued reports to the Reporter, oldest first per session,
// until ctx is done. While the host is unreachable it retries with
// backoff; a final refusal drops the report.
func (m *Manager) Run(ctx context.Context) {
	backoff := m.cfg.MinBackoff
	for {
		if m.flush(ctx) {
			backoff = m.cfg.MinBackoff
			select {
			case <-ctx.Done():
				return
			case <-m.kick:
			}
			continue
		}
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, m.cfg.MaxBackoff)
	}
}

// flush sends every queued report and reports whether all were delivered.
func (m *Manager) flush(ctx context.Context) bool {
	if m.cfg.Reporter == nil {
		return true
	}
	for _, id := range m.ids() {
		for {
			m.mu.Lock()
			rec := m.recs[id]
			if rec == nil || len(rec.Pending) == 0 {
				m.mu.Unlock()
				break
			}
			r := rec.Pending[0]
			m.mu.Unlock()
			err := m.cfg.Reporter.ReportSession(ctx, r)
			if ctx.Err() != nil {
				return false
			}
			if err != nil && !finalRefusal(err) {
				m.log.Warn("reporting a session to the host failed; will retry", "session", id.String(), "seq", r.Seq, "err", err)
				return false
			}
			if err != nil {
				m.log.Warn("the host refused a session report; dropped", "session", id.String(), "seq", r.Seq, "state", string(r.State), "err", err)
			}
			hook("reported")
			m.mu.Lock()
			if len(rec.Pending) > 0 && rec.Pending[0].Seq == r.Seq {
				rec.Pending = rec.Pending[1:]
				if err := writeRecord(m.dir(id), rec); err != nil {
					m.log.Error("cannot persist the session record", "session", id.String(), "err", err)
				}
			}
			m.removeIfDone(rec)
			m.mu.Unlock()
		}
	}
	return true
}

func finalRefusal(err error) bool {
	var ce *core.Error
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Code == core.CodeInvalidArgument || ce.Code == core.CodeNotFound || ce.Code == core.CodeConflict
}

// makeRoom keeps the total scrollback bound with room for extra more live
// sessions: every live session may grow to MaxScrollback, so the closed
// sessions' scrollback is deleted, oldest first, until it fits. It is a
// core.CodeUnavailable error when even with every closed session dropped
// there is no room. Callers hold mu.
func (m *Manager) makeRoom(extra int) error {
	type closed struct {
		rec  *record
		at   time.Time
		size int64
	}
	live := extra
	var cs []closed
	var used int64
	for _, rec := range m.recs {
		switch {
		case rec.State == core.SessionStarting || rec.State == core.SessionRunning:
			live++
		case rec.ScrollbackDropped:
		default:
			at := rec.CreatedAt
			if rec.EndedAt != nil {
				at = *rec.EndedAt
			}
			size := scrollbackSize(m.SessionDir(rec.SessionID))
			cs = append(cs, closed{rec, at, size})
			used += size
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if !cs[i].at.Equal(cs[j].at) {
			return cs[i].at.Before(cs[j].at)
		}
		return cs[i].rec.SessionID.String() < cs[j].rec.SessionID.String()
	})
	need := int64(live) * m.cfg.MaxScrollback
	for len(cs) > 0 && used+need > m.cfg.MaxScrollbackTotal {
		m.dropScrollback(cs[0].rec)
		used -= cs[0].size
		cs = cs[1:]
	}
	if used+need > m.cfg.MaxScrollbackTotal {
		return &core.Error{
			Code: core.CodeUnavailable,
			Message: fmt.Sprintf("no room for another session's scrollback: %d live sessions may each keep %d bytes, %d in total",
				live, m.cfg.MaxScrollback, m.cfg.MaxScrollbackTotal),
			Details: map[string]string{"live": fmt.Sprint(live)},
		}
	}
	return nil
}

// dropScrollback deletes a closed session's scrollback, and the whole
// session once the host has every report of it. Callers hold mu.
func (m *Manager) dropScrollback(rec *record) {
	sv := m.SessionDir(rec.SessionID)
	for _, f := range []string{supervise.ScrollbackFile, supervise.ScrollbackPrevFile} {
		if err := os.Remove(filepath.Join(sv, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			m.log.Warn("cannot delete scrollback", "session", rec.SessionID.String(), "err", err)
		}
	}
	rec.ScrollbackDropped = true
	if err := writeRecord(m.dir(rec.SessionID), rec); err != nil {
		m.log.Error("cannot persist the session record", "session", rec.SessionID.String(), "err", err)
	}
	m.log.Info("scrollback dropped to keep the total bound", "session", rec.SessionID.String())
	m.removeIfDone(rec)
}

// removeIfDone forgets a session whose scrollback was dropped, that is not
// live, and whose reports the host has all. Callers hold mu.
func (m *Manager) removeIfDone(rec *record) {
	if !rec.ScrollbackDropped || len(rec.Pending) > 0 || !(rec.State.Terminal() || rec.State == core.SessionLost) {
		return
	}
	if err := os.RemoveAll(m.dir(rec.SessionID)); err != nil {
		m.log.Warn("cannot remove the session directory", "session", rec.SessionID.String(), "err", err)
		return
	}
	delete(m.recs, rec.SessionID)
}

// scrollbackSize is the bytes of scrollback in supervisor directory sv.
func scrollbackSize(sv string) int64 {
	var n int64
	for _, f := range []string{supervise.ScrollbackFile, supervise.ScrollbackPrevFile} {
		if fi, err := os.Lstat(filepath.Join(sv, f)); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// redactArgv returns argv with every secret replaced, as the supervisor
// records it.
func redactArgv(argv, secrets []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		r := supervise.NewRedactor(secrets)
		out[i] = string(append(r.Write([]byte(a)), r.Flush()...))
	}
	return out
}

// ensurePrivateDir creates dir at 0700 if missing and refuses it unless it
// is a real directory with no group or other access.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("session directory root: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("session directory root: %w", err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("session directory root %s is a symbolic link", dir)
	case !fi.IsDir():
		return fmt.Errorf("session directory root %s is not a directory", dir)
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("session directory root %s has mode %s, accessible by group or others", dir, fi.Mode().Perm())
	}
	return nil
}

// readRecord reads dir's session record.
func readRecord(dir string) (*record, error) {
	b, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		return nil, err
	}
	var rec record
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", filepath.Join(dir, recordFile), err)
	}
	if rec.Version != recordVersion || rec.SessionID.IsZero() || !rec.State.Valid() {
		return nil, fmt.Errorf("%s: not a version %d session record", filepath.Join(dir, recordFile), recordVersion)
	}
	return &rec, nil
}

// writeRecord replaces dir's session record atomically: a 0600 temporary
// file, synced, renamed over the old one, and the directory synced.
func writeRecord(dir string, rec *record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".session-*.tmp") // 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, recordFile)); err != nil {
		return err
	}
	ok = true
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
