DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM tool_approvals)
       OR EXISTS (SELECT 1 FROM runs WHERE status='WAITING_APPROVAL')
       OR EXISTS (SELECT 1 FROM tool_audits WHERE policy_decision='REQUIRE_APPROVAL') THEN
        RAISE EXCEPTION 'cannot remove tool approvals while approval state or audit history exists';
    END IF;
END $$;

DROP TABLE tool_approvals;

ALTER TABLE tool_audits DROP CONSTRAINT tool_audits_approval;
ALTER TABLE tool_audits DROP CONSTRAINT tool_audits_decision;
ALTER TABLE tool_audits DROP COLUMN approval_id;
ALTER TABLE tool_audits ADD CONSTRAINT tool_audits_decision
    CHECK (policy_decision IN ('ALLOW','DENY'));
ALTER TABLE tool_audits DROP CONSTRAINT tool_audits_effect;
ALTER TABLE tool_audits ADD CONSTRAINT tool_audits_effect CHECK (effect='READ_ONLY');

ALTER TABLE tool_calls DROP CONSTRAINT tool_calls_effect_valid;
ALTER TABLE tool_calls ADD CONSTRAINT tool_calls_effect_valid CHECK (effect='READ_ONLY');

ALTER TABLE runs DROP CONSTRAINT runs_status_valid;
ALTER TABLE runs ADD CONSTRAINT runs_status_valid CHECK (status IN (
    'QUEUED','RUNNING','CANCEL_REQUESTED','SUCCEEDED','SELECTED',
    'SUPERSEDED','FAILED','CANCELLED','TIMED_OUT'
));
