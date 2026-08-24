package config

import (
	"fmt"
	"io"
)

const redacted = "[REDACTED]"

// Secret is a configuration value that is redacted by common formatting and
// serialization paths. Reveal must only be called at the adapter that consumes
// the credential.
type Secret struct {
	value string
}

// NewSecret wraps a sensitive value.
func NewSecret(value string) Secret {
	return Secret{value: value}
}

// Reveal returns the sensitive value explicitly.
func (s Secret) Reveal() string {
	return s.value
}

// IsZero reports whether the underlying secret is empty.
func (s Secret) IsZero() bool {
	return s.value == ""
}

func (Secret) String() string   { return redacted }
func (Secret) GoString() string { return redacted }

// Format redacts every fmt formatting verb, including width and Go syntax.
func (Secret) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, redacted)
}

// MarshalText prevents text serializers from exposing the value.
func (Secret) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}

// MarshalJSON prevents JSON serializers from exposing the value.
func (Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}
