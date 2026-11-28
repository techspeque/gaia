//go:build linux || darwin

package node

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/supervise"
)

// The test binary is also a session supervisor and a node "daemon"
// running a Manager, chosen by GAIA_TEST_ROLE.
const roleEnv = "GAIA_TEST_ROLE"

var fakeAgent string // path of the built testdata/fakeagent

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case "supervise":
		os.Exit(supervise.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	case "daemon":
		os.Exit(runTestDaemon())
	}
	dir, err := os.MkdirTemp("", "fakeagent")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeAgent = filepath.Join(dir, "fakeagent")
	build := exec.Command("go", "build", "-o", fakeAgent, "github.com/techspeque/gaia/testdata/fakeagent")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the fake agent: %v\n%s", err, out)
		os.Exit(1)
	}
	startWait = testStartWait
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// testStartWait shortens startWait in tests and in test daemons.
const testStartWait = 1500 * time.Millisecond

func baseConfig(root string, r Reporter) ManagerConfig {
	return ManagerConfig{
		Dir:           root,
		Supervisor:    []string{os.Args[0]},
		SupervisorEnv: []string{roleEnv + "=supervise"},
		Reporter:      r,
		MinBackoff:    20 * time.Millisecond,
		MaxBackoff:    100 * time.Millisecond,
	}
}

// newRoot returns a manager root short enough for supervisor sockets. On
// cleanup every supervisor left in it is stopped.
func newRoot(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "gn")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "s")
	t.Cleanup(func() {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			sv := filepath.Join(root, e.Name(), supervisorDir)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, _ = supervise.NewClient(sv).Stop(ctx)
			cancel()
		}
		_ = os.RemoveAll(base)
	})
	return root
}

func newID(t *testing.T) core.SessionID {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return core.NewSessionID(b)
}

func newStart(t *testing.T, steps ...string) StartSpec {
	t.Helper()
	return StartSpec{
		ID:      newID(t),
		Mode:    core.SessionHeadless,
		Argv:    append([]string{fakeAgent}, steps...),
		WorkDir: t.TempDir(),
		Env:     []string{"PATH=" + os.Getenv("PATH")},
	}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// fakeHost is an in-process Reporter.
type fakeHost struct {
	mu      sync.Mutex
	reports []core.SessionReport
	down    bool
	refuse  func(core.SessionReport) error
}

func (h *fakeHost) ReportSession(_ context.Context, r core.SessionReport) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return core.NewError(core.CodeUnavailable, "host down")
	}
	if h.refuse != nil {
		if err := h.refuse(r); err != nil {
			return err
		}
	}
	h.reports = append(h.reports, r)
	return nil
}

func (h *fakeHost) of(id core.SessionID) []core.SessionReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []core.SessionReport
	for _, r := range h.reports {
		if r.SessionID == id {
			out = append(out, r)
		}
	}
	return out
}

// openManager opens a manager on root reporting to h and runs it until
// the test ends.
func openManager(t *testing.T, cfg ManagerConfig) *Manager {
	t.Helper()
	m, err := OpenManager(ctxT(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; m.Close() })
	return m
}

// waitInfo polls until ok(info) holds.
func waitInfo(t *testing.T, get func() (SessionInfo, error), ok func(SessionInfo) bool, what string) SessionInfo {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := get()
		if err == nil && ok(info) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: last %+v, %v", what, info, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitState(t *testing.T, m *Manager, id core.SessionID, want core.SessionState) SessionInfo {
	t.Helper()
	return waitInfo(t, func() (SessionInfo, error) { return m.Get(id) },
		func(i SessionInfo) bool { return i.State == want }, string(want))
}

func waitReports(t *testing.T, h *fakeHost, id core.SessionID, ok func([]core.SessionReport) bool) []core.SessionReport {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		rs := h.of(id)
		if ok(rs) {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("reports %+v", rs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func lastIs(state core.SessionState) func([]core.SessionReport) bool {
	return func(rs []core.SessionReport) bool { return len(rs) > 0 && rs[len(rs)-1].State == state }
}

func wantCode(t *testing.T, err error, code core.ErrorCode) {
	t.Helper()
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("error %v, want code %s", err, code)
	}
}

func wantExit(t *testing.T, info SessionInfo, state core.SessionState, code int) {
	t.Helper()
	if info.State != state || info.ExitCode == nil || *info.ExitCode != code || info.EndedAt == nil {
		t.Fatalf("session %+v, want %s with exit code %d", info, state, code)
	}
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// checkReportOrder fails unless seqs never go down and reports of the same
// seq are equal (a resend).
func checkReportOrder(t *testing.T, rs []core.SessionReport) {
	t.Helper()
	for i := 1; i < len(rs); i++ {
		a, b := rs[i-1], rs[i]
		if b.Seq < a.Seq || (b.Seq == a.Seq && a.State != b.State) {
			t.Fatalf("reports out of order: %+v", rs)
		}
	}
	for _, r := range rs {
		if err := r.Validate(); err != nil {
			t.Fatalf("report %+v: %v", r, err)
		}
	}
}

// ----- the test daemon: a Manager in its own process -----

// daemonRequest is one line a test sends a test daemon.
type daemonRequest struct {
	Op    string         `json:"op"`
	ID    core.SessionID `json:"id,omitzero"`
	Steps []string       `json:"steps,omitempty"`
	Work  string         `json:"work,omitempty"`
}

type daemonResponse struct {
	Info  *SessionInfo  `json:"info,omitempty"`
	List  []SessionInfo `json:"list,omitempty"`
	Error string        `json:"error,omitempty"`
}

// fileHost is the test daemon's Reporter: it appends each report to a file
// as a JSON line, and is down while a marker file exists.
type fileHost struct{ path, down string }

func (h fileHost) ReportSession(_ context.Context, r core.SessionReport) error {
	if _, err := os.Stat(h.down); err == nil {
		return core.NewError(core.CodeUnavailable, "host down")
	}
	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, err = f.Write(append(b, '\n'))
	return err
}

// runTestDaemon opens a Manager on $GAIA_TEST_ROOT, reporting to
// $GAIA_TEST_HOST, and serves requests from stdin until stdin closes. With
// $GAIA_TEST_DIE_AT it kills itself with SIGKILL at that step.
func runTestDaemon() int {
	startWait = testStartWait
	fakeAgent = os.Getenv("GAIA_TEST_AGENT")
	if step := os.Getenv("GAIA_TEST_DIE_AT"); step != "" {
		testHook = func(s string) {
			if s == step {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				time.Sleep(time.Hour)
			}
		}
	}
	cfg := baseConfig(os.Getenv("GAIA_TEST_ROOT"), fileHost{os.Getenv("GAIA_TEST_HOST"), os.Getenv("GAIA_TEST_DOWN")})
	ctx := context.Background()
	m, err := OpenManager(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	go m.Run(ctx)
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(daemonResponse{})
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req daemonRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			fmt.Fprintf(os.Stderr, "decoding the request %q: %v\n", sc.Bytes(), err)
			return 1
		}
		var res daemonResponse
		var info SessionInfo
		switch req.Op {
		case "start":
			info, err = m.Start(ctx, StartSpec{
				ID: req.ID, Mode: core.SessionHeadless, Argv: append([]string{fakeAgent}, req.Steps...),
				WorkDir: req.Work, Env: []string{"PATH=" + os.Getenv("PATH")},
			})
			res.Info = &info
		case "stop":
			info, err = m.Stop(ctx, req.ID)
			res.Info = &info
		case "list":
			res.List, err = m.List(), nil
		}
		if err != nil {
			res.Error = err.Error()
		}
		if res.Info != nil && res.Info.ID.IsZero() {
			res.Info = nil // a zero id does not encode
		}
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(os.Stderr, "encoding the response:", err)
			return 1
		}
	}
	return 0
}

// testDaemon is a test daemon process.
type testDaemon struct {
	t     *testing.T
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
	done  chan struct{}
	stder *strings.Builder
}

// env is the environment shared by a test's daemons.
type daemonEnv struct {
	root, host, down string
}

func newDaemonEnv(t *testing.T) daemonEnv {
	root := newRoot(t)
	files := t.TempDir()
	return daemonEnv{root: root, host: filepath.Join(files, "host.jsonl"), down: filepath.Join(files, "down")}
}

func (e daemonEnv) setDown(t *testing.T, down bool) {
	t.Helper()
	if down {
		if err := os.WriteFile(e.down, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Remove(e.down); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

func (e daemonEnv) reports(t *testing.T, id core.SessionID) []core.SessionReport {
	t.Helper()
	b, err := os.ReadFile(e.host)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []core.SessionReport
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r core.SessionReport
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("host file line %q: %v", line, err)
		}
		if r.SessionID == id {
			out = append(out, r)
		}
	}
	return out
}

func (e daemonEnv) waitReports(t *testing.T, id core.SessionID, ok func([]core.SessionReport) bool) []core.SessionReport {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		rs := e.reports(t, id)
		if ok(rs) {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("reports %+v", rs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startDaemon starts a test daemon and waits until its manager is open.
func startDaemon(t *testing.T, e daemonEnv, dieAt string) *testDaemon {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), roleEnv+"=daemon", "GAIA_TEST_ROOT="+e.root,
		"GAIA_TEST_HOST="+e.host, "GAIA_TEST_DOWN="+e.down, "GAIA_TEST_DIE_AT="+dieAt, "GAIA_TEST_AGENT="+fakeAgent)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	d := &testDaemon{t: t, cmd: cmd, in: in, out: bufio.NewReader(out), done: make(chan struct{}), stder: &strings.Builder{}}
	cmd.Stderr = &lockedWriter{w: d.stder}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(d.done) }()
	t.Cleanup(d.kill)
	if _, err := d.read(); err != nil {
		t.Fatalf("daemon did not open: %v", err)
	}
	return d
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (d *testDaemon) read() (daemonResponse, error) {
	line, err := d.out.ReadBytes('\n')
	if err != nil {
		return daemonResponse{}, err
	}
	var res daemonResponse
	return res, json.Unmarshal(line, &res)
}

// call sends req and reads the response; an error when the daemon died.
func (d *testDaemon) call(req daemonRequest) (daemonResponse, error) {
	b, err := json.Marshal(req)
	if err != nil {
		d.t.Fatalf("encoding %+v: %v", req, err)
	}
	if _, err := d.in.Write(append(b, '\n')); err != nil {
		return daemonResponse{}, err
	}
	type result struct {
		res daemonResponse
		err error
	}
	got := make(chan result, 1)
	go func() { r, err := d.read(); got <- result{r, err} }()
	select {
	case r := <-got:
		return r.res, r.err
	case <-time.After(40 * time.Second):
		// Make it print its goroutines, then fail with them.
		_ = d.cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-d.done:
		case <-time.After(5 * time.Second):
		}
		d.t.Fatalf("%s: no answer from the daemon; stderr:\n%s", req.Op, d.stderr())
		return daemonResponse{}, nil
	}
}

func (d *testDaemon) stderr() string {
	w := d.cmd.Stderr.(*lockedWriter)
	w.mu.Lock()
	defer w.mu.Unlock()
	return d.stder.String()
}

func (d *testDaemon) mustCall(req daemonRequest) daemonResponse {
	d.t.Helper()
	res, err := d.call(req)
	if err != nil {
		d.t.Fatalf("%s: daemon gone: %v", req.Op, err)
	}
	if res.Error != "" {
		d.t.Fatalf("%s: %s", req.Op, res.Error)
	}
	return res
}

func (d *testDaemon) get(id core.SessionID) (SessionInfo, error) {
	res, err := d.call(daemonRequest{Op: "list"})
	if err != nil {
		return SessionInfo{}, fmt.Errorf("daemon gone (%w); stderr:\n%s", err, d.stderr())
	}
	for _, i := range res.List {
		if i.ID == id {
			return i, nil
		}
	}
	return SessionInfo{}, fmt.Errorf("session %s not listed", id)
}

// kill SIGKILLs the daemon and waits for it.
func (d *testDaemon) kill() {
	_ = d.cmd.Process.Signal(syscall.SIGKILL)
	<-d.done
}

// waitDead waits for a daemon that kills itself.
func (d *testDaemon) waitDead() {
	d.t.Helper()
	select {
	case <-d.done:
	case <-time.After(30 * time.Second):
		d.t.Fatalf("daemon did not die; stderr: %s", d.stderr())
	}
}

// ----- tests -----

// TestDaemonKilledSessionSurvives: SIGKILL of the daemon mid-session leaves
// the session running; a new daemon lists it running, then records its
// real exit code and reports it.
func TestDaemonKilledSessionSurvives(t *testing.T) {
	e := newDaemonEnv(t)
	d1 := startDaemon(t, e, "")
	id := newID(t)
	res := d1.mustCall(daemonRequest{Op: "start", ID: id, Work: t.TempDir(), Steps: []string{"print:hi", "sleep:3s", "exit:7"}})
	if res.Info.State != core.SessionRunning {
		t.Fatalf("started %+v", res.Info)
	}
	sup, agent := res.Info.SupervisorPID, res.Info.PID
	d1.kill()
	if !alive(sup) || !alive(agent) {
		t.Fatalf("supervisor (%d) or agent (%d) died with the daemon", sup, agent)
	}
	d2 := startDaemon(t, e, "")
	info, err := d2.get(id)
	if err != nil || info.State != core.SessionRunning {
		t.Fatalf("after restart: %+v, %v", info, err)
	}
	info = waitInfo(t, func() (SessionInfo, error) { return d2.get(id) },
		func(i SessionInfo) bool { return i.State.Terminal() }, "the end")
	wantExit(t, info, core.SessionExited, 7)
	rs := e.waitReports(t, id, lastIs(core.SessionExited))
	checkReportOrder(t, rs)
	if c := rs[len(rs)-1].ExitCode; c == nil || *c != 7 {
		t.Fatalf("reported exit code %v, want 7", c)
	}
}

// TestSupervisorKilledIsLost: a supervisor killed with SIGKILL yields a
// lost session, never exited, and a new manager keeps it lost.
func TestSupervisorKilledIsLost(t *testing.T) {
	root := newRoot(t)
	h := &fakeHost{}
	m := openManager(t, baseConfig(root, h))
	spec := newStart(t, "sleep:30s")
	info, err := m.Start(ctxT(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-info.PID, syscall.SIGKILL) }) // the orphaned agent
	if err := syscall.Kill(info.SupervisorPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	lost := waitState(t, m, spec.ID, core.SessionLost)
	if lost.ExitCode != nil || lost.EndedAt != nil {
		t.Fatalf("lost session with an outcome: %+v", lost)
	}
	rs := waitReports(t, h, spec.ID, lastIs(core.SessionLost))
	checkReportOrder(t, rs)
	for _, r := range rs {
		if r.State == core.SessionExited {
			t.Fatalf("reported exited: %+v", rs)
		}
	}
	m2, err := OpenManager(ctxT(t), baseConfig(root, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if again, _ := m2.Get(spec.ID); again.State != core.SessionLost {
		t.Fatalf("after reopening: %+v", again)
	}
}

// TestReportsReplayAfterHostOutage: a session that ends while the host is
// down, across a daemon restart, is reported in order with the right exit
// code once the host is back.
func TestReportsReplayAfterHostOutage(t *testing.T) {
	e := newDaemonEnv(t)
	e.setDown(t, true)
	d1 := startDaemon(t, e, "")
	id := newID(t)
	d1.mustCall(daemonRequest{Op: "start", ID: id, Work: t.TempDir(), Steps: []string{"sleep:300ms", "exit:4"}})
	waitInfo(t, func() (SessionInfo, error) { return d1.get(id) },
		func(i SessionInfo) bool { return i.State == core.SessionExited }, "exited")
	d1.kill()
	if rs := e.reports(t, id); len(rs) != 0 {
		t.Fatalf("reports while the host was down: %+v", rs)
	}
	startDaemon(t, e, "")
	time.Sleep(200 * time.Millisecond)
	e.setDown(t, false)
	rs := e.waitReports(t, id, lastIs(core.SessionExited))
	checkReportOrder(t, rs)
	if len(rs) != 2 || rs[0].State != core.SessionRunning || rs[1].Seq != 2 {
		t.Fatalf("reports %+v, want running then exited", rs)
	}
	if c := rs[1].ExitCode; c == nil || *c != 4 || rs[1].EndedAt == nil {
		t.Fatalf("exited report %+v, want exit code 4", rs[1])
	}
}

// TestRestartAtEveryStep: the daemon dies at each step of a session's life
// and a new daemon reaches the right outcome and reports it.
func TestRestartAtEveryStep(t *testing.T) {
	for _, c := range []struct {
		step  string
		steps []string
		stop  bool
		state core.SessionState
		code  int
	}{
		{step: "record-written", steps: []string{"sleep:500ms", "exit:5"}, state: core.SessionFailed},
		{step: "spawned", steps: []string{"sleep:1s", "exit:5"}, state: core.SessionExited, code: 5},
		{step: "ready", steps: []string{"sleep:1s", "exit:5"}, state: core.SessionExited, code: 5},
		{step: "stop-recorded", steps: []string{"sleep:30s"}, stop: true, state: core.SessionStopped, code: 128 + int(syscall.SIGTERM)},
		{step: "ended", steps: []string{"sleep:300ms", "exit:5"}, state: core.SessionExited, code: 5},
		{step: "reported", steps: []string{"sleep:300ms", "exit:5"}, state: core.SessionExited, code: 5},
	} {
		t.Run(c.step, func(t *testing.T) {
			e := newDaemonEnv(t)
			d1 := startDaemon(t, e, c.step)
			id := newID(t)
			res, err := d1.call(daemonRequest{Op: "start", ID: id, Work: t.TempDir(), Steps: c.steps})
			if err == nil && c.stop {
				_, err = d1.call(daemonRequest{Op: "stop", ID: id})
			}
			if err == nil && res.Error != "" {
				t.Fatalf("start: %s", res.Error)
			}
			d1.waitDead()
			d2 := startDaemon(t, e, "")
			info := waitInfo(t, func() (SessionInfo, error) { return d2.get(id) },
				func(i SessionInfo) bool { return i.State.Terminal() }, "the end")
			if info.State != c.state {
				t.Fatalf("ended %+v, want %s", info, c.state)
			}
			if c.state != core.SessionFailed {
				wantExit(t, info, c.state, c.code)
			}
			rs := e.waitReports(t, id, lastIs(c.state))
			checkReportOrder(t, rs)
			last := rs[len(rs)-1]
			if c.state != core.SessionFailed && (last.ExitCode == nil || *last.ExitCode != c.code) {
				t.Fatalf("last report %+v, want exit code %d", last, c.code)
			}
			if c.step == "reported" && (len(rs) < 2 || rs[0].Seq != 1 || rs[1].Seq != 1) {
				t.Fatalf("reports %+v: want the acknowledged report resent", rs)
			}
		})
	}
}

// TestRestartAfterEndAtReady: the daemon dies after the ready line and
// before recording running, the agent ends before a new daemon opens, and
// the new daemon still records and reports the real exit code.
func TestRestartAfterEndAtReady(t *testing.T) {
	e := newDaemonEnv(t)
	d1 := startDaemon(t, e, "ready")
	id := newID(t)
	_, _ = d1.call(daemonRequest{Op: "start", ID: id, Work: t.TempDir(), Steps: []string{"sleep:300ms", "exit:7"}})
	d1.waitDead()
	sv := filepath.Join(e.root, id.String(), supervisorDir)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if st, err := supervise.ReadState(sv); err == nil && st.Ended() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the supervisor did not record the end")
		}
		time.Sleep(50 * time.Millisecond)
	}
	d2 := startDaemon(t, e, "")
	info := waitInfo(t, func() (SessionInfo, error) { return d2.get(id) },
		func(i SessionInfo) bool { return i.State.Terminal() }, "the end")
	wantExit(t, info, core.SessionExited, 7)
	rs := e.waitReports(t, id, lastIs(core.SessionExited))
	checkReportOrder(t, rs)
	if c := rs[len(rs)-1].ExitCode; c == nil || *c != 7 {
		t.Fatalf("last report %+v, want exit code 7", rs[len(rs)-1])
	}
}

// TestStopRecordsStopped: stop yields stopped with 128+SIGTERM; stopping
// again returns the same session.
func TestStopRecordsStopped(t *testing.T) {
	h := &fakeHost{}
	m := openManager(t, baseConfig(newRoot(t), h))
	spec := newStart(t, "sleep:30s")
	if _, err := m.Start(ctxT(t), spec); err != nil {
		t.Fatal(err)
	}
	info, err := m.Stop(ctxT(t), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, info, core.SessionStopped, 143)
	if info.Signal != "SIGTERM" {
		t.Errorf("signal %q", info.Signal)
	}
	again, err := m.Stop(ctxT(t), spec.ID)
	if err != nil || again.State != core.SessionStopped || *again.ExitCode != 143 {
		t.Fatalf("second stop: %+v, %v", again, err)
	}
	rs := waitReports(t, h, spec.ID, lastIs(core.SessionStopped))
	checkReportOrder(t, rs)
	if len(rs) != 2 || rs[0].State != core.SessionRunning || *rs[1].ExitCode != 143 {
		t.Fatalf("reports %+v", rs)
	}
	_, err = m.Stop(ctxT(t), newID(t))
	wantCode(t, err, core.CodeNotFound)
}

// TestStopCancelledStaysRunning: a stop whose context is already done
// leaves a reachable session running, not lost.
func TestStopCancelledStaysRunning(t *testing.T) {
	h := &fakeHost{}
	m := openManager(t, baseConfig(newRoot(t), h))
	spec := newStart(t, "sleep:30s")
	if _, err := m.Start(ctxT(t), spec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Stop(ctx, spec.ID); err == nil {
		t.Fatal("stop with a cancelled context succeeded")
	}
	if _, err := supervise.NewClient(m.SessionDir(spec.ID)).Status(ctxT(t)); err != nil {
		t.Fatalf("supervisor unreachable: %v", err)
	}
	info, err := m.Get(spec.ID)
	if err != nil || info.State != core.SessionRunning {
		t.Fatalf("after a cancelled stop: %+v, %v", info, err)
	}
	for _, r := range h.of(spec.ID) {
		if r.State == core.SessionLost {
			t.Fatalf("reported lost: %+v", h.of(spec.ID))
		}
	}
}

// TestSignalExitCode: death by signal N is exit code 128+N, state exited.
func TestSignalExitCode(t *testing.T) {
	m := openManager(t, baseConfig(newRoot(t), nil))
	for sig, n := range map[string]syscall.Signal{"KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM} {
		spec := newStart(t, "kill:"+sig)
		if _, err := m.Start(ctxT(t), spec); err != nil {
			t.Fatal(err)
		}
		info := waitInfo(t, func() (SessionInfo, error) { return m.Get(spec.ID) },
			func(i SessionInfo) bool { return i.State.Terminal() }, "the end")
		wantExit(t, info, core.SessionExited, 128+int(n))
		if info.Signal != "SIG"+sig {
			t.Errorf("signal %q, want SIG%s", info.Signal, sig)
		}
	}
}

// TestStartFailureIsFailed: a supervisor's refusal leaves the session
// failed and is returned; a known id is a conflict.
func TestStartFailureIsFailed(t *testing.T) {
	h := &fakeHost{}
	m := openManager(t, baseConfig(newRoot(t), h))
	spec := newStart(t)
	spec.Argv = []string{"/nonexistent/agent"}
	_, err := m.Start(ctxT(t), spec)
	wantCode(t, err, core.CodeInvalidArgument)
	info, err := m.Get(spec.ID)
	if err != nil || info.State != core.SessionFailed || info.EndedAt == nil || info.ExitCode != nil {
		t.Fatalf("after a refused start: %+v, %v", info, err)
	}
	rs := waitReports(t, h, spec.ID, lastIs(core.SessionFailed))
	checkReportOrder(t, rs)
	_, err = m.Start(ctxT(t), spec)
	wantCode(t, err, core.CodeConflict)
	bad := newStart(t, "exit:0")
	bad.WorkDir = "relative"
	_, err = m.Start(ctxT(t), bad)
	wantCode(t, err, core.CodeInvalidArgument)
}

// TestTotalScrollbackBound: the scrollback of all sessions together never
// exceeds the total bound; the oldest closed session is dropped first; a
// start with no room is refused.
func TestTotalScrollbackBound(t *testing.T) {
	root := newRoot(t)
	h := &fakeHost{}
	cfg := baseConfig(root, h)
	cfg.MaxScrollback, cfg.MaxScrollbackTotal = 1000, 2500
	m := openManager(t, cfg)
	total := func() int64 {
		var n int64
		for _, i := range m.List() {
			n += scrollbackSize(m.SessionDir(i.ID))
		}
		return n
	}
	text := strings.Repeat("x", 799) // 800 bytes with the newline
	var ids []core.SessionID
	for i := range 3 {
		spec := newStart(t, "print:"+text, "exit:0")
		if _, err := m.Start(ctxT(t), spec); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		waitState(t, m, spec.ID, core.SessionExited)
		waitReports(t, h, spec.ID, lastIs(core.SessionExited))
		ids = append(ids, spec.ID)
		if n := total(); n > cfg.MaxScrollbackTotal {
			t.Fatalf("after session %d: %d bytes of scrollback, bound %d", i, n, cfg.MaxScrollbackTotal)
		}
	}
	// The third start needed 1000 bytes of room next to 1600 closed: the
	// oldest session went, the second stayed.
	if _, err := m.Get(ids[0]); err == nil {
		t.Errorf("oldest session kept: %v", m.List())
	}
	if n := scrollbackSize(m.SessionDir(ids[1])); n != 800 {
		t.Errorf("second session's scrollback %d bytes, want 800", n)
	}
	// Two live sessions take 2000 of 2500: a third does not fit.
	for range 2 {
		spec := newStart(t, "sleep:30s")
		if _, err := m.Start(ctxT(t), spec); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = m.Stop(context.Background(), spec.ID) })
	}
	if n := total(); n+2*cfg.MaxScrollback > cfg.MaxScrollbackTotal {
		t.Errorf("%d closed bytes next to two live sessions, bound %d", n, cfg.MaxScrollbackTotal)
	}
	_, err := m.Start(ctxT(t), newStart(t, "exit:0"))
	wantCode(t, err, core.CodeUnavailable)
}

// TestFileModesNoSecrets: directories are 0700 and files 0600 under the
// root, and no environment value or secret of the spec is written there.
func TestFileModesNoSecrets(t *testing.T) {
	const secret = "sk-node-SECRET-0123456789"
	const envValue = "env-value-SECRET-987654"
	root := newRoot(t)
	m := openManager(t, baseConfig(root, nil))
	spec := newStart(t, "print:"+secret, "args", secret, "exit:0")
	spec.Env = append(spec.Env, "GAIA_TEST_TOKEN="+envValue)
	spec.Secrets = []string{secret}
	if _, err := m.Start(ctxT(t), spec); err != nil {
		t.Fatal(err)
	}
	info := waitState(t, m, spec.ID, core.SessionExited)
	for _, a := range info.Argv {
		if strings.Contains(a, secret) {
			t.Errorf("argv holds the secret: %q", info.Argv)
		}
	}
	files := 0
	err := filepath.WalkDir(filepath.Dir(root), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != filepath.Dir(root) && fi.Mode().Perm() != 0o700 {
				t.Errorf("%s: mode %s, want 0700", path, fi.Mode().Perm())
			}
			return nil
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %s, want 0600", path, fi.Mode().Perm())
		}
		if !fi.Mode().IsRegular() {
			return nil // the socket, until the supervisor removes it
		}
		files++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), secret) || strings.Contains(string(b), envValue) {
			t.Errorf("%s holds a secret: %q", path, b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 3 {
		t.Fatalf("only %d files under the root", files)
	}
	if got := fmt.Sprint(spec); strings.Contains(got, secret) || strings.Contains(got, envValue) {
		t.Errorf("StartSpec formats its secrets: %s", got)
	}
}

// TestReporterRefusalDropped: a final refusal drops that report and the
// next ones still go.
func TestReporterRefusalDropped(t *testing.T) {
	root := newRoot(t)
	h := &fakeHost{refuse: func(r core.SessionReport) error {
		if r.State == core.SessionRunning {
			return core.NewError(core.CodeConflict, "no")
		}
		return nil
	}}
	m := openManager(t, baseConfig(root, h))
	spec := newStart(t, "sleep:200ms", "exit:2")
	if _, err := m.Start(ctxT(t), spec); err != nil {
		t.Fatal(err)
	}
	rs := waitReports(t, h, spec.ID, lastIs(core.SessionExited))
	if len(rs) != 1 || rs[0].Seq != 2 {
		t.Fatalf("reports %+v, want only the exited one", rs)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := readRecord(m.dir(spec.ID))
		if err != nil {
			t.Fatal(err)
		}
		if len(rec.Pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending reports left: %+v", rec.Pending)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestOpenManagerRefusesBadConfig covers the configuration checks.
func TestOpenManagerRefusesBadConfig(t *testing.T) {
	root := newRoot(t)
	for name, mut := range map[string]func(*ManagerConfig){
		"no supervisor":   func(c *ManagerConfig) { c.Supervisor = nil },
		"relative dir":    func(c *ManagerConfig) { c.Dir = "rel" },
		"total too small": func(c *ManagerConfig) { c.MaxScrollback, c.MaxScrollbackTotal = 100, 50 },
	} {
		cfg := baseConfig(root, nil)
		mut(&cfg)
		if _, err := OpenManager(ctxT(t), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := os.Chmod(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenManager(ctxT(t), baseConfig(root, nil)); err == nil {
		t.Error("a group-readable root accepted")
	}
}
