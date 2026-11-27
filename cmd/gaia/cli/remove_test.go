//go:build linux || darwin

package cli

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// removeDaemon answers DELETE on node paths: known names are removed once.
func removeDaemon(nodes map[string]core.Node) (http.Handler, func() []string) {
	var mu sync.Mutex
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc(core.VersionPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, core.VersionResponse{APIVersion: core.APIVersion, GaiaVersion: "v1"})
	})
	mux.HandleFunc(core.NodesPath+"/{name}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		name := r.PathValue("name")
		n, ok := nodes[name]
		if r.Method != http.MethodDelete || !ok {
			writeJSON(w, http.StatusNotFound, core.ErrorResponse{Error: core.NewError(core.CodeNotFound, "no node named "+name)})
			return
		}
		delete(nodes, name)
		writeJSON(w, http.StatusOK, core.RemoveNodeResponse{Node: n})
	})
	return mux, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
}

func TestNodesRemove(t *testing.T) {
	var b [16]byte
	b[0] = 7
	id := core.NewNodeID(b)
	h, calls := removeDaemon(map[string]core.Node{
		"old-box": {ID: id, Name: "old-box", State: core.NodeOffline},
		"live":    {ID: id, Name: "live", State: core.NodeOnline},
	})
	c := &Client{Dir: fakeDaemon(t, h)}
	now := time.Now()

	code, out, errOut := runNodes(c, now, "remove", "old-box")
	if code != 0 || out != "removed node old-box ("+id.String()+")\n" || errOut != "" {
		t.Errorf("remove: %d %q %q", code, out, errOut)
	}
	code, out, _ = runNodes(c, now, "remove", "live")
	if code != 0 || !strings.Contains(out, "registers again at its next heartbeat") {
		t.Errorf("remove online node: %d %q", code, out)
	}
	code, out, errOut = runNodes(c, now, "remove", "old-box")
	if code != 1 || out != "" || oneLine(t, errOut) != "gaia: no node named \"old-box\" is registered; `gaia nodes` lists them\n" {
		t.Errorf("remove again: %d %q %q", code, out, errOut)
	}
	if got := calls(); len(got) != 3 || got[0] != "DELETE "+core.NodePath("old-box") {
		t.Errorf("calls %q", got)
	}
}

func TestNodesRemoveUsage(t *testing.T) {
	for _, args := range [][]string{{"remove"}, {"remove", "a", "b"}, {"remove", "Bad_Name"}, {"remove", "../x"}} {
		var out, errOut bytes.Buffer
		c := &Client{Dir: t.TempDir()} // no daemon: usage errors must not dial
		code := Nodes(args, Env{Stdout: &out, Stderr: &errOut, Client: c})
		if code != int(core.ExitUsage) || !strings.HasPrefix(errOut.String(), "gaia: ") ||
			!strings.Contains(errOut.String(), removeUsage) || out.Len() != 0 {
			t.Errorf("%q: %d %q %q", args, code, out.String(), errOut.String())
		}
	}
	var out, errOut bytes.Buffer
	if code := Nodes([]string{"remove", "--help"}, Env{Stdout: &out, Stderr: &errOut, Client: &Client{Dir: t.TempDir()}}); code != 0 ||
		!strings.Contains(errOut.String(), "stop `gaia node` there first") {
		t.Errorf("help: %d %q", code, errOut.String())
	}
}

func TestNodesRemoveNoDaemon(t *testing.T) {
	var out, errOut bytes.Buffer
	c := &Client{Dir: shortDir(t)}
	if code := Nodes([]string{"remove", "x"}, Env{Stdout: &out, Stderr: &errOut, Client: c}); code != int(core.ExitNoDaemon) {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}
