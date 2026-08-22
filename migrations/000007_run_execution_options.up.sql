ALTER TABLE runs
    ADD COLUMN max_steps smallint NOT NULL DEFAULT 12,
    ADD CONSTRAINT runs_max_steps_range CHECK (max_steps BETWEEN 1 AND 20);
