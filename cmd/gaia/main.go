// Command gaia is the single Gaia binary. The role (host, node or CLI
// client) is chosen by the first argument (ADR-0002).
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
)

// version and commit are set at build time with
// -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = ""
)

const usage = `usage: gaia <command> [flags]

  host [--state DIR] [--node]   run the host daemon; --node also runs this
                                machine as a node (local-only mode)
  --version                     print version, commit and platform
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches on args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "--version", "-version":
		fmt.Fprintln(stdout, versionString(version, resolveCommit(commit, readBuildInfo)))
		return 0
	case "host":
		return runHost(args[1:], stderr, runtime.GOOS)
	case "node":
		return runNode(args[1:], stderr, runtime.GOOS)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "gaia: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// versionString formats the one-line --version output.
func versionString(version, commit string) string {
	return fmt.Sprintf("gaia %s (commit %s) %s/%s", version, commit, runtime.GOOS, runtime.GOARCH)
}

// resolveCommit returns the ldflags commit when set, else the VCS revision
// the Go toolchain embedded, else "unknown".
func resolveCommit(ldflagsCommit string, info func() (*debug.BuildInfo, bool)) string {
	if ldflagsCommit != "" {
		return ldflagsCommit
	}
	if bi, ok := info(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				rev += "-dirty"
			}
			return rev
		}
	}
	return "unknown"
}

func readBuildInfo() (*debug.BuildInfo, bool) { return debug.ReadBuildInfo() }
