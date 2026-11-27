package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/service"
)

// serviceE2EEnv enables TestServiceE2E. It installs a real user service on
// the machine running the test, so it runs in the CI job `service` only.
const serviceE2EEnv = "GAIA_SERVICE_E2E"

// TestServiceE2E builds gaia, installs `host --node` as a user service,
// waits for `gaia nodes` to answer through it, installs again (nothing
// changes), uninstalls, and checks the daemon is gone and the state
// directory is intact.
//
// On macOS HOME is a temporary directory, so the agent's plist is too;
// launchd loads it from there. On Linux systemd reads units only from the
// real user config directory, so the test uses it and skips if a unit is
// already there; XDG_RUNTIME_DIR must be the user manager's.
func TestServiceE2E(t *testing.T) {
	if os.Getenv(serviceE2EEnv) != "1" {
		t.Skip("set " + serviceE2EEnv + "=1 to install a real user service")
	}
	skipIfNoDaemon(t)

	bin := filepath.Join(t.TempDir(), "gaia")
	build := exec.Command("go", "build", "-tags", "ts_omit_peerapiserver", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	env := os.Environ()
	setenv := func(k, v string) { env = append(env, k+"="+v) }
	switch runtime.GOOS {
	case "darwin":
		home := t.TempDir()
		setenv("HOME", home)
		setenv("XDG_RUNTIME_DIR", shortTempDir(t))
		label := "gui/" + strconv.Itoa(os.Getuid()) + "/" + service.LaunchdLabel
		if exec.Command("launchctl", "print", label).Run() == nil {
			t.Skip("a gaia launchd agent is already loaded on this machine")
		}
	case "linux":
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			t.Fatal("XDG_RUNTIME_DIR is unset: systemctl --user cannot reach the user manager")
		}
		p, err := hostPlatform("linux")
		if err != nil {
			t.Fatal(err)
		}
		path, _ := service.UnitPath(p)
		if _, err := os.Lstat(path); err == nil {
			t.Skipf("%s already exists on this machine", path)
		}
	}
	state := filepath.Join(t.TempDir(), "state")

	gaia := func(args ...string) (int, string) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("gaia %q: %v", args, err)
		}
		return code, string(out)
	}
	t.Cleanup(func() { gaia("service", "uninstall") })

	if code, out := gaia("service", "install", "host", "--node", "--state", state); code != 0 || !strings.Contains(out, "wrote ") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	online := func() bool {
		cmd := exec.Command(bin, "nodes", "-o", "json")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			return false
		}
		var r core.ListNodesResponse
		return json.Unmarshal(out, &r) == nil && len(r.Nodes) == 1 && r.Nodes[0].State == core.NodeOnline
	}
	deadline := time.Now().Add(30 * time.Second)
	for !online() {
		if time.Now().After(deadline) {
			_, out := gaia("nodes")
			t.Fatalf("gaia nodes did not list this machine online through the service:\n%s", out)
		}
		time.Sleep(200 * time.Millisecond)
	}

	p, err := hostPlatformEnv(env)
	if err != nil {
		t.Fatal(err)
	}
	unit, _ := service.UnitPath(p)
	before, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := gaia("service", "install", "host", "--node", "--state", state); code != 0 || !strings.Contains(out, "nothing changed") {
		t.Errorf("second install: exit %d\n%s", code, out)
	}
	if after, err := os.Stat(unit); err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("second install rewrote %s", unit)
	}

	if code, out := gaia("service", "uninstall"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if _, err := os.Lstat(unit); !os.IsNotExist(err) {
		t.Errorf("unit still present after uninstall: %v", err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		code, _ := gaia("nodes")
		if code == int(core.ExitNoDaemon) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gaia nodes still exits %d after uninstall; the daemon is running", code)
		}
		time.Sleep(200 * time.Millisecond)
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) == 0 {
		t.Errorf("state directory after uninstall: %v, %d entries", err, len(entries))
	}
}

// hostPlatformEnv is hostPlatform as seen with HOME and XDG_CONFIG_HOME
// from env.
func hostPlatformEnv(env []string) (service.Platform, error) {
	p, err := hostPlatform(runtime.GOOS)
	if err != nil {
		return p, err
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			p.Home = v
		}
		if v, ok := strings.CutPrefix(kv, "XDG_CONFIG_HOME="); ok {
			p.ConfigHome = v
		}
	}
	return p, nil
}
