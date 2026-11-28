package node

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLink(t *testing.T) {
	var nilLink *Link
	if !nilLink.Ready() {
		t.Error("a nil link is not ready")
	}
	nilLink.Set(PhaseRegistering) // no panic
	nilLink.Failed(errors.New("x"))

	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	l := &Link{now: func() time.Time { return now }}
	if s := l.State(); s.Phase != PhaseJoining || l.Ready() {
		t.Errorf("zero link %+v", s)
	}
	l.Set(PhaseJoining)
	if s := l.State(); !s.Since.Equal(now) {
		t.Errorf("joining since %v, want %v", s.Since, now)
	}

	l.Set(PhaseRegistering)
	l.Failed(errors.New("context deadline exceeded"))
	now = now.Add(4 * time.Second)
	l.Set(PhaseRegistering) // same phase: Since kept
	s := l.State()
	if got := s.Describe(now); got != "registering with the host for 4s (last error: context deadline exceeded)" {
		t.Errorf("Describe = %q", got)
	}

	l.Set(PhaseConfirming)
	if got := l.State().Describe(now.Add(1500 * time.Millisecond)); !strings.HasPrefix(got,
		"waiting for the host to answer its first heartbeat since registering for 2s") {
		t.Errorf("Describe = %q", got)
	}

	l.Set(PhaseReady)
	if s := l.State(); !l.Ready() || s.LastErr != nil {
		t.Errorf("ready %+v", s)
	}
	if got := (LinkState{Phase: PhaseJoining}).Describe(now); got != "joining the tailnet" {
		t.Errorf("Describe with no Since = %q", got)
	}
}
