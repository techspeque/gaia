//go:build linux || darwin

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

const (
	sessionsFixture      = "../../../testdata/contract/gaia/api-v1/sessions.json"
	sessionsEmptyFixture = "../../../testdata/contract/gaia/api-v1/sessions-empty.json"
)

// testNode is the node of the sessions the fake daemons answer with; a
// zero NodeID does not encode.
var testNode = core.NewNodeID([16]byte{7})

// sessionDaemon is a fake host for the session commands: version, the
// session routes given, and a record of the requests it was sent.
type sessionDaemon struct {
	mu   sync.Mutex
	reqs []string
	mux  *http.ServeMux
}

func newSessionDaemon() *sessionDaemon {
	d := &sessionDaemon{mux: http.NewServeMux()}
	d.mux.HandleFunc(core.VersionPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.VersionResponse{APIVersion: core.APIVersion, GaiaVersion: "1.0.0"})
	})
	return d
}

func (d *sessionDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != core.VersionPath {
		d.mu.Lock()
		d.reqs = append(d.reqs, r.Method+" "+r.URL.RequestURI())
		d.mu.Unlock()
	}
	d.mux.ServeHTTP(w, r)
}

func (d *sessionDaemon) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.reqs...)
}

func runCmd(cmd func([]string, Env) int, c *Client, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := cmd(args, Env{Stdout: &out, Stderr: &errOut, Client: c, Now: func() time.Time {
		return time.Date(2026, 11, 27, 14, 30, 0, 0, time.UTC)
	}})
	return code, out.String(), errOut.String()
}

func TestRunUsage(t *testing.T) {
	d := newSessionDaemon()
	c := &Client{Dir: fakeDaemon(t, d)}
	for _, args := range [][]string{
		{},
		{"--node", "n", "--detach", "--"},
		{"--node", "n", "--detach", "claude"},
		{"--node", "n", "--", "claude"},
		{"--detach", "--", "claude"},
		{"--node", "Bad_Name", "--detach", "--", "claude"},
		{"--node", "n", "--detach", "--", ""},
		{"--node", "n", "--detach", "--wait", "--", "claude"},
		{"--node", "n", "--detach", "stray", "--", "claude"},
	} {
		code, out, errOut := runCmd(Run, c, args...)
		if code != int(core.ExitUsage) || out != "" || !strings.HasPrefix(errOut, "gaia: gaia run: ") || !strings.Contains(errOut, "usage: gaia run") {
			t.Errorf("%q: exit %d %q %q", args, code, out, errOut)
		}
	}
	if r := d.requests(); len(r) != 0 {
		t.Errorf("usage errors sent %q", r)
	}
	if code, _, errOut := runCmd(Run, c, "--help"); code != 0 || !strings.Contains(errOut, "usage: gaia run") {
		t.Errorf("--help: %d %q", code, errOut)
	}
}

func TestRunStartsAndPrintsID(t *testing.T) {
	d := newSessionDaemon()
	id := core.NewSessionID([16]byte{1})
	var got core.StartSessionRequest
	d.mux.HandleFunc("POST "+core.SessionsPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, core.StartSessionResponse{Session: core.Session{ID: id, NodeID: testNode, State: core.SessionRunning}})
	})
	c := &Client{Dir: fakeDaemon(t, d)}
	code, out, errOut := runCmd(Run, c, "--node", "node-b", "--detach", "--profile", "p", "--", "claude", "-p", "--", "x y")
	if code != 0 || out != id.String()+"\n" {
		t.Fatalf("exit %d %q %q", code, out, errOut)
	}
	if got.Node != "node-b" || got.Mode != core.SessionHeadless || got.Profile != "p" ||
		strings.Join(got.Argv, "|") != "claude|-p|--|x y" || got.Repo != "" {
		t.Errorf("request %+v", got)
	}
}

func TestSessionExitCodes(t *testing.T) {
	for _, c := range []struct {
		reason string
		want   core.ExitCode
	}{
		{"node_unavailable", core.ExitNodeUnavailable},
		{"start_failed", core.ExitStartFailed},
		{"session_lost", core.ExitSessionLost},
		{"", core.ExitInternal},
	} {
		d := newSessionDaemon()
		d.mux.HandleFunc("POST "+core.SessionsPath, func(w http.ResponseWriter, _ *http.Request) {
			e := core.NewError(core.CodeUnavailable, "node \"node-b\" is offline")
			if c.reason != "" {
				e.Details = map[string]string{core.DetailReason: c.reason}
			}
			writeJSON(w, http.StatusServiceUnavailable, core.ErrorResponse{Error: e})
		})
		cl := &Client{Dir: fakeDaemon(t, d)}
		code, out, errOut := runCmd(Run, cl, "--node", "node-b", "--detach", "--", "claude")
		if code != int(c.want) || out != "" || !strings.Contains(oneLine(t, errOut), "offline") {
			t.Errorf("reason %q: exit %d %q %q, want %d", c.reason, code, out, errOut, c.want)
		}
	}
}

func TestLsJSONMatchesFixtures(t *testing.T) {
	for _, path := range []string{sessionsFixture, sessionsEmptyFixture} {
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var l core.ListSessionsResponse
		if err := json.Unmarshal(want, &l); err != nil {
			t.Fatal(err)
		}
		d := newSessionDaemon()
		d.mux.HandleFunc("GET "+core.SessionsPath, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, l)
		})
		c := &Client{Dir: fakeDaemon(t, d)}
		code, out, errOut := runCmd(Ls, c, "-o", "json")
		if code != 0 || out != string(want) {
			t.Errorf("%s: exit %d %q\n got: %s\nwant: %s", path, code, errOut, out, want)
		}
	}
}

func TestLsText(t *testing.T) {
	b, err := os.ReadFile(sessionsFixture)
	if err != nil {
		t.Fatal(err)
	}
	var l core.ListSessionsResponse
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	d := newSessionDaemon()
	d.mux.HandleFunc("GET "+core.SessionsPath, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, l) })
	c := &Client{Dir: fakeDaemon(t, d)}
	code, out, _ := runCmd(Ls, c)
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(rows) != len(l.Sessions)+1 || !strings.HasPrefix(rows[0], "ID ") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	first := strings.Fields(rows[1])
	if first[0] != l.Sessions[0].ID.String() || first[1] != "gaia-studio" || first[2] != "exited" || first[3] != "0" ||
		!strings.Contains(rows[1], `claude -p "Fix the failing test"`) {
		t.Errorf("row %q", rows[1])
	}
	for _, args := range [][]string{{"-o", "yaml"}, {"extra"}} {
		if code, _, _ := runCmd(Ls, c, args...); code != int(core.ExitUsage) {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	empty := newSessionDaemon()
	empty.mux.HandleFunc("GET "+core.SessionsPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.ListSessionsResponse{})
	})
	if code, out, _ := runCmd(Ls, &Client{Dir: fakeDaemon(t, empty)}); code != 0 || out != "no sessions\n" {
		t.Errorf("empty: %d %q", code, out)
	}
}

func TestStop(t *testing.T) {
	d := newSessionDaemon()
	id, unknown := core.NewSessionID([16]byte{1}), core.NewSessionID([16]byte{2})
	code143 := 143
	d.mux.HandleFunc("POST "+core.SessionStopPath(id), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.StopSessionResponse{Session: core.Session{ID: id, NodeID: testNode, State: core.SessionStopped, ExitCode: &code143}})
	})
	d.mux.HandleFunc("POST "+core.SessionStopPath(unknown), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, core.ErrorResponse{Error: core.NewError(core.CodeNotFound, "session not found")})
	})
	c := &Client{Dir: fakeDaemon(t, d)}
	if code, out, errOut := runCmd(Stop, c, id.String()); code != 0 || out != id.String()+" stopped (exit code 143)\n" {
		t.Errorf("stop: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := runCmd(Stop, c, unknown.String()); code != 1 || !strings.Contains(errOut, "no session") {
		t.Errorf("unknown: %d %q", code, errOut)
	}
	for _, args := range [][]string{{}, {"x"}, {id.String(), "y"}} {
		if code, _, _ := runCmd(Stop, c, args...); code != int(core.ExitUsage) {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

// TestLogsFollowResumes: a followed stream that ends while the session runs
// is resumed from the bytes received; once the session has ended the rest
// is read once. Every byte is printed once, in order.
func TestLogsFollowResumes(t *testing.T) {
	d := newSessionDaemon()
	id := core.NewSessionID([16]byte{1})
	var mu sync.Mutex
	gets := 0
	chunks := map[string]string{
		core.LogsQuery(0, true):  "abc",   // cut after abc
		core.LogsQuery(3, true):  "def",   // cut again
		core.LogsQuery(6, false): "ghi\n", // the rest, after the end
	}
	d.mux.HandleFunc("GET "+core.SessionLogsPath(id), func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, chunks[r.URL.RawQuery])
	})
	d.mux.HandleFunc("GET "+core.SessionPath(id), func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		gets++
		state := core.SessionRunning
		if gets >= 2 {
			state = core.SessionExited
		}
		mu.Unlock()
		writeJSON(w, http.StatusOK, core.GetSessionResponse{Session: core.Session{ID: id, NodeID: testNode, State: state}})
	})
	c := &Client{Dir: fakeDaemon(t, d)}
	code, out, errOut := runCmd(Logs, c, id.String(), "--follow")
	if code != 0 || out != "abcdefghi\n" {
		t.Errorf("exit %d %q %q; requests %q", code, out, errOut, d.requests())
	}
}

func TestLogs(t *testing.T) {
	d := newSessionDaemon()
	id, unknown := core.NewSessionID([16]byte{1}), core.NewSessionID([16]byte{2})
	d.mux.HandleFunc("GET "+core.SessionLogsPath(id), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != core.LogsQuery(0, false) {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, "raw \x1b[1mbytes\x1b[0m\n")
	})
	d.mux.HandleFunc("GET "+core.SessionLogsPath(unknown), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, core.ErrorResponse{Error: core.NewError(core.CodeNotFound, "session not found")})
	})
	c := &Client{Dir: fakeDaemon(t, d)}
	if code, out, _ := runCmd(Logs, c, id.String()); code != 0 || out != "raw \x1b[1mbytes\x1b[0m\n" {
		t.Errorf("logs: %d %q", code, out)
	}
	if code, _, errOut := runCmd(Logs, c, unknown.String()); code != 1 || !strings.Contains(errOut, "no session") {
		t.Errorf("unknown: %d %q", code, errOut)
	}
	for _, args := range [][]string{{}, {"--bogus", id.String()}, {id.String(), id.String()}, {"not-an-id"}} {
		if code, _, _ := runCmd(Logs, c, args...); code != int(core.ExitUsage) {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}
