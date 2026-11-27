package core

import (
	"fmt"
	"slices"
)

// NodeState is whether the host currently hears a node's heartbeat.
type NodeState string

// Node states.
const (
	NodeOnline  NodeState = "online"
	NodeOffline NodeState = "offline"
)

// NodeStates lists every node state.
var NodeStates = []NodeState{NodeOnline, NodeOffline}

var nodeTransitions = map[NodeState][]NodeState{
	NodeOnline:  {NodeOffline},
	NodeOffline: {NodeOnline},
}

// Valid reports whether s is a known node state.
func (s NodeState) Valid() bool { _, ok := nodeTransitions[s]; return ok }

// CanTransition reports whether a node may move from s to to. A state to
// itself is not a transition.
func (s NodeState) CanTransition(to NodeState) bool {
	return slices.Contains(nodeTransitions[s], to)
}

// Transition returns to if s may move to it, and a typed conflict error
// otherwise.
func (s NodeState) Transition(to NodeState) (NodeState, error) {
	if !s.CanTransition(to) {
		return s, illegalTransition("node", string(s), string(to))
	}
	return to, nil
}

// SessionState is where a session is in its lifecycle.
type SessionState string

// Session states.
const (
	SessionStarting SessionState = "starting"
	SessionRunning  SessionState = "running"
	SessionExited   SessionState = "exited"  // the agent exited by itself
	SessionStopped  SessionState = "stopped" // stopped on request
	SessionFailed   SessionState = "failed"  // Gaia could not start or keep it
	SessionLost     SessionState = "lost"    // supervisor unreachable, outcome unknown
)

// SessionStates lists every session state.
var SessionStates = []SessionState{
	SessionStarting, SessionRunning, SessionExited, SessionStopped, SessionFailed, SessionLost,
}

// sessionTransitions is the legal transition table. A lost session may come
// back to running when its supervisor is rediscovered (ADR-0008 rule 4), or
// end once its outcome is learned. exited, stopped and failed are terminal.
var sessionTransitions = map[SessionState][]SessionState{
	SessionStarting: {SessionRunning, SessionStopped, SessionFailed, SessionLost},
	SessionRunning:  {SessionExited, SessionStopped, SessionFailed, SessionLost},
	SessionLost:     {SessionRunning, SessionExited, SessionStopped, SessionFailed},
	SessionExited:   nil,
	SessionStopped:  nil,
	SessionFailed:   nil,
}

// Valid reports whether s is a known session state.
func (s SessionState) Valid() bool { _, ok := sessionTransitions[s]; return ok }

// Terminal reports whether s is a known state no transition leaves.
func (s SessionState) Terminal() bool { return s.Valid() && len(sessionTransitions[s]) == 0 }

// CanTransition reports whether a session may move from s to to. A state to
// itself is not a transition.
func (s SessionState) CanTransition(to SessionState) bool {
	return slices.Contains(sessionTransitions[s], to)
}

// Transition returns to if s may move to it, and a typed conflict error
// otherwise.
func (s SessionState) Transition(to SessionState) (SessionState, error) {
	if !s.CanTransition(to) {
		return s, illegalTransition("session", string(s), string(to))
	}
	return to, nil
}

func illegalTransition(kind, from, to string) *Error {
	return &Error{
		Code:    CodeConflict,
		Message: fmt.Sprintf("illegal %s state transition %q -> %q", kind, from, to),
		Details: map[string]string{"from": from, "to": to},
	}
}
