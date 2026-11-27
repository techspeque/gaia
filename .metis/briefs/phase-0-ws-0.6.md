# phase-0-ws-0.6 — Tailnet join

- **Type:** feat | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.6

## Goal

`gaia host --tailnet [--control-url URL]` joins a tailnet through embedded
`tsnet` (state inside the host state directory), serves `/api/v1` over HTTPS
on the tsnet listener only, authorises every tailnet request by `WhoIs` peer
identity against a host-side allowlist (`403` and a log line otherwise), and
reads the auth key from an environment variable or a file, never an argument
and never logged; without `--tailnet` no tsnet code runs.

## Architectural context

- OVERVIEW §2 (only daemons join the tailnet), §2.3 (`internal/net` is a
  leaf importing `core` only; all tsnet use stays in it), §3.2 (no
  telemetry), §3.3 (listeners: tailnet, 127.0.0.1, user-only socket only),
  §5 boundary 1, §7 (tsnet tests against a local test control server), §8.
- ADR-0004 rules 4, 5, 6, 7, 8; ADR-0002 (CGO off, six targets);
  ADR-0012 (no telemetry, no real names in the repo).
- Consumes from 0.4: `host.Server` (`Handler`, `Serve`), `logging.New`,
  `logging.Redactor.Register(core.Secret)`, `sock`, `cmd/gaia/daemon.go`
  start order and `awaitShutdown`. From 0.2: `core.Secret`, `core.Error`,
  `core.ErrorResponse`, codes, `core.APIVersion`.
- Consumers: 0.7 (nodes dial the host over the tailnet with
  `net.Tailnet.Dial`; node registration routes will be added to the tailnet
  handler there).

Design decisions:

- **Dependency:** `tailscale.com v1.98.8`, the newest release whose `go`
  directive (1.26.3) the pinned toolchain satisfies; v1.98.9 and later need
  Go ≥ 1.26.5 and v1.104+ need 1.27.4. A toolchain bump is not this slice's.
- **`internal/net`** (package `net`, imports `core` only):
  - `Config{Dir, Hostname, ControlURL string; AuthKey core.Secret;
    Logf func(format string, args ...any)}`.
  - `Join(ctx, Config) (*Tailnet, error)`: calls `logtail.Disable()` before
    starting (tsnet otherwise uploads logs to log.tailscale.com; OVERVIEW
    §3.2), sets `tsnet.Server{Dir: <state>/tsnet, Hostname, ControlURL,
    AuthKey, Logf, UserLogf}` with both loggers routed through the caller's
    (redacting) logger, `Up(ctx)`, returns once Running. The tsnet dir is
    created 0700. An empty control URL means Tailscale's default.
  - `(*Tailnet).ListenTLS(port)`: `tsnet.Listen("tcp", ":443")` wrapped in
    TLS. Certificate: the Tailscale-issued cert for the MagicDNS name
    (`LocalClient.GetCertificate`) when the client's SNI is one of
    `CertDomains()`; otherwise a self-signed ECDSA P-256 cert generated once
    and kept at `<state>/tsnet/gaia-tls.{crt,key}` (0600), so Headscale (no
    cert issuance) works. TLS 1.3 minimum.
  - `(*Tailnet).WhoIs(ctx, remoteAddr) (Peer, error)` with
    `Peer{LoginName, NodeName string; Tags []string}`.
  - `(*Tailnet).Dial`, `Close`.
- **Allowlist** (`internal/host`): `host.Allowlist` of entries, each a
  login name (`user@example.com`), a tag (`tag:gaia`) or a node name
  (`node:<name>`); a peer is allowed when any entry matches. Given with
  `--allow ENTRY` (repeatable) or `--allow-file PATH` (one per line, `#`
  comments). Empty allowlist with `--tailnet` is a start error: default deny
  is the safe choice and an allowlist that admits no one is a misconfig.
- **`host.Server.TailnetHandler(whois)`**: wraps the `/api/v1` routes (not
  `/healthz`, which stays socket-only) with the WhoIs check: lookup failure
  or no match → `403` `core.ErrorResponse` (`permission_denied`, generic
  message) and a `warn` log line with remote addr and peer login/node name;
  a match passes through. `whois` is an interface so the handler is
  unit-tested without tsnet.
- **Auth key:** `TS_AUTHKEY`/`GAIA_AUTHKEY` environment variable or
  `--auth-key-file PATH` (file must not be group/other readable). Never a
  flag value. Registered with the redactor before any log line; the env
  variable is unset in the process after reading. Not needed once the tsnet
  state holds a node key.
- **`cmd/gaia host`**: flags `--tailnet`, `--control-url URL`,
  `--allow`, `--allow-file`, `--auth-key-file`; `--control-url`, `--allow*`,
  `--auth-key-file` without `--tailnet` exit 2. Without `--tailnet` the
  start path does not call into `internal/net` at all. With it: logger →
  store → socket → tailnet join → HTTPS serve on tsnet + socket serve.
  Shutdown closes both servers and the tsnet server.
- **README:** a "Joining a tailnet" section covering Tailscale and Headscale
  equally (control URL, auth key, allowlist, certificates).

## Declared file scope

- **owned_paths:** internal/net/, internal/host/, cmd/gaia/daemon.go, cmd/gaia/daemon_test.go, cmd/gaia/tailnet_test.go, cmd/gaia/main.go, README.md, go.mod, go.sum
- **read_only_paths:** internal/core/, internal/logging/, internal/sock/, internal/store/, internal/node/, cmd/gaia/cli/, deps_test.go, OVERVIEW.md, .metis/adr/, Makefile

## Definition of Done

- With `--tailnet` against a local test control server, `/api/v1/version`
  over HTTPS on the tailnet answers `200` to an allowlisted peer and `403`
  (with a log line) to a peer that is not allowlisted.
- Without `--tailnet`, no tsnet code runs and no network call is made:
  `internal/net` is not entered (test hook), and the child process has no
  inet socket (existing 0.4 check).
- No TCP listener is bound to 0.0.0.0, `::` or a LAN address in either mode;
  the API's TCP listener exists only inside tsnet's netstack. (See open
  question on tsnet's WireGuard UDP socket.)
- The auth key appears in no log line (tsnet logs included), process
  argument or API response, checked by a test with a known key.
- `--control-url` points at any control server; README covers Tailscale and
  Headscale.
- `make verify` and `make build-all` green; dependency-rule test and
  `TestCLIDoesNotReachTsnet` pass.

## Test plan

- `internal/net`: join two `tsnet` servers to `testcontrol` with in-repo
  DERP (`tstest/integration`), dial over the tailnet, TLS handshake with the
  self-signed fallback, `WhoIs` returns the peer's identity; auth key
  required by the control server is passed; logs captured contain no key.
- `internal/host`: allowlist parse and match (login, tag, node, comments,
  blank lines, bad entries); `TailnetHandler` with a fake whois: allowed →
  200, not allowed → 403 shape + log line, whois error → 403, `/healthz`
  not served on the tailnet handler.
- `cmd/gaia`: flag validation (tailnet-only flags without `--tailnet` exit
  2; `--auth-key` as an argument is not a flag); auth key from env and from a
  file, file mode check; end-to-end host with `--tailnet` against
  testcontrol answering an allowlisted peer and refusing another; no auth
  key in captured stderr; no non-tsnet TCP listener.

## Out-of-scope touches

None.

## Open question (human)

tsnet's magicsock binds its WireGuard UDP socket on all interfaces
(`net.JoinHostPort("", port)` in `magicsock.Conn.listenPacket`, no option to
change it in tsnet v1.98.8). Plan §0.6 says "no listener is bound to
0.0.0.0 … in either mode". Proposed reading: the criterion covers Gaia's
TCP/API listeners; the WireGuard UDP socket is the tailnet transport ADR-0004
accepted. Needs a person's decision before code.

**Decided by the Human, 2026-11-27:** the proposed reading is accepted. The
criterion covers Gaia's own listeners; the WireGuard UDP socket is not
counted. Plan §0.6, the gate scenario and OVERVIEW §3 are amended to say so.
The other decisions in this brief (tsnet v1.98.8, log upload disabled,
empty allowlist refuses to start, auth key from environment or file) stand.

**Second decision, 2026-11-27 (peerapi):** turn it off. tsnet's peerapi
server opens a TCP listener on all interfaces, and Gaia uses nothing it
serves, so the exception made for the WireGuard UDP socket is not widened.
Build with `-tags ts_omit_peerapiserver`. For this, and only this, the
slice may edit the `Makefile`: add the tag to every target that builds,
tests or analyses (test, build, build-all, vet, staticcheck), in one
variable. The listener check stays as strict as written, so a build without
the tag fails it; say so in the README section. Record in the Report that
the release build (workstream 0.8) must carry the same tag.

## Report

Filled at implementation.
