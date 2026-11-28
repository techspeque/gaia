package repo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// seen records every argv the package passed to git during the test run.
var (
	seenMu sync.Mutex
	seen   [][]string
)

// isForbiddenArgv is the tests' own statement of what Gaia must never run,
// written apart from the package's guard so a gap in one shows up in the
// other.
func isForbiddenArgv(argv []string) bool {
	sub := ""
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if sub == "" && a == "-c" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree=") {
			continue
		}
		if sub == "" {
			sub = a
			switch a {
			case "reset", "rebase", "stash", "clean":
				return true
			}
			continue
		}
		if a == "-f" || a == "--hard" || a == "--mirror" || a == "--delete" || a == "-D" || a == "-d" ||
			strings.HasPrefix(a, "--force") || strings.HasPrefix(a, "+") ||
			(sub == "push" && strings.Contains(a, ":+")) {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gaia-repo-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n[user]\n\tname = Test\n\temail = test@example.invalid\n[protocol \"file\"]\n\tallow = always\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	observeArgv = func(argv []string) {
		seenMu.Lock()
		seen = append(seen, argv)
		seenMu.Unlock()
	}
	code := m.Run()
	seenMu.Lock()
	n := len(seen)
	for _, argv := range seen {
		if isForbiddenArgv(argv) {
			fmt.Fprintf(os.Stderr, "FAIL: package ran forbidden git argv %q\n", argv)
			code = 1
		}
	}
	seenMu.Unlock()
	if code == 0 && n == 0 {
		fmt.Fprintln(os.Stderr, "FAIL: no git invocation was recorded")
		code = 1
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// gitT runs git in dir for test setup, outside the package's runner.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "--", file)
	gitT(t, dir, "commit", "--quiet", "-m", "change "+file)
	return gitT(t, dir, "rev-parse", "HEAD")
}

// commitNoFatal commits a new file, for use off the test goroutine.
func commitNoFatal(dir, file string) error {
	if err := os.WriteFile(filepath.Join(dir, file), []byte(file+"\n"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"add", "--", file}, {"commit", "--quiet", "-m", "change " + file}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
	}
	return nil
}

// fixture is a bare remote, a caller's clone of it and a node workspace.
type fixture struct {
	remote, caller string
	ws             *Workspace
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{remote: filepath.Join(base, "remote.git"), caller: filepath.Join(base, "caller")}
	gitT(t, base, "init", "--quiet", "--bare", f.remote)
	gitT(t, base, "clone", "--quiet", f.remote, f.caller)
	commitFile(t, f.caller, "README", "one\n")
	gitT(t, f.caller, "push", "--quiet", "origin", "main")
	ws, err := Open(filepath.Join(base, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	f.ws = ws
	return f
}

func (f *fixture) remoteTip(t *testing.T, branch string) string {
	t.Helper()
	return gitT(t, f.remote, "rev-parse", "refs/heads/"+branch)
}

func (f *fixture) acquire(t *testing.T) *Checkout {
	t.Helper()
	c, err := f.ws.Acquire(context.Background(), f.remote)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Release)
	return c
}

func (f *fixture) prepared(t *testing.T, branch string) *Checkout {
	t.Helper()
	c := f.acquire(t)
	if _, err := c.Prepare(context.Background(), branch); err != nil {
		t.Fatal(err)
	}
	return c
}

// snapshot is the checkout's work tree bytes, HEAD, local branches and
// index status.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "file %s %q\n", p, data)
	}
	fmt.Fprintf(&b, "HEAD %s\n", gitT(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"))
	fmt.Fprintf(&b, "HEADsha %s\n", gitT(t, dir, "rev-parse", "HEAD"))
	fmt.Fprintf(&b, "refs %s\n", gitT(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"))
	fmt.Fprintf(&b, "status %s\n", gitT(t, dir, "status", "--porcelain=v1", "--untracked-files=all"))
	return b.String()
}

func wantSync(t *testing.T, err error, stage Stage, reason Reason) {
	t.Helper()
	var se *SyncError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SyncError %s/%s", err, stage, reason)
	}
	if se.Stage != stage || se.Reason != reason {
		t.Fatalf("err = %s/%s (%v), want %s/%s", se.Stage, se.Reason, se, stage, reason)
	}
	want := core.ExitSyncBeforeFailed
	if stage == StageAfter {
		want = core.ExitSyncAfterFailed
	}
	if se.ExitCode() != want {
		t.Fatalf("exit code %d, want %d", se.ExitCode(), want)
	}
	if !errors.Is(err, &SyncError{Reason: reason}) {
		t.Fatalf("errors.Is by reason failed for %v", err)
	}
}

func wantInvalid(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, core.NewError(core.CodeInvalidArgument, "")) {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

// --- naming and validation -------------------------------------------------

func TestDirNameIsOneSafeComponent(t *testing.T) {
	urls := []string{
		"/../../etc/passwd",
		"/",
		"/..",
		"/tmp/x/../../..",
		"file:///../../x",
		"https://example.com/../../../../x",
		"https://example.com/%2e%2e/%2e%2e",
		"ssh://git@example.com/../..",
		"git@example.com:../../x",
		"git@example.com:..",
		"https://example.com/" + strings.Repeat("a/", 300),
		"https://example.com/ü/日本",
		"/a\\..\\..\\b",
	}
	names := map[string]string{}
	for _, u := range urls {
		n, err := DirName(u)
		if err != nil {
			t.Fatalf("DirName(%q): %v", u, err)
		}
		if n == "." || n == ".." || strings.ContainsAny(n, `/\`) || strings.HasPrefix(n, ".") || n == "" {
			t.Errorf("DirName(%q) = %q, not a safe single component", u, n)
		}
		if len(n) > maxNameLen+1+16+1 {
			t.Errorf("DirName(%q) = %q, too long", u, n)
		}
		root := t.TempDir()
		if got := filepath.Dir(filepath.Join(root, n)); got != root {
			t.Errorf("DirName(%q) = %q escapes root", u, n)
		}
		if again, _ := DirName(u); again != n {
			t.Errorf("DirName(%q) not deterministic", u)
		}
		if other, ok := names[n]; ok {
			t.Errorf("DirName(%q) == DirName(%q) = %q", u, other, n)
		}
		names[n] = u
	}
	a, _ := DirName("https://example.com/a/b")
	b, _ := DirName("https://example.com/a_b")
	if a == b {
		t.Errorf("distinct URLs share a name %q", a)
	}
}

func TestDirNameDropsUserInfo(t *testing.T) {
	n, err := DirName("ssh://alice@example.com/org/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(n, "alice") {
		t.Errorf("name %q keeps the user", n)
	}
	if !strings.HasPrefix(n, "example.com_org_repo.git-") {
		t.Errorf("name %q, want readable prefix", n)
	}
}

func TestValidateURL(t *testing.T) {
	bad := []string{
		"",
		"-uhack",
		"--upload-pack=touch /tmp/x",
		"+refs",
		"ext::sh -c touch% /tmp/pwned",
		"fd::17",
		"transport::anything",
		"https://user:secret@example.com/r.git",
		"https://ghp_token@example.com/r.git",
		"ssh://u:pw@example.com/r.git",
		"/tmp/a\nb",
		"/tmp/a\x00b",
		"relative/path",
		"./repo",
		"../repo",
		"gopher://example.com/r",
		"https://" + strings.Repeat("a", maxURLLen),
	}
	for _, u := range bad {
		wantInvalid(t, ValidateURL(u))
		_, err := DirName(u)
		wantInvalid(t, err)
	}
	good := []string{
		"/srv/git/repo.git",
		"file:///srv/git/repo.git",
		"https://github.com/org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"ssh://git@[::1]:22/repo.git",
		"git@github.com:org/repo.git",
		"github.com:org/repo.git",
	}
	for _, u := range good {
		if err := ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want ok", u, err)
		}
	}
}

func TestPrepareRejectsInvalidBranch(t *testing.T) {
	f := newFixture(t)
	c := f.acquire(t)
	for _, b := range []string{"", "-f", "+main", "a..b", "a b", "main:other", "refs/heads/x~1", "HEAD@{1}"} {
		_, err := c.Prepare(context.Background(), b)
		wantInvalid(t, err)
		_, err = c.Finish(context.Background(), b)
		wantInvalid(t, err)
	}
	if _, err := os.Lstat(c.Dir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an invalid branch cloned the checkout")
	}
}

func TestAcquireRejectsSymlinkCheckout(t *testing.T) {
	f := newFixture(t)
	name, _ := DirName(f.remote)
	if err := os.Symlink(t.TempDir(), filepath.Join(f.ws.Root(), name)); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	_, err := f.ws.Acquire(context.Background(), f.remote)
	wantSync(t, err, StageBefore, ReasonNotACheckout)
	// The refused Acquire released its hold.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := f.ws.Acquire(ctx, f.remote); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Acquire = %v, want the same refusal without waiting", err)
	}
}

// --- before a session ------------------------------------------------------

func TestPrepareClonesAndStartsAtRemoteTip(t *testing.T) {
	f := newFixture(t)
	c := f.acquire(t)
	p, err := c.Prepare(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.remoteTip(t, "main"); p.Head != want || gitT(t, c.Dir(), "rev-parse", "HEAD") != want {
		t.Fatalf("head %s, want remote tip %s", p.Head, want)
	}
	if fi, err := os.Stat(f.ws.Root()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("workspace mode %v, want 0700", fi.Mode().Perm())
	}
	if filepath.Dir(c.Dir()) != f.ws.Root() {
		t.Errorf("checkout %s not directly under %s", c.Dir(), f.ws.Root())
	}
	entries, _ := os.ReadDir(f.ws.Root())
	if len(entries) != 1 {
		t.Errorf("workspace holds %d entries, want only the checkout", len(entries))
	}
}

func TestPrepareIgnoresCallerUnpushedCommits(t *testing.T) {
	f := newFixture(t)
	tip := f.remoteTip(t, "main")
	commitFile(t, f.caller, "local-only", "not pushed\n")
	c := f.acquire(t)
	p, err := c.Prepare(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if p.Head != tip {
		t.Fatalf("head %s, want remote tip %s, not the caller's unpushed commit", p.Head, tip)
	}
}

func TestPrepareFastForwards(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	commitFile(t, f.caller, "next", "two\n")
	gitT(t, f.caller, "push", "--quiet", "origin", "main")
	p, err := c.Prepare(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.remoteTip(t, "main"); p.Head != want {
		t.Fatalf("head %s, want %s", p.Head, want)
	}
	if p.PushedEarlier {
		t.Error("PushedEarlier set with nothing to push")
	}
}

func TestPrepareNewBranchFromRemote(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	gitT(t, f.caller, "switch", "--quiet", "-c", "feature/x")
	commitFile(t, f.caller, "feat", "x\n")
	gitT(t, f.caller, "push", "--quiet", "origin", "feature/x")
	p, err := c.Prepare(context.Background(), "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.remoteTip(t, "feature/x"); p.Head != want {
		t.Fatalf("head %s, want %s", p.Head, want)
	}
	if got := gitT(t, c.Dir(), "symbolic-ref", "HEAD"); got != "refs/heads/feature/x" {
		t.Fatalf("HEAD on %s, want feature/x", got)
	}
	// Switching back to an existing branch also works.
	if _, err := c.Prepare(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareBranchNotFound(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	before := snapshot(t, c.Dir())
	_, err := c.Prepare(context.Background(), "nope")
	wantSync(t, err, StageBefore, ReasonBranchNotFound)
	if after := snapshot(t, c.Dir()); after != before {
		t.Fatalf("checkout changed:\n%s\n---\n%s", before, after)
	}
}

// failingCheckoutHooks installs post-checkout and post-merge hooks in the
// checkout that touch a marker file and exit 1. Each runs after git has changed the checkout, so a failure they
// cause cannot leave the checkout as it was.
func failingCheckoutHooks(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks need a POSIX shell")
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch '" + marker + "'\nexit 1\n"
	for _, name := range []string{"post-checkout", "post-merge"} {
		if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", name), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return marker
}

// A failing checkout hook can neither fail Prepare after the checkout has
// changed nor run at all: the session starts at the remote tip.
func TestPrepareIgnoresFailingCheckoutHooks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch string
		setup  func(t *testing.T, f *fixture, c *Checkout)
	}{
		{"remote only branch", "feature/x", func(t *testing.T, f *fixture, c *Checkout) {
			gitT(t, f.caller, "switch", "--quiet", "-c", "feature/x")
			commitFile(t, f.caller, "feat", "x\n")
			gitT(t, f.caller, "push", "--quiet", "origin", "feature/x")
		}},
		{"switch and fast-forward", "main", func(t *testing.T, f *fixture, c *Checkout) {
			gitT(t, c.Dir(), "switch", "--quiet", "--detach")
			commitFile(t, f.caller, "next", "two\n")
			gitT(t, f.caller, "push", "--quiet", "origin", "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c := f.prepared(t, "main")
			tc.setup(t, f, c)
			marker := failingCheckoutHooks(t, c.Dir())
			p, err := c.Prepare(context.Background(), tc.branch)
			if err != nil {
				t.Fatal(err)
			}
			if want := f.remoteTip(t, tc.branch); p.Head != want {
				t.Fatalf("head %s, want %s", p.Head, want)
			}
			if got := gitT(t, c.Dir(), "symbolic-ref", "HEAD"); got != "refs/heads/"+tc.branch {
				t.Fatalf("HEAD on %s, want %s", got, tc.branch)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a checkout hook ran during Prepare")
			}
		})
	}
}

// A switch or fast-forward that git refuses leaves HEAD, every local branch
// and the work tree bytes as they were.
func TestPrepareCheckoutFailureLeavesCheckout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch string
		reason Reason
		setup  func(t *testing.T, f *fixture, c *Checkout)
	}{
		{"remote only branch", "feature/x", ReasonCheckoutFailed, func(t *testing.T, f *fixture, c *Checkout) {
			gitT(t, f.caller, "switch", "--quiet", "-c", "feature/x")
			commitFile(t, f.caller, "feat", "x\n")
			gitT(t, f.caller, "push", "--quiet", "origin", "feature/x")
		}},
		{"existing branch", "main", ReasonCheckoutFailed, func(t *testing.T, f *fixture, c *Checkout) {
			gitT(t, c.Dir(), "switch", "--quiet", "--detach", "HEAD")
			commitFile(t, c.Dir(), "detached", "d\n")
			commitFile(t, f.caller, "next", "two\n")
			gitT(t, f.caller, "push", "--quiet", "origin", "main")
		}},
		{"fast-forward", "main", ReasonFastForwardFailed, func(t *testing.T, f *fixture, c *Checkout) {
			commitFile(t, f.caller, "next", "two\n")
			gitT(t, f.caller, "push", "--quiet", "origin", "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c := f.prepared(t, "main")
			tc.setup(t, f, c)
			failingCheckoutHooks(t, c.Dir())
			// A held index lock makes git refuse to update the work tree.
			lock := filepath.Join(c.Dir(), ".git", "index.lock")
			if err := os.WriteFile(lock, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, c.Dir())
			_, err := c.Prepare(context.Background(), tc.branch)
			wantSync(t, err, StageBefore, tc.reason)
			if after := snapshot(t, c.Dir()); after != before {
				t.Fatalf("checkout changed:\n%s\n---\n%s", before, after)
			}
		})
	}
}

func TestPrepareCloneFailure(t *testing.T) {
	f := newFixture(t)
	c, err := f.ws.Acquire(context.Background(), filepath.Join(t.TempDir(), "missing.git"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	_, err = c.Prepare(context.Background(), "main")
	wantSync(t, err, StageBefore, ReasonCloneFailed)
	entries, _ := os.ReadDir(f.ws.Root())
	if len(entries) != 0 {
		t.Fatalf("failed clone left %d entries in the workspace", len(entries))
	}
}

func TestPrepareDirty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty func(t *testing.T, dir string)
	}{
		{"modified", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "README"), []byte("edited\n"), 0o644)
		}},
		{"untracked", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644)
		}},
		{"staged", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "README"), []byte("staged\n"), 0o644)
			gitT(t, dir, "add", "README")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c := f.prepared(t, "main")
			tc.dirty(t, c.Dir())
			// The remote moves on, so a sync that ran would change the checkout.
			commitFile(t, f.caller, "more", "x\n")
			gitT(t, f.caller, "push", "--quiet", "origin", "main")
			before := snapshot(t, c.Dir())
			trackBefore := gitT(t, c.Dir(), "rev-parse", "refs/remotes/origin/main")
			_, err := c.Prepare(context.Background(), "main")
			wantSync(t, err, StageBefore, ReasonDirty)
			if after := snapshot(t, c.Dir()); after != before {
				t.Fatalf("checkout changed:\n%s\n---\n%s", before, after)
			}
			if got := gitT(t, c.Dir(), "rev-parse", "refs/remotes/origin/main"); got != trackBefore {
				t.Fatal("dirty checkout was fetched")
			}
		})
	}
}

func TestPrepareDiverged(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	commitFile(t, c.Dir(), "node", "node side\n")
	commitFile(t, f.caller, "caller", "caller side\n")
	gitT(t, f.caller, "push", "--quiet", "origin", "main")
	remoteBefore := f.remoteTip(t, "main")
	before := snapshot(t, c.Dir())
	_, err := c.Prepare(context.Background(), "main")
	wantSync(t, err, StageBefore, ReasonDiverged)
	if after := snapshot(t, c.Dir()); after != before {
		t.Fatalf("checkout changed:\n%s\n---\n%s", before, after)
	}
	if f.remoteTip(t, "main") != remoteBefore {
		t.Fatal("remote changed")
	}
}

func TestWrongRemote(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	gitT(t, c.Dir(), "remote", "set-url", "origin", "/somewhere/else.git")
	before := snapshot(t, c.Dir())
	_, err := c.Prepare(context.Background(), "main")
	wantSync(t, err, StageBefore, ReasonWrongRemote)
	_, err = c.Finish(context.Background(), "main")
	wantSync(t, err, StageAfter, ReasonWrongRemote)
	if after := snapshot(t, c.Dir()); after != before {
		t.Fatal("checkout changed")
	}
}

func TestPrepareRejectsCheckoutWithoutGitDir(t *testing.T) {
	f := newFixture(t)
	name, _ := DirName(f.remote)
	if err := os.Mkdir(filepath.Join(f.ws.Root(), name), 0o700); err != nil {
		t.Fatal(err)
	}
	c := f.acquire(t)
	_, err := c.Prepare(context.Background(), "main")
	wantSync(t, err, StageBefore, ReasonNotACheckout)
}

// --- after a session -------------------------------------------------------

func TestFinishPushes(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	sha := commitFile(t, c.Dir(), "work", "session work\n")
	fin, err := c.Finish(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if !fin.Pushed || fin.Head != sha {
		t.Fatalf("finish = %+v, want pushed %s", fin, sha)
	}
	if got := f.remoteTip(t, "main"); got != sha {
		t.Fatalf("remote at %s, want the session's commit %s", got, sha)
	}
}

func TestFinishPushesNewBranchCreatedBySession(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	gitT(t, c.Dir(), "switch", "--quiet", "-c", "made-here")
	sha := commitFile(t, c.Dir(), "w", "w\n")
	if _, err := c.Finish(context.Background(), "made-here"); err != nil {
		t.Fatal(err)
	}
	if got := f.remoteTip(t, "made-here"); got != sha {
		t.Fatalf("remote at %s, want %s", got, sha)
	}
}

func TestFinishNothingToPush(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	fin, err := c.Finish(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if fin.Pushed {
		t.Fatal("pushed with nothing ahead")
	}
	_, err = c.Finish(context.Background(), "gone")
	wantSync(t, err, StageAfter, ReasonBranchNotFound)
}

func TestFinishRejected(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	commitFile(t, c.Dir(), "work", "session work\n")
	commitFile(t, f.caller, "race", "someone else\n")
	gitT(t, f.caller, "push", "--quiet", "origin", "main")
	remoteBefore := f.remoteTip(t, "main")
	before := snapshot(t, c.Dir())
	_, err := c.Finish(context.Background(), "main")
	wantSync(t, err, StageAfter, ReasonPushRejected)
	if after := snapshot(t, c.Dir()); after != before {
		t.Fatalf("checkout changed:\n%s\n---\n%s", before, after)
	}
	if f.remoteTip(t, "main") != remoteBefore {
		t.Fatal("remote changed by a rejected push")
	}
	// The next run reports the divergence rather than repairing it.
	_, err = c.Prepare(context.Background(), "main")
	wantSync(t, err, StageBefore, ReasonDiverged)
}

// rejectPushes installs a pre-receive hook on the remote that refuses every
// push while the returned marker file exists.
func rejectPushes(t *testing.T, remote string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks need a POSIX shell")
	}
	marker := filepath.Join(t.TempDir(), "reject")
	hook := "#!/bin/sh\nif [ -e '" + marker + "' ]; then echo rejected by test >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestPreparePushesEarlierUnpushedWork(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	sha := commitFile(t, c.Dir(), "work", "session work\n")
	marker := rejectPushes(t, f.remote)
	_, err := c.Finish(context.Background(), "main")
	wantSync(t, err, StageAfter, ReasonPushRejected)

	os.Remove(marker)
	p, err := c.Prepare(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if !p.PushedEarlier || p.Head != sha || f.remoteTip(t, "main") != sha {
		t.Fatalf("prepare = %+v, remote %s, want earlier work %s pushed", p, f.remoteTip(t, "main"), sha)
	}
}

func TestPreparePushesEarlierUnpushedNewBranch(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	gitT(t, c.Dir(), "switch", "--quiet", "-c", "fresh")
	sha := commitFile(t, c.Dir(), "w", "w\n")
	gitT(t, c.Dir(), "switch", "--quiet", "main")
	p, err := c.Prepare(context.Background(), "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if !p.PushedEarlier || p.Head != sha || f.remoteTip(t, "fresh") != sha {
		t.Fatalf("prepare = %+v, want fresh pushed at %s", p, sha)
	}
}

func TestPrepareEarlierWorkRejectedIs93(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	commitFile(t, c.Dir(), "work", "session work\n")
	rejectPushes(t, f.remote)
	remoteBefore := f.remoteTip(t, "main")
	before := snapshot(t, c.Dir())
	_, err := c.Prepare(context.Background(), "main")
	wantSync(t, err, StageBefore, ReasonPushRejected)
	if after := snapshot(t, c.Dir()); after != before {
		t.Fatal("checkout changed")
	}
	if f.remoteTip(t, "main") != remoteBefore {
		t.Fatal("remote changed")
	}
}

func TestFinishAfterRestart(t *testing.T) {
	f := newFixture(t)
	c := f.prepared(t, "main")
	sha := commitFile(t, c.Dir(), "work", "w\n")
	c.Release()
	// A new workspace value, as a restarted daemon would open.
	ws, err := Open(f.ws.Root())
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ws.Acquire(context.Background(), f.remote)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Release()
	if _, err := c2.Finish(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	if f.remoteTip(t, "main") != sha {
		t.Fatal("restarted Finish did not push")
	}
}

// --- the git runner --------------------------------------------------------

func TestGuardRefusesForbidden(t *testing.T) {
	cases := [][]string{
		{"reset", "--hard"},
		{"reset"},
		{"rebase", "main"},
		{"stash"},
		{"clean", "-fdx"},
		{"push", "--force", "origin", "main"},
		{"push", "-f", "origin", "main"},
		{"push", "--force-with-lease", "origin", "main"},
		{"push", "--force-if-includes", "origin", "main"},
		{"push", "origin", "+refs/heads/main:refs/heads/main"},
		{"push", "--delete", "origin", "main"},
		{"push", "-d", "origin", "main"},
		{"push", "--mirror", "origin"},
		{"branch", "-D", "main"},
		{"checkout", "-f", "main"},
		{"switch", "--force", "main"},
		{"fetch", "--prune", "origin"},
		{"clean", "-xfd"},
	}
	seenMu.Lock()
	n := len(seen)
	seenMu.Unlock()
	for _, argv := range cases {
		if err := checkArgv(argv[0], argv[1:]); err == nil {
			t.Errorf("checkArgv(%q) allowed", argv)
		}
		if _, err := git(context.Background(), t.TempDir(), argv[0], argv[1:]...); err == nil {
			t.Errorf("git(%q) ran", argv)
		}
		if !isForbiddenArgv(argv) && argv[0] != "fetch" && argv[0] != "clean" && argv[0] != "checkout" {
			t.Errorf("test list and isForbiddenArgv disagree on %q", argv)
		}
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != n {
		t.Fatalf("a refused argv reached git: %q", seen[n:])
	}
	for _, ok := range [][]string{
		{"push", "--porcelain", "origin", "refs/heads/main:refs/heads/main"},
		{"switch", "--quiet", "-c", "b", "--track", "refs/remotes/origin/b"},
		{"status", "--porcelain=v1", "-z", "--untracked-files=all"},
		{"merge", "--ff-only", "--quiet", "refs/remotes/origin/main"},
	} {
		if err := checkArgv(ok[0], ok[1:]); err != nil {
			t.Errorf("checkArgv(%q) = %v, want allowed", ok, err)
		}
	}
}

func TestGitEnvStripsRepositoryVariables(t *testing.T) {
	f := newFixture(t)
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_WORK_TREE", "/elsewhere")
	t.Setenv("GIT_INDEX_FILE", "/elsewhere/index")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	env := strings.Join(gitEnv(), "\n") + "\n"
	for _, bad := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_TERMINAL_PROMPT=1"} {
		if strings.Contains("\n"+env, "\n"+bad) {
			t.Errorf("env keeps %s", bad)
		}
	}
	if !strings.Contains(env, "GIT_TERMINAL_PROMPT=0\n") {
		t.Error("env lacks GIT_TERMINAL_PROMPT=0")
	}
	// And a sync still works with them set.
	c := f.acquire(t)
	if _, err := c.Prepare(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
}

// --- queueing --------------------------------------------------------------

func TestAcquireQueues(t *testing.T) {
	f := newFixture(t)
	const runs = 6
	var (
		mu      sync.Mutex
		active  int
		overlap bool
		order   []int
		wg      sync.WaitGroup
	)
	first, err := f.ws.Acquire(context.Background(), f.remote)
	if err != nil {
		t.Fatal(err)
	}
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := f.ws.Acquire(context.Background(), f.remote)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			active++
			if active > 1 {
				overlap = true
			}
			order = append(order, i)
			mu.Unlock()
			if _, err := c.Prepare(context.Background(), "main"); err != nil {
				t.Error(err)
			}
			if err := commitNoFatal(c.Dir(), fmt.Sprintf("run%d", i)); err != nil {
				t.Error(err)
			}
			if _, err := c.Finish(context.Background(), "main"); err != nil {
				t.Error(err)
			}
			mu.Lock()
			active--
			mu.Unlock()
			c.Release()
			c.Release() // idempotent
		}(i)
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	started := len(order)
	mu.Unlock()
	if started != 0 {
		t.Fatalf("%d runs started while the checkout was held", started)
	}
	first.Release()
	wg.Wait()
	if overlap {
		t.Fatal("two runs held one checkout at once")
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("order %v, want first come first served", order)
		}
	}
	if got := gitT(t, f.remote, "rev-list", "--count", "main"); got != fmt.Sprint(runs+1) {
		t.Fatalf("remote has %s commits, want %d", got, runs+1)
	}
}

func TestAcquireCancelWhileQueued(t *testing.T) {
	f := newFixture(t)
	held := f.acquire(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.ws.Acquire(ctx, f.remote); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	held.Release()
	// The cancelled waiter did not take the checkout.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	c, err := f.ws.Acquire(ctx2, f.remote)
	if err != nil {
		t.Fatalf("checkout not free after cancelled wait: %v", err)
	}
	c.Release()
}

func TestAcquireDifferentReposDoNotQueue(t *testing.T) {
	f := newFixture(t)
	g := newFixture(t)
	f.acquire(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := f.ws.Acquire(ctx, g.remote)
	if err != nil {
		t.Fatalf("another repository waited: %v", err)
	}
	c.Release()
}
