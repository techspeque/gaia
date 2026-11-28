// Package supervise is the per-session supervisor (ADR-0008): the process
// behind the internal `gaia supervise` command. It starts one agent from
// argv with no shell, owns the agent's pipes or PTY, writes the agent's
// output to a scrollback file through a secret redactor, keeps the session's
// state file, and serves a user-only unix socket for clients. It exits when
// the agent exits or on stop.
//
// A supervisor lives in its session directory:
//
//	state.json  the core.SupervisorState, rewritten atomically on start and on exit
//	scrollback  the agent's output, redacted, append-only
//	sock        the unix socket, mode 0600, removed on exit
//
// The session's spec (argv, environment, secrets) arrives on stdin and is
// never written to disk, to argv or to a log.
//
// # Socket protocol
//
// One request per connection. The client writes one JSON line, a Request;
// the supervisor answers one JSON line, a Response. For "stream" the
// response is followed by frames: a 4-byte big-endian length and that many
// bytes of scrollback. A zero-length frame means the session has ended and
// every byte has been sent; the supervisor then closes the connection.
package supervise
