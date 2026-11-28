package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/logging"
	gnet "github.com/techspeque/gaia/internal/net"
	"github.com/techspeque/gaia/internal/node"
	"github.com/techspeque/gaia/internal/sock"
)

// hostRequestTimeout bounds one registration or heartbeat request.
const hostRequestTimeout = 15 * time.Second

// joinOnlyFlags are the node flags that mean nothing without --join.
var joinOnlyFlags = []string{"auth-key-file"}

// runNode runs `gaia node` until SIGTERM or SIGINT. It exits early only on
// a start error or when the host refuses this node's API version; an
// unreachable host is waited out.
func runNode(args []string, stderr io.Writer, goos string) int {
	if refuseDaemon("node", goos, stderr) {
		return 1
	}
	fs := flag.NewFlagSet("gaia node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hostName := fs.String("host", "", "the host's tailnet name (MagicDNS name or its first label) or tailnet IP; required")
	join := fs.Bool("join", false, "join the tailnet now, with the auth key from $GAIA_AUTHKEY, $TS_AUTHKEY or --auth-key-file; needed once")
	stateFlag := fs.String("state", "", "state directory (default: "+describeDefaultState(goos)+")")
	nameFlag := fs.String("name", "", "this node's name in the registry, and the tailnet name it asks for (default: the name recorded in the state directory, else gaia-<machine name>)")
	controlURL := fs.String("control-url", "", "tailnet control server URL, Tailscale or Headscale (default: Tailscale's)")
	authKeyFile := fs.String("auth-key-file", "", "file holding the tailnet auth key, not group or other readable; needs --join")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gaia node: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *hostName == "" {
		fmt.Fprintln(stderr, "gaia node: --host NAME is required: the host's tailnet name")
		return 2
	}
	if !*join {
		var set []string
		fs.Visit(func(f *flag.Flag) {
			if slices.Contains(joinOnlyFlags, f.Name) {
				set = append(set, "--"+f.Name)
			}
		})
		if len(set) > 0 {
			fmt.Fprintf(stderr, "gaia node: %s needs --join\n", strings.Join(set, ", "))
			return 2
		}
	}
	if *nameFlag != "" {
		if err := core.ValidNodeName(*nameFlag); err != nil {
			fmt.Fprintf(stderr, "gaia node: --name: %s\n", err.(*core.Error).Message)
			return 2
		}
	}
	state := *stateFlag
	if state == "" {
		var err error
		if state, err = defaultStateDir(goos, os.Getenv, os.UserHomeDir); err != nil {
			fmt.Fprintf(stderr, "gaia node: %v; pass --state DIR\n", err)
			return 1
		}
	}
	// Resolved before ensureStateDir creates the state directory, which
	// would make a new one look used before.
	name, err := resolveName(*nameFlag, state, machineName(os.Hostname), "node")
	if err != nil {
		fmt.Fprintf(stderr, "gaia node: %v\n", err)
		return 1
	}

	// The key is read, and the variables unset, with or without --join, so
	// a key in the environment never reaches a child process.
	key, code, err := readAuthKey(*authKeyFile, os.Getenv, os.Unsetenv)
	if err != nil {
		fmt.Fprintf(stderr, "gaia node: %v\n", err)
		return code
	}
	if !*join {
		key = core.Secret{}
	}
	if err := ensureStateDir(state); err != nil {
		fmt.Fprintf(stderr, "gaia node: %v\n", err)
		return 1
	}
	switch {
	case *join && key.Reveal() == "":
		fmt.Fprintln(stderr, "gaia node: --join needs a tailnet auth key: set $GAIA_AUTHKEY or $TS_AUTHKEY, or pass --auth-key-file")
		return 1
	case !*join && !gnet.HasState(state):
		fmt.Fprintf(stderr, "gaia node: %s holds no tailnet state: run once with --join and an auth key\n", state)
		return 1
	}
	if err := recordName(state, name); err != nil {
		fmt.Fprintf(stderr, "gaia node: %v\n", err)
		return 1
	}

	redactor := &logging.Redactor{}
	redactor.Register(key) // before any log line
	log := logging.New(stderr, redactor)
	return nodeDaemon(nodeConfig{
		host: *hostName, name: name, state: state, controlURL: *controlURL, authKey: key,
	}, log)
}

// nodeConfig is the resolved configuration of `gaia node`.
type nodeConfig struct {
	host, name, state, controlURL string
	authKey                       core.Secret
}

// nodeDaemon runs the node: socket first, so a CLI here gets an answer
// while the node is still joining; then the tailnet; then registration.
func nodeDaemon(cfg nodeConfig, log *slog.Logger) int {
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopSignals()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	l, err := sock.Listen(sock.Dir(), sock.Options{
		OnDeny: func(err error) { log.Warn("local socket peer refused", "err", err) },
	})
	if err != nil {
		log.Error("cannot bind the local socket", "err", err)
		return 1
	}
	defer func() { _ = l.Close() }()

	// link tells the forwarder where the node stands with the host, so a
	// request while it is (re)connecting gets a prompt answer saying so.
	link := &node.Link{}
	link.Set(node.PhaseJoining)
	var tn atomic.Pointer[gnet.Tailnet]
	transport := node.HostTransport(func(ctx context.Context) (net.Conn, string, error) {
		t := tn.Load()
		if t == nil {
			return nil, "", errors.New("this node has not joined the tailnet yet")
		}
		p, err := t.LookupPeer(ctx, cfg.host)
		if err != nil {
			return nil, "", err
		}
		c, err := t.Dial(ctx, "tcp", netip.AddrPortFrom(p.Addr, tailnetPort).String())
		return c, p.DNSName, dialHint(err, cfg.host, p.DNSName, p.Online)
	})
	defer transport.CloseIdleConnections()
	base := &url.URL{Scheme: "https", Host: cfg.host}

	served := make(chan error, 1)
	go func() {
		served <- serveHTTP(ctx, l, node.Handler(node.Forwarder(base, transport, cfg.host, link, log)), log)
	}()
	log.Info("node started", "socket", l.Path(), "state", cfg.state, "host", cfg.host, "name", cfg.name)

	log.Info("joining the tailnet", "control_url", describeControlURL(cfg.controlURL), "hostname", cfg.name)
	t, err := tailnetJoin(ctx, gnet.Config{
		Dir:        cfg.state,
		Hostname:   cfg.name,
		ControlURL: cfg.controlURL,
		AuthKey:    cfg.authKey,
		Logf: func(format string, args ...any) {
			log.Info(fmt.Sprintf(format, args...), "component", "tsnet")
		},
	})
	if err != nil {
		code := 0
		if sigCtx.Err() == nil {
			log.Error("cannot join the tailnet", "err", err)
			code = 1
		}
		cancel()
		<-served
		log.Info("node stopped", "exit_code", code)
		return code
	}
	defer func() {
		if err := t.Close(); err != nil {
			log.Error("leaving the tailnet", "err", err)
		}
	}()
	tn.Store(t)
	log.Info("joined the tailnet", "tailnet_addr", t.Addr())
	reportTailnetName(ctx, t, cfg.name, "node", log)

	runDone := make(chan error, 1)
	go func() {
		err := node.Run(ctx, node.RunConfig{
			Host: &node.Client{
				HTTP:    &http.Client{Transport: transport, Timeout: hostRequestTimeout},
				BaseURL: base.String(),
			},
			Request: node.Request(cfg.name, version),
			Link:    link,
			Logger:  log,
		})
		if err != nil {
			log.Error("the host refused this node", "err", err)
		}
		runDone <- err
	}()

	code := awaitShutdown(sigCtx.Done(), served, runDone, func() {
		stopSignals() // a second signal now terminates at once
		cancel()
	}, log)
	if err := l.Close(); err != nil {
		log.Error("closing the local socket", "err", err)
	}
	log.Info("node stopped", "exit_code", code)
	return code
}

// hostNameHint ends the dial hints: where the Gaia host's device is found.
const hostNameHint = "a Gaia host asks for the tailnet name gaia-<machine name> and logs the tailnet_name it was given"

// dialHint adds to a failed dial of the host what is likely wrong with the
// device --host names (dnsName; online as the tailnet reports it). Errors
// other than these, and nil, are returned as they are.
//   - Refused: the device runs no Gaia host, typically because it is the
//     host machine's own Tailscale device. A refusal reads "connection
//     refused" from the kernel and "connection was refused" from tsnet's
//     netstack.
//   - Timed out: the device is offline, or answers nothing on the port, so
//     it is not a Gaia host; typically a stale device left by an earlier
//     host name.
func dialHint(err error, host, dnsName string, online bool) error {
	if err == nil {
		return nil
	}
	if msg := err.Error(); strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection was refused") {
		return fmt.Errorf("%w: --host %s is %s, where nothing answers on port %d, so it is not a Gaia host; %s",
			err, host, dnsName, tailnetPort, hostNameHint)
	}
	var ne net.Error
	if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &ne) || !ne.Timeout()) {
		return err
	}
	if !online {
		return fmt.Errorf("%w: --host %s is %s, which the tailnet reports offline; "+
			"if the host now runs under another name, point --host at it; %s",
			err, host, dnsName, hostNameHint)
	}
	return fmt.Errorf("%w: --host %s is %s, which did not answer on port %d, so it is offline or not a Gaia host; %s",
		err, host, dnsName, tailnetPort, hostNameHint)
}

// serveHTTP serves h on l until ctx is done, then shuts down, waiting up to
// five seconds for requests in flight. It closes l.
func serveHTTP(ctx context.Context, l net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := srv.Shutdown(sctx)
	if errors.Is(err, context.DeadlineExceeded) {
		log.Warn("connections still open after the shutdown grace period; closing them")
		err = srv.Close()
	}
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		err = errors.Join(err, serr)
	}
	return err
}

// ensureStateDir creates dir at 0700 if missing and refuses it unless it is
// a real directory with no group or other access.
func ensureStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("state directory %s is a symbolic link", dir)
	case !fi.IsDir():
		return fmt.Errorf("state directory %s is not a directory", dir)
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("state directory %s has mode %s, accessible by group or others: chmod 700 it", dir, fi.Mode().Perm())
	}
	return nil
}
