CREATE INDEX outbox_run_event_pending_sequence_idx
ON outbox_events(
    tenant_id,
    aggregate_type,
    aggregate_id,
    (CASE WHEN (payload->>'sequence') ~ '^[0-9]+$'
        THEN (payload->>'sequence')::bigint END)
)
WHERE status='PENDING' AND event_type='RunEvent.v1';
