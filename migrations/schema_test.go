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
	want := []string{downMigration, upMigration}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("embedded SQL files = %v, want %v", names, want)
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
