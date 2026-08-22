package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestTurnQueryStoreRequiresAndUsesReadSnapshot(t *testing.T) {
	t.Parallel()
	if _, err := NewTurnQueryStore(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("NewTurnQueryStore(nil) = %v", err)
	}
	marker := errors.New("snapshot marker")
	store, err := NewTurnQueryStore(func(_ context.Context, options *sql.TxOptions, _ func(context.Context, DBTX) error) error {
		if options == nil || !options.ReadOnly || options.Isolation != sql.LevelRepeatableRead {
			t.Fatalf("transaction options = %+v", options)
		}
		return marker
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTurn(context.Background(), "ten_0001", "trn_0001"); !errors.Is(err, marker) {
		t.Fatalf("GetTurn = %v", err)
	}
	if _, err := store.GetRun(context.Background(), "ten_0001", "run_0001"); !errors.Is(err, marker) {
		t.Fatalf("GetRun = %v", err)
	}
	if _, err := store.ListMessages(context.Background(), "ten_0001", "ses_0001", nil, 50); !errors.Is(err, marker) {
		t.Fatalf("ListMessages = %v", err)
	}
}
