-- Retention walks only events that still hold a payload, oldest first, so
-- each purge batch is a short index range instead of a full table scan.
CREATE INDEX events_payload_retained ON events (received_at, id) WHERE raw_payload IS NOT NULL;

-- Claiming reads each application's due deliveries in order straight from
-- this index: the cost of a claim does not grow with the backlog, and one
-- application's backlog never sits in front of another's.
CREATE INDEX deliveries_pending_app ON deliveries (application_id, next_attempt_at, id) WHERE status = 'pending';
