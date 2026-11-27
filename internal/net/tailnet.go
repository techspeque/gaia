package net

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	stdnet "net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/logtail"
	"tailscale.com/tsnet"

	"github.com/techspeque/gaia/internal/core"
)

// stateSubdir is the tsnet state directory under Config.Dir.
const stateSubdir = "tsnet"

// Config configures Join.
type Config struct {
	// Dir is the daemon's state directory. tsnet keeps its state in
	// Dir/tsnet, created 0700.
	Dir string
	// Hostname is the name this machine asks the control server for.
	Hostname string
	// ControlURL is the control server: Tailscale's or a Headscale
	// server. Empty means Tailscale's default.
	ControlURL string
	// AuthKey joins the tailnet the first time. Not needed once the tsnet
	// state holds a node key.
	AuthKey core.Secret
	// Logf receives tsnet's user-facing and backend logs. The auth key is
	// replaced with core.Redacted before Logf sees a line. Nil discards.
	Logf func(format string, args ...any)
}

// Peer is the tailnet identity behind a connection.
type Peer struct {
	// LoginName is the owning user's login, e.g. user@example.com. Empty
	// for a tagged node, which is owned by its tags, not a user.
	LoginName string
	// NodeName is the node's MagicDNS short name.
	NodeName string
	// Tags are the node's ACL tags, e.g. tag:gaia.
	Tags []string
}

// Tailnet is a running tsnet node.
type Tailnet struct {
	srv  *tsnet.Server
	lc   *local.Client
	dir  string
	logf func(format string, args ...any)
}

// Join starts tsnet and returns once the node is Running on the tailnet.
// It calls logtail.Disable first, so tsnet uploads no logs to Tailscale.
func Join(ctx context.Context, cfg Config) (*Tailnet, error) {
	if cfg.Dir == "" {
		return nil, errors.New("tailnet: no state directory")
	}
	logtail.Disable()

	dir := filepath.Join(cfg.Dir, stateSubdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tailnet: state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tailnet: state directory: %w", err)
	}

	logf := redactingLogf(cfg.Logf, cfg.AuthKey)
	srv := &tsnet.Server{
		Dir:        dir,
		Hostname:   cfg.Hostname,
		ControlURL: cfg.ControlURL,
		AuthKey:    cfg.AuthKey.Reveal(),
		Logf:       logf,
		UserLogf:   logf,
	}
	if _, err := srv.Up(ctx); err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("tailnet: join: %w", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		_ = srv.Close()
		return nil, fmt.Errorf("tailnet: local client: %w", err)
	}
	return &Tailnet{srv: srv, lc: lc, dir: dir, logf: logf}, nil
}

// redactingLogf returns a logger that replaces key's value with
// core.Redacted, or a discarding logger when logf is nil.
func redactingLogf(logf func(string, ...any), key core.Secret) func(string, ...any) {
	if logf == nil {
		return func(string, ...any) {}
	}
	v := key.Reveal()
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if v != "" {
			line = strings.ReplaceAll(line, v, core.Redacted)
		}
		logf("%s", line)
	}
}

// ListenTLS listens on port on the tailnet only and wraps the listener in
// TLS 1.3. The certificate is the Tailscale-issued one for a MagicDNS name
// the client asked for by SNI; otherwise, or when the control server
// issues none (Headscale), a self-signed certificate kept in the tsnet
// state directory.
func (t *Tailnet) ListenTLS(port int) (stdnet.Listener, error) {
	fallback, err := loadOrCreateSelfSigned(t.dir, t.selfSignedNames())
	if err != nil {
		return nil, fmt.Errorf("tailnet: self-signed certificate: %w", err)
	}
	l, err := t.srv.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("tailnet: listen: %w", err)
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hi.ServerName != "" && slices.Contains(t.srv.CertDomains(), hi.ServerName) {
				c, err := t.lc.GetCertificate(hi)
				if err == nil {
					return c, nil
				}
				t.logf("gaia: no Tailscale certificate for %s, serving the self-signed one: %v", hi.ServerName, err)
			}
			return fallback, nil
		},
	}
	return tls.NewListener(l, cfg), nil
}

// selfSignedNames are the DNS names and IP addresses the self-signed
// certificate covers.
func (t *Tailnet) selfSignedNames() []string {
	names := slices.Clone(t.srv.CertDomains())
	ip4, ip6 := t.srv.TailscaleIPs()
	for _, ip := range []netip.Addr{ip4, ip6} {
		if ip.IsValid() {
			names = append(names, ip.String())
		}
	}
	return names
}

// WhoIs returns the tailnet identity behind remoteAddr (ip:port), as seen
// by this node.
func (t *Tailnet) WhoIs(ctx context.Context, remoteAddr string) (Peer, error) {
	res, err := t.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Peer{}, fmt.Errorf("tailnet: whois %s: %w", remoteAddr, err)
	}
	if res.Node == nil {
		return Peer{}, fmt.Errorf("tailnet: whois %s: no node", remoteAddr)
	}
	p := Peer{NodeName: nodeName(res.Node.Name, res.Node.ComputedName), Tags: slices.Clone(res.Node.Tags)}
	if !res.Node.IsTagged() && res.UserProfile != nil {
		p.LoginName = res.UserProfile.LoginName
	}
	return p, nil
}

// nodeName is the first label of the node's FQDN, else its computed name.
func nodeName(fqdn, computed string) string {
	if n, _, _ := strings.Cut(strings.TrimSuffix(fqdn, "."), "."); n != "" {
		return n
	}
	return computed
}

// Dial connects to address over the tailnet.
func (t *Tailnet) Dial(ctx context.Context, network, address string) (stdnet.Conn, error) {
	return t.srv.Dial(ctx, network, address)
}

// Addr returns this node's tailnet IPv4 address, or "" when it has none.
func (t *Tailnet) Addr() string {
	ip4, _ := t.srv.TailscaleIPs()
	if !ip4.IsValid() {
		return ""
	}
	return ip4.String()
}

// Close leaves the tailnet and stops tsnet.
func (t *Tailnet) Close() error { return t.srv.Close() }
