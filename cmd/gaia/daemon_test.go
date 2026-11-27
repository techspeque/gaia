package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/sock"
)

// runMainEnv makes the test binary act as the gaia binary, so daemon tests
// run the real main in a child process.
const runMainEnv = "GAIA_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// daemon is a `gaia host` child process.
type daemon struct {
	cmd     *exec.Cmd
	rtDir   string // XDG_RUNTIME_DIR/gaia
	stderr  *lockedBuffer
	exited  chan struct{}
	exitErr error
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func skipIfNoDaemon(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the daemon roles run on linux and darwin only")
	}
}

// shortTempDir returns a directory short enough to hold a unix socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// startDaemon starts `gaia host` with args, a fresh state dir unless args
// set one, and XDG_RUNTIME_DIR set to xdg.
func startDaemon(t *testing.T, xdg string, args ...string) *daemon {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !containsFlag(args, "--state") {
		args = append(args, "--state", filepath.Join(t.TempDir(), "state"))
	}
	d := &daemon{rtDir: filepath.Join(xdg, "gaia"), stderr: &lockedBuffer{}, exited: make(chan struct{})}
	d.cmd = exec.Command(exe, append([]string{"host"}, args...)...)
	d.cmd.Env = append(os.Environ(), runMainEnv+"=1", "XDG_RUNTIME_DIR="+xdg)
	d.cmd.Stderr = d.stderr
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { d.exitErr = d.cmd.Wait(); close(d.exited) }()
	t.Cleanup(func() {
		_ = d.cmd.Process.Kill()
		<-d.exited
	})
	return d
}

func containsFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f || strings.HasPrefix(a, f+"=") {
			return true
		}
	}
	return false
}

func (d *daemon) client() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return sock.Dial(ctx, d.rtDir)
		}},
	}
}

func (d *daemon) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	res, err := d.client().Get("http://gaia" + path)
	if err != nil {
		t.Fatalf("GET %s: %v\nstderr:\n%s", path, err, d.stderr)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// waitReady polls /healthz until the daemon answers.
func (d *daemon) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-d.exited:
			t.Fatalf("daemon exited early: %v\nstderr:\n%s", d.exitErr, d.stderr)
		default:
		}
		res, err := d.client().Get("http://gaia" + core.HealthzPath)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon not ready\nstderr:\n%s", d.stderr)
}

// wait waits for the daemon to exit and returns its exit code.
func (d *daemon) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-d.exited:
	case <-time.After(15 * time.Second):
		t.Fatalf("daemon did not exit\nstderr:\n%s", d.stderr)
	}
	var ee *exec.ExitError
	if errors.As(d.exitErr, &ee) {
		return ee.ExitCode()
	}
	if d.exitErr != nil {
		t.Fatal(d.exitErr)
	}
	return 0
}

func (d *daemon) nodes(t *testing.T) []core.Node {
	t.Helper()
	status, body := d.get(t, core.NodesPath)
	if status != http.StatusOK {
		t.Fatalf("nodes: %d %s", status, body)
	}
	var l core.ListNodesResponse
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatal(err)
	}
	return l.Nodes
}

func TestHostServesAndStopsOnSignal(t *testing.T) {
	skipIfNoDaemon(t)
	for _, tc := range []struct {
		name     string
		withNode bool
		sig      os.Signal
	}{
		{"host SIGTERM", false, syscall.SIGTERM},
		{"host --node SIGTERM", true, syscall.SIGTERM},
		{"host --node SIGINT", true, os.Interrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var args []string
			if tc.withNode {
				args = append(args, "--node")
			}
			d := startDaemon(t, shortTempDir(t), args...)
			d.waitReady(t)

			fi, err := os.Lstat(sock.Path(d.rtDir))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode()&fs.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
				t.Errorf("socket mode %s, want socket 0600", fi.Mode())
			}

			status, body := d.get(t, core.HealthzPath)
			if status != http.StatusOK || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
				t.Errorf("healthz %d %s", status, body)
			}
			var v core.VersionResponse
			if _, body := d.get(t, core.VersionPath); json.Unmarshal(body, &v) != nil || v.APIVersion != core.APIVersion {
				t.Errorf("version %s", body)
			}

			assertNoInetSockets(t, d.cmd.Process.Pid)

			if tc.withNode {
				// Registration happens at start, well within one heartbeat
				// interval; poll briefly for the goroutine to run.
				deadline := time.Now().Add(5 * time.Second)
				var l []core.Node
				for time.Now().Before(deadline) {
					if l = d.nodes(t); len(l) == 1 {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if len(l) != 1 || l[0].State != core.NodeOnline || l[0].OS != runtime.GOOS ||
					l[0].TailnetIdentity != "local" {
					t.Fatalf("nodes %+v", l)
				}
			} else if l := d.nodes(t); len(l) != 0 {
				t.Errorf("registry without --node lists %+v", l)
			}

			if err := d.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			if code := d.wait(t); code != 0 {
				t.Errorf("exit code %d, want 0\nstderr:\n%s", code, d.stderr)
			}
			if _, err := os.Lstat(sock.Path(d.rtDir)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("socket still present after signal: %v", err)
			}
			assertJSONLines(t, d.stderr.String())
		})
	}
}

func TestSecondHostRefused(t *testing.T) {
	skipIfNoDaemon(t)
	xdg := shortTempDir(t)
	first := startDaemon(t, xdg)
	first.waitReady(t)
	second := startDaemon(t, xdg)
	if code := second.wait(t); code != 1 {
		t.Errorf("second host exit %d, want 1", code)
	}
	if !strings.Contains(second.stderr.String(), "another gaia daemon") {
		t.Errorf("second host stderr:\n%s", second.stderr)
	}
	// The first is unaffected.
	if status, _ := first.get(t, core.HealthzPath); status != http.StatusOK {
		t.Errorf("first host healthz %d", status)
	}
}

func TestStaleSocketReplacedAfterKill(t *testing.T) {
	skipIfNoDaemon(t)
	xdg := shortTempDir(t)
	state := filepath.Join(t.TempDir(), "state")
	first := startDaemon(t, xdg, "--state", state, "--node")
	first.waitReady(t)
	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	first.wait(t)
	if _, err := os.Lstat(sock.Path(first.rtDir)); err != nil {
		t.Fatalf("SIGKILL should leave the socket behind: %v", err)
	}
	second := startDaemon(t, xdg, "--state", state, "--node")
	second.waitReady(t)
	// Same state: the local node keeps its id across restarts.
	if l := second.nodes(t); len(l) != 1 {
		t.Errorf("nodes after restart %+v", l)
	}
}

func TestHostRefusesNonSocketAtPath(t *testing.T) {
	skipIfNoDaemon(t)
	xdg := shortTempDir(t)
	rt := filepath.Join(xdg, "gaia")
	if err := os.Mkdir(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock.Path(rt), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemon(t, xdg)
	if code := d.wait(t); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if b, _ := os.ReadFile(sock.Path(rt)); string(b) != "keep" {
		t.Errorf("file at socket path changed: %q", b)
	}
}

func TestDaemonRolesRefusedOnWindows(t *testing.T) {
	for _, role := range []struct {
		name string
		run  func([]string, io.Writer, string) int
	}{{"host", runHost}, {"node", runNode}} {
		var errOut bytes.Buffer
		if code := role.run(nil, &errOut, "windows"); code != 1 {
			t.Errorf("%s: exit %d, want 1", role.name, code)
		}
		want := "gaia " + role.name + ": the daemon roles do not run on Windows yet"
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("%s: stderr %q lacks %q", role.name, errOut.String(), want)
		}
	}
}

func TestHostBadFlags(t *testing.T) {
	for _, args := range [][]string{{"--bogus"}, {"extra"}} {
		var errOut bytes.Buffer
		if code := runHost(args, &errOut, "linux"); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestDefaultStateDir(t *testing.T) {
	home := func() (string, error) { return "/home/u", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"linux", nil, "/home/u/.local/state/gaia"},
		{"darwin", nil, "/home/u/Library/Application Support/gaia"},
		{"linux", map[string]string{"XDG_STATE_HOME": "/x"}, "/x/gaia"},
		{"darwin", map[string]string{"XDG_STATE_HOME": "/x"}, "/x/gaia"},
	}
	for _, c := range cases {
		got, err := defaultStateDir(c.goos, env(c.env), home)
		if err != nil || got != filepath.FromSlash(c.want) {
			t.Errorf("%s %v: %q, %v; want %q", c.goos, c.env, got, err, c.want)
		}
	}
	_, err := defaultStateDir("linux", env(nil), func() (string, error) { return "", errors.New("no home") })
	if err == nil {
		t.Error("missing home accepted")
	}
}

// assertJSONLines checks every stderr line is a JSON object.
func assertJSONLines(t *testing.T, out string) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	n := 0
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Errorf("log line is not JSON: %q", sc.Text())
		}
		n++
	}
	if n == 0 {
		t.Error("no log lines")
	}
}

// assertNoInetSockets fails if process pid has any TCP or UDP socket.
func assertNoInetSockets(t *testing.T, pid int) {
	t.Helper()
	socks, err := inetSockets(pid)
	if errors.Is(err, errNoSocketLister) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(socks) > 0 {
		t.Errorf("process %d has inet sockets: %q", pid, socks)
	}
}

// TestSocketListerSeesTCP proves the socket check is not vacuous: it finds
// a TCP listener this test process opens.
func TestSocketListerSeesTCP(t *testing.T) {
	skipIfNoDaemon(t)
	before, err := inetSockets(os.Getpid())
	if errors.Is(err, errNoSocketLister) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	after, err := inetSockets(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Errorf("listener not seen: before %q after %q", before, after)
	}
}

var errNoSocketLister = errors.New("no way to list a process's sockets here")

// inetSockets lists pid's TCP and UDP sockets: from /proc on linux, with
// lsof on darwin.
func inetSockets(pid int) ([]string, error) {
	switch runtime.GOOS {
	case "linux":
		return procInetSockets(pid)
	case "darwin":
		lsof, err := exec.LookPath("lsof")
		if err != nil {
			return nil, errNoSocketLister
		}
		out, err := exec.Command(lsof, "-nP", "-a", "-p", strconv.Itoa(pid), "-i").Output()
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
			return nil, nil // lsof exits 1 when nothing matches
		}
		if err != nil {
			return nil, err
		}
		var socks []string
		for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if i > 0 && line != "" { // skip the header
				socks = append(socks, line)
			}
		}
		return socks, nil
	}
	return nil, errNoSocketLister
}

// procInetSockets matches pid's socket fds against the kernel's TCP and UDP
// tables by inode.
func procInetSockets(pid int) ([]string, error) {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	fds, err := os.ReadDir(filepath.Join(base, "fd"))
	if err != nil {
		return nil, err
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(base, "fd", fd.Name()))
		if err != nil {
			continue // closed since ReadDir
		}
		if ino, ok := strings.CutPrefix(target, "socket:["); ok {
			inodes[strings.TrimSuffix(ino, "]")] = true
		}
	}
	var socks []string
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
		b, err := os.ReadFile(filepath.Join(base, "net", table))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for i, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if i == 0 || len(f) < 10 {
				continue
			}
			if inodes[f[9]] {
				socks = append(socks, table+": "+line)
			}
		}
	}
	return socks, nil
}
