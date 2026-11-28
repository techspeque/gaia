package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// forbiddenSubcommands are git subcommands Gaia never runs (ADR-0010,
// OVERVIEW §3.3).
var forbiddenSubcommands = map[string]bool{
	"reset": true, "rebase": true, "stash": true, "clean": true,
}

// forbiddenArgs are arguments Gaia never passes: forced updates, ref
// deletion and hard resets.
var forbiddenArgs = map[string]bool{
	"-f": true, "-d": true, "-D": true, "--hard": true, "--mirror": true,
	"--delete": true, "--prune": true,
}

// strippedEnv are variables that would point git at another repository than
// the checkout it is run on.
var strippedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
}

// checkArgv refuses a forbidden subcommand or argument before git runs.
func checkArgv(sub string, args []string) error {
	if forbiddenSubcommands[sub] {
		return fmt.Errorf("repo: refusing forbidden git subcommand %q", sub)
	}
	for _, a := range args {
		switch {
		case forbiddenArgs[a], strings.HasPrefix(a, "--force"):
			return fmt.Errorf("repo: refusing forbidden git argument %q", a)
		case strings.HasPrefix(a, "+"):
			return fmt.Errorf("repo: refusing forced refspec %q", a)
		case len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], "fdD"):
			return fmt.Errorf("repo: refusing forbidden git argument %q", a)
		}
	}
	return nil
}

// observeArgv, when set (by tests), sees every argv that reaches git.
var observeArgv func(argv []string)

// gitResult is one git run's output and exit status.
type gitResult struct {
	stdout, stderr string
	exit           int // -1 when git did not run or was killed
}

func (r gitResult) detail() string {
	s := strings.TrimSpace(r.stderr)
	if s == "" {
		s = strings.TrimSpace(r.stdout)
	}
	return s
}

// gitEnv is the node user's environment with repository-locating variables
// removed, no terminal prompt and a fixed locale for parseable output.
func gitEnv() []string {
	var env []string
outer:
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		for _, s := range strippedEnv {
			if k == s {
				continue outer
			}
		}
		if k == "GIT_TERMINAL_PROMPT" || k == "LC_ALL" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

// git runs one git subcommand with structured argv and no shell. With dir
// set, git is pinned to that checkout with --git-dir and --work-tree so it
// never walks up into a parent repository. A non-zero exit is not an error;
// err is set only when git could not run or the argv is forbidden.
func git(ctx context.Context, dir, sub string, args ...string) (gitResult, error) {
	if err := checkArgv(sub, args); err != nil {
		return gitResult{exit: -1}, err
	}
	var argv []string
	if dir != "" {
		argv = append(argv, "--git-dir="+filepath.Join(dir, ".git"), "--work-tree="+dir)
	}
	argv = append(argv, sub)
	argv = append(argv, args...)
	if observeArgv != nil {
		observeArgv(append([]string(nil), argv...))
	}
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = gitEnv()
	cmd.Stdin = nil
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if dir != "" {
		cmd.Dir = dir
	}
	err := cmd.Run()
	res := gitResult{stdout: out.String(), stderr: errb.String()}
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 && ctx.Err() == nil {
		res.exit = ee.ExitCode()
		return res, nil
	}
	res.exit = -1
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, fmt.Errorf("repo: running git %s: %w", sub, err)
}
