package domain

import "fmt"

const (
	minResourceIDLength = 8
	maxResourceIDLength = 128
)

type (
	TenantID  string
	SessionID string
	TurnID    string
	MessageID string
	RunID     string
	StepID    string
	AttemptID string
)

func (id TenantID) String() string  { return string(id) }
func (id SessionID) String() string { return string(id) }
func (id TurnID) String() string    { return string(id) }
func (id MessageID) String() string { return string(id) }
func (id RunID) String() string     { return string(id) }
func (id StepID) String() string    { return string(id) }
func (id AttemptID) String() string { return string(id) }

func ParseTenantID(value string) (TenantID, error) {
	return parseID("tenant_id", "ten_", value, func(v string) TenantID { return TenantID(v) })
}

func ParseSessionID(value string) (SessionID, error) {
	return parseID("session_id", "ses_", value, func(v string) SessionID { return SessionID(v) })
}

func ParseTurnID(value string) (TurnID, error) {
	return parseID("turn_id", "trn_", value, func(v string) TurnID { return TurnID(v) })
}

func ParseMessageID(value string) (MessageID, error) {
	return parseID("message_id", "msg_", value, func(v string) MessageID { return MessageID(v) })
}

func ParseRunID(value string) (RunID, error) {
	return parseID("run_id", "run_", value, func(v string) RunID { return RunID(v) })
}

func ParseStepID(value string) (StepID, error) {
	return parseID("step_id", "stp_", value, func(v string) StepID { return StepID(v) })
}

func ParseAttemptID(value string) (AttemptID, error) {
	return parseID("attempt_id", "att_", value, func(v string) AttemptID { return AttemptID(v) })
}

func parseID[T ~string](field, prefix, value string, convert func(string) T) (T, error) {
	var zero T
	if len(value) < minResourceIDLength || len(value) > maxResourceIDLength {
		return zero, invalid(field, fmt.Sprintf("length must be between %d and %d", minResourceIDLength, maxResourceIDLength))
	}
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return zero, invalid(field, "has an invalid resource prefix")
	}
	for _, character := range value {
		if !isResourceIDCharacter(character) {
			return zero, invalid(field, "contains a character outside [A-Za-z0-9_-]")
		}
	}
	return convert(value), nil
}

func isResourceIDCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		character == '_' || character == '-'
}

func validateTenantID(id TenantID) error   { return validateTypedID("tenant_id", "ten_", id.String()) }
func validateSessionID(id SessionID) error { return validateTypedID("session_id", "ses_", id.String()) }
func validateTurnID(id TurnID) error       { return validateTypedID("turn_id", "trn_", id.String()) }
func validateMessageID(id MessageID) error { return validateTypedID("message_id", "msg_", id.String()) }
func validateRunID(id RunID) error         { return validateTypedID("run_id", "run_", id.String()) }
func validateStepID(id StepID) error       { return validateTypedID("step_id", "stp_", id.String()) }
func validateAttemptID(id AttemptID) error { return validateTypedID("attempt_id", "att_", id.String()) }

func validateTypedID(field, prefix, value string) error {
	_, err := parseID(field, prefix, value, func(v string) string { return v })
	return err
}
