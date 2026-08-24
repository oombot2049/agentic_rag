CREATE TABLE artifact_metadata (
    tenant_id text NOT NULL,
    id text NOT NULL,
    object_key text NOT NULL,
    content_sha256 bytea NOT NULL,
    size_bytes bigint NOT NULL,
    media_type text NOT NULL,
    encryption_mode text NOT NULL,
    encryption_key_id text,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CONSTRAINT artifact_metadata_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT artifact_object_key_unique UNIQUE (object_key),
    CONSTRAINT artifact_tenant_id_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT artifact_id_format CHECK (id ~ '^art_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT artifact_object_key_shape CHECK (
        object_key = 'tenants/' || tenant_id || '/artifacts/' || id
    ),
    CONSTRAINT artifact_hash_size CHECK (octet_length(content_sha256) = 32),
    CONSTRAINT artifact_size_valid CHECK (size_bytes >= 0 AND size_bytes <= 5497558138880),
    CONSTRAINT artifact_media_type_size CHECK (length(media_type) BETWEEN 1 AND 255),
    CONSTRAINT artifact_encryption_mode_valid CHECK (encryption_mode IN ('SSE-S3', 'SSE-KMS', 'CLIENT')),
    CONSTRAINT artifact_encryption_shape CHECK (
        (encryption_mode IN ('SSE-KMS', 'CLIENT') AND length(encryption_key_id) BETWEEN 1 AND 512)
        OR (encryption_mode = 'SSE-S3' AND encryption_key_id IS NULL)
    ),
    CONSTRAINT artifact_expiry_valid CHECK (
        expires_at > created_at AND expires_at <= created_at + interval '10 years'
    )
);

CREATE INDEX artifact_metadata_expiry_idx
    ON artifact_metadata (tenant_id, expires_at, id);
