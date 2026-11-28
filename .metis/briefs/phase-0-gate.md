# phase-0-gate — Phase 0 gate: composed-system validation

- **Type:** gate | **Risk:** high | **Priority:** p2
- **Coder:** claude-code/opus-5-5 | **Date:** 2026-11-27

## Phase being validated

Phase 0: Foundation. Release v0.0.1 (public GitHub release, keyless-signed),
installed with `scripts/install.sh` from `main`
(`GAIA_VERSION=v0.0.1 GAIA_REQUIRE_SIGNATURE=1`), as the login user, no root.

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

- [ ] All phase slices coded and reviewed
- [ ] Cross-module integration tests pass
- [ ] No interface mismatches at seam points
- [ ] Performance/resource usage acceptable

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
