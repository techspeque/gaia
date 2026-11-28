package repo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Prepared is the checkout's state when the session may start.
type Prepared struct {
	Branch string
	// Head is the commit the session starts at: the remote branch's tip.
	Head string
	// PushedEarlier is set when work left by an earlier session was pushed.
	PushedEarlier bool
}

// Finished is the result of the after-session push.
type Finished struct {
	Branch string
	Head   string
	Pushed bool
}

// Prepare readies the checkout for a session on branch (ADR-0010 rule 2):
// clone on first use, refuse a dirty checkout, fetch, push work an earlier
// session could not push, then switch to branch and fast-forward it to the
// remote. On failure it returns a *SyncError (stage before, exit 93) or a
// core invalid_argument error, and the work tree, HEAD and local branches
// are as they were.
func (c *Checkout) Prepare(ctx context.Context, branch string) (Prepared, error) {
	if err := c.checkBranch(ctx, branch); err != nil {
		return Prepared{}, err
	}
	if err := c.ensureClone(ctx); err != nil {
		return Prepared{}, err
	}
	if err := c.checkCheckout(ctx, StageBefore); err != nil {
		return Prepared{}, err
	}
	if err := c.checkClean(ctx, branch); err != nil {
		return Prepared{}, err
	}
	if r, err := git(ctx, c.dir, "fetch", "origin"); err != nil {
		return Prepared{}, err
	} else if r.exit != 0 {
		return Prepared{}, syncErr(StageBefore, ReasonFetchFailed, branch, r.detail())
	}

	local, err := c.revParse(ctx, "refs/heads/"+branch)
	if err != nil {
		return Prepared{}, err
	}
	remote, err := c.revParse(ctx, remoteRef(branch))
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{Branch: branch}
	switch {
	case local == "" && remote == "":
		return Prepared{}, syncErr(StageBefore, ReasonBranchNotFound, branch, "branch is on neither the remote nor the checkout")
	case local != "" && remote != "" && local != remote:
		localBehind, err := c.isAncestor(ctx, local, remote)
		if err != nil {
			return Prepared{}, err
		}
		if !localBehind {
			remoteBehind, err := c.isAncestor(ctx, remote, local)
			if err != nil {
				return Prepared{}, err
			}
			if !remoteBehind {
				return Prepared{}, syncErr(StageBefore, ReasonDiverged, branch, "checkout and remote have diverged")
			}
			if err := c.push(ctx, StageBefore, branch); err != nil {
				return Prepared{}, err
			}
			p.PushedEarlier = true
			remote = local
		}
	case local != "" && remote == "":
		if err := c.push(ctx, StageBefore, branch); err != nil {
			return Prepared{}, err
		}
		p.PushedEarlier = true
		remote = local
	}

	cur, err := c.currentBranch(ctx)
	if err != nil {
		return Prepared{}, err
	}
	if cur != branch {
		var r gitResult
		if local == "" {
			r, err = git(ctx, c.dir, "switch", "--quiet", "-c", branch, "--track", remoteRef(branch))
		} else {
			r, err = git(ctx, c.dir, "switch", "--quiet", branch)
		}
		if err != nil {
			return Prepared{}, err
		}
		if r.exit != 0 {
			return Prepared{}, syncErr(StageBefore, ReasonCheckoutFailed, branch, r.detail())
		}
	}
	head, err := c.revParse(ctx, "HEAD")
	if err != nil {
		return Prepared{}, err
	}
	if head != remote {
		r, err := git(ctx, c.dir, "merge", "--ff-only", "--quiet", remoteRef(branch))
		if err != nil {
			return Prepared{}, err
		}
		if r.exit != 0 {
			return Prepared{}, syncErr(StageBefore, ReasonFastForwardFailed, branch, r.detail())
		}
		if head, err = c.revParse(ctx, "HEAD"); err != nil {
			return Prepared{}, err
		}
	}
	if head != remote {
		return Prepared{}, syncErr(StageBefore, ReasonFastForwardFailed, branch, "checkout is not at the remote tip after fast-forward")
	}
	p.Head = head
	return p, nil
}

// Finish pushes branch if the checkout is ahead of the remote, fast-forward
// only (ADR-0010 rule 3). It needs nothing from Prepare. On failure it
// returns a *SyncError (stage after, exit 94) and the checkout is untouched;
// the commits stay in it for the next Prepare to push.
func (c *Checkout) Finish(ctx context.Context, branch string) (Finished, error) {
	if err := c.checkBranch(ctx, branch); err != nil {
		return Finished{}, err
	}
	if err := c.checkCheckout(ctx, StageAfter); err != nil {
		return Finished{}, err
	}
	local, err := c.revParse(ctx, "refs/heads/"+branch)
	if err != nil {
		return Finished{}, err
	}
	if local == "" {
		return Finished{}, syncErr(StageAfter, ReasonBranchNotFound, branch, "branch is not in the checkout")
	}
	f := Finished{Branch: branch, Head: local}
	remote, err := c.revParse(ctx, remoteRef(branch))
	if err != nil {
		return Finished{}, err
	}
	ahead := true
	if remote != "" {
		r, err := git(ctx, c.dir, "rev-list", "--count", remote+".."+local)
		if err != nil {
			return Finished{}, err
		}
		if r.exit != 0 {
			return Finished{}, syncErr(StageAfter, ReasonGitFailed, branch, r.detail())
		}
		n, perr := strconv.Atoi(strings.TrimSpace(r.stdout))
		if perr != nil {
			return Finished{}, syncErr(StageAfter, ReasonGitFailed, branch, "rev-list output: "+r.stdout)
		}
		ahead = n > 0
	}
	if !ahead {
		return f, nil
	}
	if err := c.push(ctx, StageAfter, branch); err != nil {
		return Finished{}, err
	}
	f.Pushed = true
	return f, nil
}

func remoteRef(branch string) string { return "refs/remotes/origin/" + branch }

// checkBranch validates branch, with git's own branch-name rules last.
func (c *Checkout) checkBranch(ctx context.Context, branch string) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	r, err := git(ctx, "", "check-ref-format", "--branch", branch)
	if err != nil {
		return err
	}
	if r.exit != 0 {
		return invalid("branch name is not valid: " + branch)
	}
	return nil
}

// ensureClone clones the remote on first use, into a temporary directory
// renamed into place, so a kill mid-clone never leaves a half-made checkout
// under the real name.
func (c *Checkout) ensureClone(ctx context.Context) error {
	if _, err := os.Lstat(c.dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("repo: checkout path: %w", err)
	}
	tmp, err := os.MkdirTemp(c.w.root, ".clone-")
	if err != nil {
		return fmt.Errorf("repo: clone directory: %w", err)
	}
	r, err := git(ctx, "", "clone", "--quiet", "--origin", "origin", "--", c.url, tmp)
	if err != nil || r.exit != 0 {
		_ = os.RemoveAll(tmp)
		if err != nil {
			return err
		}
		return syncErr(StageBefore, ReasonCloneFailed, "", r.detail())
	}
	if err := os.Rename(tmp, c.dir); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("repo: placing clone: %w", err)
	}
	return nil
}

// checkCheckout refuses a checkout path that is not a real git working tree
// whose origin is the requested URL.
func (c *Checkout) checkCheckout(ctx context.Context, stage Stage) error {
	fi, err := os.Lstat(c.dir)
	if err != nil || !fi.IsDir() {
		return syncErr(stage, ReasonNotACheckout, "", "checkout directory is missing or not a directory")
	}
	gfi, err := os.Lstat(filepath.Join(c.dir, ".git"))
	if err != nil || !gfi.IsDir() {
		return syncErr(stage, ReasonNotACheckout, "", "checkout has no .git directory")
	}
	r, err := git(ctx, c.dir, "config", "--get", "remote.origin.url")
	if err != nil {
		return err
	}
	if r.exit != 0 || strings.TrimSpace(r.stdout) != c.url {
		return syncErr(stage, ReasonWrongRemote, "", "checkout's origin is not the requested URL")
	}
	return nil
}

// checkClean refuses a checkout with any change, staged, unstaged or
// untracked. It runs before anything that could alter the checkout.
func (c *Checkout) checkClean(ctx context.Context, branch string) error {
	r, err := git(ctx, c.dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	if r.exit != 0 {
		return syncErr(StageBefore, ReasonGitFailed, branch, r.detail())
	}
	if r.stdout != "" {
		return syncErr(StageBefore, ReasonDirty, branch, "checkout has uncommitted or untracked changes")
	}
	return nil
}

// revParse returns ref's commit, or "" when ref does not exist.
func (c *Checkout) revParse(ctx context.Context, ref string) (string, error) {
	r, err := git(ctx, c.dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	if r.exit != 0 {
		return "", nil
	}
	return strings.TrimSpace(r.stdout), nil
}

// isAncestor reports whether commit a is an ancestor of commit b.
func (c *Checkout) isAncestor(ctx context.Context, a, b string) (bool, error) {
	r, err := git(ctx, c.dir, "merge-base", "--is-ancestor", a, b)
	if err != nil {
		return false, err
	}
	switch r.exit {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, syncErr(StageBefore, ReasonGitFailed, "", r.detail())
}

// currentBranch is the branch HEAD is on, or "" when HEAD is detached.
func (c *Checkout) currentBranch(ctx context.Context) (string, error) {
	r, err := git(ctx, c.dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	if r.exit != 0 {
		return "", nil
	}
	return strings.TrimPrefix(strings.TrimSpace(r.stdout), "refs/heads/"), nil
}

// push pushes branch to origin, fast-forward only. A rejected ref is
// ReasonPushRejected; any other failure ReasonPushFailed.
func (c *Checkout) push(ctx context.Context, stage Stage, branch string) error {
	ref := "refs/heads/" + branch
	r, err := git(ctx, c.dir, "push", "--porcelain", "origin", ref+":"+ref)
	if err != nil {
		return err
	}
	if r.exit == 0 {
		return nil
	}
	for line := range strings.SplitSeq(r.stdout, "\n") {
		if strings.HasPrefix(line, "!") {
			return syncErr(stage, ReasonPushRejected, branch, r.detail())
		}
	}
	return syncErr(stage, ReasonPushFailed, branch, r.detail())
}
