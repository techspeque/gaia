//go:build linux || darwin

package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/repo"
)

// repoFixture is a bare remote with a pushed main branch and a node
// workspace, with git isolated from the user's configuration.
type repoFixture struct {
	remote string
	ws     *repo.Workspace
	env    []string // the agent's environment
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n[user]\n\tname = T\n\temail = t@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	base := t.TempDir()
	f := &repoFixture{remote: filepath.Join(base, "r.git")}
	gitT(t, base, "init", "-q", "--bare", f.remote)
	seed := filepath.Join(base, "seed")
	gitT(t, base, "clone", "-q", f.remote, seed)
	gitT(t, seed, "commit", "-q", "--allow-empty", "-m", "first")
	gitT(t, seed, "push", "-q", "origin", "main")
	ws, err := repo.Open(filepath.Join(base, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	f.ws = ws
	f.env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=" + cfg, "GIT_CONFIG_NOSYSTEM=1"}
	return f
}

func (f *repoFixture) tip(t *testing.T) string {
	return gitT(t, f.remote, "rev-parse", "refs/heads/main")
}

// start is a session in the checkout running sh -c script.
func (f *repoFixture) start(t *testing.T, script string) StartSpec {
	t.Helper()
	return StartSpec{
		ID: newID(t), Mode: core.SessionHeadless, Argv: []string{"sh", "-c", script},
		Repo: f.remote, Branch: "main", Env: f.env,
	}
}

const commitScript = "pwd -P; git rev-parse HEAD; echo x >> f && git add f && git commit -qm agent"

func syncIs(s core.SyncState) func([]core.SessionReport) bool {
	return func(rs []core.SessionReport) bool { return len(rs) > 0 && rs[len(rs)-1].Sync == s }
}

func scrollback(t *testing.T, m *Manager, id core.SessionID) string {
	t.Helper()
	var b strings.Builder
	if err := (&LocalSessions{Manager: m}).SessionLogs(ctxT(t), id, 0, false, &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestRepoSessionRunsInCheckoutAndPushes: the agent starts in the node's
// checkout at the remote tip; its commit is pushed after it ends, and the
// push is reported as the session's sync.
func TestRepoSessionRunsInCheckoutAndPushes(t *testing.T) {
	f := newRepoFixture(t)
	h := &fakeHost{}
	cfg := baseConfig(newRoot(t), h)
	cfg.Workspace = f.ws
	m := openManager(t, cfg)
	tip := f.tip(t)
	spec := f.start(t, commitScript)
	info, err := m.Start(ctxT(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	if info.Sync != core.SyncPending || info.Repo != f.remote || info.Branch != "main" {
		t.Errorf("started %+v", info)
	}
	rs := waitReports(t, h, spec.ID, syncIs(core.SyncDone))
	checkReportOrder(t, rs)
	last := rs[len(rs)-1]
	if last.State != core.SessionExited || last.ExitCode == nil || *last.ExitCode != 0 {
		t.Errorf("last report %+v", last)
	}
	// The exit is reported before the push, the push after.
	if len(rs) < 2 || rs[len(rs)-2].State != core.SessionExited || rs[len(rs)-2].Sync != core.SyncPending {
		t.Errorf("reports %+v", rs)
	}
	lines := strings.Split(strings.TrimSpace(scrollback(t, m, spec.ID)), "\n")
	dir, _ := filepath.EvalSymlinks(f.ws.Root())
	if len(lines) != 2 || !strings.HasPrefix(lines[0], dir+string(filepath.Separator)) || lines[1] != tip {
		t.Errorf("agent output %q; want a checkout under %s at %s", lines, dir, tip)
	}
	if msg := gitT(t, f.remote, "log", "-1", "--format=%s", "main"); msg != "agent" {
		t.Errorf("remote head %q", msg)
	}
	if got, _ := m.Get(spec.ID); got.Sync != core.SyncDone {
		t.Errorf("info %+v", got)
	}
}

// TestRepoSessionPrepareFailure: a dirty checkout refuses the start with
// reason sync_before_failed and records nothing; an invalid branch is
// usage.
func TestRepoSessionPrepareFailure(t *testing.T) {
	f := newRepoFixture(t)
	h := &fakeHost{}
	cfg := baseConfig(newRoot(t), h)
	cfg.Workspace = f.ws
	m := openManager(t, cfg)
	first := f.start(t, "true")
	if _, err := m.Start(ctxT(t), first); err != nil {
		t.Fatal(err)
	}
	waitReports(t, h, first.ID, syncIs(core.SyncDone))
	co, err := f.ws.Acquire(ctxT(t), f.remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(co.Dir(), "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	co.Release()
	spec := f.start(t, "echo never")
	info, err := m.Start(ctxT(t), spec)
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Details[core.DetailReason] != "sync_before_failed" || ce.Details[core.DetailSyncReason] != "dirty" {
		t.Fatalf("dirty start: %+v %v", info, err)
	}
	if !info.ID.IsZero() {
		t.Errorf("a refused start returned %+v", info)
	}
	if _, err := m.Get(spec.ID); err == nil {
		t.Error("the refused start was recorded")
	}
	bad := f.start(t, "true")
	bad.Branch = "-x"
	if _, err := m.Start(ctxT(t), bad); !errors.As(err, &ce) || ce.Details[core.DetailReason] != "usage" {
		t.Errorf("invalid branch: %v", err)
	}
	// No workspace: a repo start is refused as usage.
	cfg2 := baseConfig(newRoot(t), &fakeHost{})
	m2 := openManager(t, cfg2)
	if _, err := m2.Start(ctxT(t), f.start(t, "true")); !errors.As(err, &ce) || ce.Details[core.DetailReason] != "usage" {
		t.Errorf("no workspace: %v", err)
	}
}

// TestRepoSessionPushRejected: a rejected push is the session's sync
// failed; the commit stays in the checkout.
func TestRepoSessionPushRejected(t *testing.T) {
	f := newRepoFixture(t)
	hook := filepath.Join(f.remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{}
	cfg := baseConfig(newRoot(t), h)
	cfg.Workspace = f.ws
	m := openManager(t, cfg)
	before := f.tip(t)
	spec := f.start(t, commitScript)
	if _, err := m.Start(ctxT(t), spec); err != nil {
		t.Fatal(err)
	}
	rs := waitReports(t, h, spec.ID, syncIs(core.SyncFailed))
	if last := rs[len(rs)-1]; last.State != core.SessionExited || *last.ExitCode != 0 {
		t.Errorf("last %+v", last)
	}
	if f.tip(t) != before {
		t.Error("the remote moved")
	}
}

// TestRepoSessionsQueue: a second start on the same checkout waits until
// the first has ended and been pushed, and starts on its commit.
func TestRepoSessionsQueue(t *testing.T) {
	f := newRepoFixture(t)
	h := &fakeHost{}
	cfg := baseConfig(newRoot(t), h)
	cfg.Workspace = f.ws
	m := openManager(t, cfg)
	first := f.start(t, "sleep 1; "+commitScript)
	if _, err := m.Start(ctxT(t), first); err != nil {
		t.Fatal(err)
	}
	// A start that gives up while queued records nothing.
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	gaveUp := f.start(t, "true")
	if _, err := m.Start(short, gaveUp); err == nil {
		t.Fatal("a queued start with an expired context started")
	} else if _, gerr := m.Get(gaveUp.ID); gerr == nil {
		t.Error("the given-up start was recorded")
	}
	second := f.start(t, "git log -1 --format=%s")
	t0 := time.Now()
	if _, err := m.Start(ctxT(t), second); err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) < 500*time.Millisecond {
		t.Errorf("the second start did not wait for the first (%s)", time.Since(t0))
	}
	if rs := h.of(first.ID); len(rs) == 0 || rs[len(rs)-1].Sync != core.SyncDone {
		t.Errorf("the second started before the first was pushed: %+v", rs)
	}
	waitReports(t, h, second.ID, syncIs(core.SyncDone))
	if out := strings.TrimSpace(scrollback(t, m, second.ID)); out != "agent" {
		t.Errorf("second session saw head %q, want the first's commit", out)
	}
}

// TestRepoSessionSurvivesRestart: a daemon restart mid-session holds the
// checkout again (a new start waits), and the session's commit is pushed
// when it ends; a push cut off by a closing daemon is finished by the next.
func TestRepoSessionSurvivesRestart(t *testing.T) {
	f := newRepoFixture(t)
	root := newRoot(t)
	h := &fakeHost{}
	cfg := baseConfig(root, h)
	cfg.Workspace = f.ws
	m1, err := OpenManager(ctxT(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "go")
	spec := f.start(t, "while [ ! -e '"+gate+"' ]; do sleep 0.05; done; "+commitScript)
	if _, err := m1.Start(ctxT(t), spec); err != nil {
		t.Fatal(err)
	}
	m1.Close() // the daemon goes; the session runs on

	// A new daemon on a new workspace value, as after a real restart.
	ws2, err := repo.Open(f.ws.Root())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspace = ws2
	m2 := openManager(t, cfg)
	if got, _ := m2.Get(spec.ID); got.State != core.SessionRunning {
		t.Fatalf("after restart %+v", got)
	}
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := ws2.Acquire(short, f.remote); err == nil {
		t.Fatal("the running session's checkout was free after the restart")
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitReports(t, h, spec.ID, syncIs(core.SyncDone))
	if msg := gitT(t, f.remote, "log", "-1", "--format=%s", "main"); msg != "agent" {
		t.Errorf("remote head %q", msg)
	}
}

// TestRepoSessionFinishAfterRestart: a daemon that closes while a push runs
// leaves the sync pending; the next daemon pushes.
func TestRepoSessionFinishAfterRestart(t *testing.T) {
	f := newRepoFixture(t)
	root := newRoot(t)
	h := &fakeHost{}
	cfg := baseConfig(root, h)
	cfg.Workspace = f.ws
	m1, err := OpenManager(ctxT(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	testHook = func(step string) {
		if step == "finishing" {
			m1.cancel() // the daemon is going away as the push starts
		}
	}
	spec := f.start(t, commitScript)
	if _, err := m1.Start(ctxT(t), spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for m1.ctx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	m1.Close()
	testHook = nil
	if got, _ := m1.Get(spec.ID); got.State != core.SessionExited || got.Sync != core.SyncPending {
		t.Fatalf("after the cut push %+v", got)
	}
	ws2, err := repo.Open(f.ws.Root())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspace = ws2
	m2 := openManager(t, cfg)
	waitInfo(t, func() (SessionInfo, error) { return m2.Get(spec.ID) },
		func(i SessionInfo) bool { return i.Sync == core.SyncDone }, "sync done")
	waitReports(t, h, spec.ID, syncIs(core.SyncDone))
	if msg := gitT(t, f.remote, "log", "-1", "--format=%s", "main"); msg != "agent" {
		t.Errorf("remote head %q", msg)
	}
}
