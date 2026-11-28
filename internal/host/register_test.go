package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// Two tailnet peers, both allowlisted by tag.
const (
	addrA = "100.64.0.10:4000"
	addrB = "100.64.0.11:4000"
)

func registryHandler(t *testing.T) (*Server, http.Handler, *clock) {
	t.Helper()
	s, c := newServer(t)
	a, err := ParseAllowlist([]string{"tag:gaia"})
	if err != nil {
		t.Fatal(err)
	}
	s.opts.Allowlist = a
	h := s.TailnetHandler(fakeWhoIs{
		addrA: {NodeName: "node-a", Tags: []string{"tag:gaia"}, StableID: "nSTABLEA"},
		addrB: {NodeName: "node-b", Tags: []string{"tag:gaia"}, StableID: "nSTABLEB"},
	})
	return s, h, c
}

func send(t *testing.T, h http.Handler, remote, method, path string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func registerReq(name string) core.RegisterNodeRequest {
	return core.RegisterNodeRequest{Name: name, OS: "linux", Arch: "amd64", GaiaVersion: "v1.2.3", APIVersion: core.APIVersion}
}

func errorOf(t *testing.T, body []byte) *core.Error {
	t.Helper()
	var e core.ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil || e.Error == nil {
		t.Fatalf("not an error body: %s", body)
	}
	return e.Error
}

func TestRegisterOverTailnetTakesIdentityFromWhoIs(t *testing.T) {
	s, h, _ := registryHandler(t)
	req := registerReq("node-a")
	req.TailnetIdentity = "nSTABLEB" // claimed; must be ignored
	status, body := send(t, h, addrA, http.MethodPost, core.NodesPath, req)
	if status != http.StatusOK {
		t.Fatalf("register: %d %s", status, body)
	}
	var res core.RegisterNodeResponse
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Node.TailnetIdentity != "nSTABLEA" || res.Node.Name != "node-a" || res.Node.State != core.NodeOnline {
		t.Errorf("registered %+v", res.Node)
	}
	if res.HeartbeatIntervalMS != s.HeartbeatInterval().Milliseconds() || res.HeartbeatIntervalMS == 0 {
		t.Errorf("heartbeat_interval_ms %d", res.HeartbeatIntervalMS)
	}

	// Registering again from the same peer updates the row; B cannot take
	// A's name.
	req.GaiaVersion = "v1.2.4"
	status, body = send(t, h, addrA, http.MethodPost, core.NodesPath, req)
	var again core.RegisterNodeResponse
	if status != http.StatusOK || json.Unmarshal(body, &again) != nil || again.Node.ID != res.Node.ID || again.Node.GaiaVersion != "v1.2.4" {
		t.Errorf("re-register: %d %s", status, body)
	}
	status, body = send(t, h, addrB, http.MethodPost, core.NodesPath, registerReq("node-a"))
	if status != http.StatusConflict || errorOf(t, body).Code != core.CodeConflict {
		t.Errorf("name takeover: %d %s", status, body)
	}
	if l := listNodes(t, s); len(l) != 1 {
		t.Errorf("nodes %+v", l)
	}
}

// TestRegisterSuffixedTailnetName is a node that asked for gaia-box and was
// given gaia-box-1 by the control server: the registry keeps the asked name,
// records the assigned one from WhoIs (never the body's), and a node:
// allowlist entry matches only the assigned one.
func TestRegisterSuffixedTailnetName(t *testing.T) {
	s, _ := newServer(t)
	a, err := ParseAllowlist([]string{"node:gaia-box-1"})
	if err != nil {
		t.Fatal(err)
	}
	s.opts.Allowlist = a
	h := s.TailnetHandler(fakeWhoIs{
		addrA: {NodeName: "gaia-box-1", StableID: "nSTABLEA"},
		addrB: {NodeName: "gaia-box", StableID: "nSTABLEB"},
	})
	req := registerReq("gaia-box")
	req.TailnetName = "claimed" // must be ignored
	status, body := send(t, h, addrA, http.MethodPost, core.NodesPath, req)
	if status != http.StatusOK {
		t.Fatalf("register: %d %s", status, body)
	}
	var res core.RegisterNodeResponse
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Node.Name != "gaia-box" || res.Node.TailnetName != "gaia-box-1" {
		t.Errorf("registered %+v", res.Node)
	}
	l := listNodes(t, s)
	if len(l) != 1 || l[0].TailnetName != "gaia-box-1" {
		t.Errorf("nodes %+v", l)
	}
	// A peer whose assigned name is the registry name, gaia-box, is not
	// node:gaia-box-1.
	if status, body := send(t, h, addrB, http.MethodGet, core.NodesPath, nil); status != http.StatusForbidden {
		t.Errorf("peer gaia-box: %d %s, want 403", status, body)
	}
}

func TestRegisterRejectsBadRequests(t *testing.T) {
	_, h, _ := registryHandler(t)
	for _, c := range []struct {
		name string
		body any
		code core.ErrorCode
	}{
		{"bad name", registerReq("Node_A"), core.CodeInvalidArgument},
		{"empty name", registerReq(""), core.CodeInvalidArgument},
		{"not JSON", "nope", core.CodeInvalidArgument},
	} {
		status, body := send(t, h, addrA, http.MethodPost, core.NodesPath, c.body)
		if status != http.StatusBadRequest || errorOf(t, body).Code != c.code {
			t.Errorf("%s: %d %s", c.name, status, body)
		}
	}
	// A peer with no stable id cannot register.
	s, _ := newServer(t)
	s.opts.Allowlist, _ = ParseAllowlist([]string{"tag:gaia"})
	h2 := s.TailnetHandler(fakeWhoIs{addrA: {NodeName: "x", Tags: []string{"tag:gaia"}}})
	if status, _ := send(t, h2, addrA, http.MethodPost, core.NodesPath, registerReq("x")); status != http.StatusForbidden {
		t.Errorf("no stable id: %d", status)
	}
}

func TestHeartbeatOverTailnet(t *testing.T) {
	s, h, c := registryHandler(t)
	_, body := send(t, h, addrA, http.MethodPost, core.NodesPath, registerReq("node-a"))
	var reg core.RegisterNodeResponse
	if err := json.Unmarshal(body, &reg); err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	if l := listNodes(t, s); l[0].State != core.NodeOffline {
		t.Fatalf("not offline after a minute: %+v", l[0])
	}

	// Another peer may not heartbeat for A.
	status, body := send(t, h, addrB, http.MethodPost, core.HeartbeatPath, core.HeartbeatRequest{ID: reg.Node.ID})
	if status != http.StatusForbidden || errorOf(t, body).Code != core.CodeForbidden {
		t.Errorf("heartbeat from B: %d %s", status, body)
	}
	if l := listNodes(t, s); l[0].State != core.NodeOffline {
		t.Errorf("B's heartbeat counted for A: %+v", l[0])
	}

	status, body = send(t, h, addrA, http.MethodPost, core.HeartbeatPath, core.HeartbeatRequest{ID: reg.Node.ID})
	var hb core.HeartbeatResponse
	if status != http.StatusOK || json.Unmarshal(body, &hb) != nil || hb.Node.State != core.NodeOnline || !hb.Node.LastSeen.Equal(c.now()) {
		t.Errorf("heartbeat from A: %d %s", status, body)
	}

	var unknown [16]byte
	status, body = send(t, h, addrA, http.MethodPost, core.HeartbeatPath, core.HeartbeatRequest{ID: core.NewNodeID(unknown)})
	if status != http.StatusNotFound || errorOf(t, body).Code != core.CodeNotFound {
		t.Errorf("unknown id: %d %s", status, body)
	}
	status, _ = send(t, h, addrA, http.MethodPost, core.HeartbeatPath, map[string]string{})
	if status != http.StatusBadRequest {
		t.Errorf("no id: %d", status)
	}
}

func TestVersionNegotiation(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	s.opts.APIVersion = 3
	for _, c := range []struct {
		v  int
		ok bool
	}{{3, true}, {2, true}, {1, false}, {0, false}, {4, false}} {
		req := registerReq("n")
		req.TailnetIdentity, req.APIVersion = "id", c.v
		_, err := s.Register(ctx, req)
		if c.ok {
			if err != nil {
				t.Errorf("v%d: %v", c.v, err)
			}
			continue
		}
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Code != core.CodeVersionMismatch {
			t.Fatalf("v%d: %v, want version_mismatch", c.v, err)
		}
		if ce.Details["node_api_version"] == "" || ce.Details["host_api_version"] != "3" || ce.Details["host_gaia_version"] != "v1.2.3" {
			t.Errorf("v%d details %v", c.v, ce.Details)
		}
		want := "install gaia v1.2.3 on the node"
		if c.v > 3 {
			want = "upgrade gaia on the host"
		}
		if !strings.Contains(ce.Message, want) {
			t.Errorf("v%d message %q lacks %q", c.v, ce.Message, want)
		}
	}
	// Two behind at the default host version too: never accepted.
	s2, _ := newServer(t)
	req := registerReq("n")
	req.TailnetIdentity, req.APIVersion = "id", core.APIVersion-2
	if _, err := s2.Register(ctx, req); !errors.Is(err, core.NewError(core.CodeVersionMismatch, "")) {
		t.Errorf("two behind: %v", err)
	}
	// The wire carries it as 409 with the typed error.
	_, h, _ := registryHandler(t)
	req = registerReq("node-a")
	req.APIVersion = core.APIVersion + 1
	status, body := send(t, h, addrA, http.MethodPost, core.NodesPath, req)
	if status != http.StatusConflict || errorOf(t, body).Code != core.CodeVersionMismatch {
		t.Errorf("over the wire: %d %s", status, body)
	}
}

func TestRemoveNode(t *testing.T) {
	s, h, _ := registryHandler(t)
	send(t, h, addrA, http.MethodPost, core.NodesPath, registerReq("node-a"))
	send(t, h, addrB, http.MethodPost, core.NodesPath, registerReq("node-b"))

	// On the socket.
	res, body := do(t, s.Handler(), http.MethodDelete, core.NodePath("node-a"))
	var rm core.RemoveNodeResponse
	if res.StatusCode != http.StatusOK || json.Unmarshal(body, &rm) != nil || rm.Node.Name != "node-a" {
		t.Fatalf("remove: %d %s", res.StatusCode, body)
	}
	if l := listNodes(t, s); len(l) != 1 || l[0].Name != "node-b" {
		t.Errorf("after remove %+v", l)
	}
	res, body = do(t, s.Handler(), http.MethodDelete, core.NodePath("node-a"))
	if res.StatusCode != http.StatusNotFound || !strings.Contains(errorOf(t, body).Message, `"node-a"`) {
		t.Errorf("remove again: %d %s", res.StatusCode, body)
	}
	res, _ = do(t, s.Handler(), http.MethodDelete, core.NodePath("Bad_Name"))
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad name: %d", res.StatusCode)
	}

	// Over the tailnet, as a forwarded CLI request.
	if status, body := send(t, h, addrA, http.MethodDelete, core.NodePath("node-b"), nil); status != http.StatusOK {
		t.Errorf("remove over the tailnet: %d %s", status, body)
	}
	if l := listNodes(t, s); len(l) != 0 {
		t.Errorf("after remove %+v", l)
	}
}

func TestStatusFor(t *testing.T) {
	want := map[core.ErrorCode]int{
		core.CodeInvalidArgument: 400, core.CodeUnauthorized: 401, core.CodeForbidden: 403,
		core.CodeNotFound: 404, core.CodeConflict: 409, core.CodeVersionMismatch: 409,
		core.CodeUnavailable: 503, core.CodeInternal: 500,
	}
	for _, c := range core.ErrorCodes {
		if got := statusFor(c); got != want[c] {
			t.Errorf("%s: %d, want %d", c, got, want[c])
		}
	}
}
