package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/host"
	"github.com/techspeque/gaia/internal/logging"
	gnet "github.com/techspeque/gaia/internal/net"
	"github.com/techspeque/gaia/internal/node"
	"github.com/techspeque/gaia/internal/sock"
	"github.com/techspeque/gaia/internal/store"
)

// refuseDaemon prints the refusal and reports true when the daemon roles
// cannot run on goos. Windows waits for the named-pipe and ConPTY work
// (phase 0 plan).
func refuseDaemon(role, goos string, stderr io.Writer) bool {
	if goos != "windows" {
		return false
	}
	fmt.Fprintf(stderr, "gaia %s: the daemon roles do not run on Windows yet\n", role)
	return true
}

// tailnetJoin is internal/net.Join. The start path calls it only with
// --tailnet; tests replace it to prove that.
var tailnetJoin = gnet.Join

// tailnetPort is the tailnet HTTPS port of the host API.
const tailnetPort = 443

// Auth key environment variables, read with --tailnet, in this order.
var authKeyEnv = []string{"GAIA_AUTHKEY", "TS_AUTHKEY"}

// tailnetOnlyFlags are the host flags that mean nothing without --tailnet.
var tailnetOnlyFlags = []string{"control-url", "allow", "allow-file", "auth-key-file"}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// tailnetConfig is the resolved tailnet configuration of `gaia host`.
type tailnetConfig struct {
	controlURL string
	allow      host.Allowlist
	authKey    core.Secret
}

// runHost runs `gaia host` until SIGTERM or SIGINT.
func runHost(args []string, stderr io.Writer, goos string) int {
	if refuseDaemon("host", goos, stderr) {
		return 1
	}
	fs := flag.NewFlagSet("gaia host", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateFlag := fs.String("state", "", "state directory (default: "+describeDefaultState(goos)+")")
	withNode := fs.Bool("node", false, "also run this machine as a node, in-process (local-only mode)")
	nameFlag := fs.String("name", "", "the host's name on the tailnet, and of its own node with --node (default: the name recorded in the state directory, else gaia-<machine name>)")
	withTailnet := fs.Bool("tailnet", false, "join a tailnet and serve the API on it over HTTPS")
	controlURL := fs.String("control-url", "", "tailnet control server URL, Tailscale or Headscale (default: Tailscale's); needs --tailnet")
	var allow stringList
	fs.Var(&allow, "allow", "tailnet peer allowed to call the API: user@example.com, tag:NAME or node:NAME; repeatable; needs --tailnet")
	allowFile := fs.String("allow-file", "", "file of allowed tailnet peers, one per line, # comments; needs --tailnet")
	authKeyFile := fs.String("auth-key-file", "", "file holding the tailnet auth key, not group or other readable (else $GAIA_AUTHKEY or $TS_AUTHKEY); needs --tailnet")
	beat := fs.Duration("heartbeat-interval", host.DefaultHeartbeatInterval, "how often nodes heartbeat")
	missed := fs.Int("missed-heartbeats", host.DefaultMissedHeartbeats, "heartbeats a node may miss before it is offline")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gaia host: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *beat < 10*time.Millisecond || *missed < 1 {
		fmt.Fprintln(stderr, "gaia host: --heartbeat-interval must be at least 10ms and --missed-heartbeats at least 1")
		return 2
	}
	if *nameFlag != "" {
		if err := core.ValidNodeName(*nameFlag); err != nil {
			fmt.Fprintf(stderr, "gaia host: --name: %s\n", err.(*core.Error).Message)
			return 2
		}
	}
	if !*withTailnet {
		var set []string
		fs.Visit(func(f *flag.Flag) {
			if slices.Contains(tailnetOnlyFlags, f.Name) {
				set = append(set, "--"+f.Name)
			}
		})
		if len(set) > 0 {
			fmt.Fprintf(stderr, "gaia host: %s needs --tailnet\n", strings.Join(set, ", "))
			return 2
		}
	}
	state := *stateFlag
	if state == "" {
		var err error
		if state, err = defaultStateDir(goos, os.Getenv, os.UserHomeDir); err != nil {
			fmt.Fprintf(stderr, "gaia host: %v; pass --state DIR\n", err)
			return 1
		}
	}
	// Resolved before store.Open creates the state directory, which would
	// make a new one look used before.
	name, err := resolveName(*nameFlag, state, machineName(os.Hostname), "host")
	if err != nil {
		fmt.Fprintf(stderr, "gaia host: %v\n", err)
		return 1
	}

	redactor := &logging.Redactor{}
	var tc tailnetConfig
	if *withTailnet {
		var code int
		if tc, code = resolveTailnet(*controlURL, allow, *allowFile, *authKeyFile, stderr); code != 0 {
			return code
		}
		redactor.Register(tc.authKey) // before any log line
	}
	log := logging.New(stderr, redactor)
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopSignals()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	st, err := store.Open(ctx, state)
	if err != nil {
		log.Error("cannot open the state directory", "path", state, "err", err)
		return 1
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("closing the store", "err", err)
		}
	}()
	if err := recordName(state, name); err != nil {
		log.Error("cannot record the host's name", "path", state, "err", err)
		return 1
	}

	l, err := sock.Listen(sock.Dir(), sock.Options{
		OnDeny: func(err error) { log.Warn("local socket peer refused", "err", err) },
	})
	if err != nil {
		log.Error("cannot bind the local socket", "err", err)
		return 1
	}
	defer func() { _ = l.Close() }()

	router := &sessionRouter{}
	srv := host.New(st, host.Options{Version: version, Logger: log, Allowlist: tc.allow,
		HeartbeatInterval: *beat, MissedHeartbeats: *missed, Nodes: router})
	if *withNode {
		mgr, local, err := openSessions(ctx, state, srv, log)
		if err != nil {
			log.Error("cannot start the local node's sessions", "err", err)
			return 1
		}
		defer mgr.Close()
		router.local = local
		go mgr.Run(ctx)
	}
	served := make(chan error, 1)
	tailnetAddr, tailnetName := "", ""
	if *withTailnet {
		tn, tl, code := startTailnet(ctx, state, name, tc, log)
		if tn == nil {
			if sigCtx.Err() != nil {
				log.Info("host stopped", "exit_code", 0)
				return 0
			}
			return code
		}
		defer func() {
			if err := tn.Close(); err != nil {
				log.Error("leaving the tailnet", "err", err)
			}
		}()
		tailnetAddr = tn.Addr()
		tailnetName = reportTailnetName(ctx, tn, name, "host", log)
		router.tailnet = &tailnetNodes{tn: tn}
		go func() { served <- serveBoth(ctx, cancel, srv, l, tl, tn) }()
		// A node that restarts must not wait 15 s for its WireGuard
		// session to come back (fix-0004).
		go func() {
			if err := tn.ResumeRestartedPeers(ctx); err != nil && ctx.Err() == nil {
				log.Warn("watching for restarted nodes stopped; a node that restarts may take about 15 s to reach the host", "err", err)
			}
		}()
	} else {
		go func() { served <- srv.Serve(ctx, l) }()
	}
	log.Info("host started", "socket", l.Path(), "state", state, "name", name, "node", *withNode,
		"tailnet", *withTailnet, "tailnet_addr", tailnetAddr, "tailnet_name", tailnetName,
		"heartbeat_interval", beat.String(), "missed_heartbeats", *missed)

	nodeDone := make(chan error, 1)
	if *withNode {
		req := node.LocalRequest(name, tailnetName, version)
		go func() {
			nodeDone <- node.RunLocal(ctx, srv, req, srv.HeartbeatInterval(), log)
		}()
	}

	code := awaitShutdown(sigCtx.Done(), served, nodeDone, func() {
		stopSignals() // a second signal now terminates at once
		cancel()
	}, log)
	if err := l.Close(); err != nil {
		log.Error("closing the local socket", "err", err)
	}
	log.Info("host stopped", "exit_code", code)
	return code
}

// resolveTailnet reads the allowlist and the auth key. It returns a non-zero
// exit code, having printed why, when the configuration is unusable: 2 for
// flag misuse, 1 otherwise.
func resolveTailnet(controlURL string, allow []string, allowFile, authKeyFile string, stderr io.Writer) (tailnetConfig, int) {
	tc := tailnetConfig{controlURL: controlURL}
	a, err := host.ParseAllowlist(allow)
	if err != nil {
		fmt.Fprintf(stderr, "gaia host: --allow: %v\n", err)
		return tc, 2
	}
	if allowFile != "" {
		f, err := os.Open(allowFile)
		if err != nil {
			fmt.Fprintf(stderr, "gaia host: --allow-file: %v\n", err)
			return tc, 1
		}
		fa, err := host.ReadAllowlist(f, allowFile)
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(stderr, "gaia host: --allow-file: %v\n", err)
			return tc, 1
		}
		a = a.Merge(fa)
	}
	if a.Len() == 0 {
		fmt.Fprintln(stderr, "gaia host: --tailnet needs an allowlist (--allow or --allow-file); an empty one admits no one")
		return tc, 1
	}
	tc.allow = a
	key, code, err := readAuthKey(authKeyFile, os.Getenv, os.Unsetenv)
	if err != nil {
		fmt.Fprintf(stderr, "gaia host: %v\n", err)
		return tc, code
	}
	tc.authKey = key
	return tc, 0
}

// readAuthKey returns the tailnet auth key from file, else from the first
// set variable of authKeyEnv, and unsets every authKeyEnv variable, so the
// key does not reach child processes. No key is not an error: tsnet state
// that already holds a node key needs none. The error's exit code is 2 for
// flag misuse, 1 otherwise. The key itself appears in no error.
func readAuthKey(file string, getenv func(string) string, unsetenv func(string) error) (core.Secret, int, error) {
	var fromEnv, envName string
	for _, name := range authKeyEnv {
		if v := getenv(name); v != "" && fromEnv == "" {
			fromEnv, envName = v, name
		}
		if err := unsetenv(name); err != nil {
			return core.Secret{}, 1, fmt.Errorf("unset $%s: %w", name, err)
		}
	}
	if file == "" {
		return core.NewSecret(fromEnv), 0, nil
	}
	if fromEnv != "" {
		return core.Secret{}, 2, fmt.Errorf("--auth-key-file and $%s are both set; use one", envName)
	}
	f, err := os.Open(file)
	if err != nil {
		return core.Secret{}, 1, fmt.Errorf("--auth-key-file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return core.Secret{}, 1, fmt.Errorf("--auth-key-file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return core.Secret{}, 1, fmt.Errorf("--auth-key-file %s: not a regular file", file)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return core.Secret{}, 1, fmt.Errorf("--auth-key-file %s: mode %s is group or other accessible; chmod 600 it", file, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return core.Secret{}, 1, fmt.Errorf("--auth-key-file: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return core.Secret{}, 1, fmt.Errorf("--auth-key-file %s: empty", file)
	}
	return core.NewSecret(key), 0, nil
}

// startTailnet joins the tailnet, asking for hostname, and listens for HTTPS on it. On failure it
// returns a nil Tailnet and the exit code, having logged why.
func startTailnet(ctx context.Context, state, hostname string, tc tailnetConfig, log *slog.Logger) (*gnet.Tailnet, net.Listener, int) {
	log.Info("joining the tailnet", "control_url", describeControlURL(tc.controlURL), "hostname", hostname)
	tn, err := tailnetJoin(ctx, gnet.Config{
		Dir:        state,
		Hostname:   hostname,
		ControlURL: tc.controlURL,
		AuthKey:    tc.authKey,
		Logf: func(format string, args ...any) {
			log.Info(fmt.Sprintf(format, args...), "component", "tsnet")
		},
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Error("cannot join the tailnet", "err", err)
		}
		return nil, nil, 1
	}
	tl, err := tn.ListenTLS(tailnetPort)
	if err != nil {
		log.Error("cannot listen on the tailnet", "err", err)
		_ = tn.Close()
		return nil, nil, 1
	}
	return tn, tl, 0
}

// serveBoth serves the API on the local socket and on the tailnet until ctx
// is done or either server stops; then it stops the other with cancel and
// returns both results.
func serveBoth(ctx context.Context, cancel context.CancelFunc, srv *host.Server, l, tl net.Listener, w host.WhoIser) error {
	errs := make(chan error, 2)
	go func() { errs <- srv.Serve(ctx, l) }()
	go func() { errs <- srv.ServeTailnet(ctx, tl, w) }()
	first := <-errs
	cancel()
	return errors.Join(first, <-errs)
}

func describeControlURL(u string) string {
	if u == "" {
		return "Tailscale default"
	}
	return u
}

// awaitShutdown waits for a signal, the API server or the local node to
// end, calls stop, then waits for the API server. The server's single result
// is received exactly once: a signal also cancels Serve's context, so Serve
// may return before the signal is seen, and that is a normal shutdown.
func awaitShutdown(sigDone <-chan struct{}, served, nodeDone <-chan error, stop func(), log *slog.Logger) int {
	code := 0
	var serveErr error
	serverDone := false
	select {
	case <-sigDone:
		log.Info("shutting down")
	case serveErr = <-served:
		serverDone = true
		select {
		case <-sigDone:
			log.Info("shutting down")
		default:
			log.Error("API server stopped", "err", serveErr)
			code = 1
		}
	case err := <-nodeDone:
		if err != nil {
			log.Error("node role stopped", "err", err)
			code = 1
		}
	}
	stop()
	if !serverDone {
		serveErr = <-served
	}
	if serveErr != nil && code == 0 {
		log.Error("API server shutdown", "err", serveErr)
		code = 1
	}
	return code
}

// defaultStateDir is $XDG_STATE_HOME/gaia, else ~/Library/Application
// Support/gaia on darwin, else ~/.local/state/gaia.
func defaultStateDir(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if x := getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "gaia"), nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("no home directory: %w", err)
	}
	if goos == "darwin" {
		return filepath.Join(h, "Library", "Application Support", "gaia"), nil
	}
	return filepath.Join(h, ".local", "state", "gaia"), nil
}

func describeDefaultState(goos string) string {
	if goos == "darwin" {
		return "$XDG_STATE_HOME/gaia or ~/Library/Application Support/gaia"
	}
	return "$XDG_STATE_HOME/gaia or ~/.local/state/gaia"
}
