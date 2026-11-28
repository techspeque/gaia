package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// nodesFixture is the recorded `gaia nodes -o json`, relative to this package.
const nodesFixture = "../../../testdata/contract/gaia/api-v1/nodes.json"

func readFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(nodesFixture)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestnodesFixtureRoundTrip proves the writer produces the recorded bytes:
// decoding the fixture and writing it again gives the fixture back, so a
// renamed, added or removed field, or a change of format, fails here.
func TestNodesFixtureRoundTrip(t *testing.T) {
	want := readFixture(t)
	var l core.ListNodesResponse
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		t.Fatalf("fixture does not decode as core.ListNodesResponse: %v", err)
	}
	if len(l.Nodes) != 1 {
		t.Fatalf("fixture has %d nodes, want 1", len(l.Nodes))
	}
	var got bytes.Buffer
	if err := writeNodesJSON(&got, l); err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Errorf("gaia nodes -o json differs from the fixture\ngot:\n%s\nwant:\n%s", got.String(), want)
	}
}

// jsonKinds maps each key of a JSON object to the kind of its value.
func jsonKinds(t *testing.T, b []byte) map[string]string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range m {
		out[k] = reflect.TypeOf(v).Kind().String()
	}
	return out
}

// TestnodesFixtureShape proves the fixture covers every field a core.Node
// encodes, with the same JSON types, so the fixture cannot drift from the
// type through omitempty or a zero value.
func TestNodesFixtureShape(t *testing.T) {
	var raw struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(readFixture(t), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Nodes) != 1 {
		t.Fatalf("fixture has %d nodes, want 1", len(raw.Nodes))
	}
	zero, err := json.Marshal(core.Node{ID: core.NewNodeID([16]byte{1})})
	if err != nil {
		t.Fatal(err)
	}
	want, got := jsonKinds(t, zero), jsonKinds(t, raw.Nodes[0])
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fixture node fields %v, core.Node encodes %v", sortedPairs(got), sortedPairs(want))
	}
	var l core.ListNodesResponse
	if err := json.Unmarshal(readFixture(t), &l); err != nil {
		t.Fatal(err)
	}
	if _, err := core.ParseNodeID(l.Nodes[0].ID.String()); err != nil {
		t.Errorf("fixture id: %v", err)
	}
}

func sortedPairs(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+":"+v)
	}
	sort.Strings(out)
	return out
}

func TestNodesJSONEmpty(t *testing.T) {
	var b bytes.Buffer
	if err := writeNodesJSON(&b, core.ListNodesResponse{}); err != nil {
		t.Fatal(err)
	}
	if b.String() != "{\n  \"nodes\": []\n}\n" {
		t.Errorf("got %q", b.String())
	}
}

func TestNodesText(t *testing.T) {
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	node := func(name, tailnetName string, state core.NodeState, seen time.Time) core.Node {
		return core.Node{Name: name, TailnetName: tailnetName, State: state, OS: "linux", Arch: "arm64",
			GaiaVersion: "0.1.0", LastSeen: seen}
	}
	l := core.ListNodesResponse{Nodes: []core.Node{
		node("gaia-studio", "", core.NodeOnline, now.Add(-12*time.Second)),
		node("gaia-build-box", "gaia-build-box-1", core.NodeOffline, now.Add(-2*24*time.Hour)),
	}}
	var b bytes.Buffer
	if err := writeNodesText(&b, l, now); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"NAME            TAILNET NAME      STATE    OS/ARCH      VERSION  LAST SEEN\n" +
		"gaia-studio     -                 online   linux/arm64  0.1.0    12s ago\n" +
		"gaia-build-box  gaia-build-box-1  offline  linux/arm64  0.1.0    2026-11-25T14:00:00Z\n"
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}

	b.Reset()
	if err := writeNodesText(&b, core.ListNodesResponse{}, now); err != nil {
		t.Fatal(err)
	}
	if b.String() != "no nodes registered\n" {
		t.Errorf("empty: got %q", b.String())
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "just now"},
		{999 * time.Millisecond, "just now"},
		{time.Second, "1s ago"},
		{59 * time.Second, "59s ago"},
		{time.Minute, "1m ago"},
		{59*time.Minute + 59*time.Second, "59m ago"},
		{time.Hour, "1h ago"},
		{23 * time.Hour, "23h ago"},
		{24 * time.Hour, "2026-11-26T14:00:00Z"},
		{-time.Minute, "2026-11-27T14:01:00Z"},
	}
	for _, c := range cases {
		if got := ago(now, now.Add(-c.d)); got != c.want {
			t.Errorf("ago(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestNodesUsage covers argument errors, which fail before any connection.
func TestNodesUsage(t *testing.T) {
	cases := [][]string{
		{"-o", "yaml"},
		{"--output=xml"},
		{"--bogus"},
		{"extra"},
		{"-o"},
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		// No daemon exists at this directory; a usage error must not dial.
		c := &Client{Dir: t.TempDir()}
		code := Nodes(args, Env{Stdout: &out, Stderr: &errOut, Client: c})
		if code != int(core.ExitUsage) {
			t.Errorf("%q: exit %d, want %d; stderr %q", args, code, core.ExitUsage, errOut.String())
		}
		if !strings.Contains(errOut.String(), "usage: gaia nodes") {
			t.Errorf("%q: stderr %q lacks the usage", args, errOut.String())
		}
		if !strings.HasPrefix(errOut.String(), "gaia: ") {
			t.Errorf("%q: stderr %q does not start with %q", args, errOut.String(), "gaia: ")
		}
		if n := strings.Count(errOut.String(), "usage: gaia nodes"); n != 1 {
			t.Errorf("%q: stderr %q has the usage %d times, want 1", args, errOut.String(), n)
		}
		if out.Len() != 0 {
			t.Errorf("%q: stdout %q", args, out.String())
		}
	}
}
