package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunProducesPassingMachineReadableReport(t *testing.T) {
	root := filepath.Join("..", "..", "testdata")
	arguments := []string{"-golden", filepath.Join(root, "golden", "v1", "dataset.json"),
		"-predictions", filepath.Join(root, "predictions", "reference-v1.json"),
		"-baseline", filepath.Join(root, "baselines", "v1.json")}
	var stdout, stderr bytes.Buffer
	if code := run(arguments, &stdout, &stderr); code != 0 || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), `"passed": true`) || !strings.Contains(stdout.String(), `"dataset_sha256"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsMissingArgumentsWithoutReport(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "required") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
