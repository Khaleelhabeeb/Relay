# Changelog

All notable changes to Relay are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). The configuration file (`version: 1`), the
outbound delivery contract (`v1` signatures, `relay_version: 1` envelopes) and the `/v1`
registration API are versioned on their own. Breaking changes to them bump those versions.

## [Unreleased] — 0.1.0

First release: a self-hosted Paystack webhook router.

### Added
- **Go library.** Relay can be embedded with `go get github.com/khaleelhabeeb/relay`. See
  `docs/library.md`.
  - Package `relay`: `New`, `Handler`/`WebhookHandler`/`APIHandler`, `Run`/`ListenAndServe`,
    and in-process `RegisterReference`/`RegisterDedicatedAccount`/`Deactivate`. `relay serve` is
    now built on it.
  - Package `relay/config` (moved from `internal/config`): `Load`/`LoadFile`, plus
    `Config.Validate` for configurations built in Go.
  - Package `relay/receiver` verifies Relay's deliveries in Go (`Handler`, `Verify`, `Envelope`).
  - Package `relay/provider`: the `Adapter` interface. Add providers with `relay.WithProvider`.
    Adapters now declare their routed event types with `Routes`. Built-in adapters live in
    `relay/provider/paystack` and `relay/provider/flutterwave`.
  - `relay config validate --provider <name>` accepts routes to providers added in code.
- **Flutterwave provider.**
  - `POST /webhooks/flutterwave` accepts `flutterwave-signature` (HMAC-SHA256) or `verif-hash`.
  - Routes `charge.completed` by `tx_ref` prefix, `meta`/`meta_data`, or a registered `tx_ref`.
  - Deduplicates on `event:data.id:status`.
  - Configured under `providers.flutterwave`. Both providers are now optional, but at least one is
    required.
- **Provider-scoped routes.** The optional `routes[].provider` field limits a route to one provider.
- **`X-Relay-Provider` delivery header.** The envelope's `provider` names the event's real source.
- The `relay_webhooks_received_total` metric gains a `provider` label.
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
  - Provider signature checks fail closed: `paystack.Verify` and `flutterwave.Verify` return
    `ErrMissingSecret` for an empty secret, and `relay.New` rejects a built-in adapter passed to
    `WithProvider` without its secret.
  - Fuzz tests, and a chaos test that sends SIGKILL during ingest and delivery.
- **Packaging.** Static binaries (linux and darwin, amd64 and arm64), a distroless non-root
  Docker image, a docker-compose demo, and docs.
