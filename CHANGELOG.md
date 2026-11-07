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
    `relay/provider/paystack`, `relay/provider/flutterwave`, `relay/provider/monnify` and
    `relay/provider/stripe`.
  - `relay config validate --provider <name>` accepts routes to providers added in code.
- **Flutterwave provider.**
  - `POST /webhooks/flutterwave` accepts `flutterwave-signature` (HMAC-SHA256) or `verif-hash`.
  - Routes `charge.completed` by `tx_ref` prefix, `meta`/`meta_data`, or a registered `tx_ref`.
  - Deduplicates on `event:data.id:status`.
  - Also accepts Flutterwave's legacy webhook format (a flat body with `"event.type"`, sent by
    accounts without v3 webhooks enabled). `*_TRANSACTION` charges are read as
    `charge.completed`, with `txRef` as the reference; they carry no `meta`. Other legacy types
    are stored as `unsupported`.
  - Configured under `providers.flutterwave`. Both providers are now optional, but at least one is
    required.
- **Monnify provider.**
  - `POST /webhooks/monnify` verifies `monnify-signature` (hex HMAC-SHA512 keyed with the client
    secret key).
  - Routes `SUCCESSFUL_TRANSACTION` by `paymentReference` prefix, `metaData`, or a registered
    reference. Reserved-account deposits route by their `accountReference` (`product.reference`)
    or by a `dedicated_account` registration of the receiving account, which the registration
    API now accepts for Monnify.
  - Deduplicates on `eventType:transactionReference:paymentStatus`.
  - Configured under `providers.monnify`. `confirm_with_api` (with `api_key_env`) checks each routed
    event with Monnify's transaction query API, caching the login token.
  - `allow_unsigned_sandbox` accepts Monnify's unsigned sandbox webhooks, but only for routed types
    that the sandbox API confirms. It requires `confirm_with_api` and
    `api_base_url: https://sandbox.monnify.com`.
- **Stripe provider.**
  - `POST /webhooks/stripe` verifies `Stripe-Signature` (HMAC-SHA256 over `t.body`). It accepts
    any `v1` under any configured signing secret, so secrets can be rolled, and rejects
    timestamps outside a 5-minute tolerance.
  - Routes `checkout.session.completed`, `checkout.session.async_payment_succeeded`,
    `checkout.session.async_payment_failed`, `checkout.session.expired`,
    `payment_intent.succeeded`, `payment_intent.payment_failed`, `payment_intent.processing` and
    `payment_intent.canceled`. They route by `client_reference_id` (Checkout) or
    `metadata.reference` prefix, by `metadata`, or by a registered reference. Other types, and
    all Connect events, are stored as `unsupported`.
  - Deduplicates on the event id.
  - Configured under `providers.stripe`. `livemode` is required and enforced per event.
    `confirm_with_api` (with `api_key_env`) re-fetches each routed event with
    `GET /v1/events/:id` and compares it.
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
  - Optional `confirm_with_api` for each provider: before accepting a routed event, Relay checks it
    against the provider's verify API (Paystack `GET /transaction/verify/:reference`,
    Flutterwave `GET /v3/transactions/:id/verify`, which needs the new
    `providers.flutterwave.secret_key_env`). Mismatches get `422 not_confirmed` and are never
    stored. Adapters can implement `provider.Confirmer`.
  - When the provider API is slow or down, the event is acknowledged (`200`), stored and routed,
    but held with `confirmation: pending` (migration 0004). A background worker retries the check
    with backoff for about 72 hours (30 s per call). It delivers the event once confirmed, marks
    it `rejected` (never delivered; its dedup key is released) if the provider disowns it, or
    `failed` when the window closes. `relay events confirm` retries it, and `relay events list
    --confirmation` filters on it. A different-bytes copy of a pending event gets `503` until the
    first one is settled. New metrics: `relay_confirmations_total` and
    `relay_events_awaiting_confirmation`. Previously an unreachable API got `503`, so an outage
    longer than the provider's retries lost the event.
  - A duplicate webhook whose reference or virtual account differs from the stored event is kept
    in `dedup_conflicts` (migration 0003). It is logged as an error, counted as
    `result="dedup_conflict"`, and shown by `relay events inspect`.
  - Per-application `reference_prefixes` namespaces. A registration inside another application's
    namespace, or under a `reference_prefix` route to another application, is refused with `409`.
    It no longer becomes `ambiguous` later. An application with a namespace can register only
    inside it.
  - A registration can claim webhooks that arrived before it only within its own namespace (72 h).
    Otherwise, including for virtual accounts, it can claim only ones from up to 15 minutes before
    it (`routing.RetroactiveGrace`).
  - Fuzz tests, and a chaos test that sends SIGKILL during ingest and delivery.
- **Packaging.** Static binaries (linux and darwin, amd64 and arm64), a distroless non-root
  Docker image, a docker-compose demo, and docs.
