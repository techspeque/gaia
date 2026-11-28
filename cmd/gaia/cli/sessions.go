package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// StopTimeout bounds `gaia stop`: the node's SIGTERM, grace period and
// SIGKILL, and the round trips.
const StopTimeout = 60 * time.Second

// followRetryWait is the pause before `gaia logs --follow` asks again
// after a stream ended while the session had not.
const followRetryWait = 200 * time.Millisecond

// ListSessions connects, then returns every session the host knows.
func (c *Client) ListSessions(ctx context.Context) (core.ListSessionsResponse, error) {
	var l core.ListSessionsResponse
	if _, err := c.Connect(ctx); err != nil {
		return l, err
	}
	if _, err := c.do(ctx, http.MethodGet, core.SessionsPath, &l); err != nil {
		return l, err
	}
	if l.Sessions == nil {
		l.Sessions = []core.Session{}
	}
	return l, nil
}

// GetSession returns the session with id.
func (c *Client) GetSession(ctx context.Context, id core.SessionID) (core.Session, error) {
	var r core.GetSessionResponse
	_, err := c.do(ctx, http.MethodGet, core.SessionPath(id), &r)
	return r.Session, err
}

// StartSession connects, then asks the host to start req.
func (c *Client) StartSession(ctx context.Context, req core.StartSessionRequest) (core.Session, error) {
	var r core.StartSessionResponse
	if err := req.Validate(); err != nil {
		return r.Session, err
	}
	if _, err := c.Connect(ctx); err != nil {
		return r.Session, err
	}
	_, err := c.send(ctx, c.httpClientTimeout(StopTimeout), http.MethodPost, core.SessionsPath, req, &r)
	return r.Session, err
}

// StopSession connects, then stops the session with id and returns it as
// it ended.
func (c *Client) StopSession(ctx context.Context, id core.SessionID) (core.Session, error) {
	var r core.StopSessionResponse
	if _, err := c.Connect(ctx); err != nil {
		return r.Session, err
	}
	_, err := c.send(ctx, c.httpClientTimeout(StopTimeout), http.MethodPost, core.SessionStopPath(id), struct{}{}, &r)
	return r.Session, err
}

// Logs copies the session's scrollback from byte offset off to w, with
// follow until the stream ends, and returns how many bytes it wrote. An
// error the daemon answers with before the stream starts is its
// *core.Error.
func (c *Client) Logs(ctx context.Context, id core.SessionID, off int64, follow bool, w io.Writer) (int64, error) {
	path := core.SessionLogsPath(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://gaia"+path+"?"+core.LogsQuery(off, follow), nil)
	if err != nil {
		return 0, err
	}
	res, err := c.httpClientTimeout(0).Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, daemonError(res, path)
	}
	return io.Copy(w, res.Body)
}

// Follow writes the session's whole output from byte offset off to w, as
// it is produced, and returns once the session has ended and every byte is
// written. A stream that ends while the session runs is resumed from the
// bytes received, so each byte is written once. A failure of w ends it at
// once with w's error.
func (c *Client) Follow(ctx context.Context, id core.SessionID, off int64, w io.Writer) error {
	w = markWrites{w}
	for {
		n, err := c.Logs(ctx, id, off, true, w)
		off += n
		var se sinkError
		if errors.As(err, &se) {
			return se.error
		}
		var ce *core.Error
		if errors.As(err, &ce) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sess, gerr := c.GetSession(ctx, id)
		if gerr != nil {
			return gerr
		}
		if sess.State.Terminal() {
			// Whatever the last stream did not carry.
			_, err := c.Logs(ctx, id, off, false, w)
			if errors.As(err, &se) {
				return se.error
			}
			return err
		}
		if n == 0 {
			t := time.NewTimer(followRetryWait)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
	}
}

// sinkError marks a failure of Follow's writer, as opposed to the stream's.
type sinkError struct{ error }

func (e sinkError) Unwrap() error { return e.error }

// markWrites wraps w so that its failures are sinkErrors.
type markWrites struct{ w io.Writer }

func (m markWrites) Write(p []byte) (int, error) {
	n, err := m.w.Write(p)
	if err != nil {
		return n, sinkError{err}
	}
	return n, nil
}

// runUsage is the usage text of `gaia run`.
const runUsage = `usage: gaia run --node NODE (--detach | --interactive) [--profile PROFILE] -- ARGV...
  Starts ARGV on NODE, under the node user's environment. --detach prints
  the session id and returns at once; the session runs on. --interactive
  gives ARGV a terminal and attaches to it, as gaia attach does: Ctrl-]
  detaches, and the session runs on.`

// Run runs `gaia run` and returns the exit code.
func Run(args []string, env Env) int {
	usage := func(msg string) int {
		if msg != "" {
			fmt.Fprintln(env.Stderr, "gaia: gaia run: "+msg)
		}
		fmt.Fprintln(env.Stderr, runUsage)
		return int(core.ExitUsage)
	}
	i := slices.Index(args, "--")
	flags, argv := args, []string(nil)
	if i >= 0 {
		flags, argv = args[:i], args[i+1:]
	}
	fs := flag.NewFlagSet("gaia run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	nodeName := fs.String("node", "", "")
	detach := fs.Bool("detach", false, "")
	interactive := fs.Bool("interactive", false, "")
	profile := fs.String("profile", "", "")
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(env.Stderr, runUsage)
			return 0
		}
		return usage(err.Error())
	}
	switch {
	case fs.NArg() > 0:
		return usage(fmt.Sprintf("unexpected argument %q: put the agent's argv after --", fs.Arg(0)))
	case *nodeName == "":
		return usage("--node NODE is required")
	case *detach && *interactive:
		return usage("--detach and --interactive cannot be combined")
	case !*detach && !*interactive:
		return usage("--detach or --interactive is required (--wait is not available yet)")
	case i < 0:
		return usage("put the agent's argv after --")
	case len(argv) == 0:
		return usage("nothing to run: argv after -- is empty")
	}
	mode := core.SessionHeadless
	if *interactive {
		mode = core.SessionInteractive
	}
	req := core.StartSessionRequest{Node: *nodeName, Mode: mode, Argv: argv, Profile: *profile}
	if err := req.Validate(); err != nil {
		var ce *core.Error
		errors.As(err, &ce)
		return usage(ce.Message)
	}
	sess, err := env.Client.StartSession(context.Background(), req)
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	if *interactive {
		return attach(context.Background(), sess.ID, env)
	}
	fmt.Fprintln(env.Stdout, sess.ID)
	return 0
}

// Ls runs `gaia ls [-o text|json]` and returns the exit code.
func Ls(args []string, env Env) int {
	fs := flag.NewFlagSet("gaia ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var output string
	fs.StringVar(&output, "o", "text", "")
	fs.StringVar(&output, "output", "text", "")
	usage := func() { fmt.Fprintln(env.Stderr, "usage: gaia ls [-o text|json]") }
	fs.Usage = func() {}
	err := fs.Parse(args)
	fs.Usage = usage
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return 0
		}
		return usageError(env.Stderr, fs, "gaia ls: "+err.Error())
	}
	if fs.NArg() > 0 {
		return usageError(env.Stderr, fs, fmt.Sprintf("gaia ls: unexpected argument %q", fs.Arg(0)))
	}
	if output != "text" && output != "json" {
		return usageError(env.Stderr, fs, fmt.Sprintf("gaia ls: unknown output format %q, want text or json", output))
	}
	l, err := env.Client.ListSessions(context.Background())
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	if output == "json" {
		err = writeSessionsJSON(env.Stdout, l)
	} else {
		err = writeSessionsText(env.Stdout, l, env.now())
	}
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	return 0
}

// writeSessionsJSON writes the `gaia ls -o json` shape: the API's
// ListSessionsResponse, two-space indented, with a trailing newline. The
// shape is a contract (testdata/contract/gaia/api-v1/sessions*.json).
func writeSessionsJSON(w io.Writer, l core.ListSessionsResponse) error {
	if l.Sessions == nil {
		l.Sessions = []core.Session{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(l)
}

// writeSessionsText writes a table of sessions, oldest first. EXIT is "-"
// while no exit code is known.
func writeSessionsText(w io.Writer, l core.ListSessionsResponse, now time.Time) error {
	if len(l.Sessions) == 0 {
		_, err := fmt.Fprintln(w, "no sessions")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNODE\tSTATE\tEXIT\tSTARTED\tCOMMAND")
	for _, s := range l.Sessions {
		node, exit := s.NodeName, "-"
		if node == "" {
			node = s.NodeID.String()
		}
		if s.ExitCode != nil {
			exit = strconv.Itoa(*s.ExitCode)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, node, s.State, exit, ago(now, s.StartedAt), command(s.Argv))
	}
	return tw.Flush()
}

// command is argv for a table cell: quoted where needed, cut to 60
// characters.
func command(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\") {
			a = strconv.Quote(a)
		}
		parts[i] = a
	}
	s := strings.Join(parts, " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}

// sessionArg parses the SESSION argument of name, printing the usage on
// failure.
func sessionArg(args []string, name, usageLine string, env Env) (core.SessionID, int, bool) {
	switch {
	case len(args) == 0:
		fmt.Fprintf(env.Stderr, "gaia: gaia %s: missing SESSION\n%s\n", name, usageLine)
		return core.SessionID{}, int(core.ExitUsage), false
	case len(args) > 1:
		fmt.Fprintf(env.Stderr, "gaia: gaia %s: unexpected argument %q\n%s\n", name, args[1], usageLine)
		return core.SessionID{}, int(core.ExitUsage), false
	}
	id, err := core.ParseSessionID(args[0])
	if err != nil {
		fmt.Fprintf(env.Stderr, "gaia: gaia %s: %q is not a session id (`gaia ls` lists them)\n%s\n", name, args[0], usageLine)
		return core.SessionID{}, int(core.ExitUsage), false
	}
	return id, 0, true
}

// notFound reports an unknown session and returns 1, or returns false.
func notFound(err error, id core.SessionID, env Env) (int, bool) {
	if errors.Is(err, core.NewError(core.CodeNotFound, "")) {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Details[core.DetailReason] == "" {
			fmt.Fprintf(env.Stderr, "gaia: no session %s; `gaia ls` lists them\n", id)
			return 1, true
		}
	}
	return 0, false
}

const stopUsage = "usage: gaia stop SESSION"

// Stop runs `gaia stop SESSION` and returns the exit code: 0 once the
// session has ended, also when it had already; 1 for an unknown session.
func Stop(args []string, env Env) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintln(env.Stderr, stopUsage)
		fmt.Fprintln(env.Stderr, "Stops the session: SIGTERM to the agent, SIGKILL after 10 s.")
		return 0
	}
	id, code, ok := sessionArg(args, "stop", stopUsage, env)
	if !ok {
		return code
	}
	sess, err := env.Client.StopSession(context.Background(), id)
	if code, ok := notFound(err, id, env); ok {
		return code
	}
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	msg := fmt.Sprintf("%s %s", sess.ID, sess.State)
	if sess.ExitCode != nil {
		msg += fmt.Sprintf(" (exit code %d)", *sess.ExitCode)
	}
	fmt.Fprintln(env.Stdout, msg)
	return 0
}

const logsUsage = "usage: gaia logs SESSION [-f|--follow]"

// Logs runs `gaia logs SESSION [--follow]` and returns the exit code. The
// session's output goes to stdout unchanged.
func Logs(args []string, env Env) int {
	follow := false
	var pos []string
	for _, a := range args {
		switch a {
		case "-f", "-follow", "--follow":
			follow = true
		case "-h", "-help", "--help":
			fmt.Fprintln(env.Stderr, logsUsage)
			fmt.Fprintln(env.Stderr, "Prints the session's output so far; --follow keeps printing it as it is produced, until the session ends.")
			return 0
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(env.Stderr, "gaia: gaia logs: unknown flag %s\n%s\n", a, logsUsage)
				return int(core.ExitUsage)
			}
			pos = append(pos, a)
		}
	}
	id, code, ok := sessionArg(pos, "logs", logsUsage, env)
	if !ok {
		return code
	}
	ctx := context.Background()
	if _, err := env.Client.Connect(ctx); err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	var err error
	if follow {
		err = env.Client.Follow(ctx, id, 0, env.Stdout)
	} else {
		_, err = env.Client.Logs(ctx, id, 0, false, env.Stdout)
	}
	if code, ok := notFound(err, id, env); ok {
		return code
	}
	if err != nil {
		return env.Client.Fail(env.Stderr, err)
	}
	return 0
}
