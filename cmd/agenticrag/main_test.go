package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	processrole "github.com/agenticrag/agenticrag/internal/process"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		arguments  []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no arguments prints help", wantCode: 0, wantStdout: "Usage: agenticrag"},
		{name: "help", arguments: []string{"help"}, wantCode: 0, wantStdout: "Commands:"},
		{name: "short help", arguments: []string{"-h"}, wantCode: 0, wantStdout: "Usage: agenticrag"},
		{name: "long help", arguments: []string{"--help"}, wantCode: 0, wantStdout: "Usage: agenticrag"},
		{name: "version", arguments: []string{"version"}, wantCode: 0, wantStdout: "agenticrag "},
		{name: "unknown", arguments: []string{"nope"}, wantCode: 2, wantStderr: `unknown command "nope"`},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			if got := run(test.arguments, &stdout, &stderr); got != test.wantCode {
				t.Fatalf("run() code = %d, want %d", got, test.wantCode)
			}
			if !strings.Contains(stdout.String(), test.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), test.wantStdout)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

func TestRunDispatchesProcessRoles(t *testing.T) {
	previous := runRole
	t.Cleanup(func() { runRole = previous })
	var got processrole.Role
	runRole = func(role processrole.Role) error { got = role; return nil }
	for _, role := range []string{"serve", "worker", "relay", "ingest-worker"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{role}, &stdout, &stderr); code != 0 || got != processrole.Role(role) {
			t.Fatalf("role=%s code=%d got=%s stderr=%s", role, code, got, stderr.String())
		}
	}
	runRole = func(processrole.Role) error { return errors.New("startup failed") }
	var stdout, stderr bytes.Buffer
	if code := run([]string{"serve"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "startup failed") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestRunKeepsHelpOffStderr(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(help) code = %d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
