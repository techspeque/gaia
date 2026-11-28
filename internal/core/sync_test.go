package core

import (
	"testing"
	"time"
)

func TestSyncStateMoves(t *testing.T) {
	for _, s := range SyncStates {
		if !s.Valid() {
			t.Errorf("%q not valid", s)
		}
	}
	for _, s := range []SyncState{"Done", "pushed", "x"} {
		if s.Valid() {
			t.Errorf("%q valid", s)
		}
	}
	allowed := map[[2]SyncState]bool{
		{SyncNone, SyncPending}: true, {SyncNone, SyncDone}: true, {SyncNone, SyncFailed}: true,
		{SyncPending, SyncDone}: true, {SyncPending, SyncFailed}: true,
	}
	for _, from := range SyncStates {
		for _, to := range append(SyncStates, "bogus") {
			if got := from.CanMove(to); got != allowed[[2]SyncState{from, to}] {
				t.Errorf("%q -> %q: CanMove = %v", from, to, got)
			}
		}
	}
	if SyncPending.Settled() || SyncNone.Settled() || !SyncDone.Settled() || !SyncFailed.Settled() {
		t.Error("Settled is wrong")
	}
}

func TestRepoBranchTogether(t *testing.T) {
	base := StartSessionRequest{Node: "n", Mode: SessionHeadless, Argv: []string{"a"}}
	for _, c := range []struct {
		repo, branch string
		ok           bool
	}{{"", "", true}, {"/r.git", "main", true}, {"/r.git", "", false}, {"", "main", false}} {
		r := base
		r.Repo, r.Branch = c.repo, c.branch
		if err := r.Validate(); (err == nil) != c.ok {
			t.Errorf("StartSessionRequest %q %q: %v", c.repo, c.branch, err)
		}
		nr := NodeStartRequest{ID: NewSessionID([16]byte{1}), Mode: SessionHeadless, Argv: []string{"a"}, Repo: c.repo, Branch: c.branch}
		if err := nr.Validate(); (err == nil) != c.ok {
			t.Errorf("NodeStartRequest %q %q: %v", c.repo, c.branch, err)
		}
	}
}

func TestReportSyncValidation(t *testing.T) {
	end := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	code := 0
	id := NewSessionID([16]byte{1})
	ok := []SessionReport{
		{SessionID: id, Seq: 1, State: SessionRunning, Sync: SyncPending},
		{SessionID: id, Seq: 2, State: SessionExited, ExitCode: &code, EndedAt: &end, Sync: SyncDone},
		{SessionID: id, Seq: 2, State: SessionStopped, EndedAt: &end, Sync: SyncFailed},
	}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	bad := []SessionReport{
		{SessionID: id, Seq: 1, State: SessionRunning, Sync: "pushed"},
		{SessionID: id, Seq: 1, State: SessionRunning, Sync: SyncDone},
	}
	for _, r := range bad {
		if err := r.Validate(); !isInvalid(err) {
			t.Errorf("%+v: %v, want invalid_argument", r, err)
		}
	}
}
