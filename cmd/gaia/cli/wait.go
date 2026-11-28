package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// Defaults of Env.LostGrace and Env.Unreachable.
const (
	DefaultLostGrace   = 10 * time.Second
	DefaultUnreachable = 2 * time.Minute
)

// exitInterrupted is `gaia run --wait`'s exit code after SIGINT or SIGTERM
// (launcher contract v1).
const exitInterrupted = 130

// syncWait bounds the wait for an ended session's push to be reported.
// The node bounds the push itself to 10 minutes.
const syncWait = 15 * time.Minute

// waitRetry is the pause between polls of a session that has not ended.
const waitRetry = 200 * time.Millisecond

func (e Env) lostGrace() time.Duration {
	if e.LostGrace > 0 {
		return e.LostGrace
	}
	return DefaultLostGrace
}

func (e Env) unreachable() time.Duration {
	if e.Unreachable > 0 {
		return e.Unreachable
	}
	return DefaultUnreachable
}

// callerCheckout returns the repo and branch of `gaia run --wait`: the
// flags, else the caller's checkout's origin URL and current branch. When
// the branch is the checkout's own and its origin is the repo, the branch
// must be pushed: no origin branch, or commits not on it, are
// sync_before_failed (ADR-0010 rule 1). Git is only read; the caller's
// checkout is never changed. ok is false with the exit code to return.
func callerCheckout(env Env, repoFlag, branchFlag string) (repoURL, branch string, code int, ok bool) {
	repoURL, branch = repoFlag, branchFlag
	if repoURL != "" && branch != "" {
		return repoURL, branch, 0, true
	}
	dir := env.WorkDir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			fmt.Fprintf(env.Stderr, "gaia: gaia run: no working directory to take --repo and --branch from: %v\n", err)
			return "", "", int(core.ExitUsage), false
		}
	}
	origin, oerr := callerGit(dir, "remote", "get-url", "origin")
	if repoURL == "" {
		if oerr != nil {
			fmt.Fprintf(env.Stderr, "gaia: gaia run: --repo is needed: %s is not a git checkout with an origin remote (%v)\n", dir, oerr)
			return "", "", int(core.ExitUsage), false
		}
		repoURL = origin
	}
	if branch != "" {
		return repoURL, branch, 0, true
	}
	b, err := callerGit(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || b == "" {
		fmt.Fprintf(env.Stderr, "gaia: gaia run: --branch is needed: %s has no current branch (detached HEAD?)\n", dir)
		return "", "", int(core.ExitUsage), false
	}
	branch = b
	if oerr != nil || origin != repoURL {
		// Another repository than this checkout's: nothing to compare.
		return repoURL, branch, 0, true
	}
	tracking := "refs/remotes/origin/" + branch
	if _, err := callerGit(dir, "rev-parse", "--verify", "--quiet", tracking+"^{commit}"); err != nil {
		fmt.Fprintf(env.Stderr, "gaia: branch %s is not on origin: push it first (git push -u origin %s)\n", branch, branch)
		return "", "", int(core.ExitSyncBeforeFailed), false
	}
	n, err := callerGit(dir, "rev-list", "--count", tracking+"..HEAD")
	if err != nil {
		fmt.Fprintf(env.Stderr, "gaia: cannot compare branch %s with origin: %v\n", branch, err)
		return "", "", int(core.ExitSyncBeforeFailed), false
	}
	if n != "0" {
		fmt.Fprintf(env.Stderr, "gaia: branch %s is %s commit(s) ahead of origin: push it first (git push origin %s)\n", branch, n, branch)
		return "", "", int(core.ExitSyncBeforeFailed), false
	}
	return repoURL, branch, 0, true
}

// callerGit runs a read-only git command in dir and returns its trimmed
// output.
func callerGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// runWait runs `gaia run --wait` for req (launcher contract v1) and returns
// the exit code.
func runWait(req core.StartSessionRequest, detachOnInterrupt bool, env Env) int {
	ints := env.Interrupts
	if ints == nil {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(ch)
		ints = ch
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The start is not cancelled by an interrupt: until it returns, the
	// session's id is unknown and it could not be stopped.
	type started struct {
		sess core.Session
		err  error
	}
	got := make(chan started, 1)
	go func() {
		s, err := env.Client.StartSession(ctx, req)
		got <- started{s, err}
	}()
	interrupted := false
	var st started
	for waiting := true; waiting; {
		select {
		case st = <-got:
			waiting = false
		case <-ints:
			if detachOnInterrupt {
				fmt.Fprintln(env.Stderr, "gaia: interrupted while starting; the session, if it starts, runs on (gaia ls lists it)")
				return exitInterrupted
			}
			if interrupted {
				fmt.Fprintln(env.Stderr, "gaia: interrupted again; leaving without waiting for the start (gaia ls shows the session if it started)")
				return exitInterrupted
			}
			interrupted = true
			fmt.Fprintln(env.Stderr, "gaia: interrupted; the session will be stopped once it has started (interrupt again to leave at once)")
		}
	}
	if st.err != nil {
		code := env.Client.Fail(env.Stderr, st.err)
		if interrupted {
			return exitInterrupted
		}
		return code
	}
	id := st.sess.ID
	if interrupted {
		return stopInterrupted(id, ints, env)
	}
	fmt.Fprintf(env.Stderr, "gaia: session %s running on %s\n", id, req.Node)

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	done := make(chan int, 1)
	go func() { done <- waitSession(wctx, id, env) }()
	select {
	case code := <-done:
		return code
	case <-ints:
	}
	wcancel()
	<-done // no output after this
	if detachOnInterrupt {
		fmt.Fprintf(env.Stderr, "gaia: interrupted; session %s runs on: gaia attach %s follows it, gaia stop %s ends it\n", id, id, id)
		return exitInterrupted
	}
	return stopInterrupted(id, ints, env)
}

// stopInterrupted stops the session with id after an interrupt and returns
// 130. A further interrupt leaves without waiting for the stop.
func stopInterrupted(id core.SessionID, ints <-chan os.Signal, env Env) int {
	fmt.Fprintf(env.Stderr, "gaia: interrupted; stopping session %s\n", id)
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), StopTimeout)
		defer cancel()
		_, err := env.Client.StopSession(ctx, id)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			_, msg := classify(err, env.Client.dir())
			fmt.Fprintf(env.Stderr, "gaia: stopping session %s failed: %s (gaia stop %s retries)\n", id, msg, id)
		}
	case <-ints:
		fmt.Fprintf(env.Stderr, "gaia: interrupted again; session %s may still be stopping\n", id)
	}
	return exitInterrupted
}

// waitSession writes the session's output to env.Stdout, each byte once,
// as it is produced, until the session has ended and its push has been
// reported, and returns the exit code: the agent's, or a reserved code. It
// returns -1 once ctx is done.
func waitSession(ctx context.Context, id core.SessionID, env Env) int {
	c := env.Client
	out := markWrites{env.Stdout}
	var off int64
	lastOK := time.Now()
	var lostSince, endedAt time.Time
	pause := func() bool {
		t := time.NewTimer(waitRetry)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	unreachable := func(err error) int {
		if time.Since(lastOK) < env.unreachable() {
			return 0
		}
		_, msg := classify(err, c.dir())
		fmt.Fprintf(env.Stderr, "gaia: lost contact with session %s for %s (%s); it may still be running: gaia ls shows it, gaia attach %s follows it\n",
			id, env.unreachable(), msg, id)
		return int(core.ExitSessionLost)
	}
	var sess core.Session
	for {
		// Follow while the session runs; once it has ended, read what the
		// follow did not carry.
		n, err := c.Logs(ctx, id, off, endedAt.IsZero(), out)
		off += n
		if ctx.Err() != nil {
			return -1
		}
		var se sinkError
		if errors.As(err, &se) {
			fmt.Fprintf(env.Stderr, "gaia: writing the session's output: %v\n", se.error)
			return int(core.ExitInternal)
		}
		// Progress is output read: a host that answers while the node
		// cannot be reached does not count.
		if n > 0 || err == nil {
			lastOK = time.Now()
		}
		logErr := err
		if logErr != nil {
			if code := unreachable(logErr); code != 0 {
				return code
			}
		} else if !endedAt.IsZero() && sess.Sync != core.SyncPending {
			return exitFor(sess, env)
		}
		// The state is read even when the output could not be: a lost
		// session's output cannot be.
		sess, err = c.GetSession(ctx, id)
		if ctx.Err() != nil {
			return -1
		}
		if err != nil {
			if code := unreachable(err); code != 0 {
				return code
			}
			if !pause() {
				return -1
			}
			continue
		}
		switch {
		case sess.State.Terminal():
			if endedAt.IsZero() {
				endedAt = time.Now()
				continue // read what the follow did not carry
			}
			if sess.Sync != core.SyncPending {
				continue // a last read, then the exit
			}
			if time.Since(endedAt) > syncWait {
				fmt.Fprintf(env.Stderr, "gaia: session %s ended but its push was not reported within %s; the outcome is unknown (gaia ls shows it)\n", id, syncWait)
				return int(core.ExitSessionLost)
			}
		case sess.State == core.SessionLost:
			if lostSince.IsZero() {
				lostSince = time.Now()
			}
			if time.Since(lostSince) > env.lostGrace() {
				fmt.Fprintf(env.Stderr, "gaia: session %s is lost: its supervisor on node %s is gone and the outcome is unknown (gaia ls shows it)\n", id, sess.NodeName)
				return int(core.ExitSessionLost)
			}
		default:
			lostSince = time.Time{}
		}
		if !pause() {
			return -1
		}
	}
}

// exitFor is the exit code of `gaia run --wait` for an ended session whose
// push has been reported.
func exitFor(s core.Session, env Env) int {
	pushFailed := func(what string) int {
		fmt.Fprintf(env.Stderr, "gaia: %s, but the node could not push branch %s; its commits are in the node's checkout and the next run there pushes them\n", what, s.Branch)
		return int(core.ExitSyncAfterFailed)
	}
	switch s.State {
	case core.SessionExited:
		if s.ExitCode == nil {
			break
		}
		if s.Sync == core.SyncFailed {
			return pushFailed("the agent exited " + strconv.Itoa(*s.ExitCode))
		}
		return *s.ExitCode
	case core.SessionStopped:
		if s.Sync == core.SyncFailed {
			return pushFailed("session " + s.ID.String() + " was stopped")
		}
		fmt.Fprintf(env.Stderr, "gaia: session %s was stopped\n", s.ID)
		if s.ExitCode != nil {
			return *s.ExitCode
		}
		return int(core.ExitInternal)
	case core.SessionFailed:
		fmt.Fprintf(env.Stderr, "gaia: session %s failed: Gaia could not keep it running (gaia logs %s shows its output)\n", s.ID, s.ID)
		return int(core.ExitInternal)
	}
	fmt.Fprintf(env.Stderr, "gaia: session %s ended as %s with no exit code\n", s.ID, s.State)
	return int(core.ExitInternal)
}
