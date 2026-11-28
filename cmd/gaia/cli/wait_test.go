//go:build linux || darwin

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// waitDaemon is a fake host for `gaia run --wait`: it starts one session,
// answers its state from a script, serves its output and records stops.
type waitDaemon struct {
	*sessionDaemon
	id core.SessionID

	mu      sync.Mutex
	req     core.StartSessionRequest
	states  []core.Session // answered in turn; the last one repeats
	output  string
	stopped bool
	// release, when set, holds the start's answer until closed.
	release chan struct{}
	started chan struct{}
}

func newWaitDaemon(t *testing.T, output string, states ...core.Session) (*waitDaemon, *Client) {
	t.Helper()
	d := &waitDaemon{sessionDaemon: newSessionDaemon(), id: core.NewSessionID([16]byte{4}), output: output,
		states: states, started: make(chan struct{}, 1)}
	for i := range d.states {
		d.states[i].ID, d.states[i].NodeID = d.id, testNode
	}
	d.mux.HandleFunc("POST "+core.SessionsPath, func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&d.req)
		release := d.release
		d.mu.Unlock()
		d.started <- struct{}{}
		if release != nil {
			<-release
		}
		writeJSON(w, http.StatusOK, core.StartSessionResponse{Session: core.Session{ID: d.id, NodeID: testNode, State: core.SessionRunning}})
	})
	d.mux.HandleFunc("GET "+core.SessionPath(d.id), func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		s := d.states[0]
		if len(d.states) > 1 {
			d.states = d.states[1:]
		}
		d.mu.Unlock()
		writeJSON(w, http.StatusOK, core.GetSessionResponse{Session: s})
	})
	d.mux.HandleFunc("GET "+core.SessionLogsPath(d.id), func(w http.ResponseWriter, r *http.Request) {
		off := r.URL.Query().Get("offset")
		if off == "0" {
			_, _ = w.Write([]byte(d.output))
		}
	})
	d.mux.HandleFunc("POST "+core.SessionStopPath(d.id), func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		d.stopped = true
		d.mu.Unlock()
		writeJSON(w, http.StatusOK, core.StopSessionResponse{Session: core.Session{ID: d.id, NodeID: testNode, State: core.SessionStopped}})
	})
	return d, &Client{Dir: fakeDaemon(t, d)}
}

func (d *waitDaemon) wasStopped() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped
}

func ended(state core.SessionState, code *int, sync core.SyncState) core.Session {
	return core.Session{State: state, ExitCode: code, Sync: sync, Branch: "main"}
}

func ip(v int) *int { return &v }

// runWaitCmd runs `gaia run --wait` with explicit --repo and --branch, so
// no checkout is read, and the given interrupts.
func runWaitCmd(c *Client, ints chan os.Signal, extra ...string) (int, string, string) {
	var out, errOut lockedWriter
	args := append([]string{"--node", "node-b", "--wait", "--repo", "/srv/r.git", "--branch", "main"}, extra...)
	args = append(args, "--", "agent", "x")
	code := Run(args, Env{Stdout: &out, Stderr: &errOut, Client: c, Interrupts: ints,
		LostGrace: 100 * time.Millisecond, Unreachable: 300 * time.Millisecond})
	return code, out.String(), errOut.String()
}

type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestWaitExitCodes: the exit code for each way a session ends.
func TestWaitExitCodes(t *testing.T) {
	running := core.Session{State: core.SessionRunning, Sync: core.SyncPending}
	for _, c := range []struct {
		name   string
		states []core.Session
		want   int
		stderr string
	}{
		{"exited", []core.Session{running, ended(core.SessionExited, ip(5), core.SyncDone)}, 5, ""},
		{"exited, no repo", []core.Session{ended(core.SessionExited, ip(0), core.SyncNone)}, 0, ""},
		{"push reported later", []core.Session{running, ended(core.SessionExited, ip(2), core.SyncPending),
			ended(core.SessionExited, ip(2), core.SyncPending), ended(core.SessionExited, ip(2), core.SyncDone)}, 2, ""},
		{"push failed", []core.Session{ended(core.SessionExited, ip(0), core.SyncPending), ended(core.SessionExited, ip(0), core.SyncFailed)},
			int(core.ExitSyncAfterFailed), "could not push branch main"},
		{"signal", []core.Session{ended(core.SessionExited, ip(137), core.SyncDone)}, 137, ""},
		{"stopped elsewhere", []core.Session{ended(core.SessionStopped, ip(143), core.SyncDone)}, 143, "was stopped"},
		{"failed", []core.Session{ended(core.SessionFailed, nil, core.SyncDone)}, int(core.ExitInternal), "failed"},
		{"lost", []core.Session{running, {State: core.SessionLost, Sync: core.SyncPending}}, int(core.ExitSessionLost), "is lost"},
		{"lost then back", []core.Session{{State: core.SessionLost}, running, ended(core.SessionExited, ip(1), core.SyncDone)}, 1, ""},
	} {
		d, cl := newWaitDaemon(t, "out\n", c.states...)
		code, out, errOut := runWaitCmd(cl, make(chan os.Signal))
		if code != c.want || out != "out\n" || !strings.Contains(errOut, c.stderr) {
			t.Errorf("%s: exit %d %q %q; want %d", c.name, code, out, errOut, c.want)
		}
		if d.req.Repo != "/srv/r.git" || d.req.Branch != "main" || d.req.Mode != core.SessionHeadless {
			t.Errorf("%s: request %+v", c.name, d.req)
		}
	}
}

// TestWaitVersionMismatch: an incompatible daemon is 96.
func TestWaitVersionMismatch(t *testing.T) {
	c := &Client{Dir: fakeDaemon(t, daemonAt(core.APIVersion+1, "9.0.0", nil))}
	if code, _, errOut := runWaitCmd(c, make(chan os.Signal)); code != int(core.ExitVersionMismatch) {
		t.Errorf("exit %d %q", code, errOut)
	}
}

// TestWaitInternal: a Gaia failure with no contract reason is 99.
func TestWaitInternal(t *testing.T) {
	d := newSessionDaemon()
	d.mux.HandleFunc("POST "+core.SessionsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, core.ErrorResponse{Error: core.NewError(core.CodeInternal, "internal error")})
	})
	if code, _, errOut := runWaitCmd(&Client{Dir: fakeDaemon(t, d)}, make(chan os.Signal)); code != int(core.ExitInternal) || !strings.HasPrefix(errOut, "gaia: ") {
		t.Errorf("exit %d %q", code, errOut)
	}
}

// TestWaitUnreachable: a daemon that stops answering after the start is
// 95 once Unreachable has passed.
func TestWaitUnreachable(t *testing.T) {
	d := newSessionDaemon()
	id := core.NewSessionID([16]byte{5})
	d.mux.HandleFunc("POST "+core.SessionsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.StartSessionResponse{Session: core.Session{ID: id, NodeID: testNode, State: core.SessionRunning}})
	})
	gone := (&core.Error{Code: core.CodeUnavailable, Message: "node offline"}).WithReason(core.ExitNodeUnavailable)
	d.mux.HandleFunc("GET "+core.SessionLogsPath(id), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, core.ErrorResponse{Error: gone})
	})
	d.mux.HandleFunc("GET "+core.SessionPath(id), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.GetSessionResponse{Session: core.Session{ID: id, NodeID: testNode, State: core.SessionRunning}})
	})
	start := time.Now()
	code, _, errOut := runWaitCmd(&Client{Dir: fakeDaemon(t, d)}, make(chan os.Signal))
	if code != int(core.ExitSessionLost) || !strings.Contains(errOut, "lost contact") {
		t.Errorf("exit %d %q", code, errOut)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 10*time.Second {
		t.Errorf("gave up after %s", took)
	}
}

// TestWaitInterrupt: an interrupt during the wait stops the session and
// exits 130; with --detach-on-interrupt it leaves it running.
func TestWaitInterrupt(t *testing.T) {
	for _, detach := range []bool{false, true} {
		d, cl := newWaitDaemon(t, "", core.Session{State: core.SessionRunning})
		ints := make(chan os.Signal, 1)
		go func() {
			<-d.started
			time.Sleep(300 * time.Millisecond)
			ints <- syscall.SIGINT
		}()
		var extra []string
		if detach {
			extra = []string{"--detach-on-interrupt"}
		}
		code, _, errOut := runWaitCmd(cl, ints, extra...)
		if code != 130 || d.wasStopped() == detach {
			t.Errorf("detach %v: exit %d, stopped %v, %q", detach, code, d.wasStopped(), errOut)
		}
	}
}

// TestWaitInterruptDuringStart: an interrupt while the start is in flight
// waits for it, then stops the session; a second one leaves at once.
func TestWaitInterruptDuringStart(t *testing.T) {
	d, cl := newWaitDaemon(t, "", core.Session{State: core.SessionRunning})
	d.release = make(chan struct{})
	ints := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		code, _, _ := runWaitCmd(cl, ints)
		done <- code
	}()
	<-d.started
	ints <- syscall.SIGINT
	select {
	case code := <-done:
		t.Fatalf("exited %d before the start returned", code)
	case <-time.After(300 * time.Millisecond):
	}
	close(d.release)
	if code := <-done; code != 130 || !d.wasStopped() {
		t.Errorf("exit %d, stopped %v", code, d.wasStopped())
	}

	d2, cl2 := newWaitDaemon(t, "", core.Session{State: core.SessionRunning})
	d2.release = make(chan struct{})
	defer close(d2.release)
	ints2 := make(chan os.Signal, 2)
	go func() {
		code, _, _ := runWaitCmd(cl2, ints2)
		done <- code
	}()
	<-d2.started
	ints2 <- syscall.SIGINT
	ints2 <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 130 || d2.wasStopped() {
			t.Errorf("second interrupt: exit %d, stopped %v", code, d2.wasStopped())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second interrupt did not leave")
	}
}

// TestWaitFlags: mode and flag combinations that are usage errors.
func TestWaitFlags(t *testing.T) {
	d := newSessionDaemon()
	c := &Client{Dir: fakeDaemon(t, d)}
	for _, args := range [][]string{
		{"--node", "n", "--wait", "--detach", "--", "a"},
		{"--node", "n", "--wait", "--interactive", "--", "a"},
		{"--node", "n", "--detach", "--detach-on-interrupt", "--", "a"},
		{"--node", "n", "--detach", "--repo", "/r.git", "--", "a"},
		{"--node", "n", "--detach", "--branch", "main", "--", "a"},
		{"--node", "n", "--wait", "--repo", "/r.git", "--branch", "main", "--"},
	} {
		if code, _, errOut := runCmd(Run, c, args...); code != int(core.ExitUsage) {
			t.Errorf("%q: exit %d %q", args, code, errOut)
		}
	}
	if r := d.requests(); len(r) != 0 {
		t.Errorf("usage errors reached the daemon: %v", r)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestWaitCallerCheckout: --repo and --branch default to the caller's
// checkout, which must be pushed; explicit flags skip the check.
func TestWaitCallerCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n[user]\n\tname = T\n\temail = t@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	base := t.TempDir()
	remote, caller := filepath.Join(base, "r.git"), filepath.Join(base, "c")
	git(t, base, "init", "-q", "--bare", remote)
	git(t, base, "clone", "-q", remote, caller)
	git(t, caller, "commit", "-q", "--allow-empty", "-m", "one")
	git(t, caller, "push", "-q", "-u", "origin", "main")

	run := func(dir string, extra ...string) (int, core.StartSessionRequest, string) {
		d, cl := newWaitDaemon(t, "", ended(core.SessionExited, ip(0), core.SyncDone))
		var out, errOut lockedWriter
		args := append(append([]string{"--node", "node-b", "--wait"}, extra...), "--", "agent")
		code := Run(args, Env{Stdout: &out, Stderr: &errOut, Client: cl, WorkDir: dir, Interrupts: make(chan os.Signal)})
		return code, d.req, errOut.String()
	}
	if code, req, e := run(caller); code != 0 || req.Repo != remote || req.Branch != "main" {
		t.Errorf("defaults: exit %d %+v %q", code, req, e)
	}
	before := git(t, caller, "rev-parse", "HEAD")
	git(t, caller, "commit", "-q", "--allow-empty", "-m", "two")
	if code, _, e := run(caller); code != int(core.ExitSyncBeforeFailed) || !strings.Contains(e, "1 commit(s) ahead") {
		t.Errorf("ahead: exit %d %q", code, e)
	}
	// Explicit flags: another checkout's business.
	if code, req, e := run(caller, "--repo", remote, "--branch", "main"); code != 0 || req.Branch != "main" {
		t.Errorf("explicit: exit %d %+v %q", code, req, e)
	}
	// The caller's checkout was only read.
	if got := git(t, caller, "rev-parse", "HEAD~1"); got != before {
		t.Errorf("caller HEAD~1 %s, want %s", got, before)
	}
	git(t, caller, "push", "-q")
	git(t, caller, "switch", "-q", "-c", "topic")
	if code, _, e := run(caller); code != int(core.ExitSyncBeforeFailed) || !strings.Contains(e, "not on origin") {
		t.Errorf("no upstream: exit %d %q", code, e)
	}
	// --branch given: the caller's branch is not compared.
	if code, req, e := run(caller, "--branch", "main"); code != 0 || req.Repo != remote || req.Branch != "main" {
		t.Errorf("--branch only: exit %d %+v %q", code, req, e)
	}
	git(t, caller, "switch", "-q", "--detach", "HEAD")
	if code, _, e := run(caller); code != int(core.ExitUsage) || !strings.Contains(e, "--branch is needed") {
		t.Errorf("detached: exit %d %q", code, e)
	}
	if code, _, e := run(t.TempDir()); code != int(core.ExitUsage) || !strings.Contains(e, "--repo is needed") {
		t.Errorf("not a checkout: exit %d %q", code, e)
	}
}
