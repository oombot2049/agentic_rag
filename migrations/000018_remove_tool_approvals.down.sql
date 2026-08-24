ALTER TABLE runs DROP CONSTRAINT runs_status_valid;
ALTER TABLE runs ADD CONSTRAINT runs_status_valid CHECK (status IN (
    'QUEUED','RUNNING','WAITING_APPROVAL','CANCEL_REQUESTED','SUCCEEDED','SELECTED',
    'SUPERSEDED','FAILED','CANCELLED','TIMED_OUT'
));

ALTER TABLE tool_audits DROP CONSTRAINT tool_audits_decision;
ALTER TABLE tool_audits ADD COLUMN approval_id text;
ALTER TABLE tool_audits ADD CONSTRAINT tool_audits_decision
    CHECK (policy_decision IN ('ALLOW','DENY','REQUIRE_APPROVAL'));
ALTER TABLE tool_audits ADD CONSTRAINT tool_audits_approval CHECK (
    (policy_decision='REQUIRE_APPROVAL' AND approval_id ~ '^apr_[A-Za-z0-9_-]{4,124}$')
    OR (policy_decision<>'REQUIRE_APPROVAL' AND approval_id IS NULL)
);
ALTER TABLE tool_audits DROP CONSTRAINT tool_audits_effect;
ALTER TABLE tool_audits ADD CONSTRAINT tool_audits_effect
    CHECK (effect IN ('READ_ONLY','IDEMPOTENT_WRITE','DESTRUCTIVE'));

ALTER TABLE tool_calls DROP CONSTRAINT tool_calls_effect_valid;
ALTER TABLE tool_calls ADD CONSTRAINT tool_calls_effect_valid
    CHECK (effect IN ('READ_ONLY','IDEMPOTENT_WRITE','DESTRUCTIVE'));

CREATE TABLE tool_approvals (
    tenant_id text NOT NULL,
    id text NOT NULL,
    requester_subject text NOT NULL,
    approver_subject text NOT NULL,
    session_id text NOT NULL,
    turn_id text NOT NULL,
    run_id text NOT NULL,
    step_id text NOT NULL,
    attempt_id text NOT NULL,
    tool_call_id text NOT NULL,
    tool_name text NOT NULL,
    tool_version text NOT NULL,
    effect text NOT NULL,
    arguments_sha256 bytea NOT NULL,
    nonce_sha256 bytea NOT NULL,
    worker_owner text NOT NULL,
    lease_token bigint NOT NULL,
    execution_version bigint NOT NULL,
    status text NOT NULL,
    decision_actor text,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    decided_at timestamptz,
    row_version bigint NOT NULL DEFAULT 1,
    CONSTRAINT tool_approvals_pk PRIMARY KEY (tenant_id,id),
    CONSTRAINT tool_approvals_step_unique UNIQUE (tenant_id,run_id,step_id),
    CONSTRAINT tool_approvals_id_format CHECK (id ~ '^apr_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT tool_approvals_effect_valid CHECK (effect IN ('IDEMPOTENT_WRITE','DESTRUCTIVE')),
    CONSTRAINT tool_approvals_hashes CHECK (octet_length(arguments_sha256)=32 AND octet_length(nonce_sha256)=32),
    CONSTRAINT tool_approvals_authority CHECK (length(worker_owner) BETWEEN 1 AND 256 AND lease_token > 0 AND execution_version > 0),
    CONSTRAINT tool_approvals_status_valid CHECK (status IN ('WAITING','APPROVED','REJECTED','EXPIRED')),
    CONSTRAINT tool_approvals_decision_shape CHECK (
        (status='WAITING' AND decision_actor IS NULL AND decided_at IS NULL)
        OR (status<>'WAITING' AND decision_actor IS NOT NULL AND decided_at IS NOT NULL)),
    CONSTRAINT tool_approvals_time_order CHECK (expires_at >= created_at + interval '1 minute'
        AND expires_at <= created_at + interval '7 days' AND (decided_at IS NULL OR decided_at >= created_at)),
    CONSTRAINT tool_approvals_run_fk FOREIGN KEY (tenant_id,run_id) REFERENCES runs(tenant_id,id) ON DELETE CASCADE,
    CONSTRAINT tool_approvals_step_fk FOREIGN KEY (tenant_id,step_id) REFERENCES steps(tenant_id,id) ON DELETE CASCADE,
    CONSTRAINT tool_approvals_attempt_fk FOREIGN KEY (tenant_id,attempt_id) REFERENCES attempts(tenant_id,id) ON DELETE CASCADE,
    CONSTRAINT tool_approvals_call_fk FOREIGN KEY (tenant_id,tool_call_id) REFERENCES tool_calls(tenant_id,id) ON DELETE CASCADE
);

CREATE INDEX tool_approvals_expiry_idx ON tool_approvals(tenant_id,expires_at,id) WHERE status='WAITING';
CREATE INDEX tool_approvals_actor_idx ON tool_approvals(tenant_id,approver_subject,status,created_at,id);
