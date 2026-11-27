---
type: adr
id: 0012
title: Gaia is open source, self-hosted, signed and free of telemetry
status: accepted
date: 2026-11-20
phase: 0
---

# ADR-0012: Gaia is open source, self-hosted, signed and free of telemetry

> Phase: 0 | Status: accepted | Date: 2026-11-20
> Decision drivers: Gaia ships alongside Metis as open source, and holds its users' credentials.

## Context

Metis is MIT licensed, released with GoReleaser and checksums, and installed
by script. Gaia holds every credential its users have and can expose a public
endpoint, so people will rightly ask whether they can trust the binary, the
project and the defaults before running it.

## Decision

Release Gaia under MIT in the same organisation as Metis, as self-hosted
software only, with signed releases, a private vulnerability process, no
telemetry, and defaults that are safe for a stranger who reads no docs.

### Rules this decision implies:

1. MIT licence. Contributions under the same licence; CONTRIBUTING.md as in Metis.
2. No telemetry, update pings or crash reporting. Gaia makes no network call the user did not configure.
3. Releases: GoReleaser, checksums, keyless signatures (cosign) and an SBOM per artifact. Install scripts verify the checksum and, where available, the signature.
4. SECURITY.md with a private reporting channel and a response target. Security fixes are released before they are discussed publicly.
5. Safe defaults: local-only mode, public link off, remote input off, no credentials until added.
6. No project-run service. Anything that would need one (relay, accounts, push notifications through our servers) is out of scope.
7. Examples, fixtures and docs contain no real hostnames, accounts or credentials.
8. Provider terms are documented, not interpreted for users: docs explain which credential goes to which client and link the provider's terms.

## Consequences

### Positive
- Users can audit what holds their credentials.
- Same release story as Metis.
- Enterprises can adopt it without a vendor relationship.

### Negative
- Signing and SBOM add release steps.
- Push notifications must use user-configured channels (webhooks, ntfy, email), not a project service.

### Neutral
- The two projects share conventions, not code (ADR-0001).

## Alternatives Considered

| Option | Pros | Cons | Why rejected |
|---|---|---|---|
| Open core with hosted relay | Funding route; easier remote access | Trust in our service, accounts | Self-hosted only |
| Copyleft (AGPL) | Protects against closed forks | Enterprise friction; differs from Metis | Consistency with Metis |

## References

- ADR-0001, ADR-0004, ADR-0011
- `metis/.goreleaser.yml`, `metis/LICENSE`
