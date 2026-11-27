//go:build linux || darwin

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/sock"
)

// shortDir returns a runtime directory path short enough for a unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "gc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return filepath.Join(d, "gaia")
}

// fakeDaemon serves h on a real local socket and returns its runtime dir.
func fakeDaemon(t *testing.T, h http.Handler) string {
	t.Helper()
	dir := shortDir(t)
	l, err := sock.Listen(dir, sock.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close(); _ = l.Close() })
	return dir
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// daemonAt answers like a host speaking api, with nodes in its registry.
func daemonAt(api int, gaiaVersion string, nodes []core.Node) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(core.VersionPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.VersionResponse{APIVersion: api, GaiaVersion: gaiaVersion})
	})
	mux.HandleFunc(core.NodesPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.ListNodesResponse{Nodes: nodes})
	})
	return mux
}

// runNodes runs `gaia nodes args` with client c and returns the exit code,
// stdout and stderr.
func runNodes(c *Client, now time.Time, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Nodes(args, Env{Stdout: &out, Stderr: &errOut, Client: c, Now: func() time.Time { return now }})
	return code, out.String(), errOut.String()
}

func oneLine(t *testing.T, s string) string {
	t.Helper()
	if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") || !strings.HasPrefix(s, "gaia: ") {
		t.Fatalf("stderr %q is not one line prefixed \"gaia: \"", s)
	}
	return s
}

func TestConnectMatchingVersion(t *testing.T) {
	dir := fakeDaemon(t, daemonAt(core.APIVersion, "1.0.0", nil))
	c := &Client{Dir: dir, Version: "1.0.0"}
	v, err := c.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v.APIVersion != core.APIVersion || v.GaiaVersion != "1.0.0" {
		t.Errorf("version %+v", v)
	}
}

func TestCLIBehindDaemon(t *testing.T) {
	dir := fakeDaemon(t, daemonAt(core.APIVersion+1, "2.0.0", nil))
	c := &Client{Dir: dir, Version: "1.0.0"}
	code, out, errOut := runNodes(c, time.Now())
	if code != int(core.ExitVersionMismatch) {
		t.Fatalf("exit %d, want %d; stderr %q", code, core.ExitVersionMismatch, errOut)
	}
	if out != "" {
		t.Errorf("stdout %q, want nothing", out)
	}
	msg := oneLine(t, errOut)
	for _, want := range []string{"older than the daemon", "install gaia 2.0.0", "1.0.0", "API v2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestCLIAheadOfDaemon(t *testing.T) {
	dir := fakeDaemon(t, daemonAt(core.APIVersion, "1.0.0", nil))
	c := &Client{Dir: dir, Version: "2.0.0", APIVersion: core.APIVersion + 1}
	code, _, errOut := runNodes(c, time.Now())
	if code != int(core.ExitVersionMismatch) {
		t.Fatalf("exit %d, want %d; stderr %q", code, core.ExitVersionMismatch, errOut)
	}
	msg := oneLine(t, errOut)
	for _, want := range []string{"daemon (1.0.0, API v1) is older", "restart the daemon", "gaia host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestDaemonWithoutThisAPI(t *testing.T) {
	dir := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, core.ErrorResponse{Error: core.NewError(core.CodeNotFound, "no such endpoint")})
	}))
	code, _, errOut := runNodes(&Client{Dir: dir}, time.Now())
	if code != int(core.ExitVersionMismatch) {
		t.Fatalf("exit %d, want %d; stderr %q", code, core.ExitVersionMismatch, errOut)
	}
	if msg := oneLine(t, errOut); !strings.Contains(msg, "does not serve API v1") {
		t.Errorf("message %q", msg)
	}
}

func TestDaemonErrorSurfaced(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(core.VersionPath, daemonAt(core.APIVersion, "1.0.0", nil))
	mux.HandleFunc(core.NodesPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, core.ErrorResponse{Error: core.NewError(core.CodeInternal, "internal error")})
	})
	code, out, errOut := runNodes(&Client{Dir: fakeDaemon(t, mux)}, time.Now())
	if code != int(core.ExitInternal) {
		t.Fatalf("exit %d, want %d", code, core.ExitInternal)
	}
	if out != "" {
		t.Errorf("stdout %q", out)
	}
	if msg := oneLine(t, errOut); !strings.Contains(msg, "internal: internal error") {
		t.Errorf("message %q", msg)
	}
}

func TestNoDaemon(t *testing.T) {
	missing := shortDir(t)
	empty := shortDir(t)
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{missing, empty} {
		start := time.Now()
		code, out, errOut := runNodes(&Client{Dir: dir}, time.Now())
		elapsed := time.Since(start)
		if code != int(core.ExitNoDaemon) {
			t.Fatalf("%s: exit %d, want %d; stderr %q", dir, code, core.ExitNoDaemon, errOut)
		}
		if elapsed >= 200*time.Millisecond {
			t.Errorf("%s: took %v, want under 200ms", dir, elapsed)
		}
		if out != "" {
			t.Errorf("stdout %q", out)
		}
		msg := oneLine(t, errOut)
		for _, want := range []string{"no Gaia daemon is running", "`gaia host`", "`gaia node", sock.Path(dir)} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q lacks %q", msg, want)
			}
		}
	}
}

func TestUnsafeRuntimeDirIsNotNoDaemon(t *testing.T) {
	dir := fakeDaemon(t, daemonAt(core.APIVersion, "1.0.0", nil))
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	code, _, errOut := runNodes(&Client{Dir: dir}, time.Now())
	if code != int(core.ExitInternal) {
		t.Fatalf("exit %d, want %d; stderr %q", code, core.ExitInternal, errOut)
	}
	if msg := oneLine(t, errOut); !strings.Contains(msg, "accessible by group or others") {
		t.Errorf("message %q", msg)
	}
}
