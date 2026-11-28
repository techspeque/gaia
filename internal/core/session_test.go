package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Recorded `gaia ls -o json`, relative to this package.
const (
	sessionsFixture      = "../../testdata/contract/gaia/api-v1/sessions.json"
	sessionsEmptyFixture = "../../testdata/contract/gaia/api-v1/sessions-empty.json"
)

func isInvalid(err error) bool { return errors.Is(err, NewError(CodeInvalidArgument, "")) }

func TestSessionModeValid(t *testing.T) {
	for _, m := range SessionModes {
		if !m.Valid() {
			t.Errorf("%q not valid", m)
		}
	}
	for _, m := range []SessionMode{"", "Headless", "pty", "detach"} {
		if m.Valid() {
			t.Errorf("%q valid", m)
		}
	}
}

func TestSessionPaths(t *testing.T) {
	if SessionsPath != "/api/v1/sessions" {
		t.Errorf("SessionsPath = %q", SessionsPath)
	}
	id := NewSessionID([16]byte{1})
	if got := SessionPath(id); got != "/api/v1/sessions/"+id.String() {
		t.Errorf("SessionPath = %q", got)
	}
}

func TestValidArgv(t *testing.T) {
	for _, ok := range [][]string{
		{"claude"},
		{"claude", "", "-p", "say \"hi\"\nand 'bye'\t\\", "ünïcödé ✓"},
	} {
		if err := ValidArgv(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range [][]string{nil, {}, {""}, {"", "x"}, {"a", "b\x00c"}, {"a", "\xff"}, {"\xc3"}} {
		if err := ValidArgv(bad); !isInvalid(err) {
			t.Errorf("%q: %v, want invalid_argument", bad, err)
		}
	}
}

func TestStartSessionRequestValidate(t *testing.T) {
	good := StartSessionRequest{Node: "build-box", Mode: SessionHeadless, Argv: []string{"claude", "-p", "x"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*StartSessionRequest){
		"no node":   func(r *StartSessionRequest) { r.Node = "" },
		"bad node":  func(r *StartSessionRequest) { r.Node = "Build Box" },
		"no mode":   func(r *StartSessionRequest) { r.Mode = "" },
		"bad mode":  func(r *StartSessionRequest) { r.Mode = "detach" },
		"no argv":   func(r *StartSessionRequest) { r.Argv = nil },
		"empty [0]": func(r *StartSessionRequest) { r.Argv = []string{""} },
	} {
		r := good
		r.Argv = append([]string(nil), good.Argv...)
		mut(&r)
		if err := r.Validate(); !isInvalid(err) {
			t.Errorf("%s: %v, want invalid_argument", name, err)
		}
	}
}

// TestStartSessionRequestShape fixes the start request's JSON keys.
func TestStartSessionRequestShape(t *testing.T) {
	r := StartSessionRequest{Node: "n", Repo: "r", Branch: "b", Mode: SessionInteractive, Argv: []string{"a"}, Profile: "p"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"node":"n","repo":"r","branch":"b","mode":"interactive","argv":["a"],"profile":"p"}`
	if string(b) != want {
		t.Errorf("got %s\nwant %s", b, want)
	}
}

// encodeLikeCLI encodes v the way `gaia nodes -o json` does: two-space
// indent and a trailing newline.
func encodeLikeCLI(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func decodeSessionsFixture(t *testing.T, file string) (ListSessionsResponse, []byte) {
	t.Helper()
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var l ListSessionsResponse
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		t.Fatalf("%s does not decode as ListSessionsResponse: %v", file, err)
	}
	return l, want
}

// TestSessionsFixturesRoundTrip proves the recorded `gaia ls -o json`
// matches the types: decoding with unknown fields refused and encoding again
// gives the same bytes, so a renamed, added or removed field, or a changed
// encoding, fails here.
func TestSessionsFixturesRoundTrip(t *testing.T) {
	for _, file := range []string{sessionsFixture, sessionsEmptyFixture} {
		l, want := decodeSessionsFixture(t, file)
		if got := encodeLikeCLI(t, l); got != string(want) {
			t.Errorf("%s: re-encoded differs\ngot:\n%s\nwant:\n%s", file, got, want)
		}
	}
	if l, _ := decodeSessionsFixture(t, sessionsEmptyFixture); l.Sessions == nil || len(l.Sessions) != 0 {
		t.Errorf("empty fixture: %#v", l.Sessions)
	}
}

// TestSessionsFixtureCoversEveryState checks the fixture holds one session
// in each state, each consistent with its state.
func TestSessionsFixtureCoversEveryState(t *testing.T) {
	l, _ := decodeSessionsFixture(t, sessionsFixture)
	seen := map[SessionState]int{}
	for _, s := range l.Sessions {
		seen[s.State]++
		if !s.State.Valid() || !s.Mode.Valid() {
			t.Errorf("%s: state %q mode %q", s.ID, s.State, s.Mode)
		}
		if err := ValidArgv(s.Argv); err != nil {
			t.Errorf("%s: %v", s.ID, err)
		}
		if s.State.Terminal() != (s.EndedAt != nil) {
			t.Errorf("%s: state %s with ended_at %v", s.ID, s.State, s.EndedAt)
		}
		if s.ExitCode != nil && s.EndedAt == nil {
			t.Errorf("%s: exit code without ended_at", s.ID)
		}
		if s.State == SessionExited && s.ExitCode == nil {
			t.Errorf("%s: exited without exit code", s.ID)
		}
	}
	for _, st := range SessionStates {
		if seen[st] != 1 {
			t.Errorf("state %s appears %d times, want 1", st, seen[st])
		}
	}
	if len(l.Sessions) != len(SessionStates) {
		t.Errorf("%d sessions, want %d", len(l.Sessions), len(SessionStates))
	}
}

// TestSessionNullsEncoded checks a live session encodes ended_at and
// exit_code as null, present, so the shape does not depend on state.
func TestSessionNullsEncoded(t *testing.T) {
	s := Session{
		ID: NewSessionID([16]byte{1}), NodeID: NewNodeID([16]byte{2}), Mode: SessionHeadless,
		Argv: []string{"a"}, State: SessionRunning, StartedAt: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ended_at":null`, `"exit_code":null`, `"profile":""`, `"slice":""`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %s", b, want)
		}
	}
}
