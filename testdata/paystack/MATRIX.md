# Paystack fixture compatibility matrix

Every fixture here is **synthetic**: hand-written by following the sample payloads in Paystack's
public docs (Webhooks; Dedicated Virtual Accounts, checked 2026-11-02), with all identifiers
sanitized. Swap each one for a sanitized payload captured from a Paystack **test-mode**
integration before the routing it covers counts as verified (Phase 0 gate).

Legend: ✅ present · — absent · (s) the value is a JSON-encoded string, not an object

| Fixture | `event` | `data.id` | `data.reference` | `data.metadata` | `data.channel` | `authorization.receiver_bank_account_number` / `receiver_bank` | v1 routing | Verified with real payload |
|---|---|---|---|---|---|---|---|---|
| `charge_success_checkout.json` | `charge.success` | ✅ | ✅ merchant-set (`APPA_…`) | ✅ object | `card` | — | reference prefix / registered reference | ☐ |
| `charge_success_metadata_object.json` | `charge.success` | ✅ | ✅ Paystack-generated | ✅ object with `application` | `card` | — | metadata match | ☐ |
| `charge_success_metadata_string.json` | `charge.success` | ✅ | ✅ Paystack-generated | ✅ (s) | `bank` | — | metadata match (after decoding the string) | ☐ |
| `charge_success_dedicated_nuban.json` | `charge.success` | ✅ | ✅ Paystack-generated | ✅ (receiver fields duplicated, unverified) | `dedicated_nuban` | ✅ / ✅ | registered `dedicated_account` | ☐ **blocks Phase 4 DVA routing** |
| `transfer_success.json` | `transfer.success` | ✅ | ✅ merchant-set | — | — | — | `unsupported` in v1 (v1.1 after real fixtures) | ☐ |
| `subscription_create.json` | `subscription.create` | ✅ | — | — | — | — | `unsupported` | ☐ |

## Observations that drive the code

- The webhook body has no event ID. For `charge.success`, `data.id` (the transaction ID) is the
  semantic dedup key. Every other event type falls back to `sha256(raw body)`.
- `data.metadata` can be an object, a JSON-encoded string, or empty (`""`/`null`). The extractor
  accepts all three and only reads top-level scalar values.
- Dedicated-account deposits use Paystack-generated references, so a reference prefix can never
  route them. Only a registered `(receiver_bank, receiver_bank_account_number)` can.
- Customer fields (`customer.id`, `customer_code`, `email`) appear in every charge, but routing
  ignores them on purpose, because one customer can pay several applications.

## To capture (test mode)

1. Checkout `charge.success`, with a merchant-set reference and metadata, initialized through `POST /transaction/initialize`.
2. DVA `charge.success`: create a DVA with `preferred_bank: "test-bank"` and transfer to it.
   Confirm the receiver fields, and whether `data.metadata.receiver_account_number` really appears.
3. A dashboard/Events-API **resend** of (1), to confirm `data.id` stays the same and to see whether the bytes differ.
4. `refund.processed`, `transfer.success`/`failed`/`reversed`, `charge.dispute.create` for v1.1.
