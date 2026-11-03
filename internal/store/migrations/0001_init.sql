-- Times are Unix milliseconds (UTC).

CREATE TABLE events (
    id                TEXT PRIMARY KEY,
    provider          TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    dedup_key         TEXT NOT NULL,
    raw_payload       BLOB,             -- exact signed bytes; cleared by retention purge
    payload_sha256    TEXT NOT NULL,
    reference         TEXT,             -- ownership identifiers, kept for reprocessing
    dedicated_account TEXT,
    received_at       INTEGER NOT NULL,
    last_seen_at      INTEGER NOT NULL,
    duplicate_count   INTEGER NOT NULL DEFAULT 0,
    routing_status    TEXT NOT NULL
        CHECK (routing_status IN ('matched', 'unresolved', 'ambiguous', 'unsupported')),
    application_id    TEXT,             -- owner when routing_status = 'matched'
    UNIQUE (provider, dedup_key)
);
CREATE INDEX events_routing_status ON events (routing_status, received_at);
CREATE INDEX events_reference ON events (provider, reference) WHERE reference IS NOT NULL;
CREATE INDEX events_dedicated_account ON events (provider, dedicated_account) WHERE dedicated_account IS NOT NULL;

-- Every routing outcome for an event, oldest first: the ingest decision,
-- then any reprocess or operator reroute.
CREATE TABLE routing_decisions (
    id             TEXT PRIMARY KEY,
    event_id       TEXT NOT NULL REFERENCES events (id),
    outcome        TEXT NOT NULL
        CHECK (outcome IN ('matched', 'unresolved', 'ambiguous', 'unsupported')),
    application_id TEXT,
    candidates     TEXT NOT NULL,     -- JSON array of {application, source, detail}
    reason         TEXT NOT NULL,
    decided_by     TEXT NOT NULL
        CHECK (decided_by IN ('ingest', 'reprocess', 'registration', 'operator')),
    decided_at     INTEGER NOT NULL
);
CREATE INDEX routing_decisions_event ON routing_decisions (event_id, decided_at);

CREATE TABLE deliveries (
    id              TEXT PRIMARY KEY,
    event_id        TEXT NOT NULL REFERENCES events (id),
    application_id  TEXT NOT NULL,
    status          TEXT NOT NULL
        CHECK (status IN ('pending', 'in_flight', 'delivered', 'exhausted')),
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    lease_owner     TEXT,
    lease_until     INTEGER,
    last_error      TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    delivered_at    INTEGER,
    UNIQUE (event_id, application_id)
);
CREATE INDEX deliveries_due ON deliveries (status, next_attempt_at);
CREATE INDEX deliveries_lease ON deliveries (status, lease_until);

CREATE TABLE delivery_attempts (
    id          TEXT PRIMARY KEY,
    delivery_id TEXT NOT NULL REFERENCES deliveries (id),
    attempt_no  INTEGER NOT NULL,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    http_status INTEGER,
    error_code  TEXT,
    duration_ms INTEGER,
    UNIQUE (delivery_id, attempt_no)
);

CREATE TABLE registrations (
    id               TEXT PRIMARY KEY,
    provider         TEXT NOT NULL,
    identifier_type  TEXT NOT NULL
        CHECK (identifier_type IN ('transaction_reference', 'dedicated_account')),
    identifier_value TEXT NOT NULL,
    application_id   TEXT NOT NULL,
    active           INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    created_at       INTEGER NOT NULL,
    deactivated_at   INTEGER
);
-- At most one active owner per identifier; history is kept for reuse rules.
CREATE UNIQUE INDEX registrations_active
    ON registrations (provider, identifier_type, identifier_value) WHERE active = 1;
CREATE INDEX registrations_lookup
    ON registrations (provider, identifier_type, identifier_value, created_at);
CREATE INDEX registrations_application ON registrations (application_id, active);
