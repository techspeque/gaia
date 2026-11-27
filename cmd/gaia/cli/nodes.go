package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// Env is what a client command needs from its process.
type Env struct {
	Stdout, Stderr io.Writer
	// Client reaches the daemon.
	Client *Client
	// Now is the clock for relative times. Default: time.Now.
	Now func() time.Time
}

func (e Env) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// usageError prints msg and the command's usage, and returns ExitUsage.
func usageError(stderr io.Writer, fs *flag.FlagSet, msg string) int {
	if msg != "" {
		fmt.Fprintln(stderr, "gaia: "+msg)
	}
	fs.Usage()
	return int(core.ExitUsage)
}

// Nodes runs `gaia nodes [-o text|json]` and returns the exit code.
func Nodes(args []string, env Env) int {
	fs := flag.NewFlagSet("gaia nodes", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var output string
	fs.StringVar(&output, "o", "text", "output format: text or json")
	fs.StringVar(&output, "output", "text", "output format: text or json")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "usage: gaia nodes [-o text|json]")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return int(core.ExitUsage)
	}
	if fs.NArg() > 0 {
		return usageError(env.Stderr, fs, fmt.Sprintf("gaia nodes: unexpected argument %q", fs.Arg(0)))
	}
	if output != "text" && output != "json" {
		return usageError(env.Stderr, fs, fmt.Sprintf("gaia nodes: unknown output format %q, want text or json", output))
	}

	l, err := env.Client.ListNodes(context.Background())
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	if output == "json" {
		err = writeNodesJSON(env.Stdout, l)
	} else {
		err = writeNodesText(env.Stdout, l, env.now())
	}
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	return 0
}

// writeNodesJSON writes the `gaia nodes -o json` shape: the API's
// ListNodesResponse, two-space indented, with a trailing newline. The shape
// is a contract (testdata/contract/gaia/api-v1/nodes.json).
func writeNodesJSON(w io.Writer, l core.ListNodesResponse) error {
	if l.Nodes == nil {
		l.Nodes = []core.Node{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(l)
}

// writeNodesText writes a table of nodes, one per line.
func writeNodesText(w io.Writer, l core.ListNodesResponse, now time.Time) error {
	if len(l.Nodes) == 0 {
		_, err := fmt.Fprintln(w, "no nodes registered")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tOS/ARCH\tVERSION\tLAST SEEN")
	for _, n := range l.Nodes {
		fmt.Fprintf(tw, "%s\t%s\t%s/%s\t%s\t%s\n",
			n.Name, n.State, n.OS, n.Arch, n.GaiaVersion, ago(now, n.LastSeen))
	}
	return tw.Flush()
}

// ago formats t relative to now: "just now", "12s ago", "5m ago", "3h ago",
// and the RFC 3339 time from a day on, or for a time in the future.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0 || d >= 24*time.Hour:
		return t.UTC().Format(time.RFC3339)
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	default:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
}
