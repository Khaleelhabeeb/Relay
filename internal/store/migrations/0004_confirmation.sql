-- Provider API confirmation (confirm_with_api). An event is delivered only
-- when confirmation is 'none' (not required) or 'confirmed'.
--   pending   the API could not be reached when the webhook arrived; a
--             background worker retries until confirm_next_at
--   rejected  the API disowned the event; kept as evidence, never delivered,
--             and its dedup_key is rewritten so the genuine event can arrive
--   failed    still unconfirmable when the retry window closed; an operator
--             can retry with `relay events confirm`
ALTER TABLE events ADD COLUMN confirmation TEXT NOT NULL DEFAULT 'none'
    CHECK (confirmation IN ('none', 'confirmed', 'pending', 'rejected', 'failed'));
ALTER TABLE events ADD COLUMN confirm_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN confirm_next_at INTEGER;
ALTER TABLE events ADD COLUMN confirm_error TEXT;

CREATE INDEX events_confirm_due ON events (confirm_next_at) WHERE confirmation = 'pending';
