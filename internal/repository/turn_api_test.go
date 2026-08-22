package repository

import (
	"errors"
	"testing"
)

func TestNewTurnAPIStoreRequiresTransactionRunner(t *testing.T) {
	t.Parallel()
	if _, err := NewTurnAPIStore(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("NewTurnAPIStore(nil) = %v", err)
	}
}
