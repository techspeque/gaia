# phase-0-gate — Phase 0 gate: composed-system validation

- **Type:** gate | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27

## Phase being validated

Phase 0: Foundation. Run 1: release v0.0.1 (commit b9b2838). Run 2: release
v0.0.2 (commit 948f2d8607e9), carrying fix-0003. Both public GitHub releases,
keyless-signed, installed with `scripts/install.sh` from `main`
(`GAIA_VERSION=… GAIA_REQUIRE_SIGNATURE=1`), as the login user, no root.

Machines: gate host A runs `gaia host`; gate host B (the GPU machine) runs
`gaia node`; the laptop is a third device of the same tailnet user, joined
through the system Tailscale client only. Both gate hosts are already on the
tailnet through their own system Tailscale; Gaia joins as separate devices
through its embedded client. Host names, addresses and the tailnet's domain
stay in `.local/` (gitignored) and `~/gaia-gate/` on the hosts.

## Composition scenarios

From `.metis/plans/phase-0-plan.md`, Phase Gate, followed through README as
written; where README is wrong, that is a finding:

1. One machine: install.sh release build, `gaia host --node`; `gaia nodes -o json` lists it `online`; no TCP listener
2. Two machines on a tailnet: `gaia host --tailnet` on A, `gaia node --host … --join` on B; `gaia nodes` on both lists both
3. Stopping the node shows it `offline`; bringing it back shows `online` without a second join
4. Restarting the host keeps both nodes registered with the same ids
5. Gaia's TCP listeners only on the tailnet interface and 127.0.0.1, plus the local socket; the only other socket is the WireGuard UDP port
6. A tailnet device not on the allowlist is refused by the host API (the laptop: 403 and the host's warning line)
7. Every `-o json` output matches its recorded fixture (`testdata/contract/gaia/api-v1/nodes.json`)
8. Both daemons run under the unit from `gaia service install`, no root, up after a reboot (the reboot is the Human's); `gaia service uninstall` leaves the state directory

Performance: `gaia nodes` under 100 ms on the host and under 500 ms on a node;
idle RSS of host and node recorded in README; release binary size recorded in
README.

Allowlist: the node is admitted by `node:<name>`, not by login, so that the
laptop (same login) shows the refusal. The auth key is used only through
`--auth-key-file`; afterwards it is checked to appear in no log, unit,
process list or state file other than tsnet's own state.

## Evidence criteria

- [x] All phase slices coded and reviewed (ws-0.1..0.9, fix-0001..0003 archived)
- [x] Cross-module integration tests pass — run 2: all eight scenarios evidenced on v0.0.2
- [x] No interface mismatches at seam points — run 2: node and host 0.0.2 over the tailnet; `gaia nodes -o json` matches its fixture
- [x] Performance/resource usage acceptable — run 2: figures in the report and README; the f-012 window after an outage is for the Reviewer

## Declared file scope

- **owned_paths:** .metis/briefs/phase-0-gate.md, README.md (the footprint figures the plan asks for)
- **read_only_paths:** everything else

## Report

### Run 1 — 2026-11-27, v0.0.1 — verdict FAIL (scenario 2 as documented)

Gate host A and gate host B: Ubuntu 26.04 x86_64, systemd 259, lingering on
for the login user, both already on the tailnet through the system Tailscale
client under their machine names. `metis verify --pre`: ALL GREEN. Install,
both hosts, as the login user: `install.sh` (sha256 `e04cfa8a…09adb94`) from
`main` with `GAIA_VERSION=v0.0.1 GAIA_REQUIRE_SIGNATURE=1`: "signature of
checksums.txt verified", "checksum … verified", `~/.local/bin/gaia`
`gaia 0.0.1 (commit b9b2838) linux/amd64`, sha256 `a23818d3…b7103e` on both.
Everything below was run by the agent over ssh without sudo, 19:04–19:25
GMT. Raw evidence: `.local/gate/` (gitignored) and `~/gaia-gate/` on the
hosts. Below, `<A>` and `<B>` stand for the machine names.

#### Scenario 1: one machine, local-only — PASS (host A)

`gaia host --node --state ~/gaia-gate/s1-state`: `gaia nodes -o json` lists
one node, `tailnet_identity` `local`, `state` `online`. The process's sockets
(`ss -anp` filtered to its pid): one unix listener `/tmp/gaia-1000/gaia.sock`
(no `XDG_RUNTIME_DIR` in these ssh sessions), no TCP or UDP socket at all.
SIGTERM: "host stopped", exit 0, state directory left (`gaia.db`).

#### Scenario 2: two machines on a tailnet — FAIL as documented; PASS with the assigned names

- **As README says:** `gaia host --tailnet --node --auth-key-file
  ~/.config/gaia/authkey --allow node:<B>` on A (joined: "host started",
  `tailnet: true`), then `gaia node --host <A> --join --auth-key-file …` on B.
- **Actual:** the control server gave Gaia's devices the names `<A>-1` and
  `<B>-1` (`tailscale status --json` on A and B, before and after: the
  machines' own devices hold `<A>` and `<B>`). Gaia logged only the address
  it was given, never the name. The node's `--host <A>` resolved to A's own
  Tailscale device: "registration with the host failed; retrying …
  connect tcp <A's system tailnet address>:443: connection was refused",
  backing off to 16 s, with no hint, indefinitely.
- **With `--host <A>-1`:** the node reached the host and got 403; the host
  logged "tailnet request refused: peer not allowlisted … node: <B>-1".
  `node:<B>` (the node's registry name, its machine name) does not match:
  the allowlist sees the tailnet name. README says `--name` is "the node's
  name in the registry and on the tailnet" and suggests `node:NAME`.
- **With `--allow node:<B>-1`** (host restarted, no auth key: joined from
  its state): "node registered … name <B>". `gaia nodes` on A and on B both
  list `<A>` and `<B>` `online`, linux/amd64, 0.0.1. The registry keeps the
  unsuffixed `<B>`.
- **Why it fails:** the names `--host` and `node:` need exist only after
  each daemon has joined, are not printed by Gaia, and differ from the
  machine names README uses whenever the machine already runs Tailscale
  (ADR-0004 promises a stable name from `--join`). Filed as fix-0003.

#### Scenario 3: node offline and back without a second join — PASS

Stopped the node (process at 19:07:26, then the user service at 19:09:52):
`gaia nodes` on A shows B `offline` at 30 s and 41 s since its last
heartbeat (10 s × 3). Started again with no `--join` and no key: "node
registered" with the same id; `online` within 6 s; still one Gaia device for
B on the tailnet (no `<B>-2`). Stopping the service, not suspending the
machine.

#### Scenario 4: host restart keeps ids — PASS

`gaia nodes -o json` before and after a host restart (19:08:32, from state,
no key): both ids identical (`node_4d8w…` local, `node_xnhd…` for B with
the same tailnet identity); host logs "node registered … updated: true" for
its own node; B `online` 9 s later without re-registering.

#### Scenario 5: listeners — PASS (both hosts)

`ss -tulnap` and `ss -xlp` for the Gaia pid, under the user services:
no kernel TCP listener on either host; one unix listener
`/tmp/gaia-1000/gaia.sock` (mode `srw-------`); two UDP sockets, IPv4 and
IPv6, whose port is the one tsnet logs as its WireGuard endpoint
("endpoints changed: … :34875 (local)"). The host's API is on the tailnet
only, inside the embedded netstack: from the laptop over the tailnet, A-1
port 443 open, 80 and 8080 closed; A's LAN address port 443 refused; A's
system-Tailscale address port 443 refused. B's Gaia device: 443 and 80
closed. Outbound TCP only to Tailscale's control and DERP servers
(reverse-resolved). Logtail upload is off (`internal/net/tailnet.go:81`).

#### Scenario 6: a tailnet device not on the allowlist is refused — PASS

The laptop (same tailnet user, system Tailscale only) at 19:06:50:
`curl -k https://<A>-1.<tailnet>/api/v1/nodes` → `403`,
`{"error":{"code":"forbidden","message":"permission denied","details":{}}}`.
Host: `WARN "tailnet request refused: peer not allowlisted" … node: <laptop>`.
The node, admitted by `node:<B>-1` and not by login, registered and
heartbeats. Without `-k` curl fails: the host serves its self-signed
certificate (IP SANs only) because the tailnet has no HTTPS certificates,
as README describes.

#### Scenario 7: `-o json` matches its fixture — PASS

`gaia nodes` is the only `-o json` command in v0.0.1. Outputs from A local-only,
A tailnet host and B node, against `testdata/contract/gaia/api-v1/nodes.json`:
same keys in the same order, same value types, same line layout
(2-space indent) once strings and numbers are masked; ids match
`^node_[0-9a-z]{26}$`.

#### Scenario 8: user services, no root, reboot, uninstall — needs the Human (reboot)

- After the hand join and stop, as README says: on A `gaia service install
  host --tailnet --node --allow node:<B>-1`, on B `gaia service install node
  --host <A>-1`. Each wrote `~/.config/systemd/user/gaia.service` and started
  it; unit enabled (`WantedBy=default.target`), `ExecStart` with absolute
  `--state`, no key, `UnsetEnvironment=XDG_RUNTIME_DIR`/`TMPDIR` (the ssh
  sessions have neither), `NRestarts=0`, process uid 1000.
- `gaia service uninstall` on B: "stopped the service and removed … the
  state directory is left in place"; unit `not-found`, no process; the state
  directory's file listing (mode, owner, size, path) identical before and
  after. Reinstalled: B online again with the same id.
- **Not yet evidenced:** both up after a reboot. Needs `sudo reboot` by the
  Human.

#### Auth key

Used only through `--auth-key-file`. Checked afterwards on both hosts by
fixed-string match against the key file (blank lines removed), printing no
content: no hit in `~/gaia-gate/`, the state directory (including
`tsnet/tailscaled.state`), the unit files, `~/.local/share/tailscale`,
`/tmp/gaia-1000`, the user journal, or any own process's cmdline or environ.

#### Performance

- `gaia nodes`, 20 runs each: host A 6–8 ms; node B 9–11 ms (forwarded to the
  host over the tailnet). Thresholds 100 ms and 500 ms.
- Binary: linux/amd64 26,177,698 bytes; archives 8.6–10.6 MB. In README.
- Idle, under the user services, 60 s sample from `/proc/PID/stat` and
  `status`: host A (up 12 min) VmRSS 41,788 kB (HWM 42,572 kB), 8 CPU ticks
  in 60 s (0.08 s); node B (up 10 min) VmRSS 40,316 kB, 11 ticks (0.11 s).
  Cgroup `MemoryCurrent` about 20 MB each. In README "Footprint".

### Findings

| Finding | Severity | Offending slice | Filed as |
|---|---|---|---|
| Machine already on the tailnet: Gaia's devices get suffixed tailnet names; README's `--host <machine>` reaches the wrong device and `node:<name>` is refused; Gaia never reports the name it was given | P1 (slice p0) | phase-0-ws-0.6, phase-0-ws-0.7 (archived) | fix-0003 |
| TLS handshake error log names no peer (`%!s(<nil>)`) | advisory | phase-0-ws-0.6 | f-011 |
| One 10–16 s stall of forwarding and heartbeat after a node restart following a 40 s outage | advisory | phase-0-ws-0.7 | f-012 |
| Empty `~/.local/share/tailscale` created outside the state directory | advisory | phase-0-ws-0.6 | f-013 |

### Verdict

**FAIL.** Scenario 2 fails through the steps README documents, on the
ordinary setup of machines that already run Tailscale. fix-0003 is inserted
before this gate. Scenarios 1, 3–7 and the performance checks pass on
v0.0.1 with the assigned names; scenario 8 waits for the reboot. The gate is
re-run from scenario 2 on the release carrying fix-0003.

### Run 2 — 2026-11-28, v0.0.2 — Coder's evidence complete; verdict is the Reviewer's

Same hosts, same tailnet, `<A>`/`<B>` as in run 1. `metis verify --pre`
on the branch after fix-0003: ALL GREEN. Install, both hosts, 10:05–10:06 GMT: the same `install.sh`
(sha256 `e04cfa8a…09adb94`) with `GAIA_VERSION=v0.0.2
GAIA_REQUIRE_SIGNATURE=1` over the running v0.0.1: "signature of
checksums.txt verified", "checksum of gaia_0.0.2_linux_amd64.tar.gz
verified", `gaia 0.0.2 (commit 948f2d8) linux/amd64`, sha256
`53649b6c…7c0919` on both. Run by the agent over ssh as the login user,
no sudo, 10:05–10:22 GMT. Raw evidence: `.local/gate/run2/` and
`~/gaia-gate/run2/` on the hosts. No run 1 result is carried over.

#### Moving the v0.0.1 installation to the new names — PASS, as README says

README "Machines set up with Gaia v0.0.1", step by step:
- Stopped both user services; copied each state directory to
  `~/gaia-gate/run2/v001-state-backup/`; deleted `<state>/tsnet` (and
  `<state>/name`, absent) on both; kept the host's `gaia.db`.
- **Deviation, deliberate:** README's step "remove the device from the
  tailnet" is the Human's (admin console) and is deferred. It is not needed
  for the new names: the old devices hold `<A>-1` and `<B>-1`, the new ones
  ask for `gaia-<A>` and `gaia-<B>`. The two old devices are offline since
  10:06 and stay listed in the tailnet until the Human deletes them.
- Host: `gaia host --tailnet --node --name gaia-<A> --auth-key-file … --allow
  node:gaia-<B>` (README says `$GAIA_AUTHKEY`; the key file is the same input
  path). Log: `"tailnet name assigned","asked":"gaia-<A>","tailnet_name":"gaia-<A>"`;
  `host started … name gaia-<A>, tailnet_name gaia-<A>`; its own node
  re-registered with the same id as in run 1 (`node_4d8w…`, `updated: true`)
  under the new name. `<state>/name` = `gaia-<A>`.
- Node: `gaia node --host gaia-<A> --join --auth-key-file …`, no `--name`
  (README's fresh-node command). Log: `node started … name gaia-<B>`,
  `"tailnet name assigned","asked":"gaia-<B>","tailnet_name":"gaia-<B>"`,
  `node registered` (new id `node_77m6…`, new tailnet identity), first try,
  no 403. `<state>/name` = `gaia-<B>`.
- `gaia nodes remove <B>` removed the v0.0.1 entry (`node_xnhd…`), as README
  says to.

#### Scenario 1: one machine, local-only — PASS (host B)

During the scenario 3 stop on B: `gaia host --node --state
~/gaia-gate/run2/s1-state`. `gaia nodes -o json`: one node `gaia-<B>`,
`tailnet_identity` `local`, `tailnet_name` `""`, `online`. Sockets of the
pid: only the unix listener `/tmp/gaia-1000/gaia.sock`; no TCP or UDP.
`gaia nodes` 6–7 ms (20 runs). SIGTERM: `host stopped`, exit 0; state
directory left (`gaia.db`, `name`).

#### Scenario 2: two machines on a tailnet — PASS (README as written)

From the steps above: both devices got exactly the names they asked for;
the node reached the host with README's `--host gaia-<A>` and was admitted
by `node:gaia-<B>`, the `tailnet_name` it logged. `gaia nodes` on A and on
B: `gaia-<A>` and `gaia-<B>`, `TAILNET NAME` equal to the name, `online`,
linux/amd64, 0.0.2.
- The fix-0003 hint, checked with B's joined state and no `--join`:
  `gaia node --host <A>` logs "connect tcp …:443: connection was refused:
  --host <A> is <A>.<tailnet>, where nothing answers on port 443, so it is
  not a Gaia host; a Gaia host asks for the tailnet name gaia-<machine name>
  and logs the tailnet_name it was given". `--host <A>-1` (the stale, offline
  v0.0.1 device) times out with no hint (advisory f-018).
- Stale devices do not interfere: `gaia-<A>` matches one peer (MagicDNS first
  label), the allowlist names only `node:gaia-<B>`, and every registration
  and heartbeat after the move went to the new devices.

#### Scenario 3: node offline and back without a second join — PASS

Twice. (a) Node process stopped 10:07:01; at 10:07:44 A lists `gaia-<B>`
`offline`, 50 s since the last heartbeat; restarted with README's `gaia node
--host gaia-<A>` (no key): `node registered`, same id `node_77m6…`.
(b) Node user service stopped 10:09:47; at 10:10:27 `offline`, 40 s;
`systemctl --user start`: `online`, same id. The tailnet lists one
`gaia-<B>` device throughout (no `-1`). Stopping, not suspending.
Both times f-012 recurred: see Performance.

#### Scenario 4: host restart keeps ids — PASS

Host restarted 10:08:35 with no `--name` and no key: log `asked: gaia-<A>`
(from `<state>/name`), `tailnet_name gaia-<A>`. `[id, name,
tailnet_identity, tailnet_name]` of both nodes identical before and after
(`diff`: none); both `online` within 10 s.

#### Scenario 5: listeners — PASS (both hosts)

Under the user services (`ss -tulnap`, `ss -xlp` for the unit's MainPID):
no kernel TCP listener; unix listener `/tmp/gaia-1000/gaia.sock`
`srw-------`; two UDP sockets (IPv4, IPv6) whose port is the one tsnet logs
as its WireGuard endpoint ("endpoints changed: … :34778 (local)" on A,
":54516" on B). TCP otherwise only outbound to Tailscale's control and DERP
servers. From the laptop over the tailnet: `gaia-<A>` port 443 open, 80 and
8080 closed; `gaia-<B>` 443 and 80 closed; A's and B's LAN addresses and
A's system-Tailscale address, port 443, refused.

#### Scenario 6: a tailnet device not on the allowlist is refused — PASS

Laptop, 10:09:03: `curl -k https://gaia-<A>.<tailnet>/api/v1/nodes` → 403,
`{"error":{"code":"forbidden","message":"permission denied","details":{}}}`.
Host: `WARN "tailnet request refused: peer not allowlisted" … node: <laptop>`.
Same login as the admitted node, so admission is by `node:` only. Without
`-k` the certificate is refused (self-signed; tailnet HTTPS certificates
off), and f-011's log line recurs.

#### Scenario 7: `-o json` matches its fixture — PASS

`gaia nodes` is still the only `-o json` command. The fixture
`testdata/contract/gaia/api-v1/nodes.json` gained `tailnet_name` in fix-0003
(commit 6d43791). B local-only, A host and B node outputs: same keys in the
same order, same types, same line layout; ids match `^node_[0-9a-z]{26}$`;
`tailnet_name` `""` local-only, the assigned names on the tailnet.

#### Scenario 8: user services, no root, reboot, uninstall — PASS (both hosts)

- After the hand runs, as README says: A `gaia service install host
  --tailnet --node --allow node:gaia-<B>` (rewrote the v0.0.1 unit, "wrote …
  and started gaia host"); B `gaia service install node --host gaia-<A>`.
  Units: `ExecStart` with absolute `--state`, no `--name` (taken from
  `<state>/name`), no key; enabled; `NRestarts=0`; processes run as the
  login user.
- `gaia service uninstall` on B: "stopped the service and removed … the
  state directory is left in place"; unit `not-found`; no process; file
  listing and sha256 of `name` and `tsnet/tailscaled.state` identical before
  and after. Reinstalled: online, same id.
- **Reboot (the Human's `sudo reboot`, both hosts), checked 10:35 GMT:**
  - Host A: boot 10:29:46 (`uptime -s`; `journalctl --list-boots` boot 0
    from 10:29:48), later than the unit (written 10:09:12). The user
    manager started `gaia.service` at 10:29:54 in this boot
    (`ActiveEnterTimestamp`), MainPID started 10:29:53, `NRestarts=0`, as
    the login user. Journal: `tailnet name assigned … asked gaia-<A>,
    tailnet_name gaia-<A>`, `host started`, its own node `node_4d8w…`
    re-registered (`updated: true`), B's `node_77m6…` registered at 10:30:00.
  - Node B: boot 10:29:38, later than the unit (10:11:16); service active
    from 10:29:45 in this boot, `NRestarts=0`. Journal: `tailnet name
    assigned … gaia-<B>`; two registration retries ("context deadline
    exceeded", while A was still booting), then `node registered` 10:30:00
    with the same id. No auth key: the unit has none, and no `--join`.
  - `gaia nodes -o json` on A and on B: the same two ids as before the
    reboot, names and `tailnet_name` `gaia-<A>`/`gaia-<B>`, `online`, 0.0.2.
  - Tailnet peers seen from each host: the laptop, the other host's own
    device, `gaia-<A>`, `gaia-<B>`; no new Gaia device. The two stale
    v0.0.1 devices (`<A>-1`, `<B>-1`) no longer appear: the Human has
    deleted them.
  - Auth key, this boot: no hit in the user journal, the state directory,
    the units, or any own process's cmdline or environ.

#### Auth key

Only `--auth-key-file`. Afterwards on both hosts, fixed-string match against
the key file with blank lines removed, no content printed: no hit in
`~/gaia-gate/` (including the v0.0.1 state backup), the state directory, the
unit files, `~/.local/share/tailscale`, `/tmp/gaia-1000`, the user journal
(also no `tskey-` prefix), or any own process's cmdline or environ.

#### Performance

- `gaia nodes`, steady state, 20 runs each, text and json: host A 6–8 ms;
  node B 9–10 ms. Thresholds 100 ms and 500 ms: PASS at steady state.
- **Not under 500 ms right after a node returns from an outage (f-012,
  reproduced twice, f-017):** under the service, 1 s after start exit 90 (no
  daemon yet), 11 s exit 99 after 10,020 ms, 16 s 4,060 ms, then 14–15 ms.
  The Reviewer decides whether the plan's check covers this window.
- Binary: linux/amd64 26,198,178 bytes; archives 8.6–10.6 MB (26 assets).
- Idle, under the user services, 60 s sample from `/proc/PID/stat` and
  `status`: host A (up 12 min) VmRSS 41,664 kB (HWM 43,104 kB), 9 CPU ticks
  in 60 s (0.09 s); node B (up 11 min) VmRSS 39,904 kB (HWM 41,152 kB), 10
  ticks (0.10 s). Cgroup `MemoryCurrent` 19.5 and 19.9 MB; `NRestarts=0`.
  README "Footprint" updated to v0.0.2.

#### Run 2 findings

| Finding | Severity | Status |
|---|---|---|
| fix-0003 (run 1's P1) | — | Fixed on v0.0.2: scenario 2 passes as README is written |
| f-011 TLS handshake log names no peer | advisory | Seen again (10:09:03) |
| f-012 stall after a node returns from an outage | advisory | Reproduced twice; evidence in f-017 |
| f-013 empty `~/.local/share/tailscale` | advisory | Still referenced: v0.0.2 logs "using system state directory" there |
| f-018 stale `--host` gives no hint | advisory | New |

#### Run 2 status

Every scenario and performance check has evidence on v0.0.2, scenario 8's
reboot included (the Human's reboot, 10:29 GMT). The open question for the
Reviewer: whether the `gaia nodes` under 500 ms check covers the f-012
window after a node returns from an outage. Open advisories f-010, f-011,
f-012/f-017, f-013, f-018 are to become fix slices after the review.
Flipped coded for review.
