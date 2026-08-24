package concurrency_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConcurrencyGateIsRaceEnabledAndMandatory(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate concurrency contract test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	suite, err := os.ReadFile(filepath.Join(root, "tests", "concurrency", "session_lease_concurrency_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"//go:build concurrency", "postgres:18.6-alpine", "SESSION_BUSY", "ErrFinalFenced"} {
		if !strings.Contains(string(suite), required) {
			t.Errorf("concurrency suite is missing %q", required)
		}
	}
	command := "go test -race -tags=concurrency -count=1 -v ./tests/concurrency"
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), command) {
		t.Error("Linux CI does not require the tagged race suite")
	}
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(makefile), "test -race -tags=concurrency -count=1 -v ./tests/concurrency") {
		t.Error("Makefile does not expose the concurrency gate")
	}
}
