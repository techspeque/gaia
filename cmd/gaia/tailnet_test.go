package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	gnet "github.com/techspeque/gaia/internal/net"
	"github.com/techspeque/gaia/internal/net/nettest"
)

// testAuthKey is a known key: tests assert it appears in no output.
const testAuthKey = "tskey-auth-gaiae2e-7c41d09ab3e85f26"

func TestHostTailnetOnlyFlagsNeedTailnet(t *testing.T) {
	for _, args := range [][]string{
		{"--control-url", "https://hs.example.com"},
		{"--allow", "user@example.com"},
		{"--allow-file", "/nonexistent"},
		{"--auth-key-file", "/nonexistent"},
	} {
		var errOut bytes.Buffer
		if code := runHost(args, &errOut, "linux"); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(errOut.String(), args[0]+" needs --tailnet") {
			t.Errorf("%v: stderr %q", args, errOut.String())
		}
	}
}

func TestHostAuthKeyIsNotAFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--tailnet", "--allow", "tag:gaia", "--auth-key=" + testAuthKey},
		{"--tailnet", "--allow", "tag:gaia", "--auth-key", testAuthKey},
		{"--tailnet", "--allow", "tag:gaia", "--authkey", testAuthKey},
	} {
		var errOut bytes.Buffer
		if code := runHost(args, &errOut, "linux"); code != 2 {
			t.Errorf("%v: exit %d, want 2", args[3], code)
		}
		if strings.Contains(errOut.String(), testAuthKey) {
			t.Errorf("%v: stderr echoes the key", args[3])
		}
	}
}

func TestHostTailnetAllowlistErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("user@example.com\nnot-an-entry\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("# nobody\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--tailnet"}, 1, "needs an allowlist"},
		{[]string{"--tailnet", "--allow-file", empty}, 1, "needs an allowlist"},
		{[]string{"--tailnet", "--allow", "everyone"}, 2, `"everyone"`},
		{[]string{"--tailnet", "--allow-file", bad}, 1, bad + ":2"},
		{[]string{"--tailnet", "--allow-file", filepath.Join(dir, "missing")}, 1, "--allow-file"},
	} {
		var errOut bytes.Buffer
		if code := runHost(c.args, &errOut, "linux"); code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
		if !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%v: stderr %q lacks %q", c.args, errOut.String(), c.want)
		}
	}
}

// fakeEnv is an environment for readAuthKey.
type fakeEnv map[string]string

func (e fakeEnv) get(k string) string { return e[k] }
func (e fakeEnv) unset(k string) error {
	delete(e, k)
	return nil
}

func TestReadAuthKey(t *testing.T) {
	dir := t.TempDir()
	writeKey := func(name string, mode os.FileMode, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := writeKey("good", 0o600, "  "+testAuthKey+"\n")
	readable := writeKey("readable", 0o644, testAuthKey)
	groupOnly := writeKey("group", 0o640, testAuthKey)
	blank := writeKey("blank", 0o600, "\n")

	for _, c := range []struct {
		name string
		env  fakeEnv
		file string
		want string
		code int
		err  string
	}{
		{"no key", fakeEnv{}, "", "", 0, ""},
		{"GAIA_AUTHKEY", fakeEnv{"GAIA_AUTHKEY": "a", "TS_AUTHKEY": "b"}, "", "a", 0, ""},
		{"TS_AUTHKEY", fakeEnv{"TS_AUTHKEY": "b"}, "", "b", 0, ""},
		{"file", fakeEnv{}, good, testAuthKey, 0, ""},
		{"file and env", fakeEnv{"TS_AUTHKEY": testAuthKey}, good, "", 2, "both set"},
		{"file other-readable", fakeEnv{}, readable, "", 1, "chmod 600"},
		{"file group-readable", fakeEnv{}, groupOnly, "", 1, "chmod 600"},
		{"file blank", fakeEnv{}, blank, "", 1, "empty"},
		{"file missing", fakeEnv{}, filepath.Join(dir, "missing"), "", 1, "--auth-key-file"},
		{"file is a directory", fakeEnv{}, dir, "", 1, "not a regular file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			key, code, err := readAuthKey(c.file, c.env.get, c.env.unset)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) || code != c.code {
					t.Fatalf("err %v code %d, want %q code %d", err, code, c.err, c.code)
				}
				if strings.Contains(err.Error(), testAuthKey) {
					t.Errorf("error reveals the key: %v", err)
				}
			} else if err != nil || key.Reveal() != c.want {
				t.Fatalf("key %q err %v, want %q", key.Reveal(), err, c.want)
			}
			for _, name := range authKeyEnv {
				if _, ok := c.env[name]; ok {
					t.Errorf("$%s still set", name)
				}
			}
		})
	}
}

func TestMachineName(t *testing.T) {
	for _, c := range []struct {
		h    string
		err  error
		want string
	}{
		{"Build-Box.local", nil, "build-box"},
		{"laptop", nil, "laptop"},
		{"", nil, ""},
		{"x", errors.New("no"), ""},
	} {
		if got := machineName(func() (string, error) { return c.h, c.err }); got != c.want {
			t.Errorf("machineName(%q) = %q, want %q", c.h, got, c.want)
		}
	}
}

// TestTailnetTrapIsReal proves the test hook that the other daemon tests
// rely on fires: with --tailnet and no allowTailnetEnv, the child exits
// with the trap code.
func TestTailnetTrapIsReal(t *testing.T) {
	skipIfNoDaemon(t)
	d := startDaemon(t, shortTempDir(t), "--tailnet", "--allow", "tag:gaia")
	if code := d.wait(t); code != tailnetTrapCode {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, tailnetTrapCode, d.stderr)
	}
	if !strings.Contains(d.stderr.String(), tailnetTrapMsg) {
		t.Errorf("stderr lacks the trap message:\n%s", d.stderr)
	}
}

// TestHostTailnetEndToEnd runs `gaia host --tailnet` against a local test
// control server and calls /api/v1/version over HTTPS on the tailnet from
// an allowlisted peer and from one that is not.
func TestHostTailnetEndToEnd(t *testing.T) {
	skipIfNoDaemon(t)
	control := nettest.StartControl(t, testAuthKey)
	url := control.HTTPTestServer.URL
	state := filepath.Join(t.TempDir(), "state")
	d := startDaemonEnv(t, shortTempDir(t),
		[]string{allowTailnetEnv + "=1", "GAIA_AUTHKEY=" + testAuthKey, "TS_AUTHKEY="},
		"--state", state, "--tailnet", "--control-url", url, "--allow", "node:gaia-allowed")
	d.waitReadyWithin(t, 60*time.Second)

	hostAddr := startedField(t, d.stderr.String(), "tailnet_addr")
	if hostAddr == "" {
		t.Fatalf("no tailnet address logged\nstderr:\n%s", d.stderr)
	}

	allowed := joinClient(t, url, "gaia-allowed")
	other := joinClient(t, url, "gaia-other")

	pin := x509.NewCertPool()
	pem, err := os.ReadFile(gnet.CertPath(state))
	if err != nil {
		t.Fatal(err)
	}
	if !pin.AppendCertsFromPEM(pem) {
		t.Fatal("no certificate in the host's certificate file")
	}
	get := func(c *gnet.Tailnet, path string) (int, []byte) {
		t.Helper()
		hc := &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext:     c.Dial,
				TLSClientConfig: &tls.Config{RootCAs: pin, ServerName: hostAddr, MinVersion: tls.VersionTLS13},
			},
		}
		res, err := hc.Get("https://" + hostAddr + path)
		if err != nil {
			t.Fatalf("GET %s over the tailnet: %v\nstderr:\n%s", path, err, d.stderr)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}

	status, body := get(allowed, core.VersionPath)
	var v core.VersionResponse
	if status != http.StatusOK || json.Unmarshal(body, &v) != nil || v.APIVersion != core.APIVersion {
		t.Errorf("allowlisted peer: %d %s, want 200 and the version", status, body)
	}
	status, body = get(other, core.VersionPath)
	var e core.ErrorResponse
	if status != http.StatusForbidden || json.Unmarshal(body, &e) != nil || e.Error == nil || e.Error.Code != core.CodeForbidden {
		t.Errorf("other peer: %d %s, want 403 forbidden", status, body)
	}
	if status, _ := get(allowed, core.HealthzPath); status != http.StatusNotFound {
		t.Errorf("/healthz on the tailnet: %d, want 404", status)
	}
	if !logged(d.stderr.String(), "tailnet request refused: peer not allowlisted", `"node":"gaia-other"`) {
		t.Errorf("no warn line naming the refused peer\nstderr:\n%s", d.stderr)
	}

	// The socket still serves /healthz.
	if status, _ := d.get(t, core.HealthzPath); status != http.StatusOK {
		t.Errorf("socket /healthz %d", status)
	}

	// No TCP listener outside tsnet's netstack: the API listener is not a
	// host socket. tsnet's WireGuard UDP socket is the one allowed host
	// socket (OVERVIEW §3.3).
	if l, err := tcpListeners(d.cmd.Process.Pid); errors.Is(err, errNoSocketLister) {
		t.Log(err)
	} else if err != nil {
		t.Fatal(err)
	} else if len(l) > 0 {
		t.Errorf("host has TCP listeners outside tsnet: %q\nstderr:\n%s", l, d.stderr)
	}

	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := d.wait(t); code != 0 {
		t.Errorf("exit %d, want 0\nstderr:\n%s", code, d.stderr)
	}
	out := d.stderr.String()
	if strings.Contains(out, testAuthKey) {
		t.Error("the auth key appears in the host's stderr")
	}
	if !strings.Contains(out, `"component":"tsnet"`) {
		t.Error("no tsnet log lines reached the host log, so the key check above covers none")
	}
	for _, a := range d.cmd.Args {
		if strings.Contains(a, testAuthKey) {
			t.Error("the auth key is a process argument")
		}
	}
	assertJSONLines(t, out)
	for _, f := range []string{"tsnet", "tsnet/gaia-tls.key", "tsnet/gaia-tls.crt"} {
		fi, err := os.Stat(filepath.Join(state, f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %s, want no group or other access", f, fi.Mode().Perm())
		}
	}
}

// waitReadyWithin is waitReady with a longer deadline, for a tailnet join.
func (d *daemon) waitReadyWithin(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
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
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon not ready\nstderr:\n%s", d.stderr)
}

func joinClient(t *testing.T, controlURL, name string) *gnet.Tailnet {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := gnet.Join(ctx, gnet.Config{
		Dir: t.TempDir(), Hostname: name, ControlURL: controlURL, AuthKey: core.NewSecret(testAuthKey),
	})
	if err != nil {
		t.Fatalf("join %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// startedField returns a string field of the "host started" log line.
func startedField(t *testing.T, out, field string) string {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil && m["msg"] == "host started" {
			s, _ := m[field].(string)
			return s
		}
	}
	return ""
}

// logged reports whether one log line contains every part.
func logged(out string, parts ...string) bool {
	for _, line := range strings.Split(out, "\n") {
		ok := true
		for _, p := range parts {
			if !strings.Contains(line, p) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// tcpListeners returns pid's listening TCP sockets.
func tcpListeners(pid int) ([]string, error) {
	socks, err := inetSockets(pid)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range socks {
		if strings.Contains(s, "(LISTEN)") { // lsof
			out = append(out, s)
			continue
		}
		// /proc/<pid>/net/tcp{,6}: the state column is 0A for LISTEN.
		if table, line, ok := strings.Cut(s, ": "); ok && strings.HasPrefix(table, "tcp") {
			if f := strings.Fields(line); len(f) > 3 && f[3] == "0A" {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// TestTCPListenersSeesListen proves tcpListeners is not vacuous.
func TestTCPListenersSeesListen(t *testing.T) {
	skipIfNoDaemon(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got, err := tcpListeners(os.Getpid())
	if errors.Is(err, errNoSocketLister) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	found := false
	for _, s := range got {
		// lsof prints the port in decimal, /proc in hex.
		if strings.Contains(s, ":"+strconv.Itoa(port)) || strings.Contains(strings.ToUpper(s), fmt.Sprintf(":%04X", port)) {
			found = true
		}
	}
	if !found {
		t.Errorf("listener on port %d not found in %q", port, got)
	}
}
