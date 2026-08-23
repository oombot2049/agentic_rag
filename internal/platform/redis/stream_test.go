package redis

import "testing"

func TestValidStreamIDRequiresCanonicalPositiveSequence(t *testing.T) {
	valid := []string{"0-0", "1-0", "9223372036854775807-0"}
	invalid := []string{"", "0-1", "00-0", "+1-0", "-1-0", "1", "1-0-0", "9223372036854775808-0"}
	for _, value := range valid {
		if !validStreamID(value) {
			t.Errorf("validStreamID(%q) = false", value)
		}
	}
	for _, value := range invalid {
		if validStreamID(value) {
			t.Errorf("validStreamID(%q) = true", value)
		}
	}
}
