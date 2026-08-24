CREATE TABLE run_event_streams (
    tenant_id text NOT NULL,
    run_id text NOT NULL,
    session_id text NOT NULL,
    turn_id text NOT NULL,
    next_sequence bigint NOT NULL DEFAULT 1,
    retained_from bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, run_id),
    CHECK (next_sequence > 0 AND retained_from > 0 AND retained_from <= next_sequence),
    CHECK (updated_at >= created_at),
    FOREIGN KEY (tenant_id, session_id, turn_id, run_id)
        REFERENCES runs (tenant_id, session_id, turn_id, id) ON DELETE CASCADE
);

CREATE TABLE run_events (
    tenant_id text NOT NULL,
    run_id text NOT NULL,
    sequence bigint NOT NULL,
    turn_id text NOT NULL,
    schema_version smallint NOT NULL,
    event_type text NOT NULL,
    event_data jsonb NOT NULL,
    occurred_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, run_id, sequence),
    CHECK (sequence > 0),
    CHECK (schema_version > 0),
    CHECK (event_type ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
    CHECK (jsonb_typeof(event_data) = 'object' AND octet_length(event_data::text) <= 131072),
    CHECK (expires_at > occurred_at),
    FOREIGN KEY (tenant_id, run_id) REFERENCES run_event_streams (tenant_id, run_id) ON DELETE CASCADE
);

CREATE INDEX run_events_expiry_idx ON run_events (tenant_id, expires_at, run_id, sequence);
