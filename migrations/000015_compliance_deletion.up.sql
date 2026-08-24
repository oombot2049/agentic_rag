CREATE TABLE compliance_external_targets (
    tenant_id text NOT NULL,
    session_id text NOT NULL,
    kind text NOT NULL,
    locator text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id,session_id,kind,locator),
    CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$' AND session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$'),
    CHECK (kind IN ('OBJECT','REDIS','INDEX')),
    CHECK (length(locator) BETWEEN 1 AND 512 AND locator=btrim(locator)
        AND locator !~ '[[:cntrl:]]'),
    FOREIGN KEY (tenant_id,session_id) REFERENCES sessions(tenant_id,id) ON DELETE CASCADE
);

CREATE TABLE legal_holds (
    tenant_id text NOT NULL,
    session_id text NOT NULL,
    id text NOT NULL,
    reason_code text NOT NULL,
    created_at timestamptz NOT NULL,
    released_at timestamptz,
    PRIMARY KEY (tenant_id,id),
    CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$' AND session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$'
        AND id ~ '^hld_[A-Za-z0-9_-]{4,124}$'),
    CHECK (reason_code ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (released_at IS NULL OR released_at>=created_at)
);

CREATE UNIQUE INDEX legal_holds_one_active_session_idx
    ON legal_holds(tenant_id,session_id) WHERE released_at IS NULL;

CREATE TABLE session_deletion_jobs (
    tenant_id text NOT NULL,
    id text NOT NULL,
    session_id text NOT NULL,
    actor_sha256 bytea NOT NULL,
    policy_version text NOT NULL,
    manifest jsonb NOT NULL,
    phase text NOT NULL,
    status text NOT NULL,
    attempt_count bigint NOT NULL DEFAULT 0,
    lease_owner text,
    lease_token bigint NOT NULL DEFAULT 0,
    lease_expires_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    evidence_retention_millis bigint NOT NULL,
    PRIMARY KEY (tenant_id,id),
    UNIQUE (tenant_id,session_id),
    CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$' AND id ~ '^del_[A-Za-z0-9_-]{4,124}$'
        AND session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$'),
    CHECK (octet_length(actor_sha256)=32 AND policy_version ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (jsonb_typeof(manifest)='array' AND jsonb_array_length(manifest)<=10000),
    CHECK (phase IN ('PENDING','EXTERNAL_DELETED','POSTGRES_DELETED','COMPLETED')),
    CHECK (status IN ('PENDING','RUNNING','RETRY','COMPLETED')),
    CHECK (attempt_count>=0 AND lease_token>=0),
    CHECK ((status='RUNNING' AND lease_owner ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$'
        AND lease_token>0 AND lease_expires_at IS NOT NULL)
        OR (status<>'RUNNING' AND lease_owner IS NULL AND lease_expires_at IS NULL)),
    CHECK ((status='COMPLETED' AND phase='COMPLETED' AND completed_at IS NOT NULL)
        OR (status<>'COMPLETED' AND completed_at IS NULL)),
    CHECK (evidence_retention_millis BETWEEN 86400000 AND 220752000000),
    CHECK (updated_at>=created_at AND (completed_at IS NULL OR completed_at>=created_at))
);

CREATE INDEX session_deletion_jobs_claim_idx
    ON session_deletion_jobs(status,lease_expires_at,updated_at,tenant_id,id)
    WHERE status IN ('PENDING','RUNNING','RETRY');

CREATE TABLE compliance_deletion_evidence (
    tenant_id text NOT NULL,
    id text NOT NULL,
    job_id text NOT NULL,
    session_id text NOT NULL,
    event_type text NOT NULL,
    phase text NOT NULL,
    failure_class text,
    actor_sha256 bytea NOT NULL,
    policy_version text NOT NULL,
    target_count integer NOT NULL,
    occurred_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id,id),
    CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$' AND id ~ '^cev_[A-Za-z0-9_-]{4,124}$'
        AND job_id ~ '^del_[A-Za-z0-9_-]{4,124}$' AND session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$'),
    CHECK (event_type IN ('REQUESTED','EXTERNAL_DELETED','POSTGRES_DELETED','FAILED','COMPLETED')),
    CHECK (phase IN ('PENDING','EXTERNAL_DELETED','POSTGRES_DELETED','COMPLETED')),
    CHECK ((event_type='FAILED' AND failure_class ~ '^[a-z][a-z0-9_.-]{0,63}$')
        OR (event_type<>'FAILED' AND failure_class IS NULL)),
    CHECK (octet_length(actor_sha256)=32 AND policy_version ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (target_count BETWEEN 0 AND 10000 AND expires_at>=occurred_at)
);

CREATE INDEX compliance_deletion_evidence_query_idx
    ON compliance_deletion_evidence(tenant_id,occurred_at,id);
CREATE INDEX compliance_deletion_evidence_expiry_idx
    ON compliance_deletion_evidence(tenant_id,expires_at,id);
