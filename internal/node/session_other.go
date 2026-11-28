//go:build !(linux || darwin)

package node

import "os/exec"

// detach does nothing here: the supervisor does not run on this platform.
func detach(*exec.Cmd) {}

func signalNumber(string) (int, bool) { return 0, false }
