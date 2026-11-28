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

(Filled during execution with actual evidence)
