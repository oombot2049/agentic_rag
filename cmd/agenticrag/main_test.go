package main

import (
	"bytes"
	"strings"
	"testing"
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
