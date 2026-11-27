// Package net wires the embedded tsnet node. All tsnet use stays inside
// this package (OVERVIEW §2.3); it imports no other package of this module
// but internal/core.
//
// Join starts a tsnet server with log upload disabled (OVERVIEW §3.2) and
// its state under the caller's state directory. ListenTLS serves on the
// tsnet listener only, so the API's TCP listener exists inside tsnet's
// netstack and never on a host interface (ADR-0004 rule 4). WhoIs names
// the tailnet peer behind a connection, for the host's allowlist
// (ADR-0004 rule 5).
package net
