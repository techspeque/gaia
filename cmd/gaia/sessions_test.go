package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/net/nettest"
)

var (
	fakeAgentOnce sync.Once
	fakeAgentPath string
	fakeAgentErr  error
)

// fakeAgent builds testdata/fakeagent once and returns its path.
func fakeAgent(t *testing.T) string {
	t.Helper()
	fakeAgentOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeagent")
		if err != nil {
			fakeAgentErr = err
			return
		}
		fakeAgentPath = filepath.Join(dir, "fakeagent")
		out, err := exec.Command("go", "build", "-o", fakeAgentPath, "github.com/techspeque/gaia/testdata/fakeagent").CombinedOutput()
		if err != nil {
			fakeAgentErr = fmt.Errorf("building the fake agent: %v\n%s", err, out)
		}
	})
	if fakeAgentErr != nil {
		t.Fatal(fakeAgentErr)
	}
	return fakeAgentPath
}

// shortState returns a state directory short enough that the supervisors'
// socket paths under it stay within the unix socket path limit.
func shortState(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "gs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return filepath.Join(d, "s")
}

// cliSessions runs `gaia ls -o json` against the daemon in xdg.
func cliSessions(t *testing.T, xdg string) []core.Session {
	t.Helper()
	code, out, errOut := runCLI(t, xdg, "ls", "-o", "json")
	if code != 0 {
		t.Fatalf("gaia ls: exit %d, stderr %q", code, errOut)
	}
	var l core.ListSessionsResponse
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("gaia ls -o json: %v\n%s", err, out)
	}
	return l.Sessions
}

func findSession(l []core.Session, id string) (core.Session, bool) {
	for _, s := range l {
		if s.ID.String() == id {
			return s, true
		}
	}
	return core.Session{}, false
}

// waitSession polls `gaia ls` in xdg until the session with id satisfies ok.
func waitSession(t *testing.T, xdg, id string, within time.Duration, what string, ok func(core.Session) bool) core.Session {
	t.Helper()
	deadline := time.Now().Add(within)
	var last core.Session
	for time.Now().Before(deadline) {
		if s, found := findSession(cliSessions(t, xdg), id); found {
			last = s
			if ok(s) {
				return s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %s: %s not reached; last %+v", id, what, last)
	return last
}

// runSession runs `gaia run --detach` in xdg and returns the session id.
func runSession(t *testing.T, xdg, nodeName string, argv ...string) string {
	t.Helper()
	code, out, errOut := runCLI(t, xdg, append([]string{"run", "--node", nodeName, "--detach", "--"}, argv...)...)
	if code != 0 {
		t.Fatalf("gaia run: exit %d, stderr %q", code, errOut)
	}
	id := strings.TrimSpace(out)
	if _, err := core.ParseSessionID(id); err != nil {
		t.Fatalf("gaia run printed %q: %v", out, err)
	}
	return id
}

// followTimed runs `gaia logs --follow id` as a child process against the
// daemon in xdg and returns its output lines with the time each arrived,
// and its exit code.
func followTimed(t *testing.T, xdg, id string) ([]string, []time.Time, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "logs", id, "--follow")
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "XDG_RUNTIME_DIR="+xdg)
	var errOut lockedBuffer
	cmd.Stderr = &errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	var at []time.Time
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		at = append(at, time.Now())
	}
	code := 0
	if err := cmd.Wait(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	if code != 0 {
		t.Logf("gaia logs --follow stderr: %s", errOut.String())
	}
	return lines, at, code
}

func streamLines(n int) []string {
	var l []string
	for i := 1; i <= n; i++ {
		l = append(l, fmt.Sprintf("line %d", i))
	}
	return l
}

// checkFollow checks a followed session's output: every line once, in
// order, and the first ones before the session ended.
func checkFollow(t *testing.T, lines []string, at []time.Time, code int, want []string) {
	t.Helper()
	if code != 0 {
		t.Fatalf("gaia logs --follow: exit %d", code)
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gaia logs --follow printed %q, want %q", lines, want)
	}
	// The agent writes a line every 300 ms: output streamed as produced
	// spreads over most of that time, not one burst at the end.
	if spread := at[len(at)-1].Sub(at[0]); spread < 800*time.Millisecond {
		t.Errorf("lines arrived within %s of each other: not streamed as produced", spread)
	}
}

// TestSessionsLocalOnly: run, ls, logs, stop and the refusals against
// `gaia host --node` with no tailnet; the tailnet trap is armed, so no
// tailnet code runs.
func TestSessionsLocalOnly(t *testing.T) {
	skipIfNoDaemon(t)
	agent := fakeAgent(t)
	xdg := shortTempDir(t)
	d := startDaemon(t, xdg, "--node", "--state", shortState(t))
	d.waitReady(t)
	waitLocalOnline(t, d)
	name := d.nodes(t)[0].Name

	// Refusals: empty argv (usage), unknown node (91); neither leaves a
	// session.
	if code, _, errOut := runCLI(t, xdg, "run", "--node", name, "--detach", "--"); code != int(core.ExitUsage) || !strings.Contains(errOut, "empty") {
		t.Errorf("empty argv: exit %d %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, xdg, "run", "--node", "nosuch", "--detach", "--", agent); code != int(core.ExitNodeUnavailable) || !strings.Contains(errOut, "nosuch") {
		t.Errorf("unknown node: exit %d %q", code, errOut)
	}
	if l := cliSessions(t, xdg); len(l) != 0 {
		t.Errorf("refused runs left sessions: %+v", l)
	}

	// A streaming session, followed to its end.
	id := runSession(t, xdg, name, agent, "stream:6:300ms", "exit:3")
	s, ok := findSession(cliSessions(t, xdg), id)
	if !ok || s.NodeName != name || s.Mode != core.SessionHeadless || s.State.Terminal() {
		t.Errorf("gaia ls right after run: %+v (found %v)", s, ok)
	}
	lines, at, code := followTimed(t, xdg, id)
	checkFollow(t, lines, at, code, streamLines(6))
	s = waitSession(t, xdg, id, 5*time.Second, "exited", func(s core.Session) bool { return s.State == core.SessionExited })
	if s.ExitCode == nil || *s.ExitCode != 3 || s.EndedAt == nil {
		t.Errorf("ended session %+v", s)
	}
	code, out, errOut := runCLI(t, xdg, "logs", id)
	if code != 0 || out != strings.Join(streamLines(6), "\n")+"\n" {
		t.Errorf("gaia logs: exit %d %q %q", code, out, errOut)
	}

	// Stop: stopped, not failed, with the SIGTERM exit code; stopping again
	// returns it unchanged.
	id2 := runSession(t, xdg, name, agent, "print:up", "sleep:1h")
	waitSession(t, xdg, id2, 5*time.Second, "running", func(s core.Session) bool { return s.State == core.SessionRunning })
	code, out, errOut = runCLI(t, xdg, "stop", id2)
	if code != 0 || !strings.Contains(out, "stopped (exit code 143)") {
		t.Errorf("gaia stop: exit %d %q %q", code, out, errOut)
	}
	s = waitSession(t, xdg, id2, 5*time.Second, "stopped", func(s core.Session) bool { return s.State.Terminal() })
	if s.State != core.SessionStopped || s.ExitCode == nil || *s.ExitCode != 143 {
		t.Errorf("stopped session %+v", s)
	}
	if code, out, _ := runCLI(t, xdg, "stop", id2); code != 0 || !strings.Contains(out, "stopped") {
		t.Errorf("second stop: exit %d %q", code, out)
	}
	if code, out, _ := runCLI(t, xdg, "logs", id2); code != 0 || out != "up\n" {
		t.Errorf("logs of the stopped session: exit %d %q", code, out)
	}
	unknown := core.NewSessionID([16]byte{9}).String()
	if code, _, errOut := runCLI(t, xdg, "stop", unknown); code != 1 || !strings.Contains(errOut, "no session") {
		t.Errorf("stop of an unknown session: exit %d %q", code, errOut)
	}

	// A program that does not exist: start_failed, and the session failed.
	code, _, errOut = runCLI(t, xdg, "run", "--node", name, "--detach", "--", filepath.Join(t.TempDir(), "no-such-agent"))
	if code != int(core.ExitStartFailed) {
		t.Errorf("missing program: exit %d %q", code, errOut)
	}
	for _, s := range cliSessions(t, xdg) {
		if s.ID.String() != id && s.ID.String() != id2 && s.State != core.SessionFailed {
			t.Errorf("the refused start left %+v", s)
		}
	}

	select {
	case <-d.exited:
		t.Fatalf("daemon exited: %v\nstderr:\n%s", d.exitErr, d.stderr)
	default:
	}
	if strings.Contains(d.stderr.String(), tailnetTrapMsg) {
		t.Error("local-only sessions entered internal/net")
	}
	assertJSONLines(t, d.stderr.String())
}

// TestSessionsOverTailnet: a host with its own node and node-b on a test
// tailnet. A run on the host machine starts the agent on node-b; both
// machines list it; node-b follows and stops sessions through its
// forwarder; a run on node-b once it is offline exits 91 and leaves no
// session.
func TestSessionsOverTailnet(t *testing.T) {
	skipIfNoDaemon(t)
	agent := fakeAgent(t)
	control := nettest.StartControl(t, "")
	url := control.HTTPTestServer.URL
	hostName := defaultName(machineName(os.Hostname), "host")
	env := []string{allowTailnetEnv + "=1", "GAIA_AUTHKEY=" + testAuthKey, "TS_AUTHKEY="}

	xdgH, xdgB := shortTempDir(t), shortTempDir(t)
	h := startRole(t, "host", xdgH, env, "--state", shortState(t), "--tailnet", "--control-url", url, "--node",
		"--allow", "node:node-b", "--heartbeat-interval", "250ms", "--missed-heartbeats", "2")
	h.waitReadyWithin(t, 60*time.Second)
	b := startRole(t, "node", xdgB, env, "--host", hostName, "--name", "node-b", "--state", shortState(t),
		"--control-url", url, "--join")
	b.waitReadyWithin(t, 10*time.Second)
	waitNodes(t, h, 90*time.Second, "node-b online", online(hostName, "node-b"))

	// Started from the host machine, on node-b; listed on both.
	id := runSession(t, xdgH, "node-b", agent, "stream:6:300ms", "exit:0")
	for _, xdg := range []string{xdgH, xdgB} {
		s, ok := findSession(cliSessions(t, xdg), id)
		if !ok || s.NodeName != "node-b" {
			t.Errorf("gaia ls via %s: %+v (found %v)", xdg, s, ok)
		}
	}
	// Followed from node-b, through its forwarder and the host to node-b.
	lines, at, code := followTimed(t, xdgB, id)
	checkFollow(t, lines, at, code, streamLines(6))
	s := waitSession(t, xdgH, id, 10*time.Second, "exited", func(s core.Session) bool { return s.State.Terminal() })
	if s.State != core.SessionExited || s.ExitCode == nil || *s.ExitCode != 0 {
		t.Errorf("ended session %+v", s)
	}

	// Stopped from node-b.
	id2 := runSession(t, xdgB, "node-b", agent, "sleep:1h")
	waitSession(t, xdgH, id2, 10*time.Second, "running", func(s core.Session) bool { return s.State == core.SessionRunning })
	if code, out, errOut := runCLI(t, xdgB, "stop", id2); code != 0 || !strings.Contains(out, "stopped (exit code 143)") {
		t.Errorf("gaia stop via node-b: exit %d %q %q", code, out, errOut)
	}
	waitSession(t, xdgH, id2, 10*time.Second, "stopped", func(s core.Session) bool { return s.State == core.SessionStopped })

	// The host's own node still runs sessions in-process.
	id3 := runSession(t, xdgH, hostName, agent, "print:local")
	waitSession(t, xdgH, id3, 10*time.Second, "exited", func(s core.Session) bool { return s.State == core.SessionExited })

	// node-b down: a run on it exits 91 and records nothing.
	if err := b.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := b.wait(t); code != 0 {
		t.Errorf("node-b exit %d\nstderr:\n%s", code, b.stderr)
	}
	waitNodes(t, h, 10*time.Second, "node-b offline", func(m map[string]core.Node) bool {
		return m["node-b"].State == core.NodeOffline
	})
	before := len(cliSessions(t, xdgH))
	if code, _, errOut := runCLI(t, xdgH, "run", "--node", "node-b", "--detach", "--", agent); code != int(core.ExitNodeUnavailable) || !strings.Contains(errOut, "offline") {
		t.Errorf("run on offline node-b: exit %d %q", code, errOut)
	}
	if after := len(cliSessions(t, xdgH)); after != before {
		t.Errorf("the refused run left a session: %d → %d", before, after)
	}

	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := h.wait(t); code != 0 {
		t.Errorf("host exit %d\nstderr:\n%s", code, h.stderr)
	}
	for _, d := range []*daemon{h, b} {
		if strings.Contains(d.stderr.String(), testAuthKey) {
			t.Errorf("the auth key appears in %v's stderr", d.cmd.Args[1:3])
		}
		assertJSONLines(t, d.stderr.String())
	}
}
