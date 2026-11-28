package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/net/nettest"
)

func TestNodeFlagErrors(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "--host NAME is required"},
		{[]string{"--host", "h", "extra"}, `unexpected argument "extra"`},
		{[]string{"--host", "h", "--auth-key-file", "/k"}, "--auth-key-file needs --join"},
		{[]string{"--host", "h", "--name", "Bad_Name"}, "--name: invalid node name"},
		{[]string{"--host", "h", "--join", "--auth-key", testAuthKey}, "flag provided but not defined"},
		{[]string{"--bogus"}, "flag provided but not defined"},
	} {
		var errOut bytes.Buffer
		if code := runNode(c.args, &errOut, "linux"); code != 2 {
			t.Errorf("%v: exit %d, want 2", c.args, code)
		}
		if !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%v: stderr %q lacks %q", c.args, errOut.String(), c.want)
		}
		if strings.Contains(errOut.String(), testAuthKey) {
			t.Errorf("%v: stderr echoes the key", c.args)
		}
	}
}

// TestNodeStartErrors runs the start checks in a child, where entering
// internal/net hits the test trap, so each check is proven to run first.
func TestNodeStartErrors(t *testing.T) {
	skipIfNoDaemon(t)
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	joined := filepath.Join(t.TempDir(), "joined")
	if err := os.MkdirAll(filepath.Join(joined, "tsnet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(joined, "tsnet", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	noKey := []string{"GAIA_AUTHKEY=", "TS_AUTHKEY="}
	for _, c := range []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{"join without a key", noKey, []string{"--join"}, "--join needs a tailnet auth key"},
		{"join without a key, existing state", noKey, []string{"--join", "--state", joined}, "--join needs a tailnet auth key"},
		{"no state without join", []string{"GAIA_AUTHKEY=" + testAuthKey}, nil, "holds no tailnet state: run once with --join"},
		{"open state dir", noKey, []string{"--join", "--state", open}, "chmod 700 it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := startRole(t, "node", shortTempDir(t), c.env, append([]string{"--host", "gaia-host"}, c.args...)...)
			if code := d.wait(t); code != 1 {
				t.Fatalf("exit %d, want 1\nstderr:\n%s", code, d.stderr)
			}
			if !strings.Contains(d.stderr.String(), c.want) {
				t.Errorf("stderr lacks %q:\n%s", c.want, d.stderr)
			}
			if strings.Contains(d.stderr.String(), testAuthKey) {
				t.Error("stderr shows the key")
			}
		})
	}
}

// TestNodeTrapIsReal proves the trap the start checks rely on fires: with
// tsnet state present and no --join, the child enters internal/net.
func TestNodeTrapIsReal(t *testing.T) {
	skipIfNoDaemon(t)
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(state, "tsnet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "tsnet", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startRole(t, "node", shortTempDir(t), nil, "--host", "gaia-host", "--state", state)
	if code := d.wait(t); code != tailnetTrapCode {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, tailnetTrapCode, d.stderr)
	}
}

// byName indexes nodes by name.
func byName(l []core.Node) map[string]core.Node {
	m := map[string]core.Node{}
	for _, n := range l {
		m[n.Name] = n
	}
	return m
}

// waitNodes polls d's node list until ok accepts it.
func waitNodes(t *testing.T, d *daemon, within time.Duration, what string, ok func(map[string]core.Node) bool) map[string]core.Node {
	t.Helper()
	var m map[string]core.Node
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		status, body := d.get(t, core.NodesPath)
		var l core.ListNodesResponse
		if status == 200 && json.Unmarshal(body, &l) == nil {
			if m = byName(l.Nodes); ok(m) {
				return m
			}
		}
	}
	t.Fatalf("timed out waiting for %s; last list %+v\nstderr:\n%s", what, m, d.stderr)
	return nil
}

// cliNodes runs `gaia nodes -o json` in-process against the daemon in xdg.
func cliNodes(t *testing.T, xdg string) map[string]core.Node {
	t.Helper()
	code, out, errOut := runCLI(t, xdg, "nodes", "-o", "json")
	if code != 0 {
		t.Fatalf("gaia nodes: exit %d, stderr %q", code, errOut)
	}
	var l core.ListNodesResponse
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatal(err)
	}
	return byName(l.Nodes)
}

func online(names ...string) func(map[string]core.Node) bool {
	return func(m map[string]core.Node) bool {
		for _, n := range names {
			if m[n].State != core.NodeOnline {
				return false
			}
		}
		return true
	}
}

// TestNodesOverTailnet is the 0.7 scenario on a local test tailnet: a host
// with its own in-process node and two nodes, node-a and node-b.
func TestNodesOverTailnet(t *testing.T) {
	skipIfNoDaemon(t)
	// testcontrol checks the auth key on every registration, even from a
	// node key it already knows, where Tailscale and Headscale do not. The
	// restart below runs without a key, as a node does once joined, so this
	// control server requires none. The daemons are still given the key on
	// --join and every leak check below covers it; that Join presents the
	// key is TestJoinWrongAuthKeyFails's (internal/net).
	control := nettest.StartControl(t, "")
	url := control.HTTPTestServer.URL
	// The host is started without --name, on a fresh state directory, so
	// it asks for the default name; README's `--host` takes that name.
	hostName := defaultName(machineName(os.Hostname), "host")
	allowNet := allowTailnetEnv + "=1"
	withKey := []string{allowNet, "GAIA_AUTHKEY=" + testAuthKey, "TS_AUTHKEY="}
	noKey := []string{allowNet, "GAIA_AUTHKEY=", "TS_AUTHKEY="}

	xdgH, xdgA, xdgB := shortTempDir(t), shortTempDir(t), shortTempDir(t)
	stateH := filepath.Join(t.TempDir(), "host")
	stateA := filepath.Join(t.TempDir(), "a")
	stateB := filepath.Join(t.TempDir(), "b")
	hostArgs := []string{"--state", stateH, "--tailnet", "--control-url", url, "--node",
		"--allow", "node:node-a", "--allow", "node:node-b",
		"--heartbeat-interval", "250ms", "--missed-heartbeats", "2"}
	nodeArgs := func(name, state string, join bool) []string {
		a := []string{"--host", hostName, "--name", name, "--state", state, "--control-url", url}
		if join {
			a = append(a, "--join")
		}
		return a
	}

	h := startRole(t, "host", xdgH, withKey, hostArgs...)
	h.waitReadyWithin(t, 60*time.Second)
	a := startRole(t, "node", xdgA, withKey, nodeArgs("node-a", stateA, true)...)
	b := startRole(t, "node", xdgB, withKey, nodeArgs("node-b", stateB, true)...)
	a.waitReadyWithin(t, 10*time.Second)
	b.waitReadyWithin(t, 10*time.Second)

	all := online(hostName, "node-a", "node-b")
	first := waitNodes(t, h, 90*time.Second, "both nodes online on the host", all)
	if len(first) != 3 {
		t.Errorf("host lists %d nodes, want 3: %+v", len(first), first)
	}
	if first["node-a"].TailnetIdentity == "" || first["node-a"].TailnetIdentity == first["node-b"].TailnetIdentity {
		t.Errorf("identities %q %q", first["node-a"].TailnetIdentity, first["node-b"].TailnetIdentity)
	}
	// Each node's assigned tailnet name, as WhoIs gave it to the host, and
	// the host's own for its in-process node.
	for name, want := range map[string]string{"node-a": "node-a", "node-b": "node-b", hostName: hostName} {
		if got := first[name].TailnetName; got != want {
			t.Errorf("%s tailnet_name %q, want %q", name, got, want)
		}
	}
	if !logged(h.stderr.String(), `"msg":"tailnet name assigned"`, `"tailnet_name":"`+hostName+`"`) {
		t.Errorf("the host did not log its assigned tailnet name\nstderr:\n%s", h.stderr)
	}

	// The same list through each node's forwarder, and on the host.
	for _, xdg := range []string{xdgH, xdgA, xdgB} {
		m := cliNodes(t, xdg)
		if len(m) != 3 || !all(m) || m["node-a"].ID != first["node-a"].ID {
			t.Errorf("gaia nodes via %s: %+v", xdg, m)
		}
	}

	// Stopping node-a turns it offline; starting it again, without --join
	// and without a key, turns it online with the same id.
	if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := a.wait(t); code != 0 {
		t.Errorf("node-a exit %d\nstderr:\n%s", code, a.stderr)
	}
	waitNodes(t, h, 5*time.Second, "node-a offline", func(m map[string]core.Node) bool {
		return m["node-a"].State == core.NodeOffline && m["node-b"].State == core.NodeOnline
	})
	a2 := startRole(t, "node", xdgA, noKey, nodeArgs("node-a", stateA, false)...)
	a2.waitReadyWithin(t, 10*time.Second)
	m := waitNodes(t, h, 60*time.Second, "node-a online again", online("node-a"))
	if m["node-a"].ID != first["node-a"].ID || len(m) != 3 {
		t.Errorf("node-a after restart %+v, first %+v", m["node-a"], first["node-a"])
	}

	// Stopping the host leaves the nodes running; they re-register when it
	// returns.
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := h.wait(t); code != 0 {
		t.Errorf("host exit %d\nstderr:\n%s", code, h.stderr)
	}
	time.Sleep(2 * time.Second) // several heartbeat intervals with no host
	select {
	case <-b.exited:
		t.Fatalf("node-b exited with the host down: %v\nstderr:\n%s", b.exitErr, b.stderr)
	default:
	}
	if status, _ := b.get(t, core.HealthzPath); status != 200 {
		t.Errorf("node-b /healthz with the host down: %d", status)
	}
	code, _, errOut := runCLI(t, xdgB, "nodes")
	if code == 0 || !strings.Contains(errOut, "unreachable") {
		t.Errorf("gaia nodes on node-b with the host down: exit %d %q", code, errOut)
	}
	h2 := startRole(t, "host", xdgH, noKey, hostArgs...)
	h2.waitReadyWithin(t, 60*time.Second)
	restarted := time.Now()
	m = waitNodes(t, h2, 60*time.Second, "nodes online after the host restart", func(m map[string]core.Node) bool {
		return all(m) && m["node-b"].LastSeen.After(restarted) && m["node-a"].LastSeen.After(restarted)
	})
	if m["node-b"].ID != first["node-b"].ID {
		t.Errorf("node-b id changed: %s → %s", first["node-b"].ID, m["node-b"].ID)
	}
	if !logged(b.stderr.String(), "heartbeat failed; registering again") ||
		strings.Count(b.stderr.String(), `"msg":"node registered"`) < 2 {
		t.Errorf("node-b did not log the re-registration\nstderr:\n%s", b.stderr)
	}

	// No TCP listener outside tsnet on a node.
	if l, err := tcpListeners(b.cmd.Process.Pid); errors.Is(err, errNoSocketLister) {
		t.Log(err)
	} else if err != nil {
		t.Fatal(err)
	} else if len(l) > 0 {
		t.Errorf("node has TCP listeners outside tsnet: %q", l)
	}

	for _, d := range []*daemon{a2, b, h2} {
		if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if code := d.wait(t); code != 0 {
			t.Errorf("exit %d\nstderr:\n%s", code, d.stderr)
		}
	}

	// The auth key is in no log line or argument, and a node's state holds
	// nothing but tsnet's own state, with no auth key in it.
	for _, d := range []*daemon{a, a2, b, h, h2} {
		if strings.Contains(d.stderr.String(), testAuthKey) {
			t.Errorf("the auth key appears in %v's stderr", d.cmd.Args[1:3])
		}
		for _, arg := range d.cmd.Args {
			if strings.Contains(arg, testAuthKey) {
				t.Error("the auth key is a process argument")
			}
		}
		assertJSONLines(t, d.stderr.String())
	}
	for _, state := range []string{stateA, stateB} {
		entries, err := os.ReadDir(state)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if strings.Join(names, " ") != nameFile+" "+sessionsDirName+" tsnet "+workspaceDirName {
			t.Errorf("node state %s holds %q, want only the name file, the sessions, tsnet and the workspace", state, names)
		}
		files := 0
		err = filepath.WalkDir(state, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			files++
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if bytes.Contains(b, []byte(testAuthKey)) {
				t.Errorf("%s holds the auth key", p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if files == 0 {
			t.Errorf("node state %s holds no files, so the key check covers nothing", state)
		}
	}
}
