# Launcher contract, v1

What `gaia run … --wait` promises to a caller that treats it as a local agent
process. Metis is the first such caller: the line goes in
`agents.<slug>.launch` (ADR-0009). Status: **draft until Phase 1's gate
passes**, then changed only with a major version (ADR-0001 rule 6).

The exit-code table below is read by a test in `internal/core`; the code and
this document cannot disagree.

## Invocation

```
gaia run --node NODE [--profile PROFILE] [--repo URL] [--branch BRANCH]
         --wait [--detach-on-interrupt] -- ARGV…
```

- `NODE` is the node's registry name, the `NAME` column of `gaia nodes`
  (`gaia-<machine name>` unless the node was given `--name`).
- Everything after `--` is the agent's argv, passed unchanged, with no shell.
- `--repo` and `--branch` default to the caller's working directory: its
  `origin` URL and its current branch.
- `--profile` is optional until Phase 2. Without it the session inherits the
  node user's environment and no credential is injected. With it, `ARGV[0]`
  must match the profile's surface.
- Standard input is not forwarded.

## What the caller sees

| Aspect | Behaviour |
|---|---|
| Output | The session's stdout and stderr, in order, on the caller's stdout, as they are produced. Gaia's own messages go to the caller's stderr, each line prefixed `gaia: `. |
| Exit code | The agent's exit code, unchanged, when the agent ran and its commits came back. Otherwise a code from the table below. |
| Agent killed by signal N | `128 + N` |
| Working directory | The node's checkout of `--repo` at `--branch`, fast-forwarded to the remote before the agent starts. |
| After exit | If the checkout is ahead, the node pushes the branch before `gaia run` returns. The caller then fetches; Gaia never touches the caller's checkout. |
| SIGINT or SIGTERM to `gaia run` | The remote session is stopped (SIGTERM, then SIGKILL after 10 s) and `gaia run` exits 130. With `--detach-on-interrupt` the session keeps running and `gaia run` exits 130 at once. |
| Caller's connection lost | The session keeps running. It can be found with `gaia ls` and followed with `gaia attach`. |

## Exit codes

Codes 90 to 99 are Gaia's own. No agent CLI Metis supports uses them.

| Code | Name | Meaning | Did the agent run? |
|---|---|---|---|
| 90 | `no_daemon` | No Gaia daemon answers on this machine's local socket | no |
| 91 | `node_unavailable` | The node is unknown or offline | no |
| 92 | `profile_rejected` | Profile or credential missing, or `ARGV[0]` does not match the profile's surface | no |
| 93 | `sync_before_failed` | The caller's branch is ahead of the remote, or the node's checkout is dirty or cannot fast-forward | no |
| 94 | `sync_after_failed` | The agent finished but the node could not push its commits; they are in the node's checkout | yes |
| 95 | `session_lost` | The node or the session's supervisor became unreachable and the outcome is unknown | unknown |
| 96 | `version_mismatch` | CLI, host and node versions are incompatible | no |
| 97 | `start_failed` | The node could not start `ARGV[0]` (not installed, not executable) | no |
| 98 | `usage` | Bad flags or arguments to `gaia run` | no |
| 99 | `internal` | Any other Gaia failure | unknown |

A caller that retries should retry only on 91 and 95, after a delay.

## What Gaia never does

- Choose the agent, model or slice (ADR-0009 rule 7).
- Reset, rebase, stash or force-push a checkout (ADR-0010).
- Write to `.metis/` (ADR-0001 rule 5).
- Alter the agent's output, other than redacting known secret values in the
  stored scrollback.

## What the caller must do

- Push the branch before calling (`sync_before_failed` otherwise).
- Fetch and fast-forward after `gaia run` returns, before reading the ledger.

Metis does both for an agent configured with `remote: true`, from v0.0.25.

## Example

```yaml
agents:
  claude-code/opus-5-5:
    surface: claude-code
    model: claude-opus-5-5
    launch: [gaia, run, --node, gaia-studio, --wait, --,
             claude, -p, "{prompt}", --model, "{model}",
             --permission-mode, acceptEdits,
             --allowedTools, "Bash(metis:*)", "Bash(git:*)"]
```
