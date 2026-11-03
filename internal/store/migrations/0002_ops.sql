-- Manual replay: an exhausted delivery gets a fresh attempt budget counted
-- from retry_base, while attempt numbers (and history) keep increasing.
ALTER TABLE deliveries ADD COLUMN retry_base INTEGER NOT NULL DEFAULT 0;

-- Readiness write probe target.
CREATE TABLE heartbeat (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    at INTEGER NOT NULL
);

CREATE INDEX deliveries_application ON deliveries (application_id, status);
CREATE INDEX delivery_attempts_finished ON delivery_attempts (finished_at);
