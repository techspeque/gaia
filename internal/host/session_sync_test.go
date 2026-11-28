package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"github.com/techspeque/gaia/internal/store"
)

// TestStartSessionRepo: repo and branch reach the node and the record; the
// node's pending sync is recorded from its answer.
func TestStartSessionRepo(t *testing.T) {
	s, _, f, _ := sessionServer(t)
	f.start = func(r core.NodeStartRequest) (core.NodeSession, error) {
		ns, _ := running(r)
		ns.Sync = core.SyncPending
		return ns, nil
	}
	req := startReq("node-a", "claude")
	req.Repo, req.Branch = "https://git.example.com/a/b.git", "feature/x"
	sess, err := s.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 || f.started[0].Repo != req.Repo || f.started[0].Branch != req.Branch {
		t.Errorf("node got %+v", f.started)
	}
	if sess.Repo != req.Repo || sess.Branch != req.Branch || sess.Sync != core.SyncPending {
		t.Errorf("session %+v", sess)
	}
}

// TestStartSessionProfileRejected: profiles do not exist yet; any profile
// is profile_rejected before anything is recorded or asked of the node.
func TestStartSessionProfileRejected(t *testing.T) {
	s, _, f, _ := sessionServer(t)
	req := startReq("node-a", "claude")
	req.Profile = "claude-max"
	_, err := s.StartSession(context.Background(), req)
	if !errors.Is(err, core.NewError(core.CodeNotFound, "")) || reasonOf(err) != "profile_rejected" {
		t.Errorf("profile: %v (reason %q)", err, reasonOf(err))
	}
	if len(f.started) != 0 || len(listAll(t, s)) != 0 {
		t.Errorf("a rejected profile reached the node or the store: %+v", f.started)
	}
}

// TestStartSessionNodeReasonKept: a node's refusal that names a contract
// reason keeps it; others are start_failed.
func TestStartSessionNodeReasonKept(t *testing.T) {
	for _, c := range []struct {
		reason string
		want   string
	}{
		{"sync_before_failed", "sync_before_failed"},
		{"usage", "usage"},
		{"start_failed", "start_failed"},
		{"session_lost", "start_failed"}, // not a reason a refusal may carry
		{"", "start_failed"},
	} {
		s, _, f, _ := sessionServer(t)
		f.start = func(core.NodeStartRequest) (core.NodeSession, error) {
			e := &core.Error{Code: core.CodeConflict, Message: "dirty", Details: map[string]string{core.DetailSyncReason: "dirty"}}
			if c.reason != "" {
				e.Details[core.DetailReason] = c.reason
			}
			return core.NodeSession{}, e
		}
		req := startReq("node-a", "claude")
		req.Repo, req.Branch = "/r.git", "main"
		_, err := s.StartSession(context.Background(), req)
		if reasonOf(err) != c.want {
			t.Errorf("node reason %q: %v (reason %q), want %q", c.reason, err, reasonOf(err), c.want)
		}
		var ce *core.Error
		if errors.As(err, &ce) && ce.Details[core.DetailSyncReason] != "dirty" {
			t.Errorf("node reason %q: sync reason lost: %+v", c.reason, ce.Details)
		}
		if l := listAll(t, s); len(l) != 1 || l[0].State != core.SessionFailed {
			t.Errorf("node reason %q: sessions %+v", c.reason, l)
		}
	}
}

// TestReportSessionSync: sync moves forward from reports, also on an ended
// session and when the state move is refused; an older report does not
// move it back.
func TestReportSessionSync(t *testing.T) {
	s, _, f, _ := sessionServer(t)
	ctx := context.Background()
	f.start = func(r core.NodeStartRequest) (core.NodeSession, error) {
		ns, _ := running(r)
		ns.Sync = core.SyncPending
		return ns, nil
	}
	req := startReq("node-a", "a")
	req.Repo, req.Branch = "/r.git", "main"
	sess, err := s.StartSession(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID
	end := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	code := 0
	report := func(seq uint64, state core.SessionState, sync core.SyncState) error {
		r := core.SessionReport{SessionID: id, Seq: seq, State: state, Sync: sync}
		if state.Terminal() {
			r.EndedAt, r.ExitCode = &end, &code
		}
		return s.ReportSession(ctx, r)
	}
	if err := report(2, core.SessionExited, core.SyncPending); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(ctx, id); got.State != core.SessionExited || got.Sync != core.SyncPending {
		t.Fatalf("after exit %+v", got)
	}
	if err := report(3, core.SessionExited, core.SyncFailed); err != nil {
		t.Fatal(err)
	}
	// Replayed older report: no change, no error.
	if err := report(2, core.SessionExited, core.SyncPending); err != nil {
		t.Errorf("replay: %v", err)
	}
	if got, _ := s.GetSession(ctx, id); got.Sync != core.SyncFailed || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("after push report %+v", got)
	}

	// A sync report whose state the host refuses still records the push.
	sess2, err := s.StartSession(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetSessionExit(ctx, sess2.ID, store.SessionExit{State: core.SessionStopped, EndedAt: end}); err != nil {
		t.Fatal(err)
	}
	r := core.SessionReport{SessionID: sess2.ID, Seq: 3, State: core.SessionExited, ExitCode: &code, EndedAt: &end, Sync: core.SyncDone}
	if err := s.ReportSession(ctx, r); !errors.Is(err, core.NewError(core.CodeConflict, "")) {
		t.Errorf("conflicting end: %v", err)
	}
	if got, _ := s.GetSession(ctx, sess2.ID); got.State != core.SessionStopped || got.Sync != core.SyncDone {
		t.Errorf("after a refused end with a push %+v", got)
	}
}
