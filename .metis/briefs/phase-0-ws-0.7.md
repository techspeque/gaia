# phase-0-ws-0.7 — Node role, registration and heartbeat

- **Type:** feat | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27
- **Plan:** .metis/plans/phase-0-plan.md §0.7

## Goal

`gaia node --host NAME [--join] [--state DIR]` joins the tailnet, binds its
own 0600 local socket, registers with the host (identity from the host's
`WhoIs`, versions negotiated: own API version and one behind), heartbeats on
the interval the host sets, forwards the CLI's `/api/v1` requests to the host
so `gaia nodes` works on any machine, reconnects with backoff and never exits
because the host is down; the host marks a node `offline` after a
configurable number of missed heartbeats; `gaia nodes remove NAME` removes a
node.

## Architectural context

- OVERVIEW §2 (only daemons join the tailnet; the CLI talks to the local
  daemon only), §2.2 (Node entity), §2.3 (dependency rule), §3.3 (listeners;
  no secret on a node's disk), §5 boundaries 1, 3, 4, §8 (versions negotiated
  on registration; host serves N and N-1 nodes; node down → it reattaches).
- ADR-0002 (one binary, two roles), ADR-0004 rules 2–5 (CLI never starts
  tsnet; user-only socket; tailnet identity by `WhoIs`; allowlist).
- Consumes from 0.2: `core.RegisterNodeRequest/Response`,
  `core.HeartbeatRequest/Response`, `core.Node`, `core.Error` and codes,
  `core.APIVersion`. From 0.3: `store` node CRUD (`CreateNode`, `GetNode`,
  `ListNodes`, `UpdateNode`, `UpdateNodeLastSeen`, `RemoveNode`; name and
  identity are unique). From 0.4: `host.Server`, `sock.Listen/Dial`,
  `logging`, `awaitShutdown`, `node.RunLocal`. From 0.5: `cli.Client`,
  `cli.Nodes`, the nodes fixture (shape unchanged). From 0.6: `gnet.Join`,
  `ListenTLS`, `WhoIs`, `Dial`, `host.TailnetHandler`, `readAuthKey`,
  `nettest.StartControl`.
- Consumers: 0.9 (`gaia service install node --host NAME`; `--join` is never
  put in a unit, so the node must run without it once joined).

Design decisions:

- **Wire API** (`internal/core/api.go`, additive; the `gaia nodes -o json`
  fixture does not change):
  - `POST /api/v1/nodes` (`RegisterNodeRequest` → `RegisterNodeResponse`),
    `POST /api/v1/heartbeat` (`HeartbeatRequest` → `HeartbeatResponse`):
    served on the tailnet handler only, not on the host's local socket (the
    in-process node calls `Server.Register`/`Heartbeat` directly).
  - `RegisterNodeResponse` gains `heartbeat_interval_ms`: the host sets the
    interval, the node follows it, so one flag configures both ends.
  - `DELETE /api/v1/nodes/{name}` → `RemoveNodeResponse{node}`, on socket and
    tailnet. `core.NodePath(name)` and `core.ValidNodeName` (DNS-label form,
    `[a-z0-9-]`, ≤63, no leading/trailing `-`) so no URL escaping is needed
    in `core`, which may not import `net/url`.
  - Error → HTTP status: invalid_argument 400, forbidden 403, not_found 404,
    conflict and version_mismatch 409, unavailable 503, else 500. Non-typed
    errors stay a generic 500 with the detail only in the log (as 0.4).
- **Identity:** over the tailnet the host takes `tailnet_identity` from
  `WhoIs` (the peer's stable node id, new `gnet.Peer.StableID`), never from
  the request body. A heartbeat whose peer is not the node's stored identity
  is `403`. Re-registration of a known identity updates its row (same id);
  a name held by another identity is `409 conflict`, so a machine cannot take
  over another's name; the fix is `gaia nodes remove NAME`. Registering peers
  must pass the 0.6 allowlist like any other caller.
- **Version negotiation** (`host.Server.Register`): accepted when
  `max(1, host−1) ≤ node ≤ host`; otherwise `version_mismatch` with details
  (`node_api_version`, `host_api_version`, `host_gaia_version`) and the
  instruction: an older node is told to install the host's gaia version on
  it and restart `gaia node`; a newer node is told to upgrade the host.
  `host.Options.APIVersion` (default `core.APIVersion`) lets tests run a host
  at v3. The node treats `version_mismatch` as fatal: it logs the error and
  exits 1. Every other registration failure is retried with backoff.
- **Heartbeat:** `gaia host` gains `--heartbeat-interval D` (default 10s)
  and `--missed-heartbeats N` (default 3); state stays derived from
  `last_seen` (0.4). The node registers, then heartbeats every interval; a
  `not_found` heartbeat (node removed, registry reset) or a transport error
  sends it back to registering, with exponential backoff 500ms → 30s. A node
  restarted with the same state keeps its tsnet identity, so it re-registers
  to the same id.
- **Removal:** `gaia nodes remove NAME` deletes the row. A node that is still
  running registers again at its next heartbeat; the command is for retired
  machines, and the help text says so.
- **Node daemon** (`internal/node`, no tsnet import; `cmd/gaia/node.go`
  wires it):
  - `node.Client`: HTTP client for register/heartbeat against the host,
    decoding `core.ErrorResponse` to `*core.Error`.
  - `node.Run(ctx, RunConfig)`: the register/heartbeat/backoff loop.
  - `node.Handler(forward)`: the node socket's handler: `/healthz` answered
    locally; every `/api/v1/` request reverse-proxied to the host unchanged,
    except the node's own `POST /api/v1/nodes` and `POST /api/v1/heartbeat`
    (404 on the socket: local processes do not speak as the node). An
    unreachable host is `503 unavailable` with a message naming the host.
  - Start order: logger (auth key registered first) → state dir (0700,
    group/other access refused) → socket bind → socket serve → tailnet join
    → registration loop. The socket answers before the join completes, so a
    CLI on the node gets "host unreachable", not "no daemon".
- **Reaching the host:** `gnet.(*Tailnet).LookupPeer(ctx, name)` finds the
  peer named `--host` in the tailnet status (first label of its MagicDNS
  name, or its tailnet IP), and the node dials that peer's IP on 443. TLS
  1.3 with the tailnet IP as server name, so the host serves its kept
  self-signed certificate (0.6) and never orders a Tailscale certificate
  over ACME on a node's behalf; the node accepts it when it is self-signed
  and names that IP (or when a chain verifies against the system roots for
  the host's MagicDNS name). The peer is already authenticated by WireGuard
  and selected by name from the control server's netmap; TLS is transport
  encryption here, not the authentication of the host. Dials are bounded
  at 5s, so a forwarded CLI request gets "host unreachable" before the CLI
  times out.
- **Shutdown:** when the 5s shutdown grace period runs out (a connection a
  node opened and abandoned before its TLS handshake holds until the
  handshake timeout), host and node log a warning, close the rest and still
  exit 0.
- **Flags:** `gaia node --host NAME [--join] [--state DIR] [--name NAME]
  [--control-url URL] [--auth-key-file PATH]`. `--join` means "join the
  tailnet now with an auth key", read as for the host (0.6: `$GAIA_AUTHKEY`,
  `$TS_AUTHKEY` or `--auth-key-file`, never an argument, unset after
  reading); `--join` without a key is an error, as is starting without
  `--join` when the state dir holds no tsnet state (instead of tsnet waiting
  for an interactive login). `--name` (default: the machine name, as the
  host's) is the registered name and the tailnet hostname; needed to run two
  nodes on one machine in tests. Default state dir: the host's default.
- **Node state directory** holds only `tsnet/` (tsnet's own state, which
  includes the machine's WireGuard key — the tailnet identity, not a Gaia
  credential in the OVERVIEW §2.2 sense). No auth key, no credential.
- **`internal/node` local name:** `LocalRequest` lower-cases the hostname and
  replaces characters outside `[a-z0-9-]` with `-` so it passes
  `core.ValidNodeName`.

## Declared file scope

- **owned_paths:** internal/node/, internal/host/, internal/net/, internal/core/api.go, internal/core/api_test.go, cmd/gaia/, README.md
- **read_only_paths:** internal/core/ (other files), internal/store/, internal/sock/, internal/logging/, deps_test.go, testdata/contract/, OVERVIEW.md, .metis/adr/, docs/contracts/

## Definition of Done

- Two nodes joined through the test control server appear in
  `gaia nodes -o json` on the host and on each node (through the node's
  forwarder).
- Stopping a node turns it `offline` after interval × missed; starting it
  again (without `--join`, same state) turns it `online` with the same id.
- A node two API versions behind the host is refused with
  `version_mismatch` and the upgrade instruction; one behind is accepted.
- Stopping the host leaves the node process running (its socket still
  answers `/healthz`; `gaia nodes` there reports the host unreachable), and
  it re-registers when the host returns.
- The node's state directory holds no auth key (all files grepped for the
  test key) and nothing but `tsnet/`.
- `gaia nodes remove NAME` removes the node; an unknown name fails with a
  clear message and exit 1.
- The node opens no TCP listener outside tsnet; the auth key appears in no
  log line or process argument of the node.
- `make verify` and `make build-all` green; `TestDependencyRule` and
  `TestCLIDoesNotReachTsnet` pass.

## Test plan

- `internal/core`: `ValidNodeName` accepts/rejects; `NodePath`.
- `internal/host`: register over the tailnet handler takes identity from
  WhoIs (body identity ignored); heartbeat from another identity 403;
  register/heartbeat not served on the socket handler; version negotiation
  (host v3: node v3, v2 accepted; v1, v4, 0 refused with instruction and
  details); name conflict 409; `DELETE /api/v1/nodes/{name}` 200 and 404;
  `heartbeat_interval_ms` in the response; error → status mapping.
- `internal/node`: `Client` against an `httptest` host (`host.Server`'s
  tailnet handler with a fake WhoIs): registration, heartbeat, typed error
  decode; `Run`: re-register on not_found, backoff while the host is down
  then re-register, fatal on version_mismatch (message carries the
  instruction), stop on ctx; `Handler`: `/healthz` local, `/api/v1` forwarded
  unchanged, the node's own POSTs 404, host down → 503 unavailable.
- `internal/net`: `LookupPeer` by name and by IP, unknown name error;
  `WhoIs` returns a non-empty `StableID` (testcontrol).
- `cmd/gaia`: node flag validation (`--host` required, `--join` without a
  key, no tsnet state without `--join`, tailnet trap proves the checks run
  before `internal/net`); end-to-end with testcontrol: host `--tailnet
  --node` with a short heartbeat, nodes A and B joined; all three listed on
  the host and through each node; A stopped → offline, restarted → online
  same id; host stopped → B still running and answering, host restarted → B
  online again; node state dir holds only `tsnet/` and no key; no auth key
  in stderr or args; no TCP listener outside tsnet. CLI `nodes remove`
  against a fake daemon: success text, not found exit 1, bad name usage.

## Out-of-scope touches

None.

## Report

- **Built:** `core` (`HeartbeatPath`, `NodePath`, `ValidNodeName`,
  `RemoveNodeResponse`, `RegisterNodeResponse.heartbeat_interval_ms`);
  `host` (register/heartbeat on the tailnet mux with identity from WhoIs,
  heartbeat identity check, version negotiation via `Options.APIVersion`,
  `RemoveNode` and `DELETE /api/v1/nodes/{name}`, error → status map,
  forced close after the shutdown grace); `net` (`Peer.StableID`,
  `LookupPeer`, `HasState`); `node` (`Client`, `Run` with backoff,
  `Handler`, `Forwarder`, `HostTransport`, `MachineName`, `Request`);
  `cmd/gaia` (`gaia node`, host `--heartbeat-interval`/`--missed-heartbeats`,
  usage); `cli` (`gaia nodes remove NAME`); README "Adding a machine".
- **Changed during implementation:** TLS server name is the tailnet IP, not
  the MagicDNS name. With the MagicDNS name the host ordered a Tailscale
  certificate over ACME per new name (an outbound request to Let's Encrypt;
  testcontrol panics on the DNS challenge). Brief updated above.
- **Test limitation:** testcontrol checks the auth key on every
  registration, even from a known node key (`serveRegister`: `RequireAuthKey
  != "" && req.Auth.AuthKey != …` → "invalid authkey"); Tailscale and
  Headscale do not. `TestNodesOverTailnet` therefore runs a control server
  requiring no key, so the restart without a key can run; the daemons are
  still given the test key on `--join` and every leak check covers it.
  `TestJoinWrongAuthKeyFails` (0.6) proves `Join` presents the key.
- **Tests:** `TestNodesOverTailnet` (host `--tailnet --node` + node-a +
  node-b: all three listed on the host and through each node; node-a
  stopped → offline, restarted without key → online, same id; host stopped →
  node-b running, `/healthz` answers, `gaia nodes` there reports unreachable;
  host restarted → both re-register, same ids; node state holds only
  `tsnet/`, no key; no key in stderr/args; no TCP listener outside tsnet);
  node flag and start checks (start checks in a child behind the tailnet
  trap, plus `TestNodeTrapIsReal`); host register/heartbeat/negotiation/
  remove/status tests; node `Run` against a real host handler (heartbeats,
  re-register after removal, v+2 host refuses with the instruction, v+1
  accepts), backoff 10→20→40→40→40ms and re-register after failed
  heartbeats, `Handler`/`Forwarder`, `verifyHost`; CLI remove tests; net
  `LookupPeer`/`HasState`/`StableID`. `TestNodesOverTailnet` passed 4 of 4
  runs (~10s each).
- **Verify:** `make verify` green; `make build-all` produced six binaries.
