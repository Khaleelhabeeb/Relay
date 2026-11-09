# Flutterwave fixture compatibility matrix

Every fixture here except `legacy_bank_transfer.json` is **synthetic**. They were hand-written from the sample payloads in
Flutterwave's public webhook docs (checked 2026-11-03), with every identifier sanitized. Before
the routing a fixture covers counts as verified, replace it with a sanitized payload captured from
a Flutterwave **test-mode** integration.

Legend: ✅ present · — absent

| Fixture | `event` | `data.id` | `data.tx_ref` / `data.reference` | `data.meta` | top-level `meta_data` | `data.status` | Routing | Verified with real payload |
|---|---|---|---|---|---|---|---|---|
| `charge_completed_card.json` | `charge.completed` | ✅ | tx_ref, merchant-set (`APPA_…`) | — | — | `successful` | reference prefix / registered tx_ref | ☐ |
| `charge_completed_meta.json` | `charge.completed` | ✅ | tx_ref | ✅ object with `application` | — | `successful` | metadata match | ☐ |
| `charge_completed_meta_data_toplevel.json` | `charge.completed` | ✅ | tx_ref | — | ✅ object with `application` | `successful` | metadata match | ☐ |
| `charge_failed.json` | `charge.completed` | ✅ | tx_ref (`APPA_…`) | — | — | `failed` | reference prefix (the recipient decides what a failure means) | ☐ |
| `legacy_bank_transfer.json` | — (`"event.type": BANK_TRANSFER_TRANSACTION`) | ✅ `id` | `txRef` (`APPA_…`) | — | — | `successful` | reference prefix / registered tx_ref; read as `charge.completed` | ✅ (test mode, 2026-11-07, sanitized) |
| `transfer_completed.json` | `transfer.completed` | ✅ | reference (`APPA_…`) | `null` | — | `SUCCESSFUL` | `unsupported` | ☐ |

## Observations that drive the code

- The body has no event ID. Flutterwave recommends `event:data.id:data.status` as the dedup key,
  so a status change of the same transaction is a new event.
- Charges carry the merchant reference in `data.tx_ref`, and transfers carry it in
  `data.reference`.
- It's unclear where custom `meta` lands: the docs' samples show neither field. Relay reads
  `data.meta` and the top-level `meta_data`, and `data.meta` wins on conflicts.
- Virtual-card and some refund payloads have no `event`/`data` envelope. Relay rejects them with
  `400 invalid_payload`, so Flutterwave retries and gives up. Supporting them needs real samples.
- v4 accounts may sign with `flutterwave-signature` (base64 HMAC-SHA256 of the raw body, keyed
  with the secret hash). That header name and encoding aren't on the public webhook page yet, so
  confirm them with a sandbox webhook.

- Accounts without v3 webhooks enabled get the legacy format: a flat body, camel-case fields
  (`txRef`, `flwRef`), the type under a key named `"event.type"`, and **no `meta`**, even when
  the payment was created with `meta`. `legacy_bank_transfer.json` is a sanitized real one.

## To capture (test mode)

- [ ] A card `charge.completed` with a merchant `tx_ref` and custom `meta`, to see where `meta` lands
- [ ] A bank-transfer / virtual-account `charge.completed`
- [ ] A legacy `CARD_TRANSACTION` (only `BANK_TRANSFER_TRANSACTION` has been seen so far)
- [ ] A failed charge followed by a retry that succeeds with the same `tx_ref`
- [ ] `transfer.completed`
- [ ] The headers on a v4-signed webhook
