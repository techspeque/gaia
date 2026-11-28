package core

// SyncState is where a session's after-session push is (ADR-0010 rule 3).
// A session without a repository has none ("").
type SyncState string

// Sync states.
const (
	// SyncNone is a session that runs in no checkout.
	SyncNone SyncState = ""
	// SyncPending is a session whose after-session push has not run yet.
	SyncPending SyncState = "pending"
	// SyncDone is a session whose branch was pushed, or had nothing to push.
	SyncDone SyncState = "done"
	// SyncFailed is a session whose push failed; its commits are in the
	// node's checkout (launcher contract: sync_after_failed, 94).
	SyncFailed SyncState = "failed"
)

// SyncStates lists every sync state.
var SyncStates = []SyncState{SyncNone, SyncPending, SyncDone, SyncFailed}

// Valid reports whether s is a known sync state.
func (s SyncState) Valid() bool {
	return s == SyncNone || s == SyncPending || s == SyncDone || s == SyncFailed
}

// Settled reports whether s is final: done or failed.
func (s SyncState) Settled() bool { return s == SyncDone || s == SyncFailed }

// CanMove reports whether a session's sync may change from s to to: from
// none to any state, from pending to a settled one. A settled state never
// changes; a state to itself is not a move.
func (s SyncState) CanMove(to SyncState) bool {
	if s == to || !to.Valid() {
		return false
	}
	switch s {
	case SyncNone:
		return true
	case SyncPending:
		return to.Settled()
	}
	return false
}

// DetailSyncReason names, in a sync_before_failed refusal, why the node's
// checkout could not be readied (e.g. "dirty", "diverged").
const DetailSyncReason = "sync_reason"
