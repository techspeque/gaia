package net

import (
	"bufio"
	"context"
	"fmt"
	"io"
	stdnet "net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/net/nettest"
)

// heldProxy is a TCP proxy to a control server whose answers to its client
// can be held back, so that client learns of tailnet changes late.
type heldProxy struct {
	mu     sync.Mutex
	resume chan struct{} // nil when not held
}

func (p *heldProxy) hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resume == nil {
		p.resume = make(chan struct{})
	}
}

func (p *heldProxy) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resume != nil {
		close(p.resume)
		p.resume = nil
	}
}

func (p *heldProxy) wait() {
	p.mu.Lock()
	c := p.resume
	p.mu.Unlock()
	if c != nil {
		<-c
	}
}

// startHeldProxy proxies 127.0.0.1:<port> to target and returns the proxy
// and its URL.
func startHeldProxy(t *testing.T, target string) (*heldProxy, string) {
	t.Helper()
	l, err := stdnet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &heldProxy{}
	t.Cleanup(func() { p.release(); _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			u, err := stdnet.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			go func() { _, _ = io.Copy(u, c); _ = u.Close() }()
			go func() {
				defer c.Close()
				buf := make([]byte, 32<<10)
				for {
					n, err := u.Read(buf)
					if n > 0 {
						p.wait()
						if _, err := c.Write(buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return p, "http://" + l.Addr().String()
}

// echoServer answers every line on port of tn with the same line.
func echoServer(t *testing.T, tn *Tailnet, port int) {
	t.Helper()
	l, err := tn.srv.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					s, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = io.WriteString(c, s)
				}
			}()
		}
	}()
}

func echo(c stdnet.Conn, r *bufio.Reader, d time.Duration) error {
	_ = c.SetDeadline(time.Now().Add(d))
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		return err
	}
	_, err := r.ReadString('\n')
	return err
}

// TestRestartedPeerResumes reproduces fix-0004 (findings f-012, f-017). A
// node restarts, and the host learns its new disco key from the control
// server only after the node has handshaken and exchanged data: the
// late-control order seen after a 40-45 s outage on real machines. The
// host's engine then drops its WireGuard session with the node. Without
// ResumeRestartedPeers, the node's next request on its open connection
// went unanswered for about 15 s (17.3 s measured), until wireguard-go's
// "stopped hearing back" timer fired. With it, the host starts a new
// session at once.
func TestRestartedPeerResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("joins a local test tailnet")
	}
	control := nettest.StartControl(t, testAuthKey)
	controlURL := control.HTTPTestServer.URL
	cu, err := url.Parse(controlURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy, proxyURL := startHeldProxy(t, cu.Host)

	hostLog := &captureLog{}
	h := join(t, proxyURL, "gaia-host", testAuthKey, hostLog)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resumed := make(chan error, 1)
	go func() { resumed <- h.ResumeRestartedPeers(ctx) }()
	const port = 8080
	echoServer(t, h, port)
	addr := h.Addr() + fmt.Sprintf(":%d", port)

	dir := t.TempDir()
	start := func() *Tailnet {
		jctx, jcancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer jcancel()
		tn, err := Join(jctx, Config{Dir: dir, Hostname: "gaia-node", ControlURL: controlURL,
			AuthKey: core.NewSecret(testAuthKey), Logf: (&captureLog{}).logf})
		if err != nil {
			t.Fatal(err)
		}
		return tn
	}
	dialEcho := func(n *Tailnet) (stdnet.Conn, *bufio.Reader) {
		t.Helper()
		dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
		defer dcancel()
		c, err := n.Dial(dctx, "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(c)
		if err := echo(c, r, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		return c, r
	}

	// The node goes down with its connection open, as a killed process
	// does. (Closing it first would leave the host with packets to send to
	// the node after the session drop, which starts a handshake from the
	// host's side and hides the stall.)
	n := start()
	_, _ = dialEcho(n)
	_ = n.Close()
	time.Sleep(2 * time.Second)

	// The node comes back while the host hears nothing from control. It
	// handshakes with the host and exchanges data, like a registration.
	proxy.hold()
	n = start()
	defer n.Close()
	c, r := dialEcho(n)
	defer c.Close()

	// Control's news reaches the host: it drops the session.
	proxy.release()
	waitLog(t, hostLog, `wgengine: Reconfig: [`, "changed from")
	time.Sleep(2 * time.Second)

	begin := time.Now()
	err = echo(c, r, 25*time.Second)
	took := time.Since(begin)
	if err != nil {
		t.Fatalf("echo on the open connection after the host learned the new key: %v", err)
	}
	if took > 5*time.Second {
		t.Errorf("echo on the open connection took %s, want under 5s (about 15s is the fix-0004 stall)", took)
	}
	if !strings.Contains(hostLog.all(), "restarted; renewing its WireGuard session") {
		t.Error("the host did not log renewing the restarted peer's session")
	}

	cancel()
	select {
	case err := <-resumed:
		if err != context.Canceled {
			t.Errorf("ResumeRestartedPeers = %v after cancel, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("ResumeRestartedPeers did not return after cancel")
	}
}

// waitLog waits up to 30 s for a log line holding every one of parts.
func waitLog(t *testing.T, log *captureLog, parts ...string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		for _, ln := range strings.Split(log.all(), "\n") {
			ok := true
			for _, p := range parts {
				ok = ok && strings.Contains(ln, p)
			}
			if ok {
				return
			}
		}
	}
	t.Fatalf("no log line with %q", parts)
}
