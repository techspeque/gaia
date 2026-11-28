package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/techspeque/gaia/internal/service"
)

// fakeService replaces the platform with a temp home and a runner that
// records commands and reports every service as running.
type fakeService struct {
	home string
	cmds []string
}

func useFakeService(t *testing.T, goos string) *fakeService {
	t.Helper()
	fs := &fakeService{home: t.TempDir()}
	oldP, oldE := servicePlatform, serviceExecutable
	servicePlatform = func(g string) (service.Platform, error) {
		return service.Platform{GOOS: g, Home: fs.home, UID: 1000, User: "alice",
			Run: func(_ context.Context, name string, args ...string) (string, error) {
				fs.cmds = append(fs.cmds, strings.Join(append([]string{name}, args...), " "))
				if name == "loginctl" {
					return "yes\n", nil
				}
				return "", nil
			}}, nil
	}
	serviceExecutable = func() (string, error) { return "/opt/gaia/bin/gaia", nil }
	t.Cleanup(func() { servicePlatform, serviceExecutable = oldP, oldE })
	return fs
}

func (fs *fakeService) unit(t *testing.T, goos string) string {
	t.Helper()
	p, _ := servicePlatform(goos)
	path, err := service.UnitPath(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runSvc(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := runService(args, &out, &errOut, "linux")
	return code, out.String(), errOut.String()
}

// withTailnetState makes dir look joined to a tailnet (net.HasState).
func withTailnetState(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "tsnet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsnet", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseServiceArgs(t *testing.T) {
	home := func() (string, error) { return "/home/a", nil }
	env := func(string) string { return "" }
	allowFile := filepath.Join(t.TempDir(), "allow")
	if err := os.WriteFile(allowFile, []byte("tag:ci\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	for _, c := range []struct {
		args    []string
		flags   []string
		tailnet bool
	}{
		{[]string{"host"}, []string{"--state=/home/a/.local/state/gaia"}, false},
		{[]string{"host", "--node", "--state", "rel"}, []string{"--state=" + filepath.Join(cwd, "rel"), "--node"}, false},
		{[]string{"host", "--node", "--name", "gaia-desk"}, []string{"--state=/home/a/.local/state/gaia", "--name=gaia-desk", "--node"}, false},
		{[]string{"host", "--tailnet", "--allow", "tag:ci", "--allow-file", allowFile, "--control-url", "https://hs",
			"--heartbeat-interval", "2s", "--missed-heartbeats", "4"},
			[]string{"--state=/home/a/.local/state/gaia", "--tailnet", "--control-url=https://hs", "--allow=tag:ci",
				"--allow-file=" + allowFile, "--heartbeat-interval=2s", "--missed-heartbeats=4"}, true},
		{[]string{"node", "--host", "h1", "--name", "box", "--control-url", "https://hs"},
			[]string{"--state=/home/a/.local/state/gaia", "--host=h1", "--name=box", "--control-url=https://hs"}, true},
	} {
		var errOut bytes.Buffer
		sa, code := parseServiceArgs(c.args, "linux", env, home, &errOut)
		if code != 0 {
			t.Errorf("%q: code %d: %s", c.args, code, errOut.String())
			continue
		}
		if !slices.Equal(sa.flags, c.flags) || sa.tailnet != c.tailnet || sa.role != c.args[0] {
			t.Errorf("%q: got %+v, want flags %q tailnet %v", c.args, sa, c.flags, c.tailnet)
		}
	}
}

func TestParseServiceArgsRefused(t *testing.T) {
	home := func() (string, error) { return "/home/a", nil }
	env := func(string) string { return "" }
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{nil, 2, "usage"},
		{[]string{"client"}, 2, "usage"},
		{[]string{"node", "--host", "h", "--join"}, 2, "--join is not accepted"},
		{[]string{"host", "--tailnet", "--allow", "tag:a", "--auth-key-file", "/k"}, 2, "--auth-key-file is not accepted"},
		{[]string{"node", "--host", "h", "--auth-key-file", "/k"}, 2, "--auth-key-file is not accepted"},
		{[]string{"node"}, 2, "--host NAME is required"},
		{[]string{"node", "--host", "h", "--name", "bad name!"}, 2, "--name"},
		{[]string{"host", "--name", "Bad_Name"}, 2, "--name: invalid node name"},
		{[]string{"node", "--host", "h", "--node"}, 2, "not defined"},
		{[]string{"host", "--allow", "tag:a"}, 2, "--allow needs --tailnet"},
		{[]string{"host", "--tailnet"}, 1, "needs an allowlist"},
		{[]string{"host", "--tailnet", "--allow", "nonsense"}, 2, "--allow"},
		{[]string{"host", "--tailnet", "--allow-file", "/does/not/exist"}, 1, "--allow-file"},
		{[]string{"host", "--missed-heartbeats", "0"}, 2, "at least 1"},
		{[]string{"host", "extra"}, 2, "unexpected argument"},
	} {
		var errOut bytes.Buffer
		_, code := parseServiceArgs(c.args, "linux", env, home, &errOut)
		if code != c.code || !strings.Contains(errOut.String(), c.msg) {
			t.Errorf("%q: code %d, stderr %q; want %d and %q", c.args, code, errOut.String(), c.code, c.msg)
		}
	}
}

func TestServiceRefusedOnWindows(t *testing.T) {
	for _, args := range [][]string{{"install", "host"}, {"uninstall"}} {
		var out, errOut bytes.Buffer
		if code := runService(args, &out, &errOut, "windows"); code != 1 || !strings.Contains(errOut.String(), "not supported on windows") {
			t.Errorf("%q on windows: code %d, stderr %q", args, code, errOut.String())
		}
	}
}

func TestServiceInstallNeedsTailnetState(t *testing.T) {
	fs := useFakeService(t, "linux")
	state := t.TempDir()
	for _, args := range [][]string{
		{"install", "node", "--host", "h", "--state", state},
		{"install", "host", "--tailnet", "--allow", "tag:a", "--state", state},
	} {
		code, _, errOut := runSvc(args...)
		if code != 1 || !strings.Contains(errOut, "holds no tailnet state") {
			t.Errorf("%q: code %d, stderr %q", args, code, errOut)
		}
	}
	if len(fs.cmds) > 0 {
		t.Errorf("ran %q", fs.cmds)
	}
	withTailnetState(t, state)
	if code, _, errOut := runSvc("install", "node", "--host", "h", "--state", state); code != 0 {
		t.Errorf("with tailnet state: code %d: %s", code, errOut)
	}
}

// The unit holds no auth key, even with one in the environment, and only
// the socket-dir variables.
func TestServiceUnitHoldsNoCredential(t *testing.T) {
	const key = "tskey-auth-kTESTSECRET123-abcdef"
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			fs := useFakeService(t, goos)
			t.Setenv("GAIA_AUTHKEY", key)
			t.Setenv("TS_AUTHKEY", key)
			t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
			state := t.TempDir()
			withTailnetState(t, state)
			var out, errOut bytes.Buffer
			code := runService([]string{"install", "host", "--tailnet", "--allow", "tag:ci", "--state", state}, &out, &errOut, goos)
			if code != 0 {
				t.Fatalf("install: %d %s", code, errOut.String())
			}
			unit := fs.unit(t, goos)
			for _, bad := range []string{key, "TESTSECRET", "tskey", "AUTHKEY", "auth-key", "--join"} {
				if strings.Contains(unit, bad) {
					t.Errorf("unit contains %q:\n%s", bad, unit)
				}
			}
			if !strings.Contains(unit, "XDG_RUNTIME_DIR") || !strings.Contains(unit, "/opt/gaia/bin/gaia") {
				t.Errorf("unit lacks the socket dir or binary:\n%s", unit)
			}
			for _, c := range fs.cmds {
				if strings.Contains(c, key) || strings.Contains(c, "sudo") {
					t.Errorf("ran %q", c)
				}
			}
		})
	}
}

func TestServiceInstallTwiceAndUninstall(t *testing.T) {
	useFakeService(t, "linux")
	state := filepath.Join(t.TempDir(), "state")
	code, out, errOut := runSvc("install", "host", "--node", "--state", state)
	if code != 0 || !strings.Contains(out, "wrote ") {
		t.Fatalf("install: %d %q %q", code, out, errOut)
	}
	code, out, _ = runSvc("install", "host", "--node", "--state", state)
	if code != 0 || !strings.Contains(out, "nothing changed") {
		t.Errorf("second install: %d %q", code, out)
	}
	code, out, _ = runSvc("uninstall")
	if code != 0 || !strings.Contains(out, "removed") || !strings.Contains(out, "state directory is left in place") {
		t.Errorf("uninstall: %d %q", code, out)
	}
	code, out, _ = runSvc("uninstall")
	if code != 0 || !strings.Contains(out, "nothing changed") {
		t.Errorf("second uninstall: %d %q", code, out)
	}
}

func TestServiceForeignUnit(t *testing.T) {
	fs := useFakeService(t, "linux")
	path := filepath.Join(fs.home, ".config", "systemd", "user", "gaia.service")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runSvc("install", "host"); code != 1 || !strings.Contains(errOut, "refusing to overwrite") {
		t.Errorf("install: %d %q", code, errOut)
	}
	if code, _, errOut := runSvc("uninstall"); code != 1 || !strings.Contains(errOut, "refusing to remove") {
		t.Errorf("uninstall: %d %q", code, errOut)
	}
	if b, _ := os.ReadFile(path); string(b) != "[Service]\n" {
		t.Errorf("foreign unit changed: %q", b)
	}
}

func TestServiceUsage(t *testing.T) {
	if code, _, _ := runSvc(); code != 2 {
		t.Errorf("no args: %d", code)
	}
	if code, _, _ := runSvc("bogus"); code != 2 {
		t.Errorf("bogus: %d", code)
	}
	if code, _, _ := runSvc("uninstall", "x"); code != 2 {
		t.Errorf("uninstall x: %d", code)
	}
	if code, _, _ := runSvc("install", "host", "-h"); code != 0 {
		t.Errorf("-h: %d", code)
	}
}
