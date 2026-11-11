-- Dismissal closes a held (unresolved or ambiguous) event without an
-- owner: an operator decided nobody owns it, or retention.held_days
-- expired it. It keeps its routing status; it is just no longer held.
ALTER TABLE events ADD COLUMN dismissed_at INTEGER;
ALTER TABLE events ADD COLUMN dismissed_by TEXT;
ALTER TABLE events ADD COLUMN dismiss_reason TEXT;

-- Operator alerts, written in the same transaction as the state change
-- they report, so none is lost to a crash. The alerter sends unsent rows
-- as a digest and marks them sent.
CREATE TABLE alert_outbox (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL,
    event_id    TEXT NOT NULL,
    delivery_id TEXT,
    created_at  INTEGER NOT NULL,
    sent_at     INTEGER
);
CREATE INDEX alert_outbox_unsent ON alert_outbox (id) WHERE sent_at IS NULL;
CREATE INDEX alert_outbox_created ON alert_outbox (created_at);

-- Small named timestamps, e.g. when the last reminder was sent.
CREATE TABLE alert_state (
    name TEXT PRIMARY KEY,
    at   INTEGER NOT NULL
);
