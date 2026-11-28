package node

import (
	"fmt"
	"sync"
	"time"
)

// Phase is where a node stands with its host.
type Phase string

// Phases of a Link, in the order a starting node goes through them.
const (
	// PhaseJoining: the node has not joined the tailnet yet.
	PhaseJoining Phase = "joining"
	// PhaseRegistering: the node is registering with the host, at start or
	// after a heartbeat failed.
	PhaseRegistering Phase = "registering"
	// PhaseConfirming: the host accepted the registration and the node is
	// waiting for its first heartbeat to be answered. Right after an outage
	// the tailnet path can still be settling (fix-0004).
	PhaseConfirming Phase = "confirming"
	// PhaseReady: a heartbeat since the last registration was answered.
	PhaseReady Phase = "ready"
)

// Link records where a node stands with its host, for the forwarder to
// tell a caller what is happening. The zero value is PhaseJoining. Safe for
// concurrent use.
type Link struct {
	mu      sync.Mutex
	phase   Phase
	since   time.Time
	lastErr error
	now     func() time.Time // tests
	onSet   func(Phase)      // tests: called, under the lock, on each change
}

// LinkState is a snapshot of a Link.
type LinkState struct {
	Phase Phase
	// Since is when the link entered Phase; zero if it never changed.
	Since time.Time
	// LastErr is the latest failure reaching the host while not ready.
	LastErr error
}

func (l *Link) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Set moves the link to p. Setting the phase it is in keeps its Since.
func (l *Link) Set(p Phase) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.phaseLocked() == p && !l.since.IsZero() {
		return
	}
	l.phase, l.since = p, l.clock()
	if p == PhaseReady {
		l.lastErr = nil
	}
	if l.onSet != nil {
		l.onSet(p)
	}
}

// Failed records err as the latest failure reaching the host.
func (l *Link) Failed(err error) {
	if l == nil || err == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastErr = err
}

func (l *Link) phaseLocked() Phase {
	if l.phase == "" {
		return PhaseJoining
	}
	return l.phase
}

// State returns a snapshot. A nil Link is always ready: nothing tracks it.
func (l *Link) State() LinkState {
	if l == nil {
		return LinkState{Phase: PhaseReady}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return LinkState{Phase: l.phaseLocked(), Since: l.since, LastErr: l.lastErr}
}

// Ready reports whether the link is in PhaseReady.
func (l *Link) Ready() bool { return l.State().Phase == PhaseReady }

// Describe says in words what a not-ready link is doing, for an error
// message: e.g. "registering with the host for 4s (last error: ...)".
func (s LinkState) Describe(now time.Time) string {
	var what string
	switch s.Phase {
	case PhaseJoining:
		what = "joining the tailnet"
	case PhaseRegistering:
		what = "registering with the host"
	case PhaseConfirming:
		what = "waiting for the host to answer its first heartbeat since registering"
	default:
		what = "connected to the host"
	}
	if !s.Since.IsZero() {
		what += fmt.Sprintf(" for %s", now.Sub(s.Since).Round(time.Second))
	}
	if s.LastErr != nil {
		what += fmt.Sprintf(" (last error: %v)", s.LastErr)
	}
	return what
}
