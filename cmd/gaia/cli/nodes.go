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

// Nodes runs `gaia nodes [-o text|json]` or `gaia nodes remove NAME` and
// returns the exit code.
func Nodes(args []string, env Env) int {
	if len(args) > 0 && args[0] == "remove" {
		return nodesRemove(args[1:], env)
	}
	fs := flag.NewFlagSet("gaia nodes", flag.ContinueOnError)
	// The parser's own diagnostic lacks the "gaia: " prefix; discard it and
	// report the returned error instead.
	fs.SetOutput(io.Discard)
	var output string
	fs.StringVar(&output, "o", "text", "output format: text or json")
	fs.StringVar(&output, "output", "text", "output format: text or json")
	usage := func() {
		fmt.Fprintln(env.Stderr, "usage: gaia nodes [-o text|json]\n       gaia nodes remove NAME")
	}
	fs.Usage = func() {}
	err := fs.Parse(args)
	fs.Usage = usage
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return 0
		}
		return usageError(env.Stderr, fs, "gaia nodes: "+err.Error())
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

// removeUsage is the usage line of `gaia nodes remove`.
const removeUsage = "usage: gaia nodes remove NAME"

// nodesRemove runs `gaia nodes remove NAME`. An unknown name exits 1.
func nodesRemove(args []string, env Env) int {
	usage := func(msg string) int {
		if msg != "" {
			fmt.Fprintln(env.Stderr, "gaia: gaia nodes remove: "+msg)
		}
		fmt.Fprintln(env.Stderr, removeUsage)
		return int(core.ExitUsage)
	}
	switch {
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help"):
		fmt.Fprintln(env.Stderr, removeUsage)
		fmt.Fprintln(env.Stderr, "Removes a retired machine from the registry. A node still running registers again at its next heartbeat: stop `gaia node` there first.")
		return 0
	case len(args) == 0:
		return usage("missing NAME")
	case len(args) > 1:
		return usage(fmt.Sprintf("unexpected argument %q", args[1]))
	}
	name := args[0]
	if err := core.ValidNodeName(name); err != nil {
		var ce *core.Error
		errors.As(err, &ce)
		return usage(ce.Message)
	}
	n, err := env.Client.RemoveNode(context.Background(), name)
	if errors.Is(err, core.NewError(core.CodeNotFound, "")) {
		fmt.Fprintf(env.Stderr, "gaia: no node named %q is registered; `gaia nodes` lists them\n", name)
		return 1
	}
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	fmt.Fprintf(env.Stdout, "removed node %s (%s)\n", n.Name, n.ID)
	if n.State == core.NodeOnline {
		fmt.Fprintf(env.Stdout, "%s was online: it registers again at its next heartbeat unless `gaia node` is stopped on it\n", n.Name)
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

// writeNodesText writes a table of nodes, one per line. TAILNET NAME is the
// name the control server assigned, "-" when unknown (local-only).
func writeNodesText(w io.Writer, l core.ListNodesResponse, now time.Time) error {
	if len(l.Nodes) == 0 {
		_, err := fmt.Fprintln(w, "no nodes registered")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTAILNET NAME\tSTATE\tOS/ARCH\tVERSION\tLAST SEEN")
	for _, n := range l.Nodes {
		tn := n.TailnetName
		if tn == "" {
			tn = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s/%s\t%s\t%s\n",
			n.Name, tn, n.State, n.OS, n.Arch, n.GaiaVersion, ago(now, n.LastSeen))
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
