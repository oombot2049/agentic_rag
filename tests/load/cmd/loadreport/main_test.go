package main

import (
	"path/filepath"
	"testing"
)

func TestRunProducesContractReport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	err := run([]string{"-profile", filepath.Join("..", "..", "profiles", "smoke-v1.json"), "-measurement", filepath.Join("..", "..", "testdata", "contract-pass.json"), "-output", out})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunRequiresInputs(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("expected required-input error")
	}
}
