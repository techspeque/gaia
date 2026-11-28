package core

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func runningState() SupervisorState {
	return SupervisorState{
		Version:   SupervisorStateVersion,
		SessionID: NewSessionID([16]byte{7}),
		PID:       4242,
		Mode:      SessionInteractive,
		Argv:      []string{"claude", "", "line\nbreak \"q\""},
		StartedAt: time.Date(2026, 11, 27, 14, 0, 0, 5, time.UTC),
	}
}

// TestSupervisorStateShape fixes the state file's JSON form, running and
// ended, and checks it decodes back to the same value.
func TestSupervisorStateShape(t *testing.T) {
	run := runningState()
	ended := run
	code := 3
	end := run.StartedAt.Add(time.Minute)
	ended.ExitCode, ended.EndedAt = &code, &end
	killed := run
	killed.Signal, killed.EndedAt = "SIGKILL", &end

	id := run.SessionID.String()
	for _, c := range []struct {
		s    SupervisorState
		want string
	}{
		{run, `{"version":1,"session_id":"` + id + `","pid":4242,"mode":"interactive","argv":["claude","","line\nbreak \"q\""],"started_at":"2026-11-27T14:00:00.000000005Z","exit_code":null,"signal":"","ended_at":null}`},
		{ended, `{"version":1,"session_id":"` + id + `","pid":4242,"mode":"interactive","argv":["claude","","line\nbreak \"q\""],"started_at":"2026-11-27T14:00:00.000000005Z","exit_code":3,"signal":"","ended_at":"2026-11-27T14:01:00.000000005Z"}`},
		{killed, `{"version":1,"session_id":"` + id + `","pid":4242,"mode":"interactive","argv":["claude","","line\nbreak \"q\""],"started_at":"2026-11-27T14:00:00.000000005Z","exit_code":null,"signal":"SIGKILL","ended_at":"2026-11-27T14:01:00.000000005Z"}`},
	} {
		if err := c.s.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		b, err := json.Marshal(c.s)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("got  %s\nwant %s", b, c.want)
		}
		var back SupervisorState
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, c.s) {
			t.Errorf("round trip: %+v, want %+v", back, c.s)
		}
	}
	if run.Ended() || !ended.Ended() || !killed.Ended() {
		t.Error("Ended wrong")
	}
}

func TestSupervisorStateValidate(t *testing.T) {
	code := 0
	before := runningState().StartedAt.Add(-time.Second)
	after := runningState().StartedAt.Add(time.Second)
	var zero time.Time
	for name, mut := range map[string]func(*SupervisorState){
		"version 0":            func(s *SupervisorState) { s.Version = 0 },
		"version 2":            func(s *SupervisorState) { s.Version = 2 },
		"no session":           func(s *SupervisorState) { s.SessionID = SessionID{} },
		"pid 0":                func(s *SupervisorState) { s.PID = 0 },
		"pid negative":         func(s *SupervisorState) { s.PID = -1 },
		"bad mode":             func(s *SupervisorState) { s.Mode = "pty" },
		"no argv":              func(s *SupervisorState) { s.Argv = nil },
		"no start":             func(s *SupervisorState) { s.StartedAt = time.Time{} },
		"code while running":   func(s *SupervisorState) { s.ExitCode = &code },
		"signal while running": func(s *SupervisorState) { s.Signal = "SIGTERM" },
		"ended, no outcome":    func(s *SupervisorState) { s.EndedAt = &after },
		"ended before start":   func(s *SupervisorState) { s.EndedAt, s.ExitCode = &before, &code },
		"ended zero":           func(s *SupervisorState) { s.EndedAt, s.ExitCode = &zero, &code },
	} {
		s := runningState()
		mut(&s)
		if err := s.Validate(); !isInvalid(err) {
			t.Errorf("%s: %v, want invalid_argument", name, err)
		}
	}
}
