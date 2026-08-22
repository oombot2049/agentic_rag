package postgres

import (
	"crypto/sha256"
	"testing"
	"testing/fstest"
)

func TestLoadMigrations(t *testing.T) {
	files := fstest.MapFS{
		"migrations/000002_add_runs.up.sql":       {Data: []byte("CREATE TABLE runs(id bigint);")},
		"migrations/000001_add_sessions.up.sql":   {Data: []byte("CREATE TABLE sessions(id bigint);")},
		"migrations/000001_add_sessions.down.sql": {Data: []byte("DROP TABLE sessions;")},
		"migrations/000002_add_runs.down.sql":     {Data: []byte("DROP TABLE runs;")},
	}
	got, err := loadMigrations(files, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Version != 1 || got[1].Version != 2 || got[0].Name != "add_sessions" {
		t.Fatalf("migrations = %+v", got)
	}
	if got[0].Checksum != sha256.Sum256([]byte(got[0].UpSQL)) {
		t.Fatal("checksum mismatch")
	}
}

func TestLoadMigrationsRejectsInvalidSets(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
	}{
		{"bad filename", fstest.MapFS{"migrations/readme.txt": {Data: []byte("x")}}},
		{"missing down", fstest.MapFS{"migrations/000001_one.up.sql": {Data: []byte("x")}}},
		{"missing up", fstest.MapFS{"migrations/000001_one.down.sql": {Data: []byte("x")}}},
		{"gap", fstest.MapFS{
			"migrations/000001_one.up.sql": {Data: []byte("x")}, "migrations/000001_one.down.sql": {Data: []byte("x")},
			"migrations/000003_three.up.sql": {Data: []byte("x")}, "migrations/000003_three.down.sql": {Data: []byte("x")},
		}},
		{"conflicting names", fstest.MapFS{"migrations/000001_one.up.sql": {Data: []byte("x")}, "migrations/000001_two.down.sql": {Data: []byte("x")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadMigrations(tt.files, "migrations"); err == nil {
				t.Fatal("error = nil")
			}
		})
	}
}

func TestNewMigratorRejectsInvalidInputs(t *testing.T) {
	if _, err := NewMigrator(nil, fstest.MapFS{}, "."); err == nil {
		t.Fatal("nil pool error = nil")
	}
	if _, err := NewMigrator(&Pool{}, nil, "."); err == nil {
		t.Fatal("nil FS error = nil")
	}
}
