package core

// ExitCode is one of Gaia's reserved process exit codes, 90 to 99. The table
// is docs/contracts/launcher-v1.md "Exit codes"; a test keeps the two equal.
type ExitCode int

// Reserved exit codes, launcher contract v1.
const (
	ExitNoDaemon         ExitCode = 90
	ExitNodeUnavailable  ExitCode = 91
	ExitProfileRejected  ExitCode = 92
	ExitSyncBeforeFailed ExitCode = 93
	ExitSyncAfterFailed  ExitCode = 94
	ExitSessionLost      ExitCode = 95
	ExitVersionMismatch  ExitCode = 96
	ExitStartFailed      ExitCode = 97
	ExitUsage            ExitCode = 98
	ExitInternal         ExitCode = 99
)

// Bounds of the reserved range, inclusive.
const (
	ExitReservedMin ExitCode = 90
	ExitReservedMax ExitCode = 99
)

var exitCodeNames = map[ExitCode]string{
	ExitNoDaemon:         "no_daemon",
	ExitNodeUnavailable:  "node_unavailable",
	ExitProfileRejected:  "profile_rejected",
	ExitSyncBeforeFailed: "sync_before_failed",
	ExitSyncAfterFailed:  "sync_after_failed",
	ExitSessionLost:      "session_lost",
	ExitVersionMismatch:  "version_mismatch",
	ExitStartFailed:      "start_failed",
	ExitUsage:            "usage",
	ExitInternal:         "internal",
}

// ExitCodes lists every reserved exit code in ascending order.
var ExitCodes = []ExitCode{
	ExitNoDaemon, ExitNodeUnavailable, ExitProfileRejected, ExitSyncBeforeFailed,
	ExitSyncAfterFailed, ExitSessionLost, ExitVersionMismatch, ExitStartFailed,
	ExitUsage, ExitInternal,
}

// Name returns the contract name of c, or "" if c is not reserved.
func (c ExitCode) Name() string { return exitCodeNames[c] }

// Reserved reports whether c is in Gaia's reserved range.
func (c ExitCode) Reserved() bool { return c >= ExitReservedMin && c <= ExitReservedMax }

// Retryable reports whether the contract allows a caller to retry after c.
func (c ExitCode) Retryable() bool { return c == ExitNodeUnavailable || c == ExitSessionLost }
