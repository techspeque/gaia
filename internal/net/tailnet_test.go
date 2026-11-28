package net

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/net/nettest"
)

// testAuthKey is a known key: tests assert it appears in no log line.
const testAuthKey = "tskey-auth-gaiatest-3f9c1a7e5b2d4806"

// captureLog collects log lines.
type captureLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLog) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *captureLog) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func join(t *testing.T, controlURL, hostname, key string, log *captureLog) *Tailnet {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tn, err := Join(ctx, Config{
		Dir:        t.TempDir(),
		Hostname:   hostname,
		ControlURL: controlURL,
		AuthKey:    core.NewSecret(key),
		Logf:       log.logf,
	})
	if err != nil {
		t.Fatalf("join %s: %v", hostname, err)
	}
	t.Cleanup(func() { _ = tn.Close() })
	return tn
}

func TestJoinDialTLSWhoIs(t *testing.T) {
	if testing.Short() {
		t.Skip("joins a local test tailnet")
	}
	control := nettest.StartControl(t, testAuthKey)
	url := control.HTTPTestServer.URL
	logA, logB := &captureLog{}, &captureLog{}
	a := join(t, url, "gaia-a", testAuthKey, logA)
	b := join(t, url, "gaia-b", testAuthKey, logB)

	l, err := a.ListenTLS(443)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	type seen struct {
		peer Peer
		err  error
	}
	got := make(chan seen, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			got <- seen{err: err}
			return
		}
		defer c.Close()
		tc := c.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			got <- seen{err: err}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := a.WhoIs(ctx, c.RemoteAddr().String())
		got <- seen{p, err}
		_, _ = io.WriteString(c, "hello\n")
	}()

	// Pin the self-signed certificate written to a's state directory.
	pem, err := os.ReadFile(filepath.Join(a.dir, certFile))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("certificate file holds no certificate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := b.Dial(ctx, "tcp", a.Addr()+":443")
	if err != nil {
		t.Fatalf("dial over the tailnet: %v", err)
	}
	conn := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: a.Addr()})
	defer conn.Close()
	if err := conn.HandshakeContext(ctx); err != nil {
		t.Fatalf("TLS handshake with the self-signed certificate: %v", err)
	}
	if v := conn.ConnectionState().Version; v != tls.VersionTLS13 {
		t.Errorf("TLS version %x, want 1.3", v)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Errorf("read %q, %v", line, err)
	}

	s := <-got
	if s.err != nil {
		t.Fatal(s.err)
	}
	if s.peer.NodeName != "gaia-b" {
		t.Errorf("whois node name %q, want gaia-b (peer %+v)", s.peer.NodeName, s.peer)
	}
	if s.peer.LoginName == "" {
		t.Errorf("whois login name empty for an untagged node: %+v", s.peer)
	}
	if s.peer.StableID == "" {
		t.Errorf("whois stable id empty: %+v", s.peer)
	}

	for _, fi := range []string{"", certFile, keyFile} {
		st, err := os.Stat(filepath.Join(a.dir, fi))
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if fi == "" {
			want = 0o700
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s mode %s, want %s", filepath.Join(a.dir, fi), st.Mode().Perm(), want)
		}
	}

	for name, log := range map[string]*captureLog{"a": logA, "b": logB} {
		out := log.all()
		if out == "" {
			t.Errorf("%s: no tsnet log lines captured", name)
		}
		if strings.Contains(out, testAuthKey) {
			t.Errorf("%s: the auth key appears in tsnet logs", name)
		}
	}
}

// TestJoinWrongAuthKeyFails proves the control server's key requirement is
// real, so the passing join above did present the key.
func TestJoinWrongAuthKeyFails(t *testing.T) {
	if testing.Short() {
		t.Skip("joins a local test tailnet")
	}
	control := nettest.StartControl(t, testAuthKey)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log := &captureLog{}
	tn, err := Join(ctx, Config{
		Dir:        t.TempDir(),
		Hostname:   "gaia-wrong",
		ControlURL: control.HTTPTestServer.URL,
		AuthKey:    core.NewSecret("tskey-auth-wrong-00000000"),
		Logf:       log.logf,
	})
	if err == nil {
		_ = tn.Close()
		t.Fatal("joined with the wrong auth key")
	}
	if strings.Contains(log.all(), "tskey-auth-wrong-00000000") {
		t.Error("the auth key appears in tsnet logs")
	}
}

func TestRedactingLogf(t *testing.T) {
	log := &captureLog{}
	f := redactingLogf(log.logf, core.NewSecret("s3cr3t"))
	f("key=%s and %d", "s3cr3t", 1)
	f("100%% plain")
	if got := log.all(); got != "key="+core.Redacted+" and 1\n100% plain" {
		t.Errorf("got %q", got)
	}
	redactingLogf(nil, core.NewSecret("x"))("no panic %d", 1)
}

func TestNodeName(t *testing.T) {
	for _, c := range []struct{ fqdn, computed, want string }{
		{"gaia-b.tail-test.ts.net.", "x", "gaia-b"},
		{"gaia-b", "x", "gaia-b"},
		{"", "comp", "comp"},
	} {
		if got := nodeName(c.fqdn, c.computed); got != c.want {
			t.Errorf("nodeName(%q, %q) = %q, want %q", c.fqdn, c.computed, got, c.want)
		}
	}
}

func TestSelfSignedKeptAndReused(t *testing.T) {
	dir := t.TempDir()
	c1, err := loadOrCreateSelfSigned(dir, []string{"gaia.example", "100.64.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c1.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "gaia.example" ||
		len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "100.64.0.1" {
		t.Errorf("names %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if leaf.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("key algorithm %v, want ECDSA", leaf.PublicKeyAlgorithm)
	}
	c2, err := loadOrCreateSelfSigned(dir, []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if string(c1.Certificate[0]) != string(c2.Certificate[0]) {
		t.Error("the kept certificate was not reused")
	}

	// A kept pair that does not parse is not overwritten.
	if err := os.WriteFile(filepath.Join(dir, certFile), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSelfSigned(dir, nil); err == nil {
		t.Error("a corrupt certificate was accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, certFile)); string(b) != "junk" {
		t.Error("a corrupt certificate was overwritten")
	}
}

func TestLookupPeerAndHasState(t *testing.T) {
	if testing.Short() {
		t.Skip("joins a local test tailnet")
	}
	control := nettest.StartControl(t, testAuthKey)
	url := control.HTTPTestServer.URL
	a := join(t, url, "gaia-host", testAuthKey, &captureLog{})
	b := join(t, url, "gaia-node", testAuthKey, &captureLog{})

	lookup := func(name string) (PeerAddr, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return b.LookupPeer(ctx, name)
	}
	// The netmap naming a may reach b a moment after b is Running.
	var got PeerAddr
	var err error
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if got, err = lookup("gaia-host"); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr.String() != a.Addr() {
		t.Errorf("lookup by name: %v, want %s", got.Addr, a.Addr())
	}
	if got.DNSName != "gaia-host."+nettest.MagicDNSDomain {
		t.Errorf("dns name %q", got.DNSName)
	}
	for _, name := range []string{"GAIA-HOST", "gaia-host." + nettest.MagicDNSDomain + ".", a.Addr()} {
		if p, err := lookup(name); err != nil || p.Addr != got.Addr {
			t.Errorf("lookup %q: %v, %v", name, p, err)
		}
	}
	for _, name := range []string{"gaia", "nobody", "gaia-node", "100.64.255.254"} {
		if p, err := lookup(name); err == nil {
			t.Errorf("lookup %q found %v", name, p)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	self, err := a.Self(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (SelfName{Name: "gaia-host", DNSName: "gaia-host." + nettest.MagicDNSDomain}); self != want {
		t.Errorf("Self = %+v, want %+v", self, want)
	}

	if !HasState(filepath.Dir(a.dir)) {
		t.Error("HasState is false after a join")
	}
	if HasState(t.TempDir()) {
		t.Error("HasState is true for an empty directory")
	}
}

func TestJoinNeedsDir(t *testing.T) {
	if _, err := Join(context.Background(), Config{}); err == nil {
		t.Error("Join without a state directory succeeded")
	}
}
