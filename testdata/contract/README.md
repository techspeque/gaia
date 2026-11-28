# Contract fixtures

Recorded output of the other tools' CLIs, which Gaia reads and never
imports (ADR-0001, ADR-0013). One directory per tool and version.

| Directory | Source | Read by |
|---|---|---|
| `metis/v0.0.23/` | `metis next`, `status`, `progress`, `show` and `handoff`, each with `-o json`, recorded from Metis 0.0.23 | `internal/metis`, Phase 3 |
| `ship/v0.0.1/` | Copies of Ship's fixtures for `ship projects`, `status`, `deployments` and `logs`. The last three are provisional shapes written before Ship's Phase 1 code | `internal/ship`, Phase 4 |

Gaia's own outputs get their fixtures here as the slices that produce them
land, one directory per API version:

| Directory | Source | Checked by |
|---|---|---|
| `gaia/api-v1/nodes.json` | `gaia nodes -o json` with one node in local-only mode (default name `gaia-<machine name>`, no tailnet name); neutral id, name and time | `cmd/gaia/cli` (shape, byte for byte) and `cmd/gaia` (against a live `gaia host --node`) |
| `gaia/api-v1/sessions-empty.json` | `gaia ls -o json` with no sessions | `internal/core` (shape, byte for byte); the `gaia ls` writer once it lands |
| `gaia/api-v1/sessions.json` | `gaia ls -o json` with one session in each state, oldest first; neutral ids, names, repository and times | `internal/core` (shape, byte for byte, one session per state); the `gaia ls` writer once it lands |

A change to one of these shapes is a breaking change for its readers. The launcher contract Gaia provides is
`docs/contracts/launcher-v1.md`.

To refresh: record again from the named version into a new directory, and
keep the old one for as long as that version is supported.
