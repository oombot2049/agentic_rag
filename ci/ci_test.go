package ci

import (
	"os"
	"strings"
	"testing"
)

func TestWorkflowContainsMandatoryGates(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	required := []string{
		"go mod verify", "./ci/check-format.sh", "go vet ./...", "staticcheck ./...",
		"go test -count=1 -shuffle=on ./...", "go test -race -count=1 ./...",
		"govulncheck ./...", "docker info", "AGENTICRAG_REQUIRE_DOCKER: '1'",
		"./ci/verify-gates.sh", "timeout-minutes:", "cache: false",
		"actionlint",
		"path: ${{ steps.gomodcache.outputs.path }}",
	}
	for _, value := range required {
		if !strings.Contains(workflow, value) {
			t.Errorf("workflow missing %q", value)
		}
	}
	for _, forbidden := range []string{"continue-on-error", "@master", "@latest", "~/.cache/go-build"} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("workflow contains forbidden %q", forbidden)
		}
	}
}

func TestWorkflowUsesExactToolAndActionVersions(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, version := range []string{
		"actions/checkout@v6.0.2", "actions/setup-go@v7.0.0", "actions/cache@v5.0.5",
		"staticcheck@v0.7.0", "govulncheck@v1.7.0",
		"actionlint@v1.7.12",
	} {
		if !strings.Contains(workflow, version) {
			t.Errorf("workflow missing pinned version %q", version)
		}
	}
	if !strings.Contains(workflow, "static-analysis:") || !strings.Contains(workflow, "go-version: '1.26.x'") {
		t.Error("stable Staticcheck job is not pinned to Go 1.26")
	}
}
