package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/host"
	gnet "github.com/techspeque/gaia/internal/net"
	"github.com/techspeque/gaia/internal/node"
)

// sessionsDirName is the session manager's root inside a daemon's state
// directory.
const sessionsDirName = "sessions"

// openSessions opens the session manager of a daemon whose state directory
// is state, reporting to reporter, and returns it with its host-facing
// side. The supervisor is this binary's `gaia supervise`. Agents start in
// the user's home directory with the daemon's environment: there are no
// credentials or checkouts yet (phase 1 plan).
func openSessions(ctx context.Context, state string, reporter node.Reporter, log *slog.Logger) (*node.Manager, *node.LocalSessions, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("locating this binary for the session supervisor: %w", err)
	}
	abs, err := filepath.Abs(state)
	if err != nil {
		return nil, nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("no home directory for sessions to start in: %w", err)
	}
	m, err := node.OpenManager(ctx, node.ManagerConfig{
		Dir:           filepath.Join(abs, sessionsDirName),
		Supervisor:    []string{exe, "supervise"},
		SupervisorEnv: os.Environ(),
		Reporter:      reporter,
		Logger:        log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("opening the session manager: %w", err)
	}
	return m, &node.LocalSessions{Manager: m, WorkDir: home, Env: os.Environ}, nil
}

// sessionRouter is the host's host.NodeRouter: the host's own node
// in-process, any other node over the tailnet.
type sessionRouter struct {
	// local is the in-process node's sessions; nil without --node.
	local host.NodeSessions
	// tailnet reaches the other nodes; nil in local-only mode.
	tailnet *tailnetNodes
}

func (r *sessionRouter) Route(n core.Node) (host.NodeSessions, error) {
	if n.TailnetIdentity == node.LocalIdentity {
		if r.local == nil {
			return nil, errors.New("this host runs no node of its own (start it with --node)")
		}
		return r.local, nil
	}
	if r.tailnet == nil {
		return nil, errors.New("this host is not on a tailnet (start it with --tailnet)")
	}
	return r.tailnet.client(n)
}

// tailnetNodes holds one node API client per node tailnet name.
type tailnetNodes struct {
	tn *gnet.Tailnet

	mu      sync.Mutex
	clients map[string]*node.SessionClient
}

func (t *tailnetNodes) client(n core.Node) (*node.SessionClient, error) {
	name := n.TailnetName
	if name == "" {
		return nil, fmt.Errorf("node %q registered without a tailnet name", n.Name)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.clients[name]; c != nil {
		return c, nil
	}
	transport := node.HostTransport(func(ctx context.Context) (net.Conn, string, error) {
		p, err := t.tn.LookupPeer(ctx, name)
		if err != nil {
			return nil, "", err
		}
		c, err := t.tn.Dial(ctx, "tcp", netip.AddrPortFrom(p.Addr, tailnetPort).String())
		return c, p.DNSName, err
	})
	c := &node.SessionClient{HTTP: &http.Client{Transport: transport}, BaseURL: "https://" + name}
	if t.clients == nil {
		t.clients = map[string]*node.SessionClient{}
	}
	t.clients[name] = c
	return c, nil
}

// hostPeerCheck admits a tailnet caller when the tailnet knows it and its
// address is the address of the peer --host names.
func hostPeerCheck(tn *gnet.Tailnet, hostName string) node.PeerCheck {
	return func(ctx context.Context, remoteAddr string) error {
		if _, err := tn.WhoIs(ctx, remoteAddr); err != nil {
			return err
		}
		p, err := tn.LookupPeer(ctx, hostName)
		if err != nil {
			return err
		}
		ap, err := netip.ParseAddrPort(remoteAddr)
		if err != nil {
			return err
		}
		if ap.Addr().Unmap() != p.Addr {
			return fmt.Errorf("the caller %s is not the host %s (%s)", ap.Addr(), hostName, p.Addr)
		}
		return nil
	}
}
