// Command gaia is the single Gaia binary. The role (host, node or CLI
// client) is chosen by the first argument (ADR-0002).
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/techspeque/gaia/cmd/gaia/cli"
)

// version and commit are set at build time with
// -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = ""
)

const usage = `usage: gaia <command> [flags]

  host [--state DIR] [--node] [--name NAME]
                                run the host daemon; --node also runs this
                                machine as a node (local-only mode); --name
                                is its tailnet and node name (default
                                gaia-<machine name>, then kept in the state)
       [--tailnet [--control-url URL] --allow ENTRY... | --allow-file PATH
        [--auth-key-file PATH]]
                                also join a tailnet and serve the API on it;
                                the auth key comes from $GAIA_AUTHKEY,
                                $TS_AUTHKEY or --auth-key-file, never a flag
       [--heartbeat-interval D] [--missed-heartbeats N]
                                how often nodes heartbeat, and how many they
                                may miss before they are offline
  node --host NAME [--join [--auth-key-file PATH]] [--state DIR]
       [--name NAME] [--control-url URL]
                                run this machine as a node of the host NAME
                                (the host's tailnet name, which it logs as
                                tailnet_name) over the tailnet; --join (once)
                                joins the tailnet with the auth key, as for
                                the host; --name as for the host
  nodes [-o text|json]          list the machines registered with the host
  nodes remove NAME             remove a retired machine from the registry
  service install (host [--node] [--tailnet ...] | node --host NAME)
                                run the daemon as a user service (systemd
                                user unit or launchd agent) and start it
  service uninstall             stop and remove that service; the state
                                directory stays
  --version                    print version, commit and platform
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
	case "nodes":
		return cli.Nodes(args[1:], cliEnv(stdout, stderr))
	case "service":
		return runService(args[1:], stdout, stderr, runtime.GOOS)
	case "supervise":
		return runSupervise(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "gaia: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// cliEnv is the environment of the client commands: the default runtime
// directory and this binary's API and Gaia versions.
func cliEnv(stdout, stderr io.Writer) cli.Env {
	return cli.Env{Stdout: stdout, Stderr: stderr, Client: &cli.Client{Version: version}}
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
