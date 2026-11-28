//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/techspeque/gaia/cmd/gaia/cli"
	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/net/nettest"
	"github.com/techspeque/gaia/internal/sock"
)

// cliProc is a gaia CLI child process, on a real terminal (a PTY) or on
// pipes.
type cliProc struct {
	cmd    *exec.Cmd
	in     io.Writer
	out    *lockedBuffer // stdout and stderr, as the caller would see them
	tty    *os.File      // the terminal the CLI runs on; nil on pipes
	before *term.State   // tty's state before the CLI started
	done   chan struct{}
	code   int
}

// startCLI starts `gaia args…` against the daemon in xdg, on a PTY when
// onTTY.
func startCLI(t *testing.T, xdg string, onTTY bool, args ...string) *cliProc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &cliProc{out: &lockedBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(exe, args...)
	p.cmd.Env = append(os.Environ(), runMainEnv+"=1", "XDG_RUNTIME_DIR="+xdg)
	if onTTY {
		ptmx, tty, err := pty.Open()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ptmx.Close(); _ = tty.Close() })
		if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
			t.Fatal(err)
		}
		if p.before, err = term.GetState(int(tty.Fd())); err != nil {
			t.Fatal(err)
		}
		p.tty, p.in = tty, ptmx
		p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = tty, tty, tty
		go func() { _, _ = io.Copy(p.out, ptmx) }()
	} else {
		w, err := p.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		p.in = w
		p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := p.cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			p.code = ee.ExitCode()
		} else if err != nil {
			p.code = -1
		}
		close(p.done)
	}()
	t.Cleanup(func() { _ = p.cmd.Process.Kill(); <-p.done })
	return p
}

func (p *cliProc) waitOut(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(p.out.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("output %q does not contain %q", p.out.String(), s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *cliProc) write(t *testing.T, s string) {
	t.Helper()
	if _, err := p.in.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

// wait waits for the CLI to exit and returns its exit code.
func (p *cliProc) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("the CLI did not exit; output %q", p.out.String())
	}
	return p.code
}

// raw reports whether the CLI's terminal is not in the state it started
// in.
func (p *cliProc) changed(t *testing.T) bool {
	t.Helper()
	now, err := term.GetState(int(p.tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return *now != *p.before
}

// checkRestored fails unless the CLI's terminal is back in its first state.
func (p *cliProc) checkRestored(t *testing.T, when string) {
	t.Helper()
	if p.changed(t) {
		t.Errorf("%s: the terminal was not restored", when)
	}
}

// waitRaw waits until the CLI has changed its terminal's mode.
func (p *cliProc) waitRaw(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !p.changed(t) {
		if time.Now().After(deadline) {
			t.Fatal("the CLI did not put its terminal in raw mode")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAttachLocalOnly: interactive and headless sessions on `gaia host
// --node`, with the tailnet trap armed. The caller's terminal is a real
// PTY, and its mode must come back after a detach, after the session ends
// and after the daemon is killed.
func TestAttachLocalOnly(t *testing.T) {
	skipIfNoDaemon(t)
	agent := fakeAgent(t)
	xdg := shortTempDir(t)
	d := startDaemon(t, xdg, "--node", "--state", shortState(t))
	d.waitReady(t)
	waitLocalOnline(t, d)
	name := d.nodes(t)[0].Name

	// run --interactive: the agent gets a terminal of the caller's size,
	// takes input, and Ctrl-] detaches.
	a := startCLI(t, xdg, true, "run", "--node", name, "--interactive", "--",
		agent, "tty", "print:ready", "echo", "size", "echo", "sleep:1s", "print:bye", "exit:4")
	a.waitOut(t, "ready")
	a.waitRaw(t)
	// The size goes out on attach, before any input: by the time the agent
	// has read a line, its terminal has the caller's size.
	a.write(t, "one\r")
	a.waitOut(t, "got:one")
	a.waitOut(t, "size=")
	if out := a.out.String(); !strings.Contains(out, "tty=yes") || !strings.Contains(out, "size=30x100") {
		t.Errorf("the agent's terminal: %q", out)
	}
	a.write(t, string(rune(cli.DetachKey)))
	if code := a.wait(t); code != 0 {
		t.Fatalf("detach: exit %d %q", code, a.out.String())
	}
	a.checkRestored(t, "after detach")
	var id string
	for _, s := range cliSessions(t, xdg) {
		if s.Mode == core.SessionInteractive {
			id = s.ID.String()
			if s.State != core.SessionRunning {
				t.Errorf("after detach the session is %s", s.State)
			}
		}
	}
	if id == "" || !strings.Contains(a.out.String(), "detached from session "+id) {
		t.Fatalf("no interactive session running after detach; output %q", a.out.String())
	}

	// Two clients: a terminal and a pipe. Both replay the screen so far;
	// input from the pipe reaches the agent, and both see the end.
	b := startCLI(t, xdg, true, "attach", id)
	b.waitOut(t, "got:one")
	c := startCLI(t, xdg, false, "attach", id)
	c.waitOut(t, "got:one")
	c.write(t, "two\n")
	b.waitOut(t, "got:two")
	c.waitOut(t, "got:two")
	for name, p := range map[string]*cliProc{"terminal": b, "pipe": c} {
		if code := p.wait(t); code != 4 {
			t.Errorf("%s client: exit %d, want the agent's 4; output %q", name, code, p.out.String())
		}
		if !strings.Contains(p.out.String(), "bye") || !strings.Contains(p.out.String(), "exited (exit code 4)") {
			t.Errorf("%s client output %q", name, p.out.String())
		}
	}
	b.checkRestored(t, "after the session ended")

	// Headless: output only, input refused, the terminal left alone.
	hid := runSession(t, xdg, name, agent, "print:hlog", "sleep:1500ms", "print:hdone")
	h := startCLI(t, xdg, true, "attach", hid)
	h.waitOut(t, "hlog")
	h.write(t, "typed\r")
	if code := h.wait(t); code != 0 {
		t.Errorf("headless attach: exit %d %q", code, h.out.String())
	}
	if out := h.out.String(); !strings.Contains(out, "input is not accepted") || !strings.Contains(out, "hdone") {
		t.Errorf("headless attach output %q", out)
	}
	h.checkRestored(t, "after a headless attach")

	// Unknown session: 1.
	if code, _, errOut := runCLI(t, xdg, "attach", core.NewSessionID([16]byte{9}).String()); code != 1 || !strings.Contains(errOut, "no session") {
		t.Errorf("attach to an unknown session: exit %d %q", code, errOut)
	}

	// A foreign origin is refused on the socket; no origin is admitted.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return sock.Dial(ctx, d.rtDir)
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws://gaia" + core.SessionTTYPath(mustID(t, hid))
	if conn, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: http.Header{"Origin": {"https://evil.example"}}}); err == nil {
		_ = conn.CloseNow()
		t.Error("a foreign origin was admitted")
	} else if res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: %v %v", res, err)
	}
	if conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc}); err != nil {
		t.Errorf("no origin: %v", err)
	} else {
		_ = conn.CloseNow()
	}

	// The daemon killed mid-attachment: 95, and the terminal restored.
	k := startCLI(t, xdg, true, "run", "--node", name, "--interactive", "--", agent, "print:up", "sleep:3s")
	k.waitOut(t, "up")
	k.waitRaw(t)
	if err := d.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if code := k.wait(t); code != int(core.ExitSessionLost) {
		t.Errorf("daemon killed: exit %d %q", code, k.out.String())
	}
	if !strings.Contains(k.out.String(), "may still be running") {
		t.Errorf("daemon killed: output %q", k.out.String())
	}
	k.checkRestored(t, "after the connection dropped")
	if strings.Contains(d.stderr.String(), tailnetTrapMsg) {
		t.Error("local-only attach entered internal/net")
	}
}

func mustID(t *testing.T, s string) core.SessionID {
	t.Helper()
	id, err := core.ParseSessionID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAttachOverTailnet: an interactive session on node-b, started from the
// host machine and attached from node-b at the same time. Both see the
// replay and the output, and input from either reaches the agent.
func TestAttachOverTailnet(t *testing.T) {
	skipIfNoDaemon(t)
	agent := fakeAgent(t)
	control := nettest.StartControl(t, "").HTTPTestServer.URL
	hostName := defaultName(machineName(os.Hostname), "host")
	env := []string{allowTailnetEnv + "=1", "GAIA_AUTHKEY=" + testAuthKey, "TS_AUTHKEY="}

	xdgH, xdgB := shortTempDir(t), shortTempDir(t)
	h := startRole(t, "host", xdgH, env, "--state", shortState(t), "--tailnet", "--control-url", control, "--node",
		"--allow", "node:node-b", "--heartbeat-interval", "250ms", "--missed-heartbeats", "2")
	h.waitReadyWithin(t, 60*time.Second)
	b := startRole(t, "node", xdgB, env, "--host", hostName, "--name", "node-b", "--state", shortState(t),
		"--control-url", control, "--join")
	b.waitReadyWithin(t, 10*time.Second)
	waitNodes(t, h, 90*time.Second, "node-b online", online(hostName, "node-b"))

	onHost := startCLI(t, xdgH, false, "run", "--node", "node-b", "--interactive", "--",
		agent, "tty", "print:ready", "echo", "echo", "print:bye", "exit:2")
	onHost.waitOut(t, "ready")
	if !strings.Contains(onHost.out.String(), "tty=yes") {
		t.Errorf("the agent on node-b has no terminal: %q", onHost.out.String())
	}
	var id string
	for _, s := range cliSessions(t, xdgH) {
		if s.Mode == core.SessionInteractive && s.NodeName == "node-b" {
			id = s.ID.String()
		}
	}
	if id == "" {
		t.Fatal("no interactive session on node-b")
	}
	onB := startCLI(t, xdgB, false, "attach", id)
	onB.waitOut(t, "ready") // the replay
	onHost.write(t, "from-host\n")
	onHost.waitOut(t, "got:from-host")
	onB.waitOut(t, "got:from-host")
	onB.write(t, "from-b\n")
	onHost.waitOut(t, "got:from-b")
	onB.waitOut(t, "got:from-b")
	for name, p := range map[string]*cliProc{"host": onHost, "node-b": onB} {
		if code := p.wait(t); code != 2 {
			t.Errorf("%s client: exit %d, want 2; output %q", name, code, p.out.String())
		}
	}
	for _, d := range []*daemon{h, b} {
		if strings.Contains(d.stderr.String(), testAuthKey) {
			t.Errorf("the auth key appears in %v's stderr", d.cmd.Args[1:3])
		}
	}
}
