ALTER TABLE runs
    ADD COLUMN execution_version bigint NOT NULL DEFAULT 1,
    ADD COLUMN lease_owner text,
    ADD COLUMN lease_token bigint NOT NULL DEFAULT 0,
    ADD COLUMN lease_expires_at timestamptz,
    ADD CONSTRAINT runs_execution_version_positive CHECK (execution_version > 0),
    ADD CONSTRAINT runs_lease_token_nonnegative CHECK (lease_token >= 0),
    ADD CONSTRAINT runs_lease_shape CHECK (
        (lease_owner IS NULL AND lease_expires_at IS NULL)
        OR (lease_owner IS NOT NULL AND length(lease_owner) BETWEEN 1 AND 256
            AND lease_owner !~ '[[:cntrl:]]' AND lease_token > 0 AND lease_expires_at IS NOT NULL)
    );

ALTER TABLE steps
    ADD COLUMN row_version bigint NOT NULL DEFAULT 1,
    ADD CONSTRAINT steps_row_version_positive CHECK (row_version > 0);

ALTER TABLE attempts
    ADD COLUMN row_version bigint NOT NULL DEFAULT 1,
    ADD CONSTRAINT attempts_row_version_positive CHECK (row_version > 0);

CREATE TABLE run_checkpoints (
    tenant_id text NOT NULL,
    run_id text NOT NULL,
    lease_token bigint NOT NULL,
    step_no bigint NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT run_checkpoints_pk PRIMARY KEY (tenant_id, run_id),
    CONSTRAINT run_checkpoints_tenant_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT run_checkpoints_run_format CHECK (run_id ~ '^run_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT run_checkpoints_lease_token_positive CHECK (lease_token > 0),
    CONSTRAINT run_checkpoints_step_nonnegative CHECK (step_no >= 0),
    CONSTRAINT run_checkpoints_payload_size CHECK (octet_length(payload::text) <= 1048576),
    CONSTRAINT run_checkpoints_time_order CHECK (updated_at >= created_at),
    CONSTRAINT run_checkpoints_run_fk FOREIGN KEY (tenant_id, run_id)
        REFERENCES runs (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX runs_lease_scan_idx
    ON runs (tenant_id, status, lease_expires_at, id)
    WHERE status IN ('QUEUED', 'RUNNING', 'CANCEL_REQUESTED', 'SUCCEEDED');

