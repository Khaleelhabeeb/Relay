-- Webhooks that share an event's dedup key but carry different ownership
-- identifiers (reference or dedicated account). A genuine resend never
-- differs, so these are kept as evidence of a forged or pre-empted event.
CREATE TABLE dedup_conflicts (
    id                TEXT PRIMARY KEY,
    event_id          TEXT NOT NULL REFERENCES events (id),
    reference         TEXT,
    dedicated_account TEXT,
    raw_payload       BLOB,             -- cleared by retention purge
    payload_sha256    TEXT NOT NULL,
    received_at       INTEGER NOT NULL
);
CREATE INDEX dedup_conflicts_event ON dedup_conflicts (event_id, received_at);
