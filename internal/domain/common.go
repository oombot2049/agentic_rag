package domain

import "time"

const maxSignedInt64 = uint64(1<<63 - 1)

func normalizeTime(field string, value time.Time) (time.Time, error) {
	if value.IsZero() {
		return time.Time{}, invalid(field, "must not be zero")
	}
	return value.UTC(), nil
}

func copySlice[T any](values []T) []T {
	return append([]T(nil), values...)
}

func validateNonNegativeInt64(field string, value uint64) error {
	if value > maxSignedInt64 {
		return invalid(field, "must remain within signed 64-bit range")
	}
	return nil
}

func validatePositiveInt64(field string, value uint64) error {
	if value == 0 {
		return invalid(field, "must be positive")
	}
	return validateNonNegativeInt64(field, value)
}
