package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/net/nettest"
)

// minMetis is the first Metis release that pushes before and fetches after
// a session of an agent with `remote: true` (docs/contracts/launcher-v1.md).
var minMetis = [3]int{0, 0, 25}

// The temporary project's agents: a coder and a reviewer of different
// vendors, as Metis requires for a review.
const (
	coderSlug    = "claude-code/coder"
	reviewerSlug = "opencode/reviewer"
	metisSlice   = "ws-1"
	workBranch   = "work"
)

// needMetis skips the test, with the reason, when metis is not on PATH or
// is older than minMetis. Gaia does not depend on Metis (ADR-0001 rule 4).
func needMetis(t *testing.T) {
	t.Helper()
	skipIfNoDaemon(t)
	path, err := exec.LookPath("metis")
	if err != nil {
		t.Skip("metis is not on PATH; the Metis end-to-end tests need it")
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Skipf("metis --version: %v", err)
	}
	v, ok := parseMetisVersion(string(out))
	if !ok {
		t.Skipf("metis --version printed %q, not a version", out)
	}
	for i := range v {
		if v[i] != minMetis[i] {
			if v[i] < minMetis[i] {
				t.Skipf("metis %d.%d.%d is older than %d.%d.%d, which syncs remote agents", v[0], v[1], v[2], minMetis[0], minMetis[1], minMetis[2])
			}
			break
		}
	}
}

// parseMetisVersion reads "metis X.Y.Z (…)" or "metis vX.Y.Z …".
func parseMetisVersion(s string) ([3]int, bool) {
	var v [3]int
	f := strings.Fields(s)
	if len(f) < 2 {
		return v, false
	}
	parts := strings.Split(strings.TrimPrefix(f[1], "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func TestParseMetisVersion(t *testing.T) {
	for _, c := range []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"metis 0.0.25 (8430128) built 2026-11-27T15:02:29Z\n", [3]int{0, 0, 25}, true},
		{"metis v1.2.3\n", [3]int{1, 2, 3}, true},
		{"metis dev\n", [3]int{}, false},
		{"", [3]int{}, false},
	} {
		if got, ok := parseMetisVersion(c.in); got != c.want || ok != c.ok {
			t.Errorf("%q: %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestNeedMetisSkips: with metis absent from PATH, or too old to sync a
// remote agent, the Metis tests skip rather than fail (ADR-0001 rule 4).
func TestNeedMetisSkips(t *testing.T) {
	skipIfNoDaemon(t)
	old := t.TempDir()
	write(t, filepath.Join(old, "metis"), "#!/bin/sh\necho 'metis 0.0.24 (abc) built 2026-11-01T00:00:00Z'\n")
	if err := os.Chmod(filepath.Join(old, "metis"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path string }{{"absent", t.TempDir()}, {"too old", old}} {
		skipped := false
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PATH", c.path)
			defer func() { skipped = t.Skipped() }()
			needMetis(t)
		})
		if !skipped {
			t.Errorf("%s: needMetis did not skip", c.name)
		}
	}
}

// metisProject is a temporary Metis project: a local bare remote and the
// caller's clone of it, on workBranch, with one slice whose brief is
// committed. Its agents run testdata/metis-agent.sh through
// `gaia run --wait` on the nodes given.
type metisProject struct {
	remote string
	caller string
	xdg    string // the caller's gaia daemon
	steps  string // directory of the agents' STEPS files, one per role
}

func newMetisProject(t *testing.T, xdg, coderNode, reviewerNode string) *metisProject {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../testdata/metis-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	p := &metisProject{
		remote: filepath.Join(base, "remote.git"),
		caller: filepath.Join(base, "caller"),
		xdg:    xdg,
		steps:  filepath.Join(base, "steps"),
	}
	if err := os.Mkdir(p.steps, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, base, "init", "-q", "--bare", p.remote)
	gitIn(t, base, "clone", "-q", p.remote, p.caller)

	launch := func(node, role, slug string) string {
		argv := []string{exe, "run", "--node", node, "--wait", "--",
			"sh", script, role, "{slice}", slug, filepath.Join(p.steps, role)}
		b, err := json.Marshal(argv)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cfg := `version: 1
project:
  name: m18
  integration_branch: ` + workBranch + `
  release_branch: main
  overview: OVERVIEW.md
agents:
  human:
    surface: human
    model: human
    label: Human
  ` + coderSlug + `:
    surface: claude-code
    model: coder-model
    label: Coder
    remote: true
    launch: ` + launch(coderNode, "coder", coderSlug) + `
  ` + reviewerSlug + `:
    surface: opencode
    model: reviewer-model
    label: Reviewer
    remote: true
    launch: ` + launch(reviewerNode, "reviewer", reviewerSlug) + `
routing:
  review: ` + reviewerSlug + `
commands:
  verify: "true"
commits:
  prefixes: [feat, fix, docs, test, chore]
  require_slice_id: true
  no_attribution: true
  format: '{prefix}({slice_id}): {message}'
auto:
  mode: step
`
	write(t, filepath.Join(p.caller, "OVERVIEW.md"), "# m18\n\nA test project.\n")
	write(t, filepath.Join(p.caller, ".metis", "project.yaml"), cfg)
	write(t, filepath.Join(p.caller, ".metis", "plans", "plan.md"), metisPlan)
	gitIn(t, p.caller, "add", ".")
	gitIn(t, p.caller, "commit", "-qm", "init")
	gitIn(t, p.caller, "push", "-q", "-u", "origin", "main")
	gitIn(t, p.caller, "switch", "-q", "-c", workBranch)
	p.metisOK(t, "init", "--from", ".metis/project.yaml")
	p.metisOK(t, "add", "feat", "--id", metisSlice, "--title", "Remote slice",
		"--coder", coderSlug, "--reviewer", reviewerSlug, "--risk", "low",
		"--plan", ".metis/plans/plan.md", "--plan-section", "§1")
	gitIn(t, p.caller, "add", ".")
	gitIn(t, p.caller, "commit", "-qm", "chore: scaffold")
	p.metisOK(t, "brief", metisSlice, "--write")
	write(t, filepath.Join(p.caller, ".metis", "briefs", metisSlice+".md"), metisBrief)
	p.metisOK(t, "commit", "--brief")
	gitIn(t, p.caller, "push", "-q", "-u", "origin", workBranch)
	return p
}

// metisPlan is the temporary project's plan: one workstream, the slice's
// plan section, which metis auto needs to run it unattended.
const metisPlan = `---
type: plan
phase: 1
title: Remote
---

# Phase 1 — Remote

## Workstream 1: Remote slice

- **Risk:** low
- **Coder:** ` + coderSlug + `
- **Reviewer:** ` + reviewerSlug + `

Tasks:
- The agent writes one file on a remote node
`

// metisBrief is the temporary slice's brief: the coder agent writes one file.
const metisBrief = `# ` + metisSlice + ` — Remote slice

## Goal

The agent writes one file on a remote node.

## Declared file scope

- **owned_paths:** work-` + metisSlice + `.txt
- **read_only_paths:** none

## Definition of Done

The file exists.

## Test plan

None.

## Out-of-scope touches

None.

## Report

None.
`

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// metisCmd is `metis ARGS…` in the caller's checkout, with the environment
// that makes the launch line's test binary act as `gaia` against xdg.
func (p *metisProject) metisCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("metis", args...)
	cmd.Dir = p.caller
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "XDG_RUNTIME_DIR="+p.xdg)
	return cmd
}

// metis runs `metis ARGS…` and returns its exit code and combined output.
func (p *metisProject) metis(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := p.metisCmd(args...)
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return exitCode(t, cmd.Run()), out.String()
}

func (p *metisProject) metisOK(t *testing.T, args ...string) string {
	t.Helper()
	code, out := p.metis(t, args...)
	if code != 0 {
		t.Fatalf("metis %q: exit %d\n%s", args, code, out)
	}
	return out
}

// setSteps writes the STEPS file of the agent for role.
func (p *metisProject) setSteps(t *testing.T, role string, steps ...string) {
	t.Helper()
	write(t, filepath.Join(p.steps, role), strings.Join(steps, "\n")+"\n")
}

// metisSliceState is the part of `metis show -o json` the tests read.
type metisSliceState struct {
	Coded    bool   `json:"coded"`
	Reviewed bool   `json:"reviewed"`
	Status   string `json:"status"`
}

func (p *metisProject) slice(t *testing.T) metisSliceState {
	t.Helper()
	var s metisSliceState
	if err := json.Unmarshal([]byte(p.metisOK(t, "show", metisSlice, "-o", "json")), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// runLog is the run log metis wrote for its n-th session of the slice.
func (p *metisProject) runLog(t *testing.T, n int, role string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.caller, ".metis", "runs", metisSlice, "auto", fmt.Sprintf("%d-%s.log", n, role)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// nodeCheckouts are the checkouts under a node's state directory.
func nodeCheckouts(t *testing.T, state string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(state, workspaceDirName, "*"))
	if err != nil || len(m) == 0 {
		t.Fatalf("node checkouts under %s: %v %v", state, m, err)
	}
	return m
}

// checkMetisDirUntouched: in checkout, .metis/ holds the committed ledger
// unchanged, and every other path under it is metis's own run output
// (.metis/runs/, .metis/.lock), written by the agents' metis calls. Gaia
// writes nothing there (ADR-0001 rule 5).
func checkMetisDirUntouched(t *testing.T, checkout string) {
	t.Helper()
	if diff := gitIn(t, checkout, "diff", "HEAD", "--name-only", "--", ".metis"); diff != "" {
		t.Errorf("%s: tracked .metis/ files changed: %q", checkout, diff)
	}
	st := gitIn(t, checkout, "status", "--porcelain", "--ignored", "--untracked-files=all", "--", ".metis")
	for _, l := range strings.Split(st, "\n") {
		if l == "" {
			continue
		}
		path := strings.TrimSpace(l[2:])
		if !strings.HasPrefix(l, "!! ") || (path != ".metis/.lock" && !strings.HasPrefix(path, ".metis/runs/")) {
			t.Errorf("%s: unexpected path under .metis/: %q", checkout, l)
		}
	}
}

// TestMetisLaunchRemote: `metis launch` runs the coder on a node through
// `gaia run --wait`; it exits with the agent's code, the agent's output is
// in metis's run log, and the agent's ledger flip is in the caller's
// checkout when it returns.
func TestMetisLaunchRemote(t *testing.T) {
	needMetis(t)
	isolateGit(t)
	xdg, state := shortTempDir(t), shortState(t)
	d := startDaemon(t, xdg, "--node", "--state", state)
	d.waitReady(t)
	waitLocalOnline(t, d)
	node := d.nodes(t)[0].Name
	p := newMetisProject(t, xdg, node, node)

	// A failing agent: metis launch exits with its code, nothing flips.
	p.setSteps(t, "coder", "print:failing on purpose", "exit:3")
	if code, out := p.metis(t, "launch"); code != 3 {
		t.Fatalf("failing agent: metis launch exit %d, want 3\n%s", code, out)
	}
	if log := p.runLog(t, 1, "coder"); !strings.Contains(log, "failing on purpose") {
		t.Errorf("run log of the failing session lacks the agent's output:\n%s", log)
	}
	if s := p.slice(t); s.Coded {
		t.Errorf("slice coded after a failing session: %+v", s)
	}

	// The coder: exit 0, and its flip is in the caller's checkout.
	p.setSteps(t, "coder", "print:coding remotely")
	code, out := p.metis(t, "launch")
	if code != 0 {
		t.Fatalf("metis launch: exit %d\n%s", code, out)
	}
	log := p.runLog(t, 2, "coder")
	for _, want := range []string{"coding remotely", "agent: coder done", "gaia: session sess_"} {
		if !strings.Contains(log, want) {
			t.Errorf("run log lacks %q:\n%s", want, log)
		}
	}
	checkouts := nodeCheckouts(t, state)
	if dir, err := filepath.EvalSymlinks(checkouts[0]); err != nil || !strings.Contains(log, "in "+dir) {
		t.Errorf("the agent did not run in the node's checkout %s (%v):\n%s", dir, err, log)
	}
	if s := p.slice(t); !s.Coded || s.Reviewed {
		t.Errorf("slice after the coder: %+v, want coded", s)
	}
	if subj := gitIn(t, p.caller, "log", "-1", "--format=%s"); subj != "chore("+metisSlice+"): flip coded" {
		t.Errorf("caller's head %q, want the agent's flip", subj)
	}
	if _, err := os.Stat(filepath.Join(p.caller, "work-"+metisSlice+".txt")); err != nil {
		t.Errorf("the agent's work is not in the caller's checkout: %v", err)
	}
	if l := cliSessions(t, xdg); len(l) != 2 || l[0].State != core.SessionExited || l[1].State != core.SessionExited {
		t.Errorf("gaia ls: %+v", l)
	}
	for _, c := range checkouts {
		checkMetisDirUntouched(t, c)
	}
	checkMetisDirUntouched(t, p.caller)
}

// TestMetisAutoRemote: `metis auto` takes one slice through coder and
// reviewer, the coder on the host's own node and the reviewer on node-b,
// a second machine on a test tailnet, both through `gaia run --wait`.
func TestMetisAutoRemote(t *testing.T) {
	needMetis(t)
	isolateGit(t)
	control := nettest.StartControl(t, "")
	url := control.HTTPTestServer.URL
	hostName := defaultName(machineName(os.Hostname), "host")
	env := []string{allowTailnetEnv + "=1", "GAIA_AUTHKEY=" + testAuthKey, "TS_AUTHKEY="}
	xdgH, xdgB := shortTempDir(t), shortTempDir(t)
	stateH, stateB := shortState(t), shortState(t)
	h := startRole(t, "host", xdgH, env, "--state", stateH, "--tailnet", "--control-url", url, "--node",
		"--allow", "node:node-b")
	h.waitReadyWithin(t, 60*time.Second)
	b := startRole(t, "node", xdgB, env, "--host", hostName, "--name", "node-b", "--state", stateB,
		"--control-url", url, "--join")
	b.waitReadyWithin(t, 10*time.Second)
	waitNodes(t, h, 90*time.Second, "node-b online", online(hostName, "node-b"))

	p := newMetisProject(t, xdgH, hostName, "node-b")
	p.setSteps(t, "coder", "print:coder on "+hostName)
	p.setSteps(t, "reviewer", "print:reviewer on node-b")
	code, out := p.metis(t, "auto", "--slice", metisSlice)
	if code != 0 {
		t.Fatalf("metis auto: exit %d\n%s", code, out)
	}
	// Coded on one node, reviewed on the other, archived, and merged into
	// the work branch of the caller's checkout.
	if !strings.Contains(out, metisSlice+" reviewed and archived") {
		t.Errorf("metis auto did not finish the slice:\n%s", out)
	}
	if br := gitIn(t, p.caller, "branch", "--show-current"); br != workBranch {
		t.Errorf("caller on %q, want %q", br, workBranch)
	}
	subjects := gitIn(t, p.caller, "log", "--format=%s", workBranch)
	for _, want := range []string{"flip coded", "flip reviewed", "archive 1 slice(s)"} {
		if !strings.Contains(subjects, "("+metisSlice+"): "+want) {
			t.Errorf("work branch lacks %q:\n%s", want, subjects)
		}
	}
	done, err := os.ReadFile(filepath.Join(p.caller, ".metis", "slices-done.yaml"))
	if err != nil || !strings.Contains(string(done), "id: "+metisSlice) || !strings.Contains(string(done), "reviewed: true") {
		t.Errorf("archive (%v):\n%s", err, done)
	}
	if log := p.runLog(t, 1, "coder"); !strings.Contains(log, "coder on "+hostName) {
		t.Errorf("coder run log:\n%s", log)
	}
	if log := p.runLog(t, 2, "reviewer"); !strings.Contains(log, "reviewer on node-b") {
		t.Errorf("reviewer run log:\n%s", log)
	}

	// One session on each node, in that order, both exited 0.
	l := cliSessions(t, xdgH)
	if len(l) != 2 {
		t.Fatalf("gaia ls: %+v", l)
	}
	sort.Slice(l, func(i, j int) bool { return l[i].StartedAt.Before(l[j].StartedAt) })
	for i, node := range []string{hostName, "node-b"} {
		s := l[i]
		if s.NodeName != node || s.State != core.SessionExited || s.ExitCode == nil || *s.ExitCode != 0 || s.Sync != core.SyncDone {
			t.Errorf("session %d: %+v, want exited 0 on %s", i, s, node)
		}
	}
	for _, state := range []string{stateH, stateB} {
		for _, c := range nodeCheckouts(t, state) {
			checkMetisDirUntouched(t, c)
		}
	}
	checkMetisDirUntouched(t, p.caller)
}

// TestMetisAutoInterrupt: interrupting `metis auto`, as Ctrl-C does (SIGINT
// to its process group), stops the remote session and leaves no agent
// process.
func TestMetisAutoInterrupt(t *testing.T) {
	needMetis(t)
	isolateGit(t)
	xdg := shortTempDir(t)
	d := startDaemon(t, xdg, "--node", "--state", shortState(t))
	d.waitReady(t)
	waitLocalOnline(t, d)
	p := newMetisProject(t, xdg, d.nodes(t)[0].Name, d.nodes(t)[0].Name)
	p.setSteps(t, "coder", "print:coder sleeping", "sleep:3600")

	cmd := p.metisCmd("auto", "--slice", metisSlice)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var id string
	deadline := time.Now().Add(30 * time.Second)
	for id == "" {
		select {
		case err := <-exited:
			t.Fatalf("metis auto ended (%v) before the coder session started:\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the coder session did not start:\n%s", out.String())
		}
		for _, s := range cliSessions(t, xdg) {
			if s.State == core.SessionRunning {
				id = s.ID.String()
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, 30*time.Second, "the agent's output", func() bool {
		b, _ := os.ReadFile(filepath.Join(p.caller, ".metis", "runs", metisSlice, "auto", "1-coder.log"))
		return strings.Contains(string(b), "coder sleeping")
	})

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		t.Logf("metis auto after SIGINT: %v\n%s", err, out.String())
	case <-time.After(60 * time.Second):
		t.Fatalf("metis auto did not exit after SIGINT:\n%s", out.String())
	}
	s := waitSession(t, xdg, id, 30*time.Second, "stopped", func(s core.Session) bool { return s.State.Terminal() })
	if s.State != core.SessionStopped {
		t.Errorf("session after the interrupt: %+v, want stopped", s)
	}
	if processWith(t, p.steps) {
		t.Error("the agent is still running after the interrupt")
	}
	if c := p.slice(t); c.Coded {
		t.Errorf("slice coded after an interrupted session: %+v", c)
	}
}

// waitFor polls ok until it holds or within passes.
func waitFor(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestGaiaNeverNamesMetisDir: no Gaia source outside tests names the
// .metis directory, so no code path can write there (ADR-0001 rule 5).
func TestGaiaNeverNamesMetisDir(t *testing.T) {
	for _, root := range []string{"..", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() && (e.Name() == "testdata" || strings.HasPrefix(e.Name(), ".")) && path != root {
				return filepath.SkipDir
			}
			if e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), ".metis") {
				t.Errorf("%s names .metis", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
