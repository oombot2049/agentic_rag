CREATE TABLE outbox_events (
    tenant_id text NOT NULL,
    id text NOT NULL,
    aggregate_type text NOT NULL,
    aggregate_id text NOT NULL,
    event_type text NOT NULL,
    payload jsonb NOT NULL,
    status text NOT NULL DEFAULT 'PENDING',
    available_at timestamptz NOT NULL,
    occurred_at timestamptz NOT NULL,
    attempt_count integer NOT NULL DEFAULT 0,
    claim_owner text,
    claim_token bigint NOT NULL DEFAULT 0,
    claim_expires_at timestamptz,
    published_at timestamptz,
    last_error text,
    CONSTRAINT outbox_events_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT outbox_tenant_id_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT outbox_id_format CHECK (id ~ '^evt_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT outbox_aggregate_type_format CHECK (aggregate_type ~ '^[A-Za-z][A-Za-z0-9_.-]{0,127}$'),
    CONSTRAINT outbox_aggregate_id_format CHECK (aggregate_id ~ '^[A-Za-z][A-Za-z0-9_-]{3,127}$'),
    CONSTRAINT outbox_event_type_format CHECK (event_type ~ '^[A-Za-z][A-Za-z0-9_.-]{0,127}$'),
    CONSTRAINT outbox_payload_size CHECK (octet_length(payload::text) BETWEEN 1 AND 1048576),
    CONSTRAINT outbox_status_valid CHECK (status IN ('PENDING', 'PUBLISHED', 'DEAD')),
    CONSTRAINT outbox_attempt_nonnegative CHECK (attempt_count >= 0),
    CONSTRAINT outbox_claim_token_nonnegative CHECK (claim_token >= 0),
    CONSTRAINT outbox_claim_shape CHECK ((claim_owner IS NULL) = (claim_expires_at IS NULL)),
    CONSTRAINT outbox_claim_owner_size CHECK (claim_owner IS NULL OR length(claim_owner) BETWEEN 1 AND 128),
    CONSTRAINT outbox_error_size CHECK (last_error IS NULL OR length(last_error) BETWEEN 1 AND 2048),
    CONSTRAINT outbox_terminal_shape CHECK (
        (status = 'PUBLISHED' AND published_at IS NOT NULL AND claim_owner IS NULL)
        OR (status = 'DEAD' AND published_at IS NULL AND claim_owner IS NULL)
        OR (status = 'PENDING' AND published_at IS NULL)
    )
);

CREATE INDEX outbox_events_ready_idx
    ON outbox_events (tenant_id, status, available_at, occurred_at, id)
    WHERE status = 'PENDING';

CREATE INDEX outbox_events_cleanup_idx
    ON outbox_events (tenant_id, published_at, id)
    WHERE status = 'PUBLISHED';

CREATE INDEX outbox_events_dead_idx
    ON outbox_events (tenant_id, occurred_at, id)
    WHERE status = 'DEAD';

