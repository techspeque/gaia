---
type: adr
id: 0009
title: gaia run is a drop-in launcher for every Metis session, headless or interactive
status: accepted
date: 2026-11-20
phase: 1
---

# ADR-0009: gaia run is a drop-in launcher for every Metis session, headless or interactive

> Phase: 1 | Status: accepted | Date: 2026-11-20
> Decision drivers: Metis must direct any session to any selected coding tool on any machine, with or without `metis auto`.

## Context

Metis starts agents in three ways:

1. `metis auto` runs coder and reviewer sessions in a loop.
2. `metis launch [<slice>]` runs one headless session with the slice's handoff prompt.
3. A person runs `metis handoff`, picks the agent named in the prompt, and starts a session by hand.

1 and 2 use the agent's argv: `agents.<slug>.launch`, else the surface
default (`metis/internal/launch/launch.go`). They expect a process that runs
in the repo, streams output and exits with the agent's exit code. 3 has no
launcher; the person is the launcher.

## Decision

`gaia run` behaves like a local agent process, so any Metis agent can point
at a Gaia node and profile through its `launch` argv. Gaia also starts
interactive sessions from a slice for path 3.

**Headless (paths 1 and 2):**

```yaml
agents:
  claude-code/opus:
    surface: claude-code
    model: opus
    launch: [gaia, run, --node, studio, --profile, claude-max, --wait, --,
             claude, -p, "{prompt}", --model, "{model}",
             --permission-mode, acceptEdits,
             --allowedTools, "Bash(metis:*)", "Bash(git:*)"]
```

**Interactive (path 3):**

```
gaia run --node studio --profile claude-max --interactive --slice ws-1.2
```

Gaia reads `metis handoff ws-1.2 -o json` in the repo, starts the agent's CLI
interactively on the node with that prompt, and attaches. The person can
detach and reattach from any machine. The UI offers the same as "start
session" on a slice.

### Rules this decision implies:

1. `--wait` streams the session's stdout and stderr to the caller and exits with the agent's exit code.
2. Gaia's own failures (node offline, credential missing, repo sync failed) exit with codes from a reserved range, documented, distinct from any agent exit code Metis interprets.
3. Interrupting `gaia run --wait` (SIGINT/SIGTERM) stops the remote session. `--detach-on-interrupt` keeps it running instead.
4. `--repo` defaults to the caller's repo: its git remote URL and current branch (ADR-0010).
5. Everything after `--` is passed as argv unchanged, with no shell, so `{prompt}` never needs quoting.
6. Profiles name a surface; Gaia refuses a run whose argv[0] doesn't match the profile's surface.
7. Gaia never chooses the agent, model or slice. Metis or the person does.

## Consequences

### Positive
- Every Metis path can use any tool on any machine; no Metis change for headless.
- Coder on one machine and tool, reviewer on another, set per agent in `metis.yaml`.
- Interactive slice sessions become remote, detachable and visible.

### Negative
- `metis.yaml` launch lines get long and machine-specific. Mitigation: a later Metis feature could read the node and profile from user config; out of scope here.
- Exit-code and signal behaviour across the network must be tested as carefully as a local process.
- Remote sessions depend on repo sync (ADR-0010).

### Neutral
- `metis launch --dry-run` shows the full `gaia run` argv, which helps debugging.

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Gaia only runs `metis auto` | Simplest | No remote single or manual sessions | Too narrow |
| A `gaia` surface in Metis | Short config | Metis code depends on Gaia | ADR-0001 |
| Gaia picks tool and node per slice | Less config | Gaia makes decisions that belong to Metis or the person | Rule 7 |

## References

- ADR-0001, ADR-0003, ADR-0010
- `metis/internal/launch/launch.go`, `metis handoff --help`
