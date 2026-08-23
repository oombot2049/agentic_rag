CREATE TABLE tool_calls (
    tenant_id text NOT NULL,
    id text NOT NULL,
    run_id text NOT NULL,
    step_id text NOT NULL,
    tool_name text NOT NULL,
    tool_version text NOT NULL,
    effect text NOT NULL,
    scope_level text NOT NULL,
    scope_session_id text,
    scope_run_id text,
    arguments_sha256 bytea NOT NULL,
    invocation_key text NOT NULL,
    status text NOT NULL,
    result jsonb,
    failure_class text,
    row_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT tool_calls_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT tool_calls_step_unique UNIQUE (tenant_id, run_id, step_id),
    CONSTRAINT tool_calls_invocation_key_unique UNIQUE (tenant_id, invocation_key),
    CONSTRAINT tool_calls_id_format CHECK (id ~ '^tcl_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT tool_calls_name_format CHECK (tool_name ~ '^[a-z][a-z0-9]*(\.[a-z][a-z0-9_-]*)*$'),
    CONSTRAINT tool_calls_version_format CHECK (tool_version ~ '^[0-9]+\.[0-9]+\.[0-9]+$'),
    CONSTRAINT tool_calls_effect_valid CHECK (effect IN ('READ_ONLY','IDEMPOTENT_WRITE','DESTRUCTIVE')),
    CONSTRAINT tool_calls_scope_valid CHECK (
        (scope_level='TENANT' AND scope_session_id IS NULL AND scope_run_id IS NULL)
        OR (scope_level='SESSION' AND scope_session_id IS NOT NULL AND scope_run_id IS NULL)
        OR (scope_level='RUN' AND scope_session_id IS NOT NULL AND scope_run_id IS NOT NULL)
    ),
    CONSTRAINT tool_calls_arguments_hash CHECK (octet_length(arguments_sha256)=32),
    CONSTRAINT tool_calls_invocation_key_format CHECK (invocation_key ~ '^tik_[A-Za-z0-9_-]{43}$'),
    CONSTRAINT tool_calls_status_valid CHECK (status IN ('PREPARED','SUCCEEDED','FAILED','UNKNOWN')),
    CONSTRAINT tool_calls_outcome_shape CHECK (
        (status='PREPARED' AND result IS NULL AND failure_class IS NULL)
        OR (status='SUCCEEDED' AND result IS NOT NULL AND jsonb_typeof(result)='object' AND failure_class IS NULL)
        OR (status IN ('FAILED','UNKNOWN') AND result IS NULL AND failure_class ~ '^[a-z][a-z0-9_]{0,63}$')
    ),
    CONSTRAINT tool_calls_result_size CHECK (result IS NULL OR octet_length(result::text) <= 65536),
    CONSTRAINT tool_calls_row_version_positive CHECK (row_version > 0),
    CONSTRAINT tool_calls_time_order CHECK (updated_at >= created_at),
    CONSTRAINT tool_calls_step_fk FOREIGN KEY (tenant_id, step_id)
        REFERENCES steps (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT tool_calls_run_fk FOREIGN KEY (tenant_id, run_id)
        REFERENCES runs (tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE tool_call_attempts (
    tenant_id text NOT NULL,
    call_id text NOT NULL,
    attempt_id text NOT NULL,
    attempt_no bigint NOT NULL,
    status text NOT NULL,
    started_at timestamptz NOT NULL,
    ended_at timestamptz,
    CONSTRAINT tool_call_attempts_pk PRIMARY KEY (tenant_id, attempt_id),
    CONSTRAINT tool_call_attempts_number_unique UNIQUE (tenant_id, call_id, attempt_no),
    CONSTRAINT tool_call_attempts_status_valid CHECK (status IN ('STARTED','SUCCEEDED','FAILED','UNKNOWN')),
    CONSTRAINT tool_call_attempts_number_positive CHECK (attempt_no > 0),
    CONSTRAINT tool_call_attempts_end_shape CHECK (
        (status='STARTED' AND ended_at IS NULL)
        OR (status<>'STARTED' AND ended_at IS NOT NULL AND ended_at >= started_at)
    ),
    CONSTRAINT tool_call_attempts_call_fk FOREIGN KEY (tenant_id, call_id)
        REFERENCES tool_calls (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT tool_call_attempts_attempt_fk FOREIGN KEY (tenant_id, attempt_id)
        REFERENCES attempts (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX tool_calls_reconciliation_idx
    ON tool_calls (tenant_id, status, updated_at, id)
    WHERE status IN ('PREPARED','UNKNOWN');

CREATE INDEX tool_call_attempts_history_idx
    ON tool_call_attempts (tenant_id, call_id, attempt_no);
