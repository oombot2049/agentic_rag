package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestTypedIdentifierParsers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		parse func(string) error
		valid string
	}{
		{"tenant", func(v string) error { _, err := ParseTenantID(v); return err }, "ten_0001"},
		{"session", func(v string) error { _, err := ParseSessionID(v); return err }, "ses_0001"},
		{"turn", func(v string) error { _, err := ParseTurnID(v); return err }, "trn_0001"},
		{"message", func(v string) error { _, err := ParseMessageID(v); return err }, "msg_0001"},
		{"run", func(v string) error { _, err := ParseRunID(v); return err }, "run_0001"},
		{"step", func(v string) error { _, err := ParseStepID(v); return err }, "stp_0001"},
		{"attempt", func(v string) error { _, err := ParseAttemptID(v); return err }, "att_0001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.parse(test.valid); err != nil {
				t.Fatalf("valid ID rejected: %v", err)
			}
			for _, invalidID := range []string{"", "bad_0001", test.valid + "/x", strings.Repeat("a", 129)} {
				if err := test.parse(invalidID); !errors.Is(err, ErrValidation) {
					t.Errorf("parse(%q) error = %v, want validation error", invalidID, err)
				}
			}
		})
	}
}

func TestIdentifierTypesAreNotInterchangeable(t *testing.T) {
	t.Parallel()
	if _, err := NewSession(SessionID("ten_0001"), TenantID("ten_0001"), testTime); !errors.Is(err, ErrValidation) {
		t.Fatalf("wrong typed prefix error = %v, want validation error", err)
	}
}
