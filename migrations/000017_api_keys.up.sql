CREATE TABLE api_keys (
    key_sha256 bytea PRIMARY KEY,
    tenant_id text NOT NULL,
    subject_id text NOT NULL,
    permissions jsonb NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT api_keys_digest_length CHECK (octet_length(key_sha256)=32),
    CONSTRAINT api_keys_tenant_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT api_keys_subject_format CHECK (
        length(subject_id) BETWEEN 1 AND 128 AND subject_id !~ '[[:space:][:cntrl:]]'
    ),
    CONSTRAINT api_keys_permissions_shape CHECK (
        jsonb_typeof(permissions)='array' AND jsonb_array_length(permissions) BETWEEN 1 AND 128
    ),
    CONSTRAINT api_keys_expiry_order CHECK (expires_at > created_at)
);

CREATE INDEX api_keys_active_expiry_idx ON api_keys(expires_at)
WHERE revoked=false;
