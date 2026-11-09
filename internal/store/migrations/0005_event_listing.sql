-- Newest-first event listing (CLI and admin API) without a full sort.
CREATE INDEX events_received ON events (received_at, id);
