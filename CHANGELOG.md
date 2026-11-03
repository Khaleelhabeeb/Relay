# Changelog

All notable changes to Relay are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). The configuration file (`version: 1`), the
outbound delivery contract (`v1` signatures, `relay_version: 1` envelopes) and the `/v1`
registration API are versioned on their own. Breaking changes to them bump those versions.

## [Unreleased] — 0.1.0

First release: a self-hosted Paystack webhook router.

### Added
- **Inbound webhooks.** `POST /webhooks/paystack` verifies HMAC-SHA512 over the exact raw body.
  It acknowledges Paystack only after the event, its routing decision and any delivery are
  committed to SQLite with `synchronous=FULL`. Repeated webhooks are deduplicated, on
  `charge.success:<data.id>` or else the SHA-256 of the body.
- **Ownership routing.**
  - Static reference-prefix and metadata rules, plus registered transaction references and
    dedicated virtual accounts.
  - Unique-owner policy: `matched`, `unresolved`, `ambiguous` or `unsupported`. Relay never
    broadcasts.
  - Statically overlapping routes are rejected at startup.
- **Delivery.**
  - Bounded worker pool with leases, recovery after crashes, and exponential backoff with
    jitter (10 attempts over about 24 h).
  - `original` or `envelope` payloads, and HMAC-SHA256 `v1` signatures over timestamp, event
    ID, delivery ID and body, with secret rotation.
  - SSRF protection: connect-time IP checks, no redirects, proxy settings ignored.
- **Registration API.**
  - `/v1/registrations`, authenticated with a per-application bearer token, rate limited, and
    optionally on a private listener.
  - Reuse rules: references are never reassignable; virtual accounts follow a time window.
  - Webhooks that arrived before their registration (last 72 h) are routed automatically.
- **Operations.**
  - CLI: `events list|inspect|reprocess|reroute`, `deliveries list|retry`, `registrations list`.
    It runs alongside the server.
  - `/healthz`, `/readyz` (a real write probe) and Prometheus `/metrics`.
  - Retention purge, single-instance lock file, forward-only migrations.
- **Hardening.**
  - Optional source-IP allowlist that is aware of trusted proxies.
  - Concurrency cap on inbound webhooks (excess get `503` with `Retry-After`).
  - The database file is created owner-only.
  - Fuzz tests, and a chaos test that sends SIGKILL during ingest and delivery.
- **Packaging.** Static binaries (linux and darwin, amd64 and arm64), a distroless non-root
  Docker image, a docker-compose demo, and docs.
