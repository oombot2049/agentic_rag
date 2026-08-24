package evals

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOfflineEvaluationIsARequiredCIGate(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate eval contract")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), ".."))
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"eval:", "run ./evals/cmd/rageval", "test -count=1 ./evals/..."} {
		if !strings.Contains(string(makefile), required) {
			t.Errorf("Makefile eval target is missing %q", required)
		}
	}
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "make eval") {
		t.Error("quality workflow does not run the offline RAG gate")
	}
}
