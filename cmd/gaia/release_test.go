package main

// Release smoke tests: they run a built release binary, named by
// $GAIA_RELEASE_BIN, and are skipped without it (`make release-smoke`, and
// the release workflow). They check what a release build must hold apart
// from the test binary: build settings, no network request on any Phase 0
// local-only command (ADR-0012 rule 2), and no TCP listener after joining a
// tailnet (OVERVIEW §3.3).

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/net/nettest"
)

const releaseBinEnv = "GAIA_RELEASE_BIN"

// releaseBin returns the release binary under test, or skips.
func releaseBin(t *testing.T) string {
	t.Helper()
	p := os.Getenv(releaseBinEnv)
	if p == "" {
		t.Skip(releaseBinEnv + " is not set")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("%s: %v", releaseBinEnv, err)
	}
	return abs
}

// releaseEnv is a minimal environment for the release binary: none of the
// test process's variables, so no proxy or auth key leaks in.
func releaseEnv(xdg string, extra ...string) []string {
	return append([]string{"PATH=/usr/bin:/bin", "HOME=" + xdg, "XDG_RUNTIME_DIR=" + xdg}, extra...)
}

func TestReleaseBuildSettings(t *testing.T) {
	bin := releaseBin(t)
	out, err := exec.Command("go", "version", "-m", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m: %v\n%s", err, out)
	}
	for _, want := range []string{"CGO_ENABLED=0", "-tags=ts_omit_peerapiserver", "-trimpath=true"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("build settings lack %s:\n%s", want, out)
		}
	}
	v, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(v))
	if strings.HasPrefix(got, "gaia dev ") || strings.Contains(got, "(commit unknown)") {
		t.Errorf("--version %q: the release version and commit were not set at build time", got)
	}
	if !strings.HasSuffix(got, " "+runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("--version %q is not for %s/%s", got, runtime.GOOS, runtime.GOARCH)
	}
}

// netCall matches a traced network syscall on an IPv4 or IPv6 socket.
var netCall = regexp.MustCompile(`\b(socket|connect|bind|sendto|sendmsg|sendmmsg)\(.*\bAF_INET6?\b`)

// tracer runs release commands under strace on linux, so any IPv4 or IPv6
// socket call is seen; there a missing strace fails the test. Elsewhere it
// runs them plainly and only the socket listing checks hold.
type tracer struct {
	strace string
	logs   []string
}

func newTracer(t *testing.T) *tracer {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Logf("no syscall tracing on %s: checking open sockets only", runtime.GOOS)
		return &tracer{}
	}
	p, err := exec.LookPath("strace")
	if err != nil {
		t.Fatal("strace is required for the release smoke test on linux")
	}
	return &tracer{strace: p}
}

// command returns bin args, traced when the tracer can trace.
func (tr *tracer) command(t *testing.T, bin string, args ...string) *exec.Cmd {
	t.Helper()
	if tr.strace == "" {
		return exec.Command(bin, args...)
	}
	log := filepath.Join(t.TempDir(), "strace.log")
	tr.logs = append(tr.logs, log)
	return exec.Command(tr.strace, append([]string{"-f", "-qq", "-e", "trace=network", "-o", log, bin}, args...)...)
}

// check fails on any IPv4 or IPv6 socket call in the traces.
func (tr *tracer) check(t *testing.T) {
	t.Helper()
	for _, log := range tr.logs {
		f, err := os.Open(log)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if netCall.MatchString(sc.Text()) {
				t.Errorf("network call: %s", sc.Text())
			}
		}
		_ = f.Close()
	}
}

// startRelease starts a release-binary daemon (traced when possible) and
// returns it with the pid of the gaia process itself.
func startRelease(t *testing.T, tr *tracer, bin, xdg string, env []string, args ...string) (*daemon, int) {
	t.Helper()
	d := &daemon{rtDir: filepath.Join(xdg, "gaia"), stderr: &lockedBuffer{}, exited: make(chan struct{})}
	d.cmd = tr.command(t, bin, args...)
	d.cmd.Env = releaseEnv(xdg, env...)
	d.cmd.Stderr = d.stderr
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { d.exitErr = d.cmd.Wait(); close(d.exited) }()
	t.Cleanup(func() {
		_ = d.cmd.Process.Kill()
		<-d.exited
	})
	pid := d.cmd.Process.Pid
	if tr.strace != "" {
		pid = tracedChild(t, pid)
	}
	return d, pid
}

// tracedChild returns the one child of the strace process pid.
func tracedChild(t *testing.T, pid int) int {
	t.Helper()
	path := filepath.Join("/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if f := strings.Fields(string(b)); len(f) > 0 {
			c, err := strconv.Atoi(f[0])
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("strace started no child")
	return 0
}

// stopRelease sends SIGTERM to the gaia process and waits for a clean exit.
func stopRelease(t *testing.T, d *daemon, pid int) {
	t.Helper()
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := d.wait(t); code != 0 {
		t.Errorf("exit %d after SIGTERM, want 0\nstderr:\n%s", code, d.stderr)
	}
}

// runRelease runs one client command and returns its exit code and output.
func runRelease(t *testing.T, tr *tracer, bin, xdg string, args ...string) (int, string) {
	t.Helper()
	cmd := tr.command(t, bin, args...)
	cmd.Env = releaseEnv(xdg)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

// TestReleaseNoNetworkLocalOnly runs every Phase 0 command that needs no
// tailnet and checks none makes a network call.
func TestReleaseNoNetworkLocalOnly(t *testing.T) {
	bin := releaseBin(t)
	skipIfNoDaemon(t)
	tr := newTracer(t)
	xdg := shortTempDir(t)

	for _, args := range [][]string{{"--version"}, {"help"}} {
		if code, out := runRelease(t, tr, bin, xdg, args...); code != 0 {
			t.Errorf("gaia %v: exit %d\n%s", args, code, out)
		}
	}
	// A client command with no daemon running fails locally.
	if code, out := runRelease(t, tr, bin, xdg, "nodes"); code == 0 {
		t.Errorf("gaia nodes with no daemon: exit 0\n%s", out)
	}

	// The host alone, then local-only mode with the client commands.
	for _, args := range [][]string{{"host"}, {"host", "--node"}} {
		state := filepath.Join(t.TempDir(), "state")
		d, pid := startRelease(t, tr, bin, xdg, nil, append(args, "--state", state)...)
		d.waitReady(t)
		for _, c := range [][]string{{"nodes"}, {"nodes", "-o", "json"}} {
			code, out := runRelease(t, tr, bin, xdg, c...)
			if code != 0 {
				t.Errorf("gaia %v against %v: exit %d\n%s", c, args, code, out)
			}
			if len(args) > 1 && !strings.Contains(out, "online") {
				t.Errorf("gaia %v in local-only mode lists no online node:\n%s", c, out)
			}
		}
		assertNoInetSockets(t, pid)
		stopRelease(t, d, pid)
	}
	tr.check(t)
}

// TestReleaseTailnetNoTCPListener joins the release host to a local test
// control server and checks it then has no TCP listener on the machine: the
// API listener lives only inside tsnet's netstack, and the peer API server
// is compiled out.
func TestReleaseTailnetNoTCPListener(t *testing.T) {
	bin := releaseBin(t)
	skipIfNoDaemon(t)
	control := nettest.StartControl(t, testAuthKey)
	xdg := shortTempDir(t)
	d, pid := startRelease(t, &tracer{}, bin, xdg, []string{"GAIA_AUTHKEY=" + testAuthKey},
		"host", "--state", filepath.Join(t.TempDir(), "state"),
		"--tailnet", "--control-url", control.HTTPTestServer.URL, "--allow", "tag:gaia")
	d.waitReadyWithin(t, 60*time.Second)
	if startedField(t, d.stderr.String(), "tailnet_addr") == "" {
		t.Fatalf("host did not join the tailnet\nstderr:\n%s", d.stderr)
	}
	l, err := tcpListeners(pid)
	if errors.Is(err, errNoSocketLister) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(l) > 0 {
		t.Errorf("release host has TCP listeners after joining a tailnet: %q", l)
	}
	stopRelease(t, d, pid)
	if strings.Contains(d.stderr.String(), testAuthKey) {
		t.Error("the auth key appears in the host's stderr")
	}
}
