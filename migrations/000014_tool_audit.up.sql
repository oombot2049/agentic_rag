CREATE TABLE tool_audits (
    tenant_id text NOT NULL,
    id text NOT NULL,
    session_id text NOT NULL,
    turn_id text NOT NULL,
    run_id text NOT NULL,
    step_id text NOT NULL,
    tool_call_id text NOT NULL,
    caller_subject text NOT NULL,
    tool_name text NOT NULL,
    tool_version text NOT NULL,
    effect text NOT NULL,
    policy_decision text NOT NULL,
    approval_id text,
    arguments_sha256 bytea NOT NULL,
    outcome text NOT NULL,
    result_sha256 bytea,
    result_size_bytes bigint NOT NULL,
    business_impact_code text NOT NULL,
    artifact_id text,
    artifact_sha256 bytea,
    artifact_size_bytes bigint,
    artifact_media_type text,
    attempt_count integer NOT NULL,
    started_at timestamptz NOT NULL,
    ended_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CONSTRAINT tool_audits_pk PRIMARY KEY (tenant_id,id),
    CONSTRAINT tool_audits_call_unique UNIQUE (tenant_id,tool_call_id),
    CONSTRAINT tool_audits_ids CHECK (
        id ~ '^aud_[A-Za-z0-9_-]{4,124}$' AND session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$'
        AND turn_id ~ '^trn_[A-Za-z0-9_-]{4,124}$' AND run_id ~ '^run_[A-Za-z0-9_-]{4,124}$'
        AND step_id ~ '^stp_[A-Za-z0-9_-]{4,124}$' AND tool_call_id ~ '^tcl_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT tool_audits_caller CHECK (caller_subject ~ '^[A-Za-z0-9][A-Za-z0-9_.:@/-]{0,255}$'),
    CONSTRAINT tool_audits_tool CHECK (tool_name ~ '^[a-z][a-z0-9]*(\.[a-z][a-z0-9_-]*)*$' AND tool_version ~ '^[0-9]+\.[0-9]+\.[0-9]+$'),
    CONSTRAINT tool_audits_effect CHECK (effect IN ('READ_ONLY','IDEMPOTENT_WRITE','DESTRUCTIVE')),
    CONSTRAINT tool_audits_decision CHECK (policy_decision IN ('ALLOW','DENY','REQUIRE_APPROVAL')),
    CONSTRAINT tool_audits_approval CHECK (
        (policy_decision='REQUIRE_APPROVAL' AND approval_id ~ '^apr_[A-Za-z0-9_-]{4,124}$')
        OR (policy_decision<>'REQUIRE_APPROVAL' AND approval_id IS NULL)),
    CONSTRAINT tool_audits_arguments_hash CHECK (octet_length(arguments_sha256)=32),
    CONSTRAINT tool_audits_outcome CHECK (outcome IN ('SUCCEEDED','FAILED','UNKNOWN','DENIED')),
    CONSTRAINT tool_audits_result CHECK (
        (outcome='SUCCEEDED' AND octet_length(result_sha256)=32 AND result_size_bytes >= 0)
        OR (outcome<>'SUCCEEDED' AND result_sha256 IS NULL AND result_size_bytes=0)),
    CONSTRAINT tool_audits_impact CHECK (business_impact_code ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CONSTRAINT tool_audits_artifact CHECK (
        (artifact_id IS NULL AND artifact_sha256 IS NULL AND artifact_size_bytes IS NULL AND artifact_media_type IS NULL)
        OR (outcome='SUCCEEDED' AND artifact_id ~ '^art_[A-Za-z0-9_-]{4,124}$' AND octet_length(artifact_sha256)=32
            AND artifact_sha256=result_sha256 AND artifact_size_bytes=result_size_bytes AND artifact_size_bytes>0
            AND artifact_media_type ~ '^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}$')),
    CONSTRAINT tool_audits_attempt_count CHECK (
        (outcome='DENIED' AND policy_decision='DENY' AND attempt_count=0 AND ended_at=started_at)
        OR (outcome<>'DENIED' AND policy_decision<>'DENY' AND attempt_count BETWEEN 1 AND 100)),
    CONSTRAINT tool_audits_times CHECK (ended_at>=started_at AND created_at>=ended_at
        AND expires_at>=created_at+interval '1 hour' AND expires_at<=created_at+interval '365 days'),
    CONSTRAINT tool_audits_run_fk FOREIGN KEY (tenant_id,run_id)
        REFERENCES runs(tenant_id,id) ON DELETE CASCADE,
    CONSTRAINT tool_audits_artifact_fk FOREIGN KEY (tenant_id,artifact_id)
        REFERENCES artifact_metadata(tenant_id,id) DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE tool_audit_attempts (
    tenant_id text NOT NULL,
    audit_id text NOT NULL,
    attempt_id text NOT NULL,
    attempt_no bigint NOT NULL,
    status text NOT NULL,
    failure_class text,
    started_at timestamptz NOT NULL,
    ended_at timestamptz NOT NULL,
    CONSTRAINT tool_audit_attempts_pk PRIMARY KEY (tenant_id,audit_id,attempt_no),
    CONSTRAINT tool_audit_attempts_id_unique UNIQUE (tenant_id,audit_id,attempt_id),
    CONSTRAINT tool_audit_attempts_shape CHECK (attempt_no>0 AND ended_at>=started_at AND (
        (status='SUCCEEDED' AND failure_class IS NULL)
        OR (status IN ('FAILED','UNKNOWN') AND failure_class ~ '^[a-z][a-z0-9_]{0,63}$'))),
    CONSTRAINT tool_audit_attempts_audit_fk FOREIGN KEY (tenant_id,audit_id)
        REFERENCES tool_audits(tenant_id,id) ON DELETE CASCADE,
    CONSTRAINT tool_audit_attempts_attempt_fk FOREIGN KEY (tenant_id,attempt_id)
        REFERENCES attempts(tenant_id,id) ON DELETE CASCADE
);

CREATE INDEX tool_audits_run_idx ON tool_audits(tenant_id,run_id,created_at,id);
CREATE INDEX tool_audits_expiry_idx ON tool_audits(tenant_id,expires_at,id);
CREATE INDEX tool_audit_attempts_order_idx ON tool_audit_attempts(tenant_id,audit_id,attempt_no);
