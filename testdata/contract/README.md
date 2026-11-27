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
| `gaia/api-v1/nodes.json` | `gaia nodes -o json` with one node in local-only mode; neutral id, name and time | `cmd/gaia/cli` (shape, byte for byte) and `cmd/gaia` (against a live `gaia host --node`) |

A change to one of these shapes is a breaking change for its readers. The launcher contract Gaia provides is
`docs/contracts/launcher-v1.md`.

To refresh: record again from the named version into a new directory, and
keep the old one for as long as that version is supported.
