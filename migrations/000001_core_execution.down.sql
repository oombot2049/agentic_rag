ALTER TABLE sessions DROP CONSTRAINT sessions_head_turn_fk;
ALTER TABLE sessions DROP CONSTRAINT sessions_active_run_fk;
ALTER TABLE runs DROP CONSTRAINT runs_output_message_fk;
ALTER TABLE turns DROP CONSTRAINT turns_selected_run_fk;
ALTER TABLE turns DROP CONSTRAINT turns_user_message_fk;
ALTER TABLE messages DROP CONSTRAINT messages_run_fk;

DROP TABLE attempts;
DROP TABLE steps;
DROP TABLE messages;
DROP TABLE runs;
DROP TABLE turns;
DROP TABLE sessions;

