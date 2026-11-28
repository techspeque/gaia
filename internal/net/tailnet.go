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
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/logtail"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/types/key"

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
	// StableID is the node's stable id from the control server. It names
	// one machine's tailnet membership for as long as its tsnet state lives.
	StableID string
}

// stateFile is the file tsnet keeps its state in, inside the tsnet dir.
const stateFile = "tailscaled.state"

// HasState reports whether the state directory dir holds tsnet state from
// an earlier join, so Join needs no auth key.
func HasState(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, stateSubdir, stateFile))
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
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
	p := Peer{
		NodeName: nodeName(res.Node.Name, res.Node.ComputedName),
		Tags:     slices.Clone(res.Node.Tags),
		StableID: string(res.Node.StableID),
	}
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

// PeerAddr is where a tailnet peer is reached.
type PeerAddr struct {
	// Addr is the peer's tailnet address, IPv4 when it has one.
	Addr netip.Addr
	// DNSName is the peer's MagicDNS name without the trailing dot, or ""
	// when the control server gives none.
	DNSName string
}

// LookupPeer finds the peer called name in this node's view of the
// tailnet: name is a MagicDNS name, its first label (case-insensitive) or
// one of the peer's tailnet IP addresses. No match, or a short name that
// matches more than one peer, is an error.
func (t *Tailnet) LookupPeer(ctx context.Context, name string) (PeerAddr, error) {
	st, err := t.lc.Status(ctx)
	if err != nil {
		return PeerAddr{}, fmt.Errorf("tailnet: status: %w", err)
	}
	want := strings.TrimSuffix(strings.ToLower(name), ".")
	ip, ipErr := netip.ParseAddr(name)
	var found []PeerAddr
	for _, p := range st.Peer {
		dns := strings.TrimSuffix(strings.ToLower(p.DNSName), ".")
		match := false
		if ipErr == nil {
			match = slices.Contains(p.TailscaleIPs, ip)
		} else {
			first, _, _ := strings.Cut(dns, ".")
			match = dns != "" && (want == dns || want == first)
		}
		if !match || len(p.TailscaleIPs) == 0 {
			continue
		}
		addr := p.TailscaleIPs[0]
		for _, a := range p.TailscaleIPs {
			if a.Is4() {
				addr = a
				break
			}
		}
		found = append(found, PeerAddr{Addr: addr, DNSName: dns})
	}
	switch len(found) {
	case 0:
		return PeerAddr{}, fmt.Errorf("tailnet: no peer named %q", name)
	case 1:
		return found[0], nil
	}
	return PeerAddr{}, fmt.Errorf("tailnet: %d peers match %q; use the full MagicDNS name or the tailnet IP", len(found), name)
}

// Dial connects to address over the tailnet.
func (t *Tailnet) Dial(ctx context.Context, network, address string) (stdnet.Conn, error) {
	return t.srv.Dial(ctx, network, address)
}

// SelfName is this node's name on the tailnet as the control server
// assigned it. It can differ from Config.Hostname: a control server that
// already has a device by that name gives this one a suffixed name.
type SelfName struct {
	// Name is the MagicDNS short name, the first label of DNSName: what
	// peers see as Peer.NodeName and look this node up by. "" when the
	// control server gives no MagicDNS name.
	Name string
	// DNSName is the full MagicDNS name without the trailing dot, or "".
	DNSName string
}

// Self returns this node's assigned tailnet name.
func (t *Tailnet) Self(ctx context.Context) (SelfName, error) {
	st, err := t.lc.Status(ctx)
	if err != nil {
		return SelfName{}, fmt.Errorf("tailnet: status: %w", err)
	}
	if st.Self == nil {
		return SelfName{}, errors.New("tailnet: status has no self node")
	}
	dns := strings.TrimSuffix(st.Self.DNSName, ".")
	return SelfName{Name: nodeName(dns, ""), DNSName: dns}, nil
}

// Addr returns this node's tailnet IPv4 address, or "" when it has none.
func (t *Tailnet) Addr() string {
	ip4, _ := t.srv.TailscaleIPs()
	if !ip4.IsValid() {
		return ""
	}
	return ip4.String()
}

// renewDelays are when, after a peer's disco key changes, ResumeRestartedPeers
// pings it. The first ping can still ride the session the engine is about
// to drop; the later ones follow the drop and start the new session.
var renewDelays = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second, 6 * time.Second}

// ResumeRestartedPeers keeps peers that restart reachable: it watches this
// node's netmap and, when a peer's disco key changes, pings that peer over
// the tailnet so this side starts a new WireGuard session. It runs until
// ctx is done and returns ctx's error, or the first error watching fails
// with. The host runs it, because it only answers nodes and never sends to them first.
//
// Why: a node gets a new disco key every time it starts. When the control
// server tells the host about the new key, the host's engine drops its
// WireGuard session with that node, so a restarted peer gets a fresh
// handshake. If the node has already handshaken and registered by then
// (after an outage, the control server can be seconds late), the node
// keeps sending on the session the host has dropped. The host discards
// those packets without telling the node, and the node notices only when
// WireGuard's 15 s "stopped hearing back" timer fires a new handshake.
// The host's ping makes it start that handshake at once (fix-0004).
func (t *Tailnet) ResumeRestartedPeers(ctx context.Context) error {
	w, err := t.lc.WatchIPNBus(ctx, ipn.NotifyInitialNetMap)
	if err != nil {
		return fmt.Errorf("tailnet: watch: %w", err)
	}
	defer w.Close()
	disco := map[tailcfg.NodeID]key.DiscoPublic{}
	for {
		n, err := w.Next()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("tailnet: watch: %w", err)
		}
		if n.NetMap == nil {
			continue
		}
		for _, p := range n.NetMap.Peers {
			k := p.DiscoKey()
			old, seen := disco[p.ID()]
			disco[p.ID()] = k
			if !seen || old == k || k.IsZero() || p.Addresses().Len() == 0 {
				continue
			}
			addr := p.Addresses().At(0).Addr()
			t.logf("gaia: peer %s (%s) restarted; renewing its WireGuard session", nodeName(p.Name(), p.ComputedName()), addr)
			go t.renew(ctx, addr)
		}
	}
}

// renew pings addr at each of renewDelays after now.
func (t *Tailnet) renew(ctx context.Context, addr netip.Addr) {
	start := time.Now()
	for _, d := range renewDelays {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(start.Add(d))):
		}
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := t.lc.Ping(pctx, addr, tailcfg.PingTSMP)
		cancel()
		if err != nil && ctx.Err() == nil {
			t.logf("gaia: renewing the WireGuard session with %s: ping after %s: %v", addr, d, err)
		}
	}
}

// Close leaves the tailnet and stops tsnet.
func (t *Tailnet) Close() error { return t.srv.Close() }
