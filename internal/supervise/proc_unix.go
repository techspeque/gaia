//go:build linux || darwin

package supervise

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"github.com/techspeque/gaia/internal/core"
	"golang.org/x/sys/unix"
)

const supported = true

// oNoFollow refuses to open a symbolic link.
const oNoFollow = syscall.O_NOFOLLOW

// fileUID returns the uid that owns fi.
func fileUID(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// maxSocketPath is the longest socket path the platform's sun_path holds,
// without its terminating NUL.
func maxSocketPath() int {
	if runtime.GOOS == "darwin" {
		return 103
	}
	return 107
}

// child is the running agent.
type child struct {
	cmd *exec.Cmd
	out *os.File // the agent's output: the PTY master or the pipe's read end
	tty *os.File // the PTY master, nil in headless mode
	pid int      // also its process group id
}

// startChild starts spec's argv with no shell, in its own process group.
func startChild(spec Spec) (*child, error) {
	path, err := lookPath(spec.Argv[0], pathEnv(spec.Env), spec.WorkDir)
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{
		Path: path,
		Args: spec.Argv,
		Dir:  spec.WorkDir,
		Env:  append([]string{}, spec.Env...), // non-nil: never the supervisor's own
	}
	if spec.Mode == core.SessionInteractive {
		rows, cols := spec.size()
		// Setsid makes the agent the leader of a new session and process
		// group; Setctty gives it the PTY as controlling terminal.
		tty, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: rows, Cols: cols},
			&syscall.SysProcAttr{Setsid: true, Setctty: true})
		if err != nil {
			return nil, startErr(spec, err)
		}
		return &child{cmd: cmd, out: tty, tty: tty, pid: cmd.Process.Pid}, nil
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdin = devnull
	cmd.Stdout = w
	cmd.Stderr = w // one pipe keeps the order of stdout and stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, startErr(spec, err)
	}
	_ = w.Close()
	return &child{cmd: cmd, out: r, pid: cmd.Process.Pid}, nil
}

// wait waits for the agent and returns its exit code, or the name of the
// signal that killed it.
func (c *child) wait() (exitCode *int, signal string) {
	_ = c.cmd.Wait()
	ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		return nil, unix.SignalName(ws.Signal())
	}
	code := c.cmd.ProcessState.ExitCode()
	return &code, ""
}

// terminate sends SIGTERM to the agent's process group.
func (c *child) terminate() { _ = syscall.Kill(-c.pid, syscall.SIGTERM) }

// kill sends SIGKILL to the agent's process group.
func (c *child) kill() { _ = syscall.Kill(-c.pid, syscall.SIGKILL) }

// resize sets the PTY's window size; the agent gets SIGWINCH.
func (c *child) resize(rows, cols uint16) error {
	return pty.Setsize(c.tty, &pty.Winsize{Rows: rows, Cols: cols})
}

// isEndOfOutput reports whether a read error from the agent's output means
// there is no more: EOF, or EIO from a PTY master whose slave is closed.
func isEndOfOutput(err error) bool { return errors.Is(err, syscall.EIO) }

// lookPath resolves name as exec would, against pathList instead of the
// supervisor's own PATH. A name with a slash is taken as is, relative to
// workDir. An empty PATH entry means the working directory.
func lookPath(name, pathList, workDir string) (string, error) {
	notFound := func(why string) error {
		return &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: fmt.Sprintf("cannot run %q: %s", name, why),
			Details: map[string]string{"argv0": name},
		}
	}
	if strings.Contains(name, "/") {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		if err := executable(p); err != nil {
			return "", notFound(err.Error())
		}
		return p, nil
	}
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			dir = "."
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workDir, dir)
		}
		p := filepath.Join(dir, name)
		if executable(p) == nil {
			return p, nil
		}
	}
	return "", notFound("not found in the session's PATH")
}

func executable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory", p)
	}
	if fi.Mode()&0o111 == 0 {
		return &fs.PathError{Op: "exec", Path: p, Err: fs.ErrPermission}
	}
	return nil
}

func startErr(spec Spec, err error) error {
	return &core.Error{
		Code:    core.CodeInvalidArgument,
		Message: fmt.Sprintf("starting %q: %v", spec.Argv[0], err),
		Details: map[string]string{"argv0": spec.Argv[0]},
	}
}

// ignoreHangup makes the supervisor immune to its starter's terminal going
// away and to writes on a closed stdout.
func ignoreHangup() { signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE) }

// stopSignals are the signals to the supervisor itself that mean stop.
var stopSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}
