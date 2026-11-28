package net

import (
	"bytes"
	"crypto/tls"
	"log"
	stdnet "net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// vanishingAddrConn reports its remote address on the first call only, as
// a netstack connection does once its peer has reset it.
type vanishingAddrConn struct {
	stdnet.Conn
	calls atomic.Int32
}

func (c *vanishingAddrConn) RemoteAddr() stdnet.Addr {
	if c.calls.Add(1) > 1 {
		return nil
	}
	return &stdnet.TCPAddr{IP: stdnet.IPv4(100, 64, 0, 7), Port: 51234}
}

// oneConnListener accepts c once, then blocks until closed.
type oneConnListener struct {
	c      stdnet.Conn
	once   sync.Once
	closed chan struct{}
}

func (l *oneConnListener) Accept() (stdnet.Conn, error) {
	var c stdnet.Conn
	l.once.Do(func() { c = l.c })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, stdnet.ErrClosed
}

func (l *oneConnListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *oneConnListener) Addr() stdnet.Addr { return &stdnet.TCPAddr{} }

// syncBuffer is a bytes.Buffer safe for the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAcceptedAddrKept(t *testing.T) {
	cert, err := loadOrCreateSelfSigned(t.TempDir(), []string{"gaia-test"})
	if err != nil {
		t.Fatal(err)
	}
	server, client := stdnet.Pipe()
	defer client.Close()
	inner := &oneConnListener{c: &vanishingAddrConn{Conn: server}, closed: make(chan struct{})}
	l := tls.NewListener(addrKeepingListener{inner}, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS13})

	logs := &syncBuffer{}
	srv := &http.Server{ErrorLog: log.New(logs, "", 0), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	// Not a TLS record and not HTTP, so the handshake fails and is logged.
	go func() { _, _ = client.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "TLS handshake error") {
		if time.Now().After(deadline) {
			t.Fatalf("no handshake error logged; log: %q", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := logs.String()
	if !strings.Contains(got, "from 100.64.0.7:51234") || strings.Contains(got, "<nil>") {
		t.Errorf("handshake error does not name the peer: %q", got)
	}
}
