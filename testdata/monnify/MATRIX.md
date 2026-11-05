# Monnify fixture compatibility matrix

Every fixture here is **synthetic**. They were hand-written from the sample payloads in
Monnify's public webhook docs (developers.monnify.com, checked 2026-11-05), with every identifier
sanitized. Before the routing a fixture covers counts as verified, replace it with a sanitized
payload captured from a Monnify **sandbox** integration.

Legend: ✅ present · — absent

| Fixture | `eventType` | `transactionReference` | Reference Relay uses | `metaData` | Destination account | `paymentStatus` | Routing | Verified with real payload |
|---|---|---|---|---|---|---|---|---|
| `successful_transaction_card.json` | `SUCCESSFUL_TRANSACTION` | ✅ | `paymentReference`, merchant-set (`APPA_…`) | `{}` | — | `PAID` | reference prefix / registered reference | ☐ |
| `successful_transaction_metadata.json` | `SUCCESSFUL_TRANSACTION` | ✅ | `paymentReference` | ✅ object with `application` | — | `PAID` | metadata match | ☐ |
| `successful_transaction_metadata_string.json` | `SUCCESSFUL_TRANSACTION` | ✅ | `paymentReference` | ✅ JSON-encoded string | — | `PAID` | metadata match | ☐ |
| `successful_transaction_reserved_account.json` | `SUCCESSFUL_TRANSACTION` | ✅ | `product.reference` (accountReference, `APPB_…`) | `{}` | ✅ `bankName` + `accountNumber` | `PAID` | accountReference prefix / registered `dedicated_account` | ☐ |
| `rejected_payment.json` | `REJECTED_PAYMENT` | ✅ | `paymentReference` | ✅ string | — | — | `unsupported` | ☐ |
| `successful_disbursement.json` | `SUCCESSFUL_DISBURSEMENT` | ✅ | `reference` | — | — | — (`status`) | `unsupported` | ☐ |

## Observations that drive the code

- The envelope is `{"eventType": …, "eventData": {…}}`. The body has no event ID, so Relay
  deduplicates on `eventType:transactionReference:paymentStatus`, or otherwise on the SHA-256
  of the body. A status change of the same transaction counts as a new event.
- `paymentReference` is the merchant's own reference for checkout and API payments. For a
  reserved-account deposit Monnify sets it to the generated `transactionReference`, so Relay
  uses `product.reference` (the accountReference you chose when reserving the account). Relay
  also reads `destinationAccountInformation` as the dedicated account.
- `metaData` is an object in most samples, but the `REJECTED_PAYMENT` sample shows a
  JSON-encoded string. Relay accepts both.
- `monnify-signature` is lowercase hex HMAC-SHA512 of the raw body, keyed with the client
  secret key. The docs' formula text says "SHA-512(secret + body)", but every code sample uses
  an HMAC. **Sandbox webhooks are unsigned.** Relay rejects them unless
  `allow_unsigned_sandbox` is set, and with that set it accepts them only after the sandbox API
  confirms the transaction.
- Monnify's published sample hash was computed over a pretty-printed body, not the bytes
  shown, so it can't be used as a test vector.
- Webhooks come from `35.242.133.146`.

## To capture (sandbox)

- [ ] A checkout `SUCCESSFUL_TRANSACTION` with a merchant `paymentReference` and custom `metaData`
- [ ] A reserved-account deposit, to confirm `product.reference` is the accountReference
- [ ] A card payment, to see `cardDetails` and `paymentMethod`
- [ ] A `REJECTED_PAYMENT` (underpayment), to see whether `metaData` really arrives as a string
- [ ] Headers on a production webhook, to confirm the `monnify-signature` encoding
