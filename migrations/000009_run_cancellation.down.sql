ALTER TABLE runs
    DROP CONSTRAINT runs_cancellation_shape,
    DROP COLUMN cancelled_at,
    DROP COLUMN cancel_requested_at;
