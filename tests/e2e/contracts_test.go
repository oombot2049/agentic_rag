package e2e_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestE2EGateRemainsExplicitAndPinned(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate E2E contract test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	mainTest, err := os.ReadFile(filepath.Join(root, "tests", "e2e", "main_path_e2e_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	sourceText := string(mainTest)
	for _, required := range []string{"//go:build e2e", `postgres:18.6-alpine`, "Last-Event-ID", "knowledge.search"} {
		if !strings.Contains(sourceText, required) {
			t.Errorf("E2E source is missing contract %q", required)
		}
	}
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(makefile), "test -tags=e2e -count=1 -v ./tests/e2e") {
		t.Error("Makefile does not expose the deterministic E2E command")
	}
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "go test -tags=e2e -count=1 -v ./tests/e2e") {
		t.Error("CI does not require the E2E gate")
	}
}
