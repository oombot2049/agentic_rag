package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationImagesArePinnedAndApproved(t *testing.T) {
	content, err := os.ReadFile("suite_integration_test.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, image := range []string{
		"postgres:18.6-alpine", "valkey/valkey:9.1.0-alpine",
		"minio/minio:RELEASE.2025-09-07T16-13-09Z", "qdrant/qdrant:v1.15.5",
	} {
		if !strings.Contains(text, `"`+image+`"`) {
			t.Errorf("missing pinned image %s", image)
		}
	}
	for _, forbidden := range []string{":latest", "postgres:latest", "valkey:latest", "minio:latest", "qdrant:latest"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("floating image reference %q", forbidden)
		}
	}
	for _, diagnostic := range []string{"t.Failed()", "container.Logs(ctx)", "256<<10", "CleanupContainer"} {
		if !strings.Contains(text, diagnostic) {
			t.Errorf("failure diagnostics missing %q", diagnostic)
		}
	}
}

func TestIntegrationCommandIsAvailableLocallyAndMandatoryInCI(t *testing.T) {
	contracts := map[string]string{
		"../../Makefile":                 "$(GO) test -tags=integration -count=1 -v ./tests/integration",
		"../../.github/workflows/ci.yml": "go test -tags=integration -count=1 -v ./tests/integration",
	}
	for path, command := range contracts {
		content, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), command) {
			t.Errorf("%s does not run %q", path, command)
		}
	}
}
