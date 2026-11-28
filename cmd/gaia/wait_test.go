package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// waitFixture is a `gaia host --node` daemon, a local bare remote with a
// pushed main branch, and the caller's clone of it, for `gaia run --wait`.
type waitFixture struct {
	d      *daemon
	xdg    string
	state  string
	node   string
	agent  string
	remote string // the bare remote, also its URL
	caller string // the caller's checkout
}

// isolateGit gives git, in this process and every child, a private
// configuration and a fixed identity.
func isolateGit(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": cfg, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		t.Setenv(k, v)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newWaitFixture(t *testing.T) *waitFixture {
	t.Helper()
	skipIfNoDaemon(t)
	isolateGit(t)
	f := &waitFixture{agent: fakeAgent(t), xdg: shortTempDir(t), state: shortState(t)}
	base := t.TempDir()
	f.remote = filepath.Join(base, "remote.git")
	f.caller = filepath.Join(base, "caller")
	gitIn(t, base, "init", "-q", "--bare", f.remote)
	gitIn(t, base, "clone", "-q", f.remote, f.caller)
	if err := os.WriteFile(filepath.Join(f.caller, "README"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.caller, "add", "README")
	gitIn(t, f.caller, "commit", "-qm", "first")
	gitIn(t, f.caller, "push", "-q", "-u", "origin", "main")
	f.d = startDaemon(t, f.xdg, "--node", "--state", f.state)
	f.d.waitReady(t)
	waitLocalOnline(t, f.d)
	f.node = f.d.nodes(t)[0].Name
	return f
}

// waitCmd is `gaia run --node NODE --wait ARGS…` as a child process in the
// caller's checkout.
func (f *waitFixture) waitCmd(t *testing.T, xdg string, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = f.caller
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "XDG_RUNTIME_DIR="+xdg)
	return cmd
}

// run runs `gaia ARGS…` in the caller's checkout and returns its exit
// code, stdout and stderr.
func (f *waitFixture) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := f.waitCmd(t, f.xdg, args...)
	var out, errOut lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	return exitCode(t, cmd.Run()), out.String(), errOut.String()
}

// wait runs `gaia run --node NODE --wait -- ARGV…`.
func (f *waitFixture) wait(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	return f.run(t, append([]string{"run", "--node", f.node, "--wait", "--"}, argv...)...)
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal(err)
	}
	return ee.ExitCode()
}

// checkout is the node's checkout of the remote.
func (f *waitFixture) checkout(t *testing.T) string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(f.state, workspaceDirName, "*"))
	if err != nil || len(m) != 1 {
		t.Fatalf("node checkouts: %v %v", m, err)
	}
	return m[0]
}

func (f *waitFixture) remoteTip(t *testing.T, branch string) string {
	t.Helper()
	return gitIn(t, f.remote, "rev-parse", "refs/heads/"+branch)
}

var sessionLine = regexp.MustCompile(`gaia: session (sess_[0-9a-z]+) running`)

// sessionRecord reads the node's record of the session with id.
func (f *waitFixture) sessionRecord(t *testing.T, id string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, sessionsDirName, id, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// startWait starts `gaia run --wait` and returns it with its stdout lines,
// as they arrive, and its stderr.
func (f *waitFixture) startWait(t *testing.T, flags []string, argv ...string) (*exec.Cmd, <-chan string, *lockedBuffer) {
	t.Helper()
	args := append(append([]string{"run", "--node", f.node, "--wait"}, flags...), "--")
	cmd := f.waitCmd(t, f.xdg, append(args, argv...)...)
	errOut := &lockedBuffer{}
	cmd.Stderr = errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	return cmd, lines, errOut
}

func nextLine(t *testing.T, lines <-chan string, within time.Duration) string {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("output ended")
		}
		return l
	case <-time.After(within):
		t.Fatalf("no output within %s", within)
	}
	return ""
}

func waitExit(t *testing.T, cmd *exec.Cmd, within time.Duration) (int, time.Duration) {
	t.Helper()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCode(t, err), time.Since(start)
	case <-time.After(within):
		t.Fatalf("gaia run --wait did not exit within %s", within)
	}
	return 0, 0
}

// processWith reports whether a process whose command line contains token
// is running.
func processWith(t *testing.T, token string) bool {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "command").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(out), token)
}

// TestWaitContract runs each row of the launcher contract's exit-code
// table that a daemon and the fake agent can produce
// (docs/contracts/launcher-v1.md): the agent's own codes and signals, and
// every reserved code whose condition this machine can create. 96 and 99
// are produced in cmd/gaia/cli against a fake daemon.
func TestWaitContract(t *testing.T) {
	f := newWaitFixture(t)

	// The agent's exit code, unchanged; 128+N for signal N.
	for _, c := range []struct {
		step string
		want int
	}{{"exit:0", 0}, {"exit:1", 1}, {"exit:2", 2}, {"exit:3", 3}, {"exit:130", 130}, {"kill:KILL", 137}} {
		if code, out, errOut := f.wait(t, f.agent, "print:ran", c.step); code != c.want || out != "ran\n" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want %d", c.step, code, out, errOut, c.want)
		}
	}

	// 90 no_daemon.
	cmd := f.waitCmd(t, shortTempDir(t), "run", "--node", f.node, "--wait", "--", f.agent)
	var errOut lockedBuffer
	cmd.Stderr = &errOut
	if code := exitCode(t, cmd.Run()); code != int(core.ExitNoDaemon) {
		t.Errorf("no daemon: exit %d %q", code, errOut.String())
	}

	// 91 node_unavailable.
	if code, _, e := f.run(t, "run", "--node", "nosuch", "--wait", "--", f.agent); code != int(core.ExitNodeUnavailable) {
		t.Errorf("unknown node: exit %d %q", code, e)
	}
	// 92 profile_rejected: no profile exists in this phase.
	if code, _, e := f.run(t, "run", "--node", f.node, "--profile", "claude-max", "--wait", "--", f.agent); code != int(core.ExitProfileRejected) || !strings.Contains(e, "claude-max") {
		t.Errorf("profile: exit %d %q", code, e)
	}
	// 97 start_failed.
	if code, _, e := f.wait(t, filepath.Join(t.TempDir(), "no-such-agent")); code != int(core.ExitStartFailed) {
		t.Errorf("missing program: exit %d %q", code, e)
	}
	// 98 usage.
	for _, args := range [][]string{
		{"run", "--node", f.node, "--wait", "--detach", "--", f.agent},
		{"run", "--node", f.node, "--detach", "--detach-on-interrupt", "--", f.agent},
		{"run", "--node", f.node, "--wait", "--bogus", "--", f.agent},
		{"run", "--node", f.node, "--wait", "--"},
	} {
		if code, _, e := f.run(t, args...); code != int(core.ExitUsage) {
			t.Errorf("%q: exit %d %q", args, code, e)
		}
	}

	// 93 sync_before_failed: the caller is ahead of the remote.
	if err := os.WriteFile(filepath.Join(f.caller, "ahead"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.caller, "add", "ahead")
	gitIn(t, f.caller, "commit", "-qm", "ahead")
	if code, _, e := f.wait(t, f.agent, "print:never"); code != int(core.ExitSyncBeforeFailed) || !strings.Contains(e, "ahead") {
		t.Errorf("caller ahead: exit %d %q", code, e)
	}
	gitIn(t, f.caller, "push", "-q")
	// … or its branch has no upstream.
	gitIn(t, f.caller, "switch", "-q", "-c", "topic")
	if code, _, e := f.wait(t, f.agent, "print:never"); code != int(core.ExitSyncBeforeFailed) || !strings.Contains(e, "not on origin") {
		t.Errorf("no upstream: exit %d %q", code, e)
	}
	gitIn(t, f.caller, "switch", "-q", "main")
	// … or the node's checkout is dirty.
	stray := filepath.Join(f.checkout(t), "stray")
	if err := os.WriteFile(stray, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, e := f.wait(t, f.agent, "print:never"); code != int(core.ExitSyncBeforeFailed) || !strings.Contains(e, "dirty") || out != "" {
		t.Errorf("dirty node checkout: exit %d %q %q", code, out, e)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}

	// 94 sync_after_failed: the remote refuses the session's push.
	marker := filepath.Join(t.TempDir(), "reject")
	hook := filepath.Join(f.remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nif [ -e '"+marker+"' ]; then echo refused >&2; exit 1; fi\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := f.remoteTip(t, "main")
	script := "echo work > w94 && git add w94 && git commit -qm w94 && touch '" + marker + "' && echo committed"
	if code, out, e := f.wait(t, "sh", "-c", script); code != int(core.ExitSyncAfterFailed) || out != "committed\n" || !strings.Contains(e, "could not push") {
		t.Errorf("rejected push: exit %d %q %q", code, out, e)
	}
	if got := f.remoteTip(t, "main"); got != before {
		t.Errorf("the rejected push moved the remote: %s -> %s", before, got)
	}
	if msg := gitIn(t, f.checkout(t), "log", "-1", "--format=%s", "main"); msg != "w94" {
		t.Errorf("the node's checkout lost the session's commit: head %q", msg)
	}
	// The next run pushes the work left behind.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if code, _, e := f.wait(t, f.agent, "print:next"); code != 0 {
		t.Errorf("run after the rejected push: exit %d %q", code, e)
	}
	if msg := gitIn(t, f.remote, "log", "-1", "--format=%s", "main"); msg != "w94" {
		t.Errorf("the left-behind commit was not pushed: remote head %q", msg)
	}

	// 95 session_lost: the session's supervisor is killed.
	token := fmt.Sprintf("lost-%d", time.Now().UnixNano())
	w, lines, wErr := f.startWait(t, nil, f.agent, "print:"+token, "sleep:1h")
	if l := nextLine(t, lines, 10*time.Second); l != token {
		t.Fatalf("first line %q", l)
	}
	m := sessionLine.FindStringSubmatch(wErr.String())
	if m == nil {
		t.Fatalf("no session line in %q", wErr.String())
	}
	rec := f.sessionRecord(t, m[1])
	agentPID, svPID := int(rec["pid"].(float64)), int(rec["supervisor_pid"].(float64))
	if err := syscall.Kill(svPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(agentPID, syscall.SIGKILL) // the orphan, not part of the check
	if code, _ := waitExit(t, w, 60*time.Second); code != int(core.ExitSessionLost) || !strings.Contains(wErr.String(), "lost") {
		t.Errorf("lost session: exit %d %q", code, wErr.String())
	}

	assertJSONLines(t, f.d.stderr.String())
}

// TestWaitStreamsAndPushes: the agent runs in the node's checkout at the
// remote tip, its output arrives while it runs, its commit is on the
// remote when `gaia run --wait` returns, and a warm start reaches the
// agent's first byte within a second.
func TestWaitStreamsAndPushes(t *testing.T) {
	f := newWaitFixture(t)
	tip := f.remoteTip(t, "main")
	script := "pwd -P; git rev-parse HEAD; for i in 1 2 3 4 5; do echo line $i; sleep 0.3; done; " +
		"echo agent > a && git add a && git commit -qm agent-commit"
	w, lines, errOut := f.startWait(t, nil, "sh", "-c", script)
	var got []string
	var at []time.Time
	for l := range lines {
		got = append(got, l)
		at = append(at, time.Now())
	}
	if code, _ := waitExit(t, w, 30*time.Second); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if len(got) != 7 {
		t.Fatalf("output %q", got)
	}
	wantDir, err := filepath.EvalSymlinks(f.checkout(t))
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != wantDir {
		t.Errorf("agent ran in %q, want the node's checkout %q", got[0], wantDir)
	}
	if got[1] != tip {
		t.Errorf("agent started at %s, want the remote tip %s", got[1], tip)
	}
	if spread := at[6].Sub(at[2]); spread < 800*time.Millisecond {
		t.Errorf("lines arrived within %s: not streamed as produced", spread)
	}
	// Pushed before gaia run returned.
	if msg := gitIn(t, f.remote, "log", "-1", "--format=%s", "main"); msg != "agent-commit" {
		t.Errorf("remote head %q after return, want the agent's commit", msg)
	}
	if l := cliSessions(t, f.xdg); len(l) != 1 || l[0].Sync != core.SyncDone || l[0].Repo != f.remote || l[0].Branch != "main" {
		t.Errorf("gaia ls: %+v", l)
	}
	// The caller fetches; Gaia never touched its checkout.
	if st := gitIn(t, f.caller, "status", "--porcelain"); st != "" {
		t.Errorf("caller's checkout changed: %q", st)
	}

	// Warm checkout: invocation to the agent's first byte.
	start := time.Now()
	w, lines, errOut = f.startWait(t, nil, f.agent, "print:first")
	if l := nextLine(t, lines, 10*time.Second); l != "first" {
		t.Fatalf("first line %q", l)
	}
	overhead := time.Since(start)
	if code, _ := waitExit(t, w, 10*time.Second); code != 0 {
		t.Fatalf("exit %d %q", code, errOut.String())
	}
	t.Logf("warm start to first byte: %s", overhead)
	if overhead > time.Second {
		t.Errorf("start-up overhead %s, want under 1s", overhead)
	}
}

// TestWaitArgv: argv with spaces, quotes, newlines, an empty string and a
// leading "-" reaches the agent unchanged.
func TestWaitArgv(t *testing.T) {
	f := newWaitFixture(t)
	args := []string{"a b", `q"uote's`, "new\nline", "", "-leading", "--", "--x=y", "$HOME", "*"}
	code, out, errOut := f.wait(t, append([]string{f.agent, "args"}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	var want strings.Builder
	for _, a := range args {
		want.WriteString(strconv.Quote(a) + "\n")
	}
	if out != want.String() {
		t.Errorf("agent saw\n%s\nwant\n%s", out, want.String())
	}
}

// TestWaitInterrupt: SIGINT stops the remote session, exits 130 and leaves
// no agent process; SIGTERM does the same.
func TestWaitInterrupt(t *testing.T) {
	f := newWaitFixture(t)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		token := fmt.Sprintf("intr-%d", time.Now().UnixNano())
		w, lines, errOut := f.startWait(t, nil, f.agent, "print:"+token, "sleep:1h")
		if l := nextLine(t, lines, 10*time.Second); l != token {
			t.Fatalf("first line %q", l)
		}
		m := sessionLine.FindStringSubmatch(errOut.String())
		if m == nil {
			t.Fatalf("no session line in %q", errOut.String())
		}
		if err := w.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		if code, _ := waitExit(t, w, 30*time.Second); code != 130 {
			t.Errorf("%s: exit %d %q", sig, code, errOut.String())
		}
		if processWith(t, token) {
			t.Errorf("%s: the agent is still running", sig)
		}
		s, ok := findSession(cliSessions(t, f.xdg), m[1])
		if !ok || s.State != core.SessionStopped {
			t.Errorf("%s: session %+v", sig, s)
		}
	}
}

// TestWaitDetachOnInterrupt: with --detach-on-interrupt an interrupt exits
// 130 at once; the session runs to its end and its commit is pushed.
func TestWaitDetachOnInterrupt(t *testing.T) {
	f := newWaitFixture(t)
	script := "echo started; sleep 2; echo late > l && git add l && git commit -qm late-commit"
	w, lines, errOut := f.startWait(t, []string{"--detach-on-interrupt"}, "sh", "-c", script)
	if l := nextLine(t, lines, 10*time.Second); l != "started" {
		t.Fatalf("first line %q", l)
	}
	m := sessionLine.FindStringSubmatch(errOut.String())
	if m == nil {
		t.Fatalf("no session line in %q", errOut.String())
	}
	if err := w.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	code, took := waitExit(t, w, 10*time.Second)
	if code != 130 || took > time.Second {
		t.Errorf("exit %d after %s, want 130 at once; stderr %q", code, took, errOut.String())
	}
	waitSession(t, f.xdg, m[1], 20*time.Second, "exited and pushed", func(s core.Session) bool {
		return s.State == core.SessionExited && s.Sync == core.SyncDone
	})
	if msg := gitIn(t, f.remote, "log", "-1", "--format=%s", "main"); msg != "late-commit" {
		t.Errorf("remote head %q, want the detached session's commit", msg)
	}
}
