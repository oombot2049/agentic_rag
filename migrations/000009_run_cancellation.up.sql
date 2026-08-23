ALTER TABLE runs
    ADD COLUMN cancel_requested_at timestamptz,
    ADD COLUMN cancelled_at timestamptz;

UPDATE runs SET cancel_requested_at=updated_at WHERE status='CANCEL_REQUESTED';
UPDATE runs SET cancel_requested_at=updated_at,cancelled_at=updated_at WHERE status='CANCELLED';

ALTER TABLE runs
    ADD CONSTRAINT runs_cancellation_shape CHECK (
        (status = 'CANCEL_REQUESTED' AND cancel_requested_at IS NOT NULL AND cancelled_at IS NULL)
        OR (status = 'CANCELLED' AND cancel_requested_at IS NOT NULL AND cancelled_at IS NOT NULL
            AND cancelled_at >= cancel_requested_at)
        OR (status NOT IN ('CANCEL_REQUESTED', 'CANCELLED')
            AND cancel_requested_at IS NULL AND cancelled_at IS NULL)
    );
