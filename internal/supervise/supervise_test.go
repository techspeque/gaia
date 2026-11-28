//go:build linux || darwin

package supervise

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// The test binary doubles as the supervisor and as a starter process,
// chosen by GAIA_TEST_ROLE.
const roleEnv = "GAIA_TEST_ROLE"

var fakeAgent string // path of the built testdata/fakeagent

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case "supervise":
		os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	case "starter":
		os.Exit(starter())
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
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// starter starts a detached supervisor for the session dir and spec in its
// environment, prints "SUP <pid>" and the ready line, then waits forever.
func starter() int {
	cmd := supervisorCmd(os.Getenv("GAIA_TEST_DIR"))
	cmd.Stdin = strings.NewReader(os.Getenv("GAIA_TEST_SPEC"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 1
	}
	if err := cmd.Start(); err != nil {
		return 1
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	fmt.Printf("SUP %d\n%s", cmd.Process.Pid, line)
	time.Sleep(time.Hour) // until the test kills it
	return 1
}

func supervisorCmd(dir string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "--session-dir", dir)
	cmd.Env = append(os.Environ(), roleEnv+"=supervise")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

func newSessionID(t *testing.T) core.SessionID {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return core.NewSessionID(b)
}

// newSpec is a spec running the fake agent with steps, in a fresh working
// directory, with only PATH in its environment.
func newSpec(t *testing.T, mode core.SessionMode, steps ...string) Spec {
	t.Helper()
	return Spec{
		SessionID: newSessionID(t),
		Mode:      mode,
		Argv:      append([]string{fakeAgent}, steps...),
		WorkDir:   t.TempDir(),
		Env:       []string{"PATH=" + os.Getenv("PATH")},
	}
}

// sessionDir returns a session directory path short enough for a socket.
func sessionDir(t *testing.T, id core.SessionID) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "gs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return filepath.Join(base, id.String())
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// session is a supervisor started by the test.
type session struct {
	t      *testing.T
	dir    string
	ready  Response
	cmd    *exec.Cmd
	stderr *syncBuffer
	done   chan struct{}
	exit   int
	client *Client
}

// launch starts a detached supervisor for spec in dir and reads its ready
// line.
func launch(t *testing.T, dir string, spec Spec) *session {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := supervisorCmd(dir)
	cmd.Stdin = bytes.NewReader(b)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &session{t: t, dir: dir, cmd: cmd, stderr: &syncBuffer{}, done: make(chan struct{}), client: NewClient(dir)}
	cmd.Stdout = w
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	go func() {
		_ = cmd.Wait()
		s.exit = cmd.ProcessState.ExitCode()
		close(s.done)
	}()
	line, err := bufio.NewReader(r).ReadBytes('\n')
	_ = r.Close()
	if err != nil {
		<-s.done
		t.Fatalf("no ready line (exit %d): %v; stderr: %s", s.exit, err, s.stderr)
	}
	if err := json.Unmarshal(line, &s.ready); err != nil {
		t.Fatalf("ready line %q: %v", line, err)
	}
	t.Cleanup(func() {
		select {
		case <-s.done:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = s.client.Stop(ctx)
		select {
		case <-s.done:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	return s
}

// start is launch for a spec that must start.
func start(t *testing.T, spec Spec) *session {
	t.Helper()
	s := launch(t, sessionDir(t, spec.SessionID), spec)
	if !s.ready.OK || s.ready.PID <= 0 {
		t.Fatalf("ready line %+v; stderr: %s", s.ready, s.stderr)
	}
	return s
}

// wait waits for the supervisor to exit and returns the final state.
func (s *session) wait() core.SupervisorState {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		s.t.Fatalf("supervisor still running; stderr: %s", s.stderr)
	}
	if s.exit != 0 {
		s.t.Fatalf("supervisor exited %d; stderr: %s", s.exit, s.stderr)
	}
	st, err := ReadState(s.dir)
	if err != nil {
		s.t.Fatal(err)
	}
	if !st.Ended() {
		s.t.Fatalf("state not ended after exit: %+v", st)
	}
	return st
}

func (s *session) scrollback() string {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, ScrollbackFile))
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

// waitFor polls the scrollback until it contains want.
func (s *session) waitFor(want string) string {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := s.scrollback()
		if strings.Contains(got, want) {
			return got
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("scrollback never contained %q: %q; stderr: %s", want, got, s.stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, err error, code core.ErrorCode) {
	t.Helper()
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("error %v, want code %s", err, code)
	}
}

func TestExitCodeRecorded(t *testing.T) {
	for _, code := range []int{0, 3, 255} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			s := start(t, newSpec(t, core.SessionHeadless, "print:hello", "exit:"+strconv.Itoa(code)))
			st := s.wait()
			if st.ExitCode == nil || *st.ExitCode != code || st.Signal != "" {
				t.Fatalf("exit_code %v signal %q, want %d and none", st.ExitCode, st.Signal, code)
			}
			if st.PID != s.ready.PID {
				t.Errorf("state pid %d, ready pid %d", st.PID, s.ready.PID)
			}
			if got := s.scrollback(); got != "hello\n" {
				t.Errorf("scrollback %q", got)
			}
		})
	}
}

func TestSignalDeathRecorded(t *testing.T) {
	for _, sig := range []string{"KILL", "TERM"} {
		t.Run(sig, func(t *testing.T) {
			s := start(t, newSpec(t, core.SessionHeadless, "kill:"+sig))
			st := s.wait()
			if st.ExitCode != nil || st.Signal != "SIG"+sig {
				t.Fatalf("exit_code %v signal %q, want none and SIG%s", st.ExitCode, st.Signal, sig)
			}
		})
	}
}

func TestSecretNeverReachesScrollback(t *testing.T) {
	const secret = "sk-test-SECRET-0123456789abcdef"
	spec := newSpec(t, core.SessionHeadless,
		"envsplit:TOKEN", "write:inline ", "env:TOKEN", "print:partial sk-test-SEC end", "exit:0")
	spec.Env = append(spec.Env, "TOKEN="+secret)
	spec.Secrets = []string{secret}
	s := start(t, spec)

	var streamed bytes.Buffer
	if _, err := s.client.Stream(ctxT(t), 0, &streamed); err != nil {
		t.Fatal(err)
	}
	st := s.wait()
	got := s.scrollback()
	want := "TOKEN=" + core.Redacted + "\ninline TOKEN=" + core.Redacted + "\npartial sk-test-SEC end\n"
	if got != want {
		t.Errorf("scrollback %q, want %q", got, want)
	}
	if streamed.String() != got {
		t.Errorf("stream %q differs from scrollback %q", streamed.String(), got)
	}
	state, err := os.ReadFile(filepath.Join(s.dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]string{"scrollback": got, "state": string(state), "stderr": s.stderr.String()} {
		if strings.Contains(b, secret) || strings.Contains(b, secret[:len(secret)/2+1]) {
			t.Errorf("%s holds the secret: %q", name, b)
		}
	}
	if strings.Contains(string(state), "PATH=") {
		t.Errorf("state file holds the environment: %s", state)
	}
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Errorf("exit %v", st.ExitCode)
	}
}

func TestSecretInArgvRedactedInStateAndStatus(t *testing.T) {
	const secret = "ARGV_SECRET_0123456789"
	spec := newSpec(t, core.SessionHeadless, "print:"+secret, "sleep:1h")
	spec.Secrets = []string{secret}
	s := start(t, spec)
	// The agent ran the original argv: it printed the secret, redacted.
	s.waitFor("[REDACTED]")
	wantArgv := []string{fakeAgent, "print:" + core.Redacted, "sleep:1h"}

	st, err := s.client.Status(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(st.State.Argv) != fmt.Sprint(wantArgv) {
		t.Errorf("status argv %q, want %q", st.State.Argv, wantArgv)
	}
	conn, err := net.Dial("unix", filepath.Join(s.dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "{\"op\":\"status\"}\n")
	rawStatus, _ := io.ReadAll(conn)
	conn.Close()
	state, err := os.ReadFile(filepath.Join(s.dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := s.client.Stop(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	final := s.wait()
	finalFile, _ := os.ReadFile(filepath.Join(s.dir, StateFile))
	for name, b := range map[string]string{
		"status": string(rawStatus), "state.json": string(state), "final state.json": string(finalFile),
		"stop status": fmt.Sprint(stopped.State.Argv), "scrollback": s.scrollback(),
	} {
		if strings.Contains(b, secret) {
			t.Errorf("%s holds the secret: %s", name, b)
		}
	}
	if fmt.Sprint(final.Argv) != fmt.Sprint(wantArgv) {
		t.Errorf("state argv %q, want %q", final.Argv, wantArgv)
	}
}

// cutWriter cancels its context once it has taken at least limit bytes.
type cutWriter struct {
	buf    *bytes.Buffer
	taken  int
	limit  int
	cancel context.CancelFunc
}

func (w *cutWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	w.taken += len(p)
	if w.taken >= w.limit {
		w.cancel()
	}
	return len(p), nil
}

func TestStreamReconnectFromOffset(t *testing.T) {
	s := start(t, newSpec(t, core.SessionHeadless, "stream:400:2ms", "exit:0"))
	var all bytes.Buffer
	var offset int64
	cuts := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		n, err := s.client.Stream(ctx, offset, &cutWriter{buf: &all, limit: 300, cancel: cancel})
		cancel()
		offset += n
		if err == nil {
			break
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream from %d: %v", offset-n, err)
		}
		cuts++
	}
	s.wait()
	if cuts < 3 {
		t.Errorf("only %d reconnections; the test proves little", cuts)
	}
	if got := s.scrollback(); all.String() != got {
		t.Fatalf("reassembled stream (%d bytes) differs from the scrollback (%d bytes)", all.Len(), len(got))
	}
	if !strings.HasSuffix(all.String(), "line 400\n") {
		t.Errorf("stream ends %q", all.String()[max(0, all.Len()-20):])
	}
}

// rawStream sends a stream request over a plain socket connection and
// returns the conn, for wire-level checks that do not use Client.
func rawStream(t *testing.T, dir string, offset int64) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", filepath.Join(dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c, "{\"op\":\"stream\",\"offset\":%d}\n", offset); err != nil {
		t.Fatal(err)
	}
	return c
}

// readRaw reads the header line and every byte after it until the
// supervisor closes the connection.
func readRaw(t *testing.T, c net.Conn) (header, body string) {
	t.Helper()
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	all, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	h, b, ok := bytes.Cut(all, []byte("\n"))
	if !ok {
		t.Fatalf("no header line in %q", all)
	}
	return string(h), string(b)
}

// TestStreamWireFormat checks the stream on the wire: the header carries
// offset, zero included, and the rest is exactly the scrollback from there,
// with nothing added.
func TestStreamWireFormat(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		s := start(t, newSpec(t, core.SessionHeadless, "sleep:1s", "exit:0"))
		header, body := readRaw(t, rawStream(t, s.dir, 0))
		s.wait()
		if header != `{"ok":true,"offset":0}` {
			t.Errorf("header %q", header)
		}
		if body != "" {
			t.Errorf("empty scrollback streamed as %q", body)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		s := start(t, newSpec(t, core.SessionHeadless, "print:hello world", "sleep:1s", "print:bye", "exit:0"))
		s.waitFor("hello world\n")
		from0, from6 := rawStream(t, s.dir, 0), rawStream(t, s.dir, 6)
		h0, b0 := readRaw(t, from0)
		h6, b6 := readRaw(t, from6)
		s.wait()
		sb := s.scrollback()
		if sb != "hello world\nbye\n" {
			t.Fatalf("scrollback %q", sb)
		}
		if h0 != `{"ok":true,"offset":0}` || b0 != sb {
			t.Errorf("from 0: header %q body %q, want the whole scrollback", h0, b0)
		}
		if h6 != `{"ok":true,"offset":6}` || b6 != sb[6:] {
			t.Errorf("from 6: header %q body %q, want %q", h6, b6, sb[6:])
		}
	})
}

func TestStreamRefusesOffsetPastEnd(t *testing.T) {
	s := start(t, newSpec(t, core.SessionHeadless, "print:abc", "sleep:1h"))
	s.waitFor("abc")
	_, err := s.client.Stream(ctxT(t), 5, io.Discard)
	wantCode(t, err, core.CodeInvalidArgument)
	_, err = s.client.Stream(ctxT(t), -1, io.Discard)
	wantCode(t, err, core.CodeInvalidArgument)
	// The end of the scrollback is a valid offset.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := s.client.Stream(ctx, 4, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stream from the end: %v, want it to wait for output", err)
	}
}

func TestStopKillsTermIgnoringGroup(t *testing.T) {
	spec := newSpec(t, core.SessionHeadless, "ignore-term", "spawn", "print:ready", "sleep:1h")
	spec.GraceMS = 500
	s := start(t, spec)
	out := s.waitFor("ready")
	var grandchild int
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "grandchild="); ok {
			grandchild, _ = strconv.Atoi(v)
		}
	}
	if grandchild <= 0 {
		t.Fatalf("no grandchild pid in %q", out)
	}
	pgid := s.ready.PID
	if g, err := syscall.Getpgid(grandchild); err != nil || g != pgid {
		t.Fatalf("grandchild group %d (%v), want %d", g, err, pgid)
	}

	t0 := time.Now()
	st, err := s.client.Stop(ctxT(t))
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Stopping || !st.State.Ended() || st.State.Signal != "SIGKILL" {
		t.Fatalf("status after stop %+v, want stopping and ended by SIGKILL", st)
	}
	if elapsed < 400*time.Millisecond || elapsed > 500*time.Millisecond+drainTimeout+time.Second {
		t.Errorf("stop took %s with a 500ms grace", elapsed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(-pgid, 0) != syscall.ESRCH || syscall.Kill(grandchild, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still has members after stop", pgid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.wait()
}

func TestStopTermRespectingAgent(t *testing.T) {
	s := start(t, newSpec(t, core.SessionHeadless, "print:ready", "sleep:1h"))
	s.waitFor("ready")
	st, err := s.client.Stop(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.State.Signal != "SIGTERM" {
		t.Errorf("signal %q, want SIGTERM", st.State.Signal)
	}
	if final := s.wait(); final.Signal != "SIGTERM" {
		t.Errorf("state signal %q", final.Signal)
	}
	// Stop is idempotent while the supervisor is still up, and a stopped
	// session's socket is gone afterwards.
	if _, err := s.client.Status(ctxT(t)); err == nil {
		t.Error("status answered after exit")
	}
}

func TestFileModesAndSocketRemoved(t *testing.T) {
	s := start(t, newSpec(t, core.SessionHeadless, "print:ready", "sleep:1h"))
	s.waitFor("ready")
	mode := func(name string) fs.FileMode {
		fi, err := os.Lstat(filepath.Join(s.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode()
	}
	if m := mode("."); !m.IsDir() || m.Perm() != 0o700 {
		t.Errorf("session dir mode %s, want 0700", m)
	}
	if m := mode(SocketFile); m&fs.ModeSocket == 0 || m.Perm() != 0o600 {
		t.Errorf("socket mode %s, want a 0600 socket", m)
	}
	for _, f := range []string{ScrollbackFile, StateFile} {
		if m := mode(f); !m.IsRegular() || m.Perm() != 0o600 {
			t.Errorf("%s mode %s, want 0600", f, m)
		}
	}
	st, err := s.client.Status(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.State.Ended() || st.Stopping || st.Size != int64(len("ready\n")) {
		t.Errorf("status %+v", st)
	}
	if _, err := s.client.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	s.wait()
	if _, err := os.Lstat(filepath.Join(s.dir, SocketFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket still there after exit: %v", err)
	}
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if e.Name() != StateFile && e.Name() != ScrollbackFile {
			t.Errorf("leftover file %s", e.Name())
		}
	}
}

func TestHeadlessHasNoTerminal(t *testing.T) {
	s := start(t, newSpec(t, core.SessionHeadless, "tty", "size", "print:ready", "sleep:1h"))
	if got := s.waitFor("ready"); got != "tty=no\ntty=no\nsize=none\nready\n" {
		t.Errorf("scrollback %q", got)
	}
	wantCode(t, s.client.Input(ctxT(t), []byte("x\n")), core.CodeInvalidArgument)
	wantCode(t, s.client.Resize(ctxT(t), 40, 120), core.CodeInvalidArgument)
}

func TestInteractivePTY(t *testing.T) {
	spec := newSpec(t, core.SessionInteractive, "tty", "size", "wait-winch", "echo", "exit:7")
	spec.Rows, spec.Cols = 30, 100
	s := start(t, spec)
	s.waitFor("waiting for winch")
	wantCode(t, s.client.Resize(ctxT(t), 0, 120), core.CodeInvalidArgument)
	if err := s.client.Resize(ctxT(t), 40, 120); err != nil {
		t.Fatal(err)
	}
	s.waitFor("size=40x120")
	if err := s.client.Input(ctxT(t), []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	st := s.wait()
	got := s.scrollback()
	for _, want := range []string{"tty=yes\r\ntty=yes\r\n", "size=30x100", "size=40x120", "got:hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrollback lacks %q: %q", want, got)
		}
	}
	if st.ExitCode == nil || *st.ExitCode != 7 {
		t.Errorf("exit %v, want 7", st.ExitCode)
	}
	if st.Mode != core.SessionInteractive {
		t.Errorf("mode %s", st.Mode)
	}
}

func TestArgvLiteralWorkDirAndEnv(t *testing.T) {
	spec := newSpec(t, core.SessionHeadless, "pwd", "env:FOO", "env:HOME", "args",
		"$HOME", "a;b", "", `q"x`, "line\nbreak", "`id`")
	spec.Env = append(spec.Env, "FOO=bar baz")
	s := start(t, spec)
	s.wait()
	wd, err := filepath.EvalSymlinks(spec.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	want := wd + "\nFOO=bar baz\nHOME unset\n" +
		strings.Join([]string{`"$HOME"`, `"a;b"`, `""`, `"q\"x"`, `"line\nbreak"`, "\"`id`\""}, "\n") + "\n"
	if got := s.scrollback(); got != want {
		t.Errorf("scrollback\n%q\nwant\n%q", got, want)
	}
	st, err := ReadState(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Argv, "\x00") != strings.Join(spec.Argv, "\x00") {
		t.Errorf("state argv %q, want %q", st.Argv, spec.Argv)
	}
}

func TestArgv0FromSessionPath(t *testing.T) {
	spec := newSpec(t, core.SessionHeadless, "print:found")
	spec.Argv[0] = "fakeagent"
	spec.Env = []string{"PATH=/nonexistent:" + filepath.Dir(fakeAgent)}
	s := start(t, spec)
	s.wait()
	if got := s.scrollback(); got != "found\n" {
		t.Errorf("scrollback %q", got)
	}
}

func TestStartFailureLeavesNothing(t *testing.T) {
	cases := map[string]func(*Spec){
		"missing argv0":     func(s *Spec) { s.Argv = []string{"no-such-agent-gaia-test"} },
		"missing path":      func(s *Spec) { s.Argv = []string{"/nonexistent/agent"} },
		"not in its PATH":   func(s *Spec) { s.Argv[0] = "fakeagent"; s.Env = nil },
		"relative work dir": func(s *Spec) { s.WorkDir = "rel" },
		"empty argv":        func(s *Spec) { s.Argv = nil },
		"bad mode":          func(s *Spec) { s.Mode = "batch" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := newSpec(t, core.SessionHeadless, "print:x")
			mutate(&spec)
			dir := sessionDir(t, spec.SessionID)
			s := launch(t, dir, spec)
			<-s.done
			if s.ready.OK || s.ready.Error == nil {
				t.Fatalf("ready %+v, want a refusal", s.ready)
			}
			if s.exit != int(core.ExitStartFailed) {
				t.Errorf("exit %d, want %d", s.exit, core.ExitStartFailed)
			}
			if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("session dir left behind: %v", err)
			}
		})
	}
}

func TestSessionDirMustBeEmpty(t *testing.T) {
	spec := newSpec(t, core.SessionHeadless, "print:x")
	dir := sessionDir(t, spec.SessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, StateFile)
	if err := os.WriteFile(keep, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := launch(t, dir, spec)
	<-s.done
	if s.ready.OK || s.exit != int(core.ExitStartFailed) {
		t.Fatalf("ready %+v exit %d, want a refusal", s.ready, s.exit)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "old" {
		t.Errorf("existing file changed: %q %v", b, err)
	}
}

func TestSupervisorOutlivesStarter(t *testing.T) {
	spec := newSpec(t, core.SessionHeadless, "stream:40:75ms", "exit:0")
	dir := sessionDir(t, spec.SessionID)
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	st := exec.Command(os.Args[0])
	st.Env = append(os.Environ(), roleEnv+"=starter", "GAIA_TEST_DIR="+dir, "GAIA_TEST_SPEC="+string(b))
	out, err := st.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Start(); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(out)
	first, _ := r.ReadString('\n')
	ready, _ := r.ReadString('\n')
	supPID, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(first, "SUP ")))
	if err != nil || !strings.Contains(ready, `"ok":true`) {
		_ = st.Process.Kill()
		t.Fatalf("starter said %q %q", first, ready)
	}
	client := NewClient(dir)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = client.Stop(ctx)
	})

	if err := st.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = st.Wait()
	sizeAt := func() int64 {
		fi, err := os.Stat(filepath.Join(dir, ScrollbackFile))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	before := sizeAt()
	time.Sleep(500 * time.Millisecond)
	if after := sizeAt(); after <= before {
		t.Fatalf("scrollback stopped growing after the starter died: %d -> %d", before, after)
	}
	deadline := time.Now().Add(20 * time.Second)
	var state core.SupervisorState
	for {
		state, err = ReadState(dir)
		if err == nil && state.Ended() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never ended: %+v %v", state, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state.ExitCode == nil || *state.ExitCode != 0 {
		t.Errorf("exit %v", state.ExitCode)
	}
	sb, _ := os.ReadFile(filepath.Join(dir, ScrollbackFile))
	if !strings.HasSuffix(string(sb), "line 40\n") {
		t.Errorf("scrollback ends %q", sb[max(0, len(sb)-20):])
	}
	for syscall.Kill(supPID, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Fatal("supervisor still running after its session ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStarterGoneBeforeReadyLine closes the read end of the supervisor's
// stdout before the spec arrives, so the ready line hits a broken pipe. The
// supervisor must not die of SIGPIPE: it drains the agent's output and
// records its exit.
func TestStarterGoneBeforeReadyLine(t *testing.T) {
	spec := newSpec(t, core.SessionHeadless, "sleep:300ms", "stream:5:20ms", "exit:3")
	dir := sessionDir(t, spec.SessionID)
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := supervisorCmd(dir)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &syncBuffer{}
	cmd.Stdout = w
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	_ = r.Close() // the starter is gone before the supervisor says ready
	if _, err := in.Write(b); err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervisor: %v (%v); stderr: %s", err, cmd.ProcessState, stderr)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("supervisor still running; stderr: %s", stderr)
	}
	st, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ended() || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("state %+v, want ended with exit 3", st)
	}
	sb, _ := os.ReadFile(filepath.Join(dir, ScrollbackFile))
	if !strings.HasSuffix(string(sb), "line 5\n") {
		t.Errorf("scrollback %q, want the agent's whole output", sb)
	}
	if syscall.Kill(-st.PID, 0) != syscall.ESRCH {
		t.Error("the agent's process group outlived its supervisor")
	}
}

func TestProtocolRefusals(t *testing.T) {
	s := start(t, newSpec(t, core.SessionHeadless, "print:ready", "sleep:1h"))
	s.waitFor("ready")
	raw := func(line string) Response {
		t.Helper()
		c, err := net.Dial("unix", filepath.Join(s.dir, SocketFile))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := io.WriteString(c, line); err != nil {
			t.Fatal(err)
		}
		b, err := bufio.NewReader(c).ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var r Response
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, line := range []string{"{\"op\":\"nope\"}\n", "not json\n", "{\"op\":\"status\"} \n"} {
		r := raw(line)
		if line == "{\"op\":\"status\"} \n" {
			if !r.OK || r.Status == nil {
				t.Errorf("%q: %+v", line, r)
			}
			continue
		}
		if r.OK || r.Error == nil || r.Error.Code != core.CodeInvalidArgument {
			t.Errorf("%q: %+v, want invalid_argument", line, r)
		}
	}
}

func TestSpecFormatHidesSecrets(t *testing.T) {
	spec := newSpec(t, core.SessionHeadless, "print:x")
	spec.Env = append(spec.Env, "TOKEN=envsecret")
	spec.Secrets = []string{"listsecret"}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		got := fmt.Sprintf(verb, spec)
		if strings.Contains(got, "envsecret") || strings.Contains(got, "listsecret") {
			t.Errorf("%s shows a secret: %s", verb, got)
		}
		if !strings.Contains(got, spec.SessionID.String()) {
			t.Errorf("%s lacks the session id: %s", verb, got)
		}
	}
}

func TestStateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadState(dir); err == nil {
		t.Fatal("read a missing state")
	} else {
		wantCode(t, err, core.CodeNotFound)
	}
	st := core.SupervisorState{
		Version: core.SupervisorStateVersion, SessionID: newSessionID(t), PID: 42,
		Mode: core.SessionHeadless, Argv: []string{"a", ""}, StartedAt: time.Unix(1, 0).UTC(),
	}
	if err := WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != st.SessionID || got.PID != 42 || len(got.Argv) != 2 || got.Ended() {
		t.Errorf("round trip %+v", got)
	}
	bad := st
	bad.PID = 0
	if err := WriteState(dir, bad); err == nil {
		t.Error("wrote an invalid state")
	}
	if err := os.WriteFile(filepath.Join(dir, StateFile), []byte(`{"version":1,"extra":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(dir); err == nil {
		t.Error("read a state with an unknown field")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
}

func TestMainUsageAndBadSpec(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(nil, strings.NewReader(""), &out, &errb); code != exitUsage {
		t.Errorf("no flags: exit %d", code)
	}
	if code := Main([]string{"--session-dir", "x", "extra"}, strings.NewReader(""), &out, &errb); code != exitUsage {
		t.Errorf("extra argument: exit %d", code)
	}
	out.Reset()
	dir := filepath.Join(t.TempDir(), "s")
	if code := Main([]string{"--session-dir", dir}, strings.NewReader(`{"bogus":1}`), &out, &errb); code != int(core.ExitStartFailed) {
		t.Errorf("bad spec: exit %d", code)
	}
	var r Response
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.OK || r.Error == nil || r.Error.Code != core.CodeInvalidArgument {
		t.Errorf("bad spec ready line %q (%v)", out.String(), err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("session dir created for a bad spec: %v", err)
	}
}
