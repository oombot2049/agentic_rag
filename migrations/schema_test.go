package migrations

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	upMigration   = "000001_core_execution.up.sql"
	downMigration = "000001_core_execution.down.sql"
)

var coreTables = []string{"attempts", "messages", "runs", "sessions", "steps", "turns"}

func TestMigrationFilesArePairedAndEmbedded(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	want := []string{
		downMigration, upMigration,
		"000002_idempotency_records.down.sql", "000002_idempotency_records.up.sql",
		"000003_execution_control.down.sql", "000003_execution_control.up.sql",
		"000004_transactional_outbox.down.sql", "000004_transactional_outbox.up.sql",
		"000005_artifact_metadata.down.sql", "000005_artifact_metadata.up.sql",
		"000006_session_metadata.down.sql", "000006_session_metadata.up.sql",
		"000007_run_execution_options.down.sql", "000007_run_execution_options.up.sql",
		"000008_run_events.down.sql", "000008_run_events.up.sql",
		"000009_run_cancellation.down.sql", "000009_run_cancellation.up.sql",
		"000010_run_queue.down.sql", "000010_run_queue.up.sql",
		"000011_retrieval_audit.down.sql", "000011_retrieval_audit.up.sql",
		"000012_tool_call_ledger.down.sql", "000012_tool_call_ledger.up.sql",
		"000013_tool_approvals.down.sql", "000013_tool_approvals.up.sql",
		"000014_tool_audit.down.sql", "000014_tool_audit.up.sql",
		"000015_compliance_deletion.down.sql", "000015_compliance_deletion.up.sql",
		"000016_run_event_outbox_order.down.sql", "000016_run_event_outbox_order.up.sql",
		"000017_api_keys.down.sql", "000017_api_keys.up.sql",
		"000018_remove_tool_approvals.down.sql", "000018_remove_tool_approvals.up.sql",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("embedded SQL files = %v, want %v", names, want)
	}
}

func TestComplianceDeletionMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000015_compliance_deletion.up.sql")
	for _, fragment := range []string{
		"CREATE TABLE compliance_external_targets", "CREATE TABLE legal_holds",
		"CREATE TABLE session_deletion_jobs", "CREATE TABLE compliance_deletion_evidence",
		"UNIQUE (tenant_id,session_id)", "jsonb_array_length(manifest)<=10000",
		"legal_holds_one_active_session_idx", "session_deletion_jobs_claim_idx",
		"compliance_deletion_evidence_query_idx", "octet_length(actor_sha256)=32",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("compliance migration missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"prompt", "message_content", "tool_arguments", "provider_error"} {
		if strings.Contains(strings.ToLower(up), forbidden) {
			t.Errorf("compliance migration stores forbidden detail %q", forbidden)
		}
	}
}

func TestToolAuditMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000014_tool_audit.up.sql")
	for _, fragment := range []string{
		"CREATE TABLE tool_audits", "CREATE TABLE tool_audit_attempts",
		"PRIMARY KEY (tenant_id,id)", "UNIQUE (tenant_id,tool_call_id)",
		"octet_length(arguments_sha256)=32", "policy_decision IN ('ALLOW','DENY','REQUIRE_APPROVAL')",
		"artifact_sha256=result_sha256", "tool_audits_run_idx", "tool_audits_expiry_idx",
		"FOREIGN KEY (tenant_id,artifact_id)",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("tool audit migration missing %q", fragment)
		}
	}
	for _, forbidden := range []string{"arguments json", "result json", "credential", "prompt", "reasoning"} {
		if strings.Contains(strings.ToLower(up), forbidden) {
			t.Errorf("tool audit migration stores forbidden content %q", forbidden)
		}
	}
	down := readMigration(t, "000014_tool_audit.down.sql")
	if strings.Index(down, "DROP TABLE tool_audit_attempts") > strings.Index(down, "DROP TABLE tool_audits") {
		t.Fatal("tool audit down migration has unsafe dependency order")
	}
}

func TestToolApprovalsAreRemovedByForwardMigration(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000018_remove_tool_approvals.up.sql")
	for _, fragment := range []string{"DROP TABLE tool_approvals", "DROP COLUMN approval_id",
		"policy_decision IN ('ALLOW','DENY')", "'QUEUED','RUNNING','CANCEL_REQUESTED'",
		"tool_calls_effect_valid CHECK (effect='READ_ONLY')", "tool_audits_effect CHECK (effect='READ_ONLY')"} {
		if !strings.Contains(up, fragment) {
			t.Errorf("tool approval removal migration missing %q", fragment)
		}
	}
}

func TestToolCallLedgerMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000012_tool_call_ledger.up.sql")
	for _, fragment := range []string{
		"CREATE TABLE tool_calls", "CREATE TABLE tool_call_attempts",
		"PRIMARY KEY (tenant_id, id)", "UNIQUE (tenant_id, run_id, step_id)",
		"UNIQUE (tenant_id, invocation_key)", "octet_length(arguments_sha256)=32",
		"'PREPARED','SUCCEEDED','FAILED','UNKNOWN'", "jsonb_typeof(result)='object'",
		"PRIMARY KEY (tenant_id, attempt_id)", "UNIQUE (tenant_id, call_id, attempt_no)",
		"tool_calls_reconciliation_idx", "FOREIGN KEY (tenant_id, attempt_id)",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("tool call ledger migration missing %q", fragment)
		}
	}
	down := readMigration(t, "000012_tool_call_ledger.down.sql")
	if strings.Index(down, "DROP TABLE tool_call_attempts") > strings.Index(down, "DROP TABLE tool_calls") {
		t.Fatal("tool call ledger down migration has unsafe dependency order")
	}
}

func TestRetrievalAuditMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000011_retrieval_audit.up.sql")
	for _, fragment := range []string{"CREATE TABLE retrieval_audits", "CREATE TABLE retrieval_audit_candidates",
		"UNIQUE (tenant_id,retrieval_id,chunk_id)", "FOREIGN KEY (tenant_id,artifact_id)",
		"octet_length(content_sha256)=32", "retrieval_audits_expiry_idx", "WHERE selected"} {
		if !strings.Contains(up, fragment) {
			t.Errorf("retrieval audit migration missing %q", fragment)
		}
	}
}

func TestRunQueueMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000010_run_queue.up.sql")
	for _, fragment := range []string{"CREATE TABLE run_queue_messages", "PRIMARY KEY (tenant_id, run_id)",
		"UNIQUE (tenant_id, delivery_id)", "status IN ('READY','LEASED','ACKED')", "lease_token bigint NOT NULL DEFAULT 0",
		"run_queue_ready_idx", "run_queue_expired_lease_idx", "run_queue_partition_idx", "REFERENCES runs"} {
		if !strings.Contains(up, fragment) {
			t.Errorf("Run queue migration missing %q", fragment)
		}
	}
}

func TestRunCancellationMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000009_run_cancellation.up.sql")
	for _, fragment := range []string{"cancel_requested_at timestamptz", "cancelled_at timestamptz", "runs_cancellation_shape",
		"status = 'CANCEL_REQUESTED'", "status = 'CANCELLED'", "cancelled_at >= cancel_requested_at"} {
		if !strings.Contains(up, fragment) {
			t.Errorf("Run cancellation migration missing %q", fragment)
		}
	}
}

func TestRunEventsMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000008_run_events.up.sql")
	for _, fragment := range []string{
		"CREATE TABLE run_event_streams", "PRIMARY KEY (tenant_id, run_id)",
		"next_sequence bigint NOT NULL DEFAULT 1", "retained_from bigint NOT NULL DEFAULT 1",
		"CREATE TABLE run_events", "PRIMARY KEY (tenant_id, run_id, sequence)",
		"event_data jsonb NOT NULL", "expires_at > occurred_at", "run_events_expiry_idx",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("Run events migration missing %q", fragment)
		}
	}
	down := readMigration(t, "000008_run_events.down.sql")
	if !strings.Contains(down, "DROP TABLE run_events;") || !strings.Contains(down, "DROP TABLE run_event_streams;") {
		t.Fatal("Run events down migration must drop both owned tables in dependency order")
	}
}

func TestRunExecutionOptionsMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000007_run_execution_options.up.sql")
	for _, fragment := range []string{
		"ADD COLUMN max_steps smallint NOT NULL DEFAULT 12",
		"max_steps BETWEEN 1 AND 20",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("Run execution options migration missing %q", fragment)
		}
	}
	if strings.TrimSpace(readMigration(t, "000007_run_execution_options.down.sql")) != "ALTER TABLE runs DROP COLUMN max_steps;" {
		t.Fatal("Run execution options down migration must drop only its column")
	}
}

func TestSessionMetadataMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000006_session_metadata.up.sql")
	for _, fragment := range []string{
		"ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}'::jsonb",
		"jsonb_typeof(metadata) = 'object'", "jsonb_object_length(metadata) <= 20",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("Session metadata migration missing %q", fragment)
		}
	}
	if strings.TrimSpace(readMigration(t, "000006_session_metadata.down.sql")) != "ALTER TABLE sessions DROP COLUMN metadata;" {
		t.Fatal("Session metadata down migration must drop only its column")
	}
}

func TestArtifactMetadataMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000005_artifact_metadata.up.sql")
	for _, fragment := range []string{
		"CREATE TABLE artifact_metadata", "PRIMARY KEY (tenant_id, id)",
		"content_sha256 bytea NOT NULL", "octet_length(content_sha256) = 32",
		"size_bytes bigint NOT NULL", "'SSE-S3', 'SSE-KMS', 'CLIENT'",
		"expires_at > created_at", "artifact_metadata_expiry_idx",
		"ON artifact_metadata (tenant_id, expires_at, id)",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("Artifact migration missing %q", fragment)
		}
	}
	if strings.Contains(up, "payload") || strings.Contains(up, "content bytea") {
		t.Fatal("Artifact migration stores payload bytes in PostgreSQL")
	}
	if strings.TrimSpace(readMigration(t, "000005_artifact_metadata.down.sql")) != "DROP TABLE artifact_metadata;" {
		t.Fatal("Artifact down migration must drop only metadata")
	}
}

func TestTransactionalOutboxMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000004_transactional_outbox.up.sql")
	for _, fragment := range []string{
		"CREATE TABLE outbox_events", "PRIMARY KEY (tenant_id, id)",
		"payload jsonb NOT NULL", "octet_length(payload::text) BETWEEN 1 AND 1048576",
		"'PENDING', 'PUBLISHED', 'DEAD'", "claim_token bigint NOT NULL DEFAULT 0",
		"outbox_events_ready_idx", "(tenant_id, status, available_at, occurred_at, id)",
		"outbox_events_cleanup_idx", "WHERE status = 'PUBLISHED'",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("outbox migration missing %q", fragment)
		}
	}
	if strings.TrimSpace(readMigration(t, "000004_transactional_outbox.down.sql")) != "DROP TABLE outbox_events;" {
		t.Fatal("outbox down migration must drop only its table")
	}
}

func TestExecutionControlMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000003_execution_control.up.sql")
	for _, fragment := range []string{
		"execution_version bigint NOT NULL DEFAULT 1",
		"lease_token bigint NOT NULL DEFAULT 0",
		"lease_expires_at timestamptz",
		"ADD COLUMN row_version bigint NOT NULL DEFAULT 1",
		"CREATE TABLE run_checkpoints",
		"PRIMARY KEY (tenant_id, run_id)",
		"octet_length(payload::text) <= 1048576",
		"ON runs (tenant_id, status, lease_expires_at, id)",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("execution migration missing %q", fragment)
		}
	}
	down := readMigration(t, "000003_execution_control.down.sql")
	for _, fragment := range []string{"DROP TABLE run_checkpoints", "DROP COLUMN execution_version"} {
		if !strings.Contains(down, fragment) {
			t.Errorf("execution down migration missing %q", fragment)
		}
	}
}

func TestIdempotencyMigrationContract(t *testing.T) {
	t.Parallel()
	up := readMigration(t, "000002_idempotency_records.up.sql")
	for _, fragment := range []string{
		"PRIMARY KEY (\n        tenant_id, authenticated_subject_id, operation, idempotency_key",
		"octet_length(request_hash) = 32",
		"'IN_PROGRESS', 'SUCCEEDED', 'FAILED_FINAL'",
		"expires_at >= created_at + interval '24 hours'",
		"ON idempotency_records (tenant_id, expires_at)",
		"response_body jsonb",
		"created_at timestamptz", "updated_at timestamptz", "expires_at timestamptz",
	} {
		if !strings.Contains(up, fragment) {
			t.Errorf("idempotency migration missing %q", fragment)
		}
	}
	down := readMigration(t, "000002_idempotency_records.down.sql")
	if strings.TrimSpace(down) != "DROP TABLE idempotency_records;" {
		t.Fatalf("unexpected idempotency down migration: %q", down)
	}
}

func TestCoreSchemaHasTenantScopedTablesAndTimeTypes(t *testing.T) {
	t.Parallel()
	up := readMigration(t, upMigration)
	createPattern := regexp.MustCompile(`(?m)^CREATE TABLE ([a-z_]+) \(`)
	matches := createPattern.FindAllStringSubmatch(up, -1)
	var gotTables []string
	for _, match := range matches {
		gotTables = append(gotTables, match[1])
	}
	sort.Strings(gotTables)
	if strings.Join(gotTables, ",") != strings.Join(coreTables, ",") {
		t.Fatalf("created tables = %v", gotTables)
	}

	for _, table := range coreTables {
		bodyPattern := regexp.MustCompile(`(?s)CREATE TABLE ` + table + ` \((.*?)\n\);`)
		body := bodyPattern.FindStringSubmatch(up)
		if len(body) != 2 {
			t.Fatalf("table body not found: %s", table)
		}
		if !strings.Contains(body[1], "tenant_id text NOT NULL") {
			t.Errorf("%s has no mandatory tenant_id", table)
		}
		if !strings.Contains(body[1], "PRIMARY KEY (tenant_id, id)") {
			t.Errorf("%s primary key is not tenant-scoped", table)
		}
	}
	timestampColumnPattern := regexp.MustCompile(`(?m)^\s+[a-z][a-z0-9_]*_at\s+([a-z]+)`)
	timestampColumns := timestampColumnPattern.FindAllStringSubmatch(up, -1)
	if len(timestampColumns) < 10 {
		t.Fatalf("timestamp column count = %d, want at least 10", len(timestampColumns))
	}
	for _, column := range timestampColumns {
		if column[1] != "timestamptz" {
			t.Errorf("timestamp column uses %q, want timestamptz", column[1])
		}
	}
	if strings.Contains(strings.ToLower(up), "timestamp without time zone") {
		t.Error("schema uses timestamp without timezone")
	}
	if strings.Contains(strings.ToLower(up), "branch") {
		t.Fatal("MVP schema introduced Branch")
	}
}

func TestCoreSchemaContainsOwnershipStateAndQueryContracts(t *testing.T) {
	t.Parallel()
	up := readMigration(t, upMigration)
	required := []string{
		"FOREIGN KEY (tenant_id, session_id, turn_id)",
		"FOREIGN KEY (tenant_id, session_id, turn_id, run_id)",
		"FOREIGN KEY (tenant_id, session_id, id, user_message_id)",
		"FOREIGN KEY (tenant_id, session_id, id, selected_run_id)",
		"DEFERRABLE INITIALLY DEFERRED",
		"turns_sequence_unique UNIQUE (tenant_id, session_id, turn_sequence)",
		"runs_number_unique UNIQUE (tenant_id, session_id, turn_id, run_no)",
		"steps_number_unique UNIQUE (tenant_id, run_id, step_no)",
		"attempts_number_unique UNIQUE (tenant_id, step_id, attempt_no)",
		"messages_one_user_per_turn_idx",
		"messages_conversation_history_idx",
		"runs_active_session_idx",
		"attempts_unknown_idx",
		"'CANCEL_REQUESTED'", "'SUPERSEDED'", "'TIMED_OUT'", "'UNKNOWN'",
	}
	for _, fragment := range required {
		if !strings.Contains(up, fragment) {
			t.Errorf("up migration missing %q", fragment)
		}
	}
	indexPattern := regexp.MustCompile(`(?s)CREATE (?:UNIQUE )?INDEX [a-z_]+\s+ON [a-z_]+ \((.*?)\)`)
	for _, match := range indexPattern.FindAllStringSubmatch(up, -1) {
		if !strings.HasPrefix(strings.TrimSpace(match[1]), "tenant_id") {
			t.Errorf("index does not lead with tenant_id: (%s)", match[1])
		}
	}
}

func TestDownMigrationDropsReverseDependencies(t *testing.T) {
	t.Parallel()
	down := readMigration(t, downMigration)
	positions := make([]int, 0, len(coreTables))
	for _, table := range []string{"attempts", "steps", "messages", "runs", "turns", "sessions"} {
		position := strings.Index(down, "DROP TABLE "+table+";")
		if position < 0 {
			t.Fatalf("down migration does not drop %s", table)
		}
		positions = append(positions, position)
	}
	for index := 1; index < len(positions); index++ {
		if positions[index] <= positions[index-1] {
			t.Fatal("tables are not dropped in reverse dependency order")
		}
	}
	if strings.Contains(down, "schema_migrations") {
		t.Fatal("feature down migration must not drop shared migration ledger")
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	data, err := Files.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}
