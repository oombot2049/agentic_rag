CREATE TABLE run_queue_messages (
    tenant_id text NOT NULL,
    run_id text NOT NULL,
    session_id text NOT NULL,
    turn_id text NOT NULL,
    delivery_id text NOT NULL,
    payload jsonb NOT NULL,
    traceparent text,
    tracestate text,
    status text NOT NULL DEFAULT 'READY',
    available_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    delivery_count bigint NOT NULL DEFAULT 0,
    lease_owner text,
    lease_token bigint NOT NULL DEFAULT 0,
    lease_expires_at timestamptz,
    acknowledged_at timestamptz,
    last_error text,
    PRIMARY KEY (tenant_id, run_id),
    UNIQUE (tenant_id, delivery_id),
    CHECK (delivery_id ~ '^evt_[A-Za-z0-9_-]{4,124}$'),
    CHECK (jsonb_typeof(payload)='object' AND octet_length(payload::text) <= 65536),
    CHECK (traceparent IS NULL OR traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'),
    CHECK (tracestate IS NULL OR (length(tracestate) <= 512 AND tracestate !~ '[[:cntrl:]]')),
    CHECK (status IN ('READY','LEASED','ACKED')),
    CHECK (available_at >= created_at),
    CHECK (delivery_count >= 0 AND lease_token >= 0),
    CHECK ((status='LEASED' AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL
            AND acknowledged_at IS NULL)
        OR (status='READY' AND lease_owner IS NULL AND lease_expires_at IS NULL
            AND acknowledged_at IS NULL)
        OR (status='ACKED' AND lease_owner IS NULL AND lease_expires_at IS NULL
            AND acknowledged_at IS NOT NULL)),
    CHECK (lease_owner IS NULL OR (length(lease_owner) BETWEEN 1 AND 256 AND lease_owner !~ '[[:cntrl:]]')),
    CHECK (last_error IS NULL OR (length(last_error) BETWEEN 1 AND 2048 AND last_error !~ '[[:cntrl:]]')),
    FOREIGN KEY (tenant_id, session_id, turn_id, run_id)
        REFERENCES runs (tenant_id, session_id, turn_id, id) ON DELETE CASCADE
);

CREATE INDEX run_queue_ready_idx ON run_queue_messages
    (tenant_id, status, available_at, created_at, run_id)
    WHERE status IN ('READY','LEASED');
CREATE INDEX run_queue_expired_lease_idx ON run_queue_messages
    (tenant_id, lease_expires_at, run_id) WHERE status='LEASED';
CREATE INDEX run_queue_cleanup_idx ON run_queue_messages
    (tenant_id, acknowledged_at, run_id) WHERE status='ACKED';
CREATE INDEX run_queue_partition_idx ON run_queue_messages
    (tenant_id, session_id, status, created_at, run_id) WHERE status <> 'ACKED';
