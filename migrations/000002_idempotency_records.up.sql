CREATE TABLE idempotency_records (
    tenant_id text NOT NULL,
    authenticated_subject_id text NOT NULL,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    status text NOT NULL,
    response_status smallint,
    response_body jsonb,
    response_location text,
    resource_type text,
    resource_id text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CONSTRAINT idempotency_records_pk PRIMARY KEY (
        tenant_id, authenticated_subject_id, operation, idempotency_key
    ),
    CONSTRAINT idempotency_records_tenant_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT idempotency_records_subject_valid CHECK (
        length(authenticated_subject_id) BETWEEN 1 AND 256
        AND authenticated_subject_id !~ '[[:cntrl:]]'
    ),
    CONSTRAINT idempotency_records_operation_valid CHECK (
        operation ~ '^[A-Z][A-Z0-9_.:-]{0,127}$'
    ),
    CONSTRAINT idempotency_records_key_valid CHECK (
        length(idempotency_key) BETWEEN 16 AND 128
        AND idempotency_key !~ '[[:cntrl:]]'
    ),
    CONSTRAINT idempotency_records_hash_size CHECK (octet_length(request_hash) = 32),
    CONSTRAINT idempotency_records_status_valid CHECK (
        status IN ('IN_PROGRESS', 'SUCCEEDED', 'FAILED_FINAL')
    ),
    CONSTRAINT idempotency_records_response_status_valid CHECK (
        response_status IS NULL OR response_status BETWEEN 100 AND 599
    ),
    CONSTRAINT idempotency_records_response_body_size CHECK (
        response_body IS NULL OR octet_length(response_body::text) <= 1048576
    ),
    CONSTRAINT idempotency_records_response_location_valid CHECK (
        response_location IS NULL OR (
            length(response_location) <= 2048 AND response_location !~ '[[:cntrl:]]'
        )
    ),
    CONSTRAINT idempotency_records_resource_values_valid CHECK (
        (resource_type IS NULL OR (
            length(resource_type) BETWEEN 1 AND 128 AND resource_type !~ '[[:cntrl:]]'
        ))
        AND (resource_id IS NULL OR (
            length(resource_id) BETWEEN 1 AND 128 AND resource_id !~ '[[:cntrl:]]'
        ))
    ),
    CONSTRAINT idempotency_records_resource_pair CHECK (
        (resource_type IS NULL AND resource_id IS NULL)
        OR (resource_type IS NOT NULL AND resource_id IS NOT NULL)
    ),
    CONSTRAINT idempotency_records_state_shape CHECK (
        (status = 'IN_PROGRESS' AND response_status IS NULL AND response_body IS NULL
            AND response_location IS NULL AND resource_type IS NULL AND resource_id IS NULL)
        OR (status = 'SUCCEEDED' AND response_status BETWEEN 200 AND 299 AND response_body IS NOT NULL)
        OR (status = 'FAILED_FINAL' AND response_status BETWEEN 400 AND 599 AND response_body IS NOT NULL)
    ),
    CONSTRAINT idempotency_records_time_order CHECK (
        updated_at >= created_at AND expires_at >= created_at + interval '24 hours'
    )
);

CREATE INDEX idempotency_records_expiry_idx
    ON idempotency_records (tenant_id, expires_at);
