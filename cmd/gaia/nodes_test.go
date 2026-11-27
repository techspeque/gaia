package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/node"
)

const nodesFixture = "../../testdata/contract/gaia/api-v1/nodes.json"

// waitLocalOnline polls until the daemon lists exactly one node, online.
func waitLocalOnline(t *testing.T, d *daemon) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if n := d.nodes(t); len(n) == 1 && n[0].State == core.NodeOnline {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("local node not online\nstderr:\n%s", d.stderr)
}

// runCLI runs the gaia CLI in-process against the daemon in xdg.
func runCLI(t *testing.T, xdg string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestNodesJSONMatchesFixture runs `gaia nodes -o json` against a fresh
// `gaia host --node` and compares it with the recorded fixture. The id and
// last_seen vary per run and are checked for form; name, os, arch and
// gaia_version are this machine's and are checked against it. Everything
// else, and the set of keys, must equal the fixture.
func TestNodesJSONMatchesFixture(t *testing.T) {
	skipIfNoDaemon(t)
	xdg := shortTempDir(t)
	d := startDaemon(t, xdg, "--node")
	d.waitReady(t)
	waitLocalOnline(t, d)

	code, out, errOut := runCLI(t, xdg, "nodes", "-o", "json")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var got map[string][]map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	fixture, err := os.ReadFile(nodesFixture)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]map[string]any
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["nodes"]) != 1 {
		t.Fatalf("want one key and one node, got %s", out)
	}
	g, w := got["nodes"][0], want["nodes"][0]

	id, _ := g["id"].(string)
	if _, err := core.ParseNodeID(id); err != nil {
		t.Errorf("id: %v", err)
	}
	seen, _ := g["last_seen"].(string)
	if _, err := time.Parse(time.RFC3339Nano, seen); err != nil {
		t.Errorf("last_seen: %v", err)
	}
	local := node.LocalRequest(version)
	for k, v := range map[string]string{"name": local.Name, "os": runtime.GOOS, "arch": runtime.GOARCH, "gaia_version": version} {
		if g[k] != v {
			t.Errorf("%s = %v, want %q", k, g[k], v)
		}
	}
	for _, k := range []string{"id", "last_seen", "name", "os", "arch", "gaia_version"} {
		if _, ok := g[k]; ok {
			g[k] = w[k]
		}
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("gaia nodes -o json differs from %s apart from per-run fields\ngot:  %v\nwant: %v", nodesFixture, g, w)
	}
}

func TestNodesText(t *testing.T) {
	skipIfNoDaemon(t)
	xdg := shortTempDir(t)
	d := startDaemon(t, xdg, "--node")
	d.waitReady(t)
	waitLocalOnline(t, d)

	code, out, errOut := runCLI(t, xdg, "nodes")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("output:\n%s", out)
	}
	for _, want := range []string{node.LocalRequest(version).Name, "online", runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row %q lacks %q", lines[1], want)
		}
	}
}

func TestNodesEmptyRegistry(t *testing.T) {
	skipIfNoDaemon(t)
	xdg := shortTempDir(t)
	d := startDaemon(t, xdg)
	d.waitReady(t)

	code, out, errOut := runCLI(t, xdg, "nodes", "--output", "json")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if out != "{\n  \"nodes\": []\n}\n" {
		t.Errorf("got %q", out)
	}
}

// TestNodesNoDaemonBinary runs the real binary with no daemon: it prints
// the one message and exits with the reserved no_daemon code.
func TestNodesNoDaemonBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "nodes")
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "XDG_RUNTIME_DIR="+shortTempDir(t))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != int(core.ExitNoDaemon) {
		t.Fatalf("err %v, want exit %d; stderr %q", err, core.ExitNoDaemon, stderr.String())
	}
	msg := stderr.String()
	if strings.Count(msg, "\n") != 1 || !strings.HasPrefix(msg, "gaia: no Gaia daemon is running") {
		t.Errorf("stderr %q", msg)
	}
	for _, want := range []string{"`gaia host`", "`gaia node"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr %q lacks %q", msg, want)
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q", stdout.String())
	}
}
