package main

import (
	"io"
	"os"

	"github.com/techspeque/gaia/internal/supervise"
)

// runSupervise is the internal `gaia supervise` command: one session's
// supervisor (ADR-0008), started by the node daemon with the session's
// spec on stdin. It is not listed in the usage text.
func runSupervise(args []string, stdout, stderr io.Writer) int {
	return supervise.Main(args, os.Stdin, stdout, stderr)
}
