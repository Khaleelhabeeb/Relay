# Relay's database

Relay keeps everything in one SQLite file: received events, routing decisions, deliveries,
attempts, registrations and operator alerts. This guide covers what you control, how to read it,
how to back it up, and what not to do. It applies to `relay serve` and to Relay embedded as a Go
library; both use the same file and schema.

- [What you control](#what-you-control)
- [Reading the database](#reading-the-database)
- [Schema reference](#schema-reference)
- [Useful queries](#useful-queries)
- [Backups and restore](#backups-and-restore)
- [Data lifecycle](#data-lifecycle)
- [Why SQLite](#why-sqlite)

## What you control

| Setting | Effect |
|---|---|
| `storage.dsn` | Where the file lives. Relay creates the directory (`0700`) and file (`0600`). Use a local disk, not a network share. |
| `retention.payload_days` | How long raw payment payloads are kept after delivery, dismissal or rejection (default 90). |
| `retention.attempt_days` | How long delivery attempt history is kept (default 90). |
| `retention.held_days` | Dismiss events nobody resolves after this many days (default 0, never). |

Only one Relay process can use a database at a time. `relay.db.lock` enforces this, and a second
`relay serve`, or a second `relay.New` on the same file, fails with `ErrLocked`. The `relay` CLI
is the exception: it opens the file alongside a running server, without the lock, for operator
commands.

## Reading the database

**Read freely, write never.** The database runs in WAL mode, so read-only queries don't block
Relay and Relay doesn't block them. Open it read-only:

```bash
sqlite3 -readonly /var/lib/relay/relay.db
```

```go
db, err := sql.Open("sqlite", "file:/var/lib/relay/relay.db?mode=ro")
```

Don't write to it yourself. Each change Relay makes is one transaction that keeps several tables
consistent. For example, a reroute writes the event's owner, a routing decision, a delivery and
an alert together. Make changes through the CLI, the [admin API](../README.md#admin-api) or the
Go methods on `*relay.Relay`. They enforce the rules and keep the audit trail.

**Stability.** The schema is versioned by numbered migrations, which run when Relay starts.
`SELECT MAX(version) FROM schema_migrations` tells you which version a file is at, and the
CHANGELOG records every migration. Migrations may add tables, columns and indexes. Renaming or
removing a column is called out as a breaking change. Write queries that name their columns
(no `SELECT *`) and they'll keep working across upgrades.

**Conventions.**
- Times are Unix milliseconds, UTC (`datetime(received_at / 1000, 'unixepoch')` in SQLite).
- IDs are prefixed ULIDs (`evt_`, `del_`, `att_`, `reg_`, `rtd_`, `ddc_`), so they sort by
  creation time.
- `raw_payload` holds the exact signed bytes the provider sent. It is payment data: treat any
  copy of the database as sensitive.

## Schema reference

### `events`
One row per received webhook (duplicates increase `duplicate_count` instead of adding rows).

| Column | Meaning |
|---|---|
| `id` | `evt_…` |
| `provider`, `event_type` | e.g. `paystack`, `charge.success` |
| `dedup_key` | What makes two webhooks the same event; unique per provider |
| `raw_payload` | Exact bytes received; `NULL` once retention clears it |
| `payload_sha256` | Hash of the payload, kept after it is cleared |
| `reference`, `dedicated_account` | Ownership identifiers |
| `received_at`, `last_seen_at`, `duplicate_count` | First arrival, latest duplicate, number of duplicates |
| `routing_status` | `matched`, `unresolved`, `ambiguous` or `unsupported` |
| `application_id` | Owner, when `matched` |
| `confirmation` | Provider API check: `none` (not checked), `confirmed`, `pending`, `failed` or `rejected` |
| `confirm_attempts`, `confirm_next_at`, `confirm_error` | Background confirmation progress |
| `dismissed_at`, `dismissed_by`, `dismiss_reason` | Set when a held event was closed without an owner |

An event is **held** (needs an owner decided) when
`routing_status IN ('unresolved', 'ambiguous') AND dismissed_at IS NULL AND confirmation != 'rejected'`.

### `routing_decisions`
Every routing outcome for an event, oldest first: the decision when it arrived, then any
reprocess, registration or operator reroute.

| Column | Meaning |
|---|---|
| `event_id` | The event |
| `outcome`, `application_id` | What was decided |
| `candidates` | JSON array of the evidence: `[{application, source, detail}]` |
| `reason` | Why, in words. Operator reroutes read `operator reroute via cli\|admin api\|go api: <reason>` |
| `decided_by` | `ingest`, `reprocess`, `registration` or `operator` |
| `decided_at` | When |

### `deliveries` and `delivery_attempts`
One delivery per event and owner. Each try is an attempt.

| Column | Meaning |
|---|---|
| `deliveries.status` | `pending`, `in_flight`, `delivered` or `exhausted` |
| `deliveries.attempt_count`, `next_attempt_at`, `last_error` | Retry progress |
| `deliveries.created_at`, `updated_at`, `delivered_at` | Timeline |
| `deliveries.lease_owner`, `lease_until`, `retry_base` | Internal to the delivery workers |
| `delivery_attempts.attempt_no`, `started_at`, `finished_at` | One try |
| `delivery_attempts.http_status`, `error_code`, `duration_ms` | Its result |

### `registrations`
Identifiers applications claimed through the registration API or `RegisterReference`.

| Column | Meaning |
|---|---|
| `provider`, `identifier_type`, `identifier_value` | `transaction_reference` or `dedicated_account` (`bank:account_number`) |
| `application_id` | Who claimed it |
| `active`, `created_at`, `deactivated_at` | Lifetime; only one active claim per identifier |

### `dedup_conflicts`
Webhooks that shared an event's dedup key but carried different ownership identifiers: possible
forgeries, kept for review. `event_id` points at the original event.

### `alert_outbox` and `alert_state`
Operator alerts waiting to be sent (`sent_at IS NULL`) or recently sent, and the time of the last
reminder. Alerts are kept for 7 days. See [Alerts](../README.md#alerts).

### `heartbeat`, `schema_migrations`
Readiness writes to `heartbeat`. `schema_migrations` lists applied migrations.

## Useful queries

Held events by provider and status:

```sql
SELECT provider, routing_status, COUNT(*), datetime(MIN(received_at) / 1000, 'unixepoch') AS oldest
FROM events
WHERE routing_status IN ('unresolved', 'ambiguous') AND dismissed_at IS NULL AND confirmation != 'rejected'
GROUP BY provider, routing_status;
```

Deliveries per application per day, with failures:

```sql
SELECT application_id, date(created_at / 1000, 'unixepoch') AS day,
       SUM(status = 'delivered') AS delivered, SUM(status = 'exhausted') AS exhausted
FROM deliveries GROUP BY application_id, day ORDER BY day DESC;
```

Slowest applications over the last day:

```sql
SELECT d.application_id, COUNT(*) AS attempts, AVG(a.duration_ms) AS avg_ms, MAX(a.duration_ms) AS max_ms
FROM delivery_attempts a JOIN deliveries d ON d.id = a.delivery_id
WHERE a.started_at > (strftime('%s', 'now') - 86400) * 1000
GROUP BY d.application_id ORDER BY avg_ms DESC;
```

Who rerouted or dismissed what:

```sql
SELECT event_id, application_id, reason, datetime(decided_at / 1000, 'unixepoch')
FROM routing_decisions WHERE decided_by = 'operator' ORDER BY decided_at DESC;

SELECT id, dismissed_by, dismiss_reason, datetime(dismissed_at / 1000, 'unixepoch')
FROM events WHERE dismissed_at IS NOT NULL ORDER BY dismissed_at DESC;
```

## Backups and restore

Back up while Relay runs. Each method below takes a consistent snapshot without stopping it.

**One-off or scheduled copy:**

```bash
sqlite3 /var/lib/relay/relay.db "VACUUM INTO '/backups/relay-$(date +%F).db'"
```

**Continuous replication with [Litestream](https://litestream.io).** It streams the WAL to
object storage, so you lose seconds rather than a day:

```yaml
# /etc/litestream.yml
dbs:
  - path: /var/lib/relay/relay.db
    replicas:
      - url: s3://my-bucket/relay
```

Run Litestream next to Relay, not instead of the lock: it only reads. Practise a restore before
you rely on it.

**Encrypt every copy.** Backups hold payment payloads and references.

**Restore:**
1. Stop Relay.
2. Put the backup in place as `relay.db`, and delete `relay.db-wal` and `relay.db-shm`.
   (`litestream restore -o /var/lib/relay/relay.db s3://my-bucket/relay` does this for Litestream.)
3. Start Relay. Migrations bring an older backup up to date.
4. Ask your provider to resend webhooks from after the backup. Duplicates of events the backup
   already has are recognised and ignored.

Deliveries that were in flight when the backup was taken are retried after a restore, so an
application may see an event twice. Applications must deduplicate on `event_id` anyway (see
[Your application's responsibilities](../README.md#your-applications-responsibilities)).

## Data lifecycle

| Data | Kept until |
|---|---|
| Raw payloads of delivered events | `retention.payload_days` after every delivery succeeded |
| Raw payloads of `unsupported` and provider-rejected events | `retention.payload_days` after arrival |
| Raw payloads of dismissed events | `retention.payload_days` after dismissal |
| Raw payloads of held, pending or exhausted events | Resolved, so nothing is lost while someone still has to act |
| Delivery attempts | `retention.attempt_days` after they finished |
| Alerts | 7 days |
| Event rows, routing decisions, deliveries, registrations | Forever: they're the audit trail and contain no payload |

Event rows keep references and dedicated account numbers after their payload is cleared. Relay
has no command to erase one customer's records. If you need that, stop Relay, delete the rows in
one transaction (the event's `delivery_attempts`, `deliveries`, `routing_decisions`,
`dedup_conflicts` and `alert_outbox`, then the event), and start it again.

## Why SQLite

Relay's guarantees depend on exact transaction behaviour: an event and its routing decision are
committed with `synchronous=FULL` before the provider gets a `200`, and an alert is written in the
same transaction as the change it reports. One embedded file gives that with nothing else to run,
and it handles far more webhooks than a payment account produces (see
[Performance and reliability](../README.md#performance-and-reliability)). The trade-off is one
Relay process per database. Everything above the store goes through one layer, so another
database (PostgreSQL, for teams that want their existing backups and failover) can be added
there without changing how Relay is operated. Open an issue if you need it.
