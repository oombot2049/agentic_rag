ALTER TABLE sessions
    ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT sessions_metadata_object CHECK (jsonb_typeof(metadata) = 'object'),
    ADD CONSTRAINT sessions_metadata_size CHECK (jsonb_object_length(metadata) <= 20);
