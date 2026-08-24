package domain

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDomainHasNoPersistenceOrTransportDependencies(t *testing.T) {
	t.Parallel()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	directory := filepath.Dir(currentFile)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read domain directory: %v", err)
	}
	for _, entry := range entries {
		filename := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		fullPath := filepath.Join(directory, filename)
		file, err := parser.ParseFile(token.NewFileSet(), fullPath, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", filename, err)
			}
			if path == "database/sql" || strings.Contains(path, "pgx") || strings.Contains(path, "redis") ||
				strings.Contains(path, "/internal/platform/") || strings.Contains(path, "/internal/api/") {
				t.Errorf("domain imports forbidden dependency %q in %s", path, filename)
			}
		}
	}
}
