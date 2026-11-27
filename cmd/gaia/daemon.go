package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/techspeque/gaia/internal/host"
	"github.com/techspeque/gaia/internal/logging"
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

// runNode is the remote node role, which arrives with workstream 0.7.
func runNode(_ []string, stderr io.Writer, goos string) int {
	if refuseDaemon("node", goos, stderr) {
		return 1
	}
	fmt.Fprintln(stderr, "gaia node: not available in this version; on a single machine run `gaia host --node`")
	return 1
}

// runHost runs `gaia host [--state DIR] [--node]` until SIGTERM or SIGINT.
func runHost(args []string, stderr io.Writer, goos string) int {
	if refuseDaemon("host", goos, stderr) {
		return 1
	}
	fs := flag.NewFlagSet("gaia host", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateFlag := fs.String("state", "", "state directory (default: "+describeDefaultState(goos)+")")
	withNode := fs.Bool("node", false, "also run this machine as a node, in-process (local-only mode)")
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
	state := *stateFlag
	if state == "" {
		var err error
		if state, err = defaultStateDir(goos, os.Getenv, os.UserHomeDir); err != nil {
			fmt.Fprintf(stderr, "gaia host: %v; pass --state DIR\n", err)
			return 1
		}
	}

	log := logging.New(stderr, &logging.Redactor{})
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

	l, err := sock.Listen(sock.Dir(), sock.Options{
		OnDeny: func(err error) { log.Warn("local socket peer refused", "err", err) },
	})
	if err != nil {
		log.Error("cannot bind the local socket", "err", err)
		return 1
	}
	defer func() { _ = l.Close() }()

	srv := host.New(st, host.Options{Version: version, Logger: log})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, l) }()
	log.Info("host started", "socket", l.Path(), "state", state, "node", *withNode)

	nodeDone := make(chan error, 1)
	if *withNode {
		go func() {
			nodeDone <- node.RunLocal(ctx, srv, node.LocalRequest(version), srv.HeartbeatInterval(), log)
		}()
	}

	code := 0
	select {
	case <-sigCtx.Done():
		log.Info("shutting down")
	case err := <-served:
		log.Error("API server stopped", "err", err)
		code = 1
	case err := <-nodeDone:
		if err != nil {
			log.Error("local node stopped", "err", err)
			code = 1
		}
	}
	stopSignals() // a second signal now terminates at once
	cancel()
	if err := <-served; err != nil && code == 0 {
		log.Error("API server shutdown", "err", err)
		code = 1
	}
	if err := l.Close(); err != nil {
		log.Error("closing the local socket", "err", err)
	}
	log.Info("host stopped", "exit_code", code)
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
