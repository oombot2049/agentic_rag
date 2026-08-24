DROP INDEX runs_lease_scan_idx;
DROP TABLE run_checkpoints;

ALTER TABLE attempts
    DROP CONSTRAINT attempts_row_version_positive,
    DROP COLUMN row_version;

ALTER TABLE steps
    DROP CONSTRAINT steps_row_version_positive,
    DROP COLUMN row_version;

ALTER TABLE runs
    DROP CONSTRAINT runs_lease_shape,
    DROP CONSTRAINT runs_lease_token_nonnegative,
    DROP CONSTRAINT runs_execution_version_positive,
    DROP COLUMN lease_expires_at,
    DROP COLUMN lease_token,
    DROP COLUMN lease_owner,
    DROP COLUMN execution_version;
