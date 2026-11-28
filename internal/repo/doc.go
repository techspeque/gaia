// Package repo manages node checkouts: one git checkout per remote URL in the
// node's workspace, synced only through the remote (ADR-0010).
//
// Before a session, Checkout.Prepare pushes work an earlier session could not
// push, fetches, and fast-forwards the requested branch. After a session,
// Checkout.Finish pushes the branch if it is ahead. Both fail with a
// *SyncError and leave the checkout as it was. Gaia never resets, rebases,
// stashes, cleans or force-pushes; the git runner refuses those before git
// runs. Workspace.Acquire queues runs on one checkout behind each other.
package repo
