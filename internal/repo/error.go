package repo

import (
	"errors"

	"github.com/techspeque/gaia/internal/core"
)

// Stage is when a sync failed: before the session started or after it ended.
type Stage string

// Stages.
const (
	StageBefore Stage = "before"
	StageAfter  Stage = "after"
)

// Reason names why a sync failed. Reasons are stable.
type Reason string

// Reasons.
const (
	ReasonDirty             Reason = "dirty"
	ReasonDiverged          Reason = "diverged"
	ReasonPushRejected      Reason = "push_rejected"
	ReasonPushFailed        Reason = "push_failed"
	ReasonFetchFailed       Reason = "fetch_failed"
	ReasonCloneFailed       Reason = "clone_failed"
	ReasonBranchNotFound    Reason = "branch_not_found"
	ReasonWrongRemote       Reason = "wrong_remote"
	ReasonNotACheckout      Reason = "not_a_checkout"
	ReasonCheckoutFailed    Reason = "checkout_failed"
	ReasonFastForwardFailed Reason = "fast_forward_failed"
	ReasonGitFailed         Reason = "git_failed"
)

// maxDetail caps the git output kept in a SyncError.
const maxDetail = 1024

// SyncError is a failed sync. The checkout is left as it was.
type SyncError struct {
	Stage  Stage
	Reason Reason
	Branch string
	// Detail is git's error output, capped at 1 KiB.
	Detail string
}

func (e *SyncError) Error() string {
	s := "sync " + string(e.Stage) + " session failed: " + string(e.Reason)
	if e.Branch != "" {
		s += " (branch " + e.Branch + ")"
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// ExitCode is the launcher contract's code for the failure: 93 before the
// session, 94 after it.
func (e *SyncError) ExitCode() core.ExitCode {
	if e.Stage == StageAfter {
		return core.ExitSyncAfterFailed
	}
	return core.ExitSyncBeforeFailed
}

// Is matches a *SyncError with the same reason, so
// errors.Is(err, &SyncError{Reason: ReasonDirty}) matches any dirty failure.
func (e *SyncError) Is(target error) bool {
	var t *SyncError
	if !errors.As(target, &t) || t == nil {
		return false
	}
	return t.Reason == e.Reason
}

func syncErr(stage Stage, reason Reason, branch, detail string) *SyncError {
	if len(detail) > maxDetail {
		detail = detail[:maxDetail]
	}
	return &SyncError{Stage: stage, Reason: reason, Branch: branch, Detail: detail}
}

func invalid(msg string) error {
	return core.NewError(core.CodeInvalidArgument, msg)
}
