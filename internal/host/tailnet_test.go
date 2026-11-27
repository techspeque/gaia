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

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/logging"
	gnet "github.com/techspeque/gaia/internal/net"
)

func TestParseAllowlist(t *testing.T) {
	good := []string{"user@example.com", "tag:gaia", "tag:ci-runner", "node:build-box", "First.Last+x@example.org"}
	if a, err := ParseAllowlist(good); err != nil || a.Len() != len(good) {
		t.Fatalf("ParseAllowlist(%q) = %d entries, %v", good, a.Len(), err)
	}
	for _, bad := range []string{
		"", " ", "user", "tag:", "node:", "tag:a b", "node:a.b", "tag:x_y",
		"@example.com", "user@", "a@b@c", "user @example.com", "tag:gaia\n", "group:eng",
	} {
		if _, err := ParseAllowlist([]string{bad}); err == nil {
			t.Errorf("ParseAllowlist(%q) accepted", bad)
		}
	}
}

func TestReadAllowlist(t *testing.T) {
	in := "# Gaia host allowlist\n\nuser@example.com\n  tag:gaia   # CI machines\n\t\nnode:laptop\n"
	a, err := ReadAllowlist(strings.NewReader(in), "allow.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a.Len() != 3 {
		t.Errorf("%d entries, want 3", a.Len())
	}
	_, err = ReadAllowlist(strings.NewReader("user@example.com\nbogus\n"), "allow.txt")
	if err == nil || !strings.Contains(err.Error(), "allow.txt:2") {
		t.Errorf("error %v, want one naming allow.txt:2", err)
	}
}

func TestAllowlistAllows(t *testing.T) {
	a, err := ParseAllowlist([]string{"User@Example.com", "tag:gaia", "node:Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p    gnet.Peer
		want bool
	}{
		{gnet.Peer{LoginName: "user@example.com", NodeName: "x"}, true},
		{gnet.Peer{LoginName: "USER@EXAMPLE.COM"}, true},
		{gnet.Peer{NodeName: "laptop"}, true},
		{gnet.Peer{Tags: []string{"tag:other", "tag:gaia"}}, true},
		{gnet.Peer{Tags: []string{"tag:GAIA"}}, false},
		{gnet.Peer{LoginName: "other@example.com", NodeName: "desktop", Tags: []string{"tag:x"}}, false},
		{gnet.Peer{LoginName: "user@example.com.evil"}, false},
		{gnet.Peer{}, false},
	} {
		if got := a.Allows(c.p); got != c.want {
			t.Errorf("Allows(%+v) = %v, want %v", c.p, got, c.want)
		}
	}
	if (Allowlist{}).Allows(gnet.Peer{LoginName: "user@example.com"}) {
		t.Error("the empty allowlist admitted a peer")
	}
	m := a.Merge(Allowlist{})
	if m.Len() != 3 || !m.Allows(gnet.Peer{NodeName: "laptop"}) {
		t.Error("Merge lost entries")
	}
}

// fakeWhoIs maps remote addresses to peers.
type fakeWhoIs map[string]gnet.Peer

func (f fakeWhoIs) WhoIs(_ context.Context, addr string) (gnet.Peer, error) {
	p, ok := f[addr]
	if !ok {
		return gnet.Peer{}, errors.New("peer not found")
	}
	return p, nil
}

func tailnetServer(t *testing.T, log *bytes.Buffer, allow ...string) http.Handler {
	t.Helper()
	s, _ := newServer(t)
	a, err := ParseAllowlist(allow)
	if err != nil {
		t.Fatal(err)
	}
	s.opts.Allowlist = a
	s.log = logging.New(log, nil)
	return s.TailnetHandler(fakeWhoIs{
		"100.64.0.2:4000": {LoginName: "user@example.com", NodeName: "laptop"},
		"100.64.0.3:4000": {LoginName: "other@example.com", NodeName: "desktop"},
	})
}

func doFrom(h http.Handler, remote, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTailnetHandlerAllowsListedPeer(t *testing.T) {
	var log bytes.Buffer
	h := tailnetServer(t, &log, "user@example.com")
	rec := doFrom(h, "100.64.0.2:4000", core.VersionPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var v core.VersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.APIVersion != core.APIVersion {
		t.Errorf("body %s", rec.Body)
	}
	if rec := doFrom(h, "100.64.0.2:4000", core.NodesPath); rec.Code != http.StatusOK {
		t.Errorf("nodes status %d", rec.Code)
	}
	if strings.Contains(log.String(), "refused") {
		t.Errorf("allowed request logged a refusal:\n%s", log.String())
	}
}

func TestTailnetHandlerRefuses(t *testing.T) {
	for _, c := range []struct {
		name, remote, logWant string
	}{
		{"not allowlisted", "100.64.0.3:4000", `"login":"other@example.com"`},
		{"whois fails", "100.64.0.9:4000", `"err":"peer not found"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var log bytes.Buffer
			h := tailnetServer(t, &log, "user@example.com")
			rec := doFrom(h, c.remote, core.VersionPath)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", rec.Code)
			}
			var e core.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error == nil ||
				e.Error.Code != core.CodeForbidden || e.Error.Message != "permission denied" {
				t.Errorf("body %s", rec.Body)
			}
			if strings.Contains(rec.Body.String(), "other@example.com") || strings.Contains(rec.Body.String(), "not found") {
				t.Errorf("403 body reveals detail: %s", rec.Body)
			}
			out := log.String()
			if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, `"remote_addr":"`+c.remote+`"`) ||
				!strings.Contains(out, c.logWant) {
				t.Errorf("log lacks a warn line with remote addr and %s:\n%s", c.logWant, out)
			}
		})
	}
}

func TestTailnetHandlerDefaultDeny(t *testing.T) {
	var log bytes.Buffer
	h := tailnetServer(t, &log) // empty allowlist
	if rec := doFrom(h, "100.64.0.2:4000", core.VersionPath); rec.Code != http.StatusForbidden {
		t.Errorf("status %d with an empty allowlist, want 403", rec.Code)
	}
}

func TestTailnetHandlerHasNoHealthz(t *testing.T) {
	var log bytes.Buffer
	h := tailnetServer(t, &log, "user@example.com")
	if rec := doFrom(h, "100.64.0.2:4000", core.HealthzPath); rec.Code != http.StatusNotFound {
		t.Errorf("allowed peer: /healthz status %d, want 404", rec.Code)
	}
	if rec := doFrom(h, "100.64.0.3:4000", core.HealthzPath); rec.Code != http.StatusForbidden {
		t.Errorf("refused peer: /healthz status %d, want 403", rec.Code)
	}
}
