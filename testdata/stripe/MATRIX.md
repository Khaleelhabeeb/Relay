# Stripe fixture compatibility matrix

Every fixture here is **synthetic**. They were hand-written from the event and object shapes in
Stripe's public API reference (docs.stripe.com, checked 2026-11-05), with every identifier
sanitized. Before the routing a fixture covers counts as verified, replace it with a sanitized
payload captured from a test-mode integration, for example with
`stripe listen --forward-to localhost:8080/webhooks/stripe` and `stripe trigger <event>`.

Legend: ✅ present · — absent

| Fixture | `type` | Object | Transaction Relay uses | Reference Relay uses | `metadata` | Routing | Verified with real payload |
|---|---|---|---|---|---|---|---|
| `checkout_session_completed.json` | `checkout.session.completed` | `checkout.session` | `payment_intent` | `client_reference_id` (`APPA_…`) | `{}` | reference prefix / registered reference | ☐ |
| `checkout_session_completed_metadata.json` | `checkout.session.completed` | `checkout.session` | `payment_intent` | — (`client_reference_id: null`) | ✅ `application` | metadata match | ☐ |
| `payment_intent_succeeded.json` | `payment_intent.succeeded` | `payment_intent` | `id` | `metadata.reference` (`APPA_…`) | ✅ `reference` | reference prefix / registered reference | ☐ |
| `payment_intent_payment_failed.json` | `payment_intent.payment_failed` | `payment_intent` | `id` | — | ✅ `application` | metadata match | ☐ |
| `charge_refunded.json` | `charge.refunded` | `charge` | `id` | `metadata.reference` | ✅ | `unsupported` | ☐ |
| `connect_payment_intent_succeeded.json` | `payment_intent.succeeded` with top-level `account` | `payment_intent` | — | — | ✅ | `unsupported` (type `connect.payment_intent.succeeded`) | ☐ |

## Observations that drive the code

- The envelope is `{"id": "evt_…", "type": …, "livemode": …, "data": {"object": {…}}}`. Relay
  hands `data.object` to routing and deduplicates on the event id (`evt:<id>`). Stripe keeps the
  id across retries.
- `Stripe-Signature` is `t=<unix>,v1=<hex>[,v1=…]`, an HMAC-SHA256 over `"<t>.<raw body>"`
  keyed with the endpoint's signing secret (`whsec_…`). Stripe sends one `v1` per active secret
  while a secret is being rolled. Relay ignores `v0` and rejects timestamps more than 5 minutes
  from now (configurable with `tolerance`).
- Stripe has no merchant reference field on a PaymentIntent. Relay reads `client_reference_id`
  on Checkout Sessions and `metadata.reference` on everything else. To make the Checkout
  Session's PaymentIntent events route too, set `payment_intent_data.metadata.reference` to the
  same value.
- Stripe doesn't copy PaymentIntent metadata onto Charges reliably across API versions, so
  `charge.*` events are left `unsupported` for now.
- Connect events carry a top-level `account`. They are stored as `unsupported` until Connect
  routing exists.

## To capture (test mode)

- `checkout.session.completed` from a Checkout Session created with `client_reference_id` and
  `payment_intent_data.metadata.reference`.
- `checkout.session.async_payment_succeeded` and `checkout.session.async_payment_failed`, from
  a delayed-notification payment method.
- `payment_intent.succeeded`, `payment_intent.payment_failed`, `payment_intent.processing` and
  `payment_intent.canceled` for a PaymentIntent created with `metadata.reference`.
