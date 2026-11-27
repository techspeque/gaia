package core

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNodeTransitionsExhaustive(t *testing.T) {
	legal := map[[2]NodeState]bool{
		{NodeOnline, NodeOffline}: true,
		{NodeOffline, NodeOnline}: true,
	}
	all := append([]NodeState{"", "unknown"}, NodeStates...)
	for _, from := range all {
		for _, to := range all {
			want := legal[[2]NodeState{from, to}]
			if got := from.CanTransition(to); got != want {
				t.Errorf("%q -> %q: CanTransition = %v, want %v", from, to, got, want)
			}
			next, err := from.Transition(to)
			if want {
				if err != nil || next != to {
					t.Errorf("%q -> %q: Transition = %q, %v", from, to, next, err)
				}
				continue
			}
			if !errors.Is(err, NewError(CodeConflict, "")) || next != from {
				t.Errorf("%q -> %q: Transition = %q, %v; want conflict", from, to, next, err)
			}
		}
	}
}

func TestSessionTransitionsExhaustive(t *testing.T) {
	legal := map[[2]SessionState]bool{}
	for _, p := range [][2]SessionState{
		{SessionStarting, SessionRunning}, {SessionStarting, SessionStopped},
		{SessionStarting, SessionFailed}, {SessionStarting, SessionLost},
		{SessionRunning, SessionExited}, {SessionRunning, SessionStopped},
		{SessionRunning, SessionFailed}, {SessionRunning, SessionLost},
		{SessionLost, SessionRunning}, {SessionLost, SessionExited},
		{SessionLost, SessionStopped}, {SessionLost, SessionFailed},
	} {
		legal[p] = true
	}
	all := append([]SessionState{"", "unknown"}, SessionStates...)
	for _, from := range all {
		for _, to := range all {
			want := legal[[2]SessionState{from, to}]
			if got := from.CanTransition(to); got != want {
				t.Errorf("%q -> %q: CanTransition = %v, want %v", from, to, got, want)
			}
			next, err := from.Transition(to)
			if want {
				if err != nil || next != to {
					t.Errorf("%q -> %q: Transition = %q, %v", from, to, next, err)
				}
				continue
			}
			if !errors.Is(err, NewError(CodeConflict, "")) || next != from {
				t.Errorf("%q -> %q: Transition = %q, %v; want conflict", from, to, next, err)
			}
		}
	}
}

func TestSessionTerminalAndValid(t *testing.T) {
	terminal := map[SessionState]bool{SessionExited: true, SessionStopped: true, SessionFailed: true}
	for _, s := range SessionStates {
		if !s.Valid() {
			t.Errorf("%q not valid", s)
		}
		if s.Terminal() != terminal[s] {
			t.Errorf("%q Terminal = %v", s, s.Terminal())
		}
	}
	if SessionState("unknown").Valid() || SessionState("unknown").Terminal() {
		t.Error("unknown session state accepted")
	}
	for _, s := range NodeStates {
		if !s.Valid() {
			t.Errorf("%q not valid", s)
		}
	}
	if NodeState("").Valid() {
		t.Error("empty node state accepted")
	}
}

func TestStateJSON(t *testing.T) {
	b, err := json.Marshal(struct {
		N NodeState    `json:"n"`
		S SessionState `json:"s"`
	}{NodeOnline, SessionLost})
	if err != nil || string(b) != `{"n":"online","s":"lost"}` {
		t.Fatalf("json = %s, %v", b, err)
	}
}
