CREATE TABLE sessions (
    tenant_id text NOT NULL,
    id text NOT NULL,
    context_version bigint NOT NULL DEFAULT 0,
    row_version bigint NOT NULL DEFAULT 1,
    next_turn_sequence bigint NOT NULL DEFAULT 1,
    committed_turn_sequence bigint NOT NULL DEFAULT 0,
    active_run_id text,
    head_turn_id text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT sessions_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT sessions_tenant_id_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT sessions_id_format CHECK (id ~ '^ses_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT sessions_context_version_nonnegative CHECK (context_version >= 0),
    CONSTRAINT sessions_row_version_positive CHECK (row_version > 0),
    CONSTRAINT sessions_next_turn_sequence_positive CHECK (next_turn_sequence > 0),
    CONSTRAINT sessions_committed_sequence_valid CHECK (
        committed_turn_sequence >= 0 AND committed_turn_sequence < next_turn_sequence
    ),
    CONSTRAINT sessions_head_consistent CHECK (
        (committed_turn_sequence = 0 AND head_turn_id IS NULL)
        OR (committed_turn_sequence > 0 AND head_turn_id IS NOT NULL)
    ),
    CONSTRAINT sessions_time_order CHECK (updated_at >= created_at)
);

CREATE TABLE turns (
    tenant_id text NOT NULL,
    id text NOT NULL,
    session_id text NOT NULL,
    turn_sequence bigint NOT NULL,
    base_context_version bigint NOT NULL,
    user_message_id text NOT NULL,
    status text NOT NULL,
    selected_run_id text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT turns_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT turns_parent_identity UNIQUE (tenant_id, session_id, id),
    CONSTRAINT turns_sequence_unique UNIQUE (tenant_id, session_id, turn_sequence),
    CONSTRAINT turns_tenant_id_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT turns_id_format CHECK (id ~ '^trn_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT turns_session_id_format CHECK (session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT turns_user_message_id_format CHECK (user_message_id ~ '^msg_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT turns_selected_run_id_format CHECK (
        selected_run_id IS NULL OR selected_run_id ~ '^run_[A-Za-z0-9_-]{4,124}$'
    ),
    CONSTRAINT turns_sequence_positive CHECK (turn_sequence > 0),
    CONSTRAINT turns_base_context_nonnegative CHECK (base_context_version >= 0),
    CONSTRAINT turns_status_valid CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED')),
    CONSTRAINT turns_completed_selected CHECK (status <> 'COMPLETED' OR selected_run_id IS NOT NULL),
    CONSTRAINT turns_time_order CHECK (updated_at >= created_at),
    CONSTRAINT turns_session_fk FOREIGN KEY (tenant_id, session_id)
        REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE messages (
    tenant_id text NOT NULL,
    id text NOT NULL,
    session_id text NOT NULL,
    turn_id text NOT NULL,
    run_id text,
    role text NOT NULL,
    status text NOT NULL DEFAULT 'COMPLETED',
    visibility text NOT NULL,
    content text NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT messages_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT messages_turn_identity UNIQUE (tenant_id, session_id, turn_id, id),
    CONSTRAINT messages_run_identity UNIQUE (tenant_id, session_id, turn_id, run_id, id),
    CONSTRAINT messages_tenant_id_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT messages_id_format CHECK (id ~ '^msg_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT messages_session_id_format CHECK (session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT messages_turn_id_format CHECK (turn_id ~ '^trn_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT messages_run_id_format CHECK (run_id IS NULL OR run_id ~ '^run_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT messages_role_valid CHECK (role IN ('USER', 'ASSISTANT', 'SYSTEM', 'TOOL')),
    CONSTRAINT messages_status_valid CHECK (status = 'COMPLETED'),
    CONSTRAINT messages_visibility_valid CHECK (visibility IN ('CONVERSATION', 'CANDIDATE', 'INTERNAL')),
    CONSTRAINT messages_content_nonempty CHECK (length(btrim(content)) > 0),
    CONSTRAINT messages_user_shape CHECK (
        role <> 'USER' OR (run_id IS NULL AND visibility = 'CONVERSATION')
    ),
    CONSTRAINT messages_assistant_shape CHECK (role <> 'ASSISTANT' OR run_id IS NOT NULL),
    CONSTRAINT messages_candidate_shape CHECK (visibility <> 'CANDIDATE' OR role = 'ASSISTANT'),
    CONSTRAINT messages_turn_fk FOREIGN KEY (tenant_id, session_id, turn_id)
        REFERENCES turns (tenant_id, session_id, id) ON DELETE CASCADE
);

CREATE TABLE runs (
    tenant_id text NOT NULL,
    id text NOT NULL,
    session_id text NOT NULL,
    turn_id text NOT NULL,
    run_no bigint NOT NULL,
    base_context_version bigint NOT NULL,
    status text NOT NULL,
    output_message_id text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT runs_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT runs_session_identity UNIQUE (tenant_id, session_id, id),
    CONSTRAINT runs_turn_identity UNIQUE (tenant_id, session_id, turn_id, id),
    CONSTRAINT runs_number_unique UNIQUE (tenant_id, session_id, turn_id, run_no),
    CONSTRAINT runs_tenant_id_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT runs_id_format CHECK (id ~ '^run_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT runs_session_id_format CHECK (session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT runs_turn_id_format CHECK (turn_id ~ '^trn_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT runs_output_message_id_format CHECK (
        output_message_id IS NULL OR output_message_id ~ '^msg_[A-Za-z0-9_-]{4,124}$'
    ),
    CONSTRAINT runs_number_positive CHECK (run_no > 0),
    CONSTRAINT runs_base_context_nonnegative CHECK (base_context_version >= 0),
    CONSTRAINT runs_status_valid CHECK (status IN (
        'QUEUED', 'RUNNING', 'CANCEL_REQUESTED', 'SUCCEEDED', 'SELECTED',
        'SUPERSEDED', 'FAILED', 'CANCELLED', 'TIMED_OUT'
    )),
    CONSTRAINT runs_success_output CHECK (
        status NOT IN ('SUCCEEDED', 'SELECTED', 'SUPERSEDED') OR output_message_id IS NOT NULL
    ),
    CONSTRAINT runs_time_order CHECK (updated_at >= created_at),
    CONSTRAINT runs_turn_fk FOREIGN KEY (tenant_id, session_id, turn_id)
        REFERENCES turns (tenant_id, session_id, id) ON DELETE CASCADE
);

CREATE TABLE steps (
    tenant_id text NOT NULL,
    id text NOT NULL,
    run_id text NOT NULL,
    step_no bigint NOT NULL,
    kind text NOT NULL,
    status text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT steps_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT steps_number_unique UNIQUE (tenant_id, run_id, step_no),
    CONSTRAINT steps_tenant_id_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT steps_id_format CHECK (id ~ '^stp_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT steps_run_id_format CHECK (run_id ~ '^run_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT steps_number_positive CHECK (step_no > 0),
    CONSTRAINT steps_kind_valid CHECK (kind IN ('CONTEXT', 'RETRIEVAL', 'TOOL', 'GENERATION')),
    CONSTRAINT steps_status_valid CHECK (status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED', 'SKIPPED')),
    CONSTRAINT steps_time_order CHECK (updated_at >= created_at),
    CONSTRAINT steps_run_fk FOREIGN KEY (tenant_id, run_id)
        REFERENCES runs (tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE attempts (
    tenant_id text NOT NULL,
    id text NOT NULL,
    step_id text NOT NULL,
    attempt_no bigint NOT NULL,
    status text NOT NULL,
    started_at timestamptz NOT NULL,
    ended_at timestamptz,
    CONSTRAINT attempts_pk PRIMARY KEY (tenant_id, id),
    CONSTRAINT attempts_number_unique UNIQUE (tenant_id, step_id, attempt_no),
    CONSTRAINT attempts_tenant_id_format CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT attempts_id_format CHECK (id ~ '^att_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT attempts_step_id_format CHECK (step_id ~ '^stp_[A-Za-z0-9_-]{4,124}$'),
    CONSTRAINT attempts_number_positive CHECK (attempt_no > 0),
    CONSTRAINT attempts_status_valid CHECK (status IN ('STARTED', 'SUCCEEDED', 'FAILED', 'UNKNOWN')),
    CONSTRAINT attempts_end_shape CHECK (
        (status = 'STARTED' AND ended_at IS NULL)
        OR (status <> 'STARTED' AND ended_at IS NOT NULL AND ended_at >= started_at)
    ),
    CONSTRAINT attempts_step_fk FOREIGN KEY (tenant_id, step_id)
        REFERENCES steps (tenant_id, id) ON DELETE CASCADE
);

ALTER TABLE messages ADD CONSTRAINT messages_run_fk
    FOREIGN KEY (tenant_id, session_id, turn_id, run_id)
    REFERENCES runs (tenant_id, session_id, turn_id, id)
    ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE turns ADD CONSTRAINT turns_user_message_fk
    FOREIGN KEY (tenant_id, session_id, id, user_message_id)
    REFERENCES messages (tenant_id, session_id, turn_id, id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE turns ADD CONSTRAINT turns_selected_run_fk
    FOREIGN KEY (tenant_id, session_id, id, selected_run_id)
    REFERENCES runs (tenant_id, session_id, turn_id, id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE runs ADD CONSTRAINT runs_output_message_fk
    FOREIGN KEY (tenant_id, session_id, turn_id, id, output_message_id)
    REFERENCES messages (tenant_id, session_id, turn_id, run_id, id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE sessions ADD CONSTRAINT sessions_active_run_fk
    FOREIGN KEY (tenant_id, id, active_run_id)
    REFERENCES runs (tenant_id, session_id, id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE sessions ADD CONSTRAINT sessions_head_turn_fk
    FOREIGN KEY (tenant_id, id, head_turn_id)
    REFERENCES turns (tenant_id, session_id, id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX sessions_active_run_idx
    ON sessions (tenant_id, active_run_id)
    WHERE active_run_id IS NOT NULL;

CREATE INDEX turns_session_created_idx
    ON turns (tenant_id, session_id, created_at DESC, id DESC);

CREATE UNIQUE INDEX messages_one_user_per_turn_idx
    ON messages (tenant_id, session_id, turn_id)
    WHERE role = 'USER';

CREATE INDEX messages_conversation_history_idx
    ON messages (tenant_id, session_id, created_at, id)
    WHERE visibility = 'CONVERSATION';

CREATE INDEX messages_run_idx
    ON messages (tenant_id, run_id, created_at, id)
    WHERE run_id IS NOT NULL;

CREATE INDEX runs_active_session_idx
    ON runs (tenant_id, session_id, updated_at, id)
    WHERE status IN ('QUEUED', 'RUNNING', 'CANCEL_REQUESTED', 'SUCCEEDED');

CREATE INDEX runs_status_idx
    ON runs (tenant_id, status, updated_at, id);

CREATE INDEX attempts_unknown_idx
    ON attempts (tenant_id, ended_at, id)
    WHERE status = 'UNKNOWN';

