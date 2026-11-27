package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newServer(t *testing.T) (*Server, *clock) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := &clock{t: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	return New(st, Options{Version: "v1.2.3", HeartbeatInterval: 10 * time.Second, MissedHeartbeats: 3, Now: c.now}), c
}

func do(t *testing.T, h http.Handler, method, path string) (*http.Response, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func TestHealthzRevealsNothing(t *testing.T) {
	s, _ := newServer(t)
	res, body := do(t, s.Handler(), http.MethodGet, core.HealthzPath)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if got := strings.TrimSpace(string(body)); got != `{"status":"ok"}` {
		t.Errorf("body %s, want exactly {\"status\":\"ok\"}", got)
	}
	host, _ := os.Hostname()
	wd, _ := os.Getwd()
	for _, leak := range []string{host, "v1.2.3", wd, "/"} {
		if leak != "" && strings.Contains(string(body), leak) {
			t.Errorf("healthz body %s contains %q", body, leak)
		}
	}
}

func TestVersion(t *testing.T) {
	s, _ := newServer(t)
	res, body := do(t, s.Handler(), http.MethodGet, core.VersionPath)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var v core.VersionResponse
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.APIVersion != core.APIVersion || v.GaiaVersion != "v1.2.3" {
		t.Errorf("version %+v", v)
	}
}

func listNodes(t *testing.T, s *Server) []core.Node {
	t.Helper()
	res, body := do(t, s.Handler(), http.MethodGet, core.NodesPath)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", res.StatusCode, body)
	}
	var l core.ListNodesResponse
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatal(err)
	}
	if l.Nodes == nil {
		t.Fatalf("nodes is null in %s", body)
	}
	return l.Nodes
}

func TestNodesStateFromHeartbeat(t *testing.T) {
	ctx := context.Background()
	s, c := newServer(t)
	if n := listNodes(t, s); len(n) != 0 {
		t.Fatalf("fresh registry lists %v", n)
	}
	req := core.RegisterNodeRequest{Name: "alpha", TailnetIdentity: "local", OS: "linux", Arch: "arm64",
		GaiaVersion: "v1.2.3", APIVersion: core.APIVersion}
	reg, err := s.Register(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if reg.State != core.NodeOnline {
		t.Errorf("registered state %s", reg.State)
	}
	l := listNodes(t, s)
	if len(l) != 1 || l[0].ID != reg.ID || l[0].State != core.NodeOnline || l[0].Name != "alpha" {
		t.Fatalf("list %+v", l)
	}

	c.add(30 * time.Second) // exactly interval × missed: still online
	if l := listNodes(t, s); l[0].State != core.NodeOnline {
		t.Errorf("at the limit: %s", l[0].State)
	}
	c.add(time.Nanosecond)
	if l := listNodes(t, s); l[0].State != core.NodeOffline {
		t.Errorf("past the limit: %s", l[0].State)
	}
	hb, err := s.Heartbeat(ctx, core.HeartbeatRequest{ID: reg.ID})
	if err != nil || hb.State != core.NodeOnline {
		t.Fatalf("heartbeat %+v, %v", hb, err)
	}
	if l := listNodes(t, s); l[0].State != core.NodeOnline || !l[0].LastSeen.Equal(c.now()) {
		t.Errorf("after heartbeat: %+v", l[0])
	}
}

func TestReregisterKeepsID(t *testing.T) {
	ctx := context.Background()
	s, c := newServer(t)
	req := core.RegisterNodeRequest{Name: "alpha", TailnetIdentity: "local", OS: "linux", Arch: "arm64",
		GaiaVersion: "v1", APIVersion: core.APIVersion}
	first, err := s.Register(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour)
	req.Name, req.GaiaVersion = "alpha2", "v2"
	second, err := s.Register(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Name != "alpha2" || second.GaiaVersion != "v2" || second.State != core.NodeOnline {
		t.Errorf("re-register %+v, first %+v", second, first)
	}
	if l := listNodes(t, s); len(l) != 1 || l[0].Name != "alpha2" {
		t.Errorf("list %+v", l)
	}
}

func TestRegisterAndHeartbeatErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	_, err := s.Register(ctx, core.RegisterNodeRequest{Name: "x"})
	if !errors.Is(err, core.NewError(core.CodeInvalidArgument, "")) {
		t.Errorf("register without identity: %v", err)
	}
	var b [16]byte
	_, err = s.Heartbeat(ctx, core.HeartbeatRequest{ID: core.NewNodeID(b)})
	if !errors.Is(err, core.NewError(core.CodeNotFound, "")) {
		t.Errorf("heartbeat unknown: %v", err)
	}
}

func wantErrorBody(t *testing.T, body []byte, code core.ErrorCode) {
	t.Helper()
	var e core.ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil || e.Error == nil || e.Error.Code != code {
		t.Errorf("body %s, want error code %s", body, code)
	}
}

func TestMethodAndPathErrors(t *testing.T) {
	s, _ := newServer(t)
	for _, p := range []string{core.HealthzPath, core.VersionPath, core.NodesPath} {
		res, body := do(t, s.Handler(), http.MethodPost, p)
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", p, res.StatusCode)
		}
		wantErrorBody(t, body, core.CodeInvalidArgument)
	}
	for _, p := range []string{"/", "/api/v1/nope", "/api/v2/nodes", core.NodesPath + "/x"} {
		res, body := do(t, s.Handler(), http.MethodGet, p)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, res.StatusCode)
		}
		wantErrorBody(t, body, core.CodeNotFound)
	}
}

func TestStoreErrorIsGeneric(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := New(st, Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	_ = st.Close()
	res, body := do(t, s.Handler(), http.MethodGet, core.NodesPath)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d", res.StatusCode)
	}
	wantErrorBody(t, body, core.CodeInternal)
	if strings.Contains(string(body), "closed") || strings.Contains(string(body), dir) {
		t.Errorf("error body leaks detail: %s", body)
	}
	if !strings.Contains(logs.String(), "request failed") {
		t.Errorf("failure not logged: %s", logs.String())
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	s, _ := newServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	res, err := http.Get("http://" + l.Addr().String() + core.HealthzPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := http.Get("http://" + l.Addr().String() + core.HealthzPath); err == nil {
		t.Error("still serving after shutdown")
	}
}
