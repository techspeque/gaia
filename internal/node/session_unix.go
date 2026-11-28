//go:build linux || darwin

package node

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// detach makes cmd lead a new session and process group, so neither the
// daemon's exit nor a signal to the daemon's group reaches it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// signalNumber returns the number of the signal named name ("SIGTERM").
func signalNumber(name string) (int, bool) {
	if name == "" {
		return 0, false
	}
	n := unix.SignalNum(name)
	return int(n), n != 0
}
