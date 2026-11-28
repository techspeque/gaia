package node

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/host"
	gnet "github.com/techspeque/gaia/internal/net"
	"github.com/techspeque/gaia/internal/store"
)

// onePeer names every connection as the same tailnet peer.
type onePeer gnet.Peer

func (p onePeer) WhoIs(context.Context, string) (gnet.Peer, error) { return gnet.Peer(p), nil }

// startHost serves a real host registry's tailnet handler over plain HTTP,
// every caller being the peer node-a.
func startHost(t *testing.T, opts host.Options) (*host.Server, *Client) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if opts.Allowlist, err = host.ParseAllowlist([]string{"tag:gaia"}); err != nil {
		t.Fatal(err)
	}
	if opts.Version == "" {
		opts.Version = "v1.2.3"
	}
	s := host.New(st, opts)
	srv := httptest.NewServer(s.TailnetHandler(onePeer{NodeName: "node-a", Tags: []string{"tag:gaia"}, StableID: "nA"}))
	t.Cleanup(srv.Close)
	return s, &Client{HTTP: srv.Client(), BaseURL: srv.URL}
}

func listOf(t *testing.T, s *host.Server) []core.Node {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, core.NodesPath, nil))
	var l core.ListNodesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Nodes
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunAgainstHost(t *testing.T) {
	s, c := startHost(t, host.Options{HeartbeatInterval: 20 * time.Millisecond, MissedHeartbeats: 2})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunConfig{Host: c, Request: Request("node-a", "v1.2.3"), MinBackoff: time.Millisecond})
	}()

	var first core.Node
	waitFor(t, "registration", func() bool {
		l := listOf(t, s)
		if len(l) == 1 {
			first = l[0]
		}
		return len(l) == 1
	})
	if first.TailnetIdentity != "nA" || first.Name != "node-a" || first.APIVersion != core.APIVersion {
		t.Errorf("registered %+v", first)
	}
	waitFor(t, "a heartbeat", func() bool {
		l := listOf(t, s)
		return len(l) == 1 && l[0].LastSeen.After(first.LastSeen)
	})
	if l := listOf(t, s); l[0].State != core.NodeOnline || l[0].ID != first.ID {
		t.Errorf("after heartbeats %+v", l[0])
	}

	// Removed while running: the next heartbeat is not_found and the node
	// registers again.
	if _, err := s.RemoveNode(context.Background(), "node-a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "registration again", func() bool { return len(listOf(t, s)) == 1 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRunRefusedTwoVersionsBehind(t *testing.T) {
	_, c := startHost(t, host.Options{APIVersion: core.APIVersion + 2, Version: "v9.0.0"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, RunConfig{Host: c, Request: Request("node-a", "v1.2.3")})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeVersionMismatch {
		t.Fatalf("Run = %v, want version_mismatch", err)
	}
	if !strings.Contains(ce.Message, "install gaia v9.0.0 on the node") {
		t.Errorf("message lacks the upgrade instruction: %q", ce.Message)
	}
	if ctx.Err() != nil {
		t.Error("Run retried a version refusal instead of returning it")
	}
}

func TestRunOneVersionBehindAccepted(t *testing.T) {
	s, c := startHost(t, host.Options{APIVersion: core.APIVersion + 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Run(ctx, RunConfig{Host: c, Request: Request("node-a", "v1")}) }()
	waitFor(t, "registration", func() bool { return len(listOf(t, s)) == 1 })
}

// flakyHost fails registration and heartbeats on cue.
type flakyHost struct {
	mu            sync.Mutex
	failRegisters int // the next n registrations fail
	failBeat      bool
	registers     []time.Time
	beats         int
}

var errDown = errors.New("connection refused")

func (f *flakyHost) Register(_ context.Context, req core.RegisterNodeRequest) (core.RegisterNodeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers = append(f.registers, time.Now())
	if f.failRegisters > 0 {
		f.failRegisters--
		return core.RegisterNodeResponse{}, errDown
	}
	return core.RegisterNodeResponse{Node: core.Node{ID: id(1), Name: req.Name}, HeartbeatIntervalMS: 5}, nil
}

func (f *flakyHost) Heartbeat(context.Context, core.HeartbeatRequest) (core.HeartbeatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	if f.failBeat {
		return core.HeartbeatResponse{}, errDown
	}
	return core.HeartbeatResponse{}, nil
}

func (f *flakyHost) snapshot() ([]time.Time, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.registers...), f.beats
}

func TestRunBacksOffWhileHostDown(t *testing.T) {
	f := &flakyHost{failRegisters: 5}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunConfig{Host: f, Request: Request("n", "v1"),
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond,
			Logger: logger(&logs)})
	}()
	waitFor(t, "registration and heartbeats", func() bool { r, b := f.snapshot(); return len(r) == 6 && b >= 2 })
	regs, _ := f.snapshot()
	// Waits: 10, 20, 40, 40, 40 ms.
	for i, min := range []time.Duration{10, 20, 40, 40, 40} {
		if gap := regs[i+1].Sub(regs[i]); gap < min*time.Millisecond {
			t.Errorf("gap %d = %s, want at least %dms", i, gap, min)
		}
	}

	// The host goes away after registration: heartbeats fail, the node
	// registers again (and keeps trying), and never returns.
	f.mu.Lock()
	f.failBeat, f.failRegisters = true, 3
	f.mu.Unlock()
	waitFor(t, "registration after a failed heartbeat", func() bool { r, _ := f.snapshot(); return len(r) >= 10 })
	select {
	case err := <-done:
		t.Fatalf("Run returned %v while the host was down", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v after cancel", err)
	}
	if !strings.Contains(logs.String(), "registration with the host failed; retrying") ||
		!strings.Contains(logs.String(), "heartbeat failed; registering again") {
		t.Errorf("logs:\n%s", logs.String())
	}
}

func TestHandler(t *testing.T) {
	type seen struct{ method, path, body, query string }
	got := make(chan seen, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, string(b), r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"from":"host"}`)
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	h := Handler(Forwarder(base, http.DefaultTransport, "gaia-host", nil, nil))

	call := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}

	if rec := call(http.MethodGet, core.HealthzPath, ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Errorf("healthz %d %s", rec.Code, rec.Body)
	}
	select {
	case s := <-got:
		t.Fatalf("healthz was forwarded: %+v", s)
	default:
	}

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, core.NodesPath, ""},
		{http.MethodGet, core.VersionPath + "?x=1", ""},
		{http.MethodDelete, core.NodePath("node-b"), ""},
		{http.MethodPut, core.NodesPath, `{"a":1}`},
		{http.MethodPost, core.SessionsPath, `{"node":"node-b"}`},
		{http.MethodGet, core.SessionsPath + "/sess_x/logs?offset=3&follow=1", ""},
	} {
		rec := call(c.method, c.path, c.body)
		if rec.Code != http.StatusTeapot || rec.Body.String() != `{"from":"host"}` {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
			continue
		}
		s := <-got
		p, q, _ := strings.Cut(c.path, "?")
		if s.method != c.method || s.path != p || s.query != q || s.body != c.body {
			t.Errorf("forwarded %+v for %+v", s, c)
		}
	}

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, core.NodesPath},
		{http.MethodPost, core.HeartbeatPath},
		{http.MethodGet, core.HeartbeatPath},
		{http.MethodPost, core.SessionReportsPath},
		{http.MethodGet, "/"},
		{http.MethodGet, "/api/v2/nodes"},
	} {
		if rec := call(c.method, c.path, "{}"); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c.method, c.path, rec.Code)
		}
	}
	select {
	case s := <-got:
		t.Errorf("forwarded %+v", s)
	default:
	}
}

func TestForwarderHostUnreachable(t *testing.T) {
	down := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("no route to host") })
	base, _ := url.Parse("https://gaia-host")
	var logs bytes.Buffer
	ready := &Link{}
	ready.Set(PhaseReady)
	h := Handler(Forwarder(base, down, "gaia-host", ready, logger(&logs)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, core.NodesPath, nil))
	var e core.ErrorResponse
	if rec.Code != http.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &e) != nil ||
		e.Error.Code != core.CodeUnavailable || !strings.Contains(e.Error.Message, "gaia-host is unreachable") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "forwarding to the host failed") {
		t.Errorf("not logged: %s", logs.String())
	}
}

// forward sends one GET for the node list through h and decodes an error
// answer, timing it.
func forward(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, *core.Error, time.Duration) {
	t.Helper()
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, core.NodesPath, nil))
	took := time.Since(start)
	var e core.ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return rec, e.Error, took
}

func TestForwarderRecovering(t *testing.T) {
	// A host that takes the request and never answers: the stalled tailnet
	// path of fix-0004.
	release := make(chan struct{})
	defer close(release)
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	base, _ := url.Parse(stalled.URL)
	link := &Link{}
	link.Set(PhaseConfirming)
	link.Failed(errors.New("context deadline exceeded"))
	h := Handler(Forwarder(base, http.DefaultTransport, "gaia-host", link, nil))

	rec, e, took := forward(t, h)
	if took > RecoveryWait+time.Second {
		t.Errorf("held the caller %s, want at most about %s", took, RecoveryWait)
	}
	if rec.Code != http.StatusServiceUnavailable || e == nil || e.Code != core.CodeUnavailable {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{
		"re-establishing its connection to the host gaia-host",
		"waiting for the host to answer its first heartbeat",
		"did not answer within 3s",
		"try again in a few seconds",
	} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message lacks %q: %s", want, e.Message)
		}
	}
	if e.Details["link"] != string(PhaseConfirming) || e.Details["host"] != "gaia-host" {
		t.Errorf("details %v", e.Details)
	}

	// Not joined yet: the dial's own error, said with the stage, at once.
	joining := &Link{}
	joining.Set(PhaseJoining)
	notJoined := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("this node has not joined the tailnet yet")
	})
	rec, e, took = forward(t, Handler(Forwarder(base, notJoined, "gaia-host", joining, nil)))
	if rec.Code != http.StatusServiceUnavailable || e == nil || took > time.Second ||
		!strings.Contains(e.Message, "re-establishing its connection to the host gaia-host (joining the tailnet") ||
		!strings.Contains(e.Message, "has not joined the tailnet yet") {
		t.Errorf("%s: %d %s", took, rec.Code, rec.Body)
	}
}

func TestForwarderRecoveringAnswered(t *testing.T) {
	// Not ready, but the host answers: the answer is passed on, and a body
	// that streams past RecoveryWait is not cut off once headers arrived.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		time.Sleep(RecoveryWait + 500*time.Millisecond)
		_, _ = io.WriteString(w, "b")
	}))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	link := &Link{}
	link.Set(PhaseRegistering)
	rec, _, _ := forward(t, Handler(Forwarder(base, http.DefaultTransport, "gaia-host", link, nil)))
	if rec.Code != http.StatusOK || rec.Body.String() != "ab" {
		t.Errorf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestForwarderReadyUnbounded(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(RecoveryWait + 500*time.Millisecond)
		_, _ = io.WriteString(w, `{"nodes":[]}`)
	}))
	defer slow.Close()
	base, _ := url.Parse(slow.URL)
	link := &Link{}
	link.Set(PhaseReady)
	rec, _, _ := forward(t, Handler(Forwarder(base, http.DefaultTransport, "gaia-host", link, nil)))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"nodes":[]}` {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// beatHost records heartbeat times and fails them on cue.
type beatHost struct {
	mu       sync.Mutex
	register time.Time
	beats    []time.Time
	failBeat bool
}

func (b *beatHost) Register(_ context.Context, req core.RegisterNodeRequest) (core.RegisterNodeResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.register = time.Now()
	// An interval far longer than the test: only the confirming heartbeat
	// comes in time.
	return core.RegisterNodeResponse{Node: core.Node{ID: id(1), Name: req.Name}, HeartbeatIntervalMS: 60_000}, nil
}

func (b *beatHost) Heartbeat(context.Context, core.HeartbeatRequest) (core.HeartbeatResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.beats = append(b.beats, time.Now())
	if b.failBeat {
		b.failBeat = false
		return core.HeartbeatResponse{}, errDown
	}
	return core.HeartbeatResponse{}, nil
}

func TestRunLinkPhases(t *testing.T) {
	b := &beatHost{failBeat: true}
	var phases []Phase // under link.mu
	link := &Link{}
	link.onSet = func(p Phase) { phases = append(phases, p) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunConfig{Host: b, Request: Request("n", "v1"), ConfirmAfter: 50 * time.Millisecond,
			MinBackoff: time.Millisecond, Link: link})
	}()
	waitFor(t, "the link to be ready", link.Ready)
	cancel()
	<-done

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.beats) != 2 {
		t.Fatalf("%d heartbeats, want 2 (one failed, one confirming)", len(b.beats))
	}
	// The first heartbeat after the second registration came after
	// ConfirmAfter, not the 60 s interval.
	if gap := b.beats[1].Sub(b.register); gap < 50*time.Millisecond || gap > 2*time.Second {
		t.Errorf("confirming heartbeat %s after registration", gap)
	}
	// A failed heartbeat goes back to registering; an answered one is ready.
	want := []Phase{PhaseRegistering, PhaseConfirming, PhaseRegistering, PhaseConfirming, PhaseReady}
	if s := link.State(); !slices.Equal(phases, want) || s.LastErr != nil {
		t.Errorf("phases %v (last error %v), want %v and no error", phases, s.LastErr, want)
	}
}

func logger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVerifyHost(t *testing.T) {
	self := makeCert(t, nil, []string{"100.64.0.1"}, nil)
	if err := verifyHost([]*x509.Certificate{self}, "gaia-host.tail.ts.net", "100.64.0.1"); err != nil {
		t.Errorf("self-signed naming the address: %v", err)
	}
	if err := verifyHost([]*x509.Certificate{self}, "", "100.64.0.1"); err != nil {
		t.Errorf("self-signed, no server name: %v", err)
	}
	if err := verifyHost([]*x509.Certificate{self}, "gaia-host.tail.ts.net", "100.64.0.2"); err == nil {
		t.Error("self-signed certificate for another address accepted")
	}
	caKey, ca := makeCA(t)
	issued := makeCert(t, []string{"gaia-host.tail.ts.net"}, []string{"100.64.0.1"}, &signer{ca, caKey})
	if err := verifyHost([]*x509.Certificate{issued, ca}, "gaia-host.tail.ts.net", "100.64.0.1"); err == nil {
		t.Error("a certificate from an untrusted CA accepted")
	}
	if err := verifyHost(nil, "x", "100.64.0.1"); err == nil {
		t.Error("no certificate accepted")
	}
}

type signer struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func makeCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return key, c
}

func makeCert(t *testing.T, dns, ips []string, by *signer) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "gaia"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: dns}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	parent, signKey := tmpl, key
	if by != nil {
		parent, signKey = by.cert, by.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func TestMachineName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Build-Box.local", "build-box"},
		{"laptop", "laptop"},
		{"Adam's_MacBook", "adam-s-macbook"},
		{"-x-", "x"},
		{"...", ""},
		{strings.Repeat("a", 70), strings.Repeat("a", 63)},
		{strings.Repeat("a", 62) + "_b", strings.Repeat("a", 62)},
	} {
		in, want := c.in, c.want
		got := MachineName(in)
		if got != want {
			t.Errorf("MachineName(%q) = %q, want %q", in, got, want)
		}
		if got != "" && core.ValidNodeName(got) != nil {
			t.Errorf("MachineName(%q) = %q is not a valid node name", in, got)
		}
	}
}

func TestRequest(t *testing.T) {
	r := Request("node-a", "v2")
	if r.Name != "node-a" || r.TailnetIdentity != "" || r.APIVersion != core.APIVersion || r.GaiaVersion != "v2" {
		t.Errorf("Request = %+v", r)
	}
}

// TestClientReportSession: a report reaches a real host as the calling
// peer's; before that peer registers, the host refuses it as retryable.
func TestClientReportSession(t *testing.T) {
	_, c := startHost(t, host.Options{})
	ctx := context.Background()
	r := core.SessionReport{SessionID: core.NewSessionID([16]byte{1}), Seq: 1, State: core.SessionRunning}
	err := c.ReportSession(ctx, r)
	if !errors.Is(err, core.NewError(core.CodeUnavailable, "")) || finalRefusal(err) {
		t.Fatalf("report before registering: %v, want a retryable unavailable", err)
	}
	if _, err := c.Register(ctx, Request("node-a", "v1.2.3")); err != nil {
		t.Fatal(err)
	}
	// Registered, but the host has no such session: a final refusal.
	if err := c.ReportSession(ctx, r); !finalRefusal(err) {
		t.Fatalf("report of an unknown session: %v, want a final refusal", err)
	}
}
