package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/agenticrag/agenticrag/internal/contextmgr"
)

func TestContextSnapshotStoreReadsOnlyCompletedSelectedHistory(t *testing.T) {
	script := &conversationScript{queries: []conversationQuery{
		{rows: [][]driver.Value{{"ten_0001", "ses_0001", int64(2), "run_0003", "trn_0003", int64(3), "run_0003", int64(2), "msg_0005", "trn_0003", "USER", "current"}}},
		{rows: [][]driver.Value{
			{"msg_0001", "trn_0001", "", "USER", "q1", int64(1)},
			{"msg_0002", "trn_0001", "run_0001", "ASSISTANT", "a1", int64(1)},
		}},
	}}
	db := openConversationDB(t, script)
	store, err := NewContextSnapshotStore(func(ctx context.Context, options *sql.TxOptions, operation func(context.Context, DBTX) error) error {
		if options == nil || options.Isolation != sql.LevelRepeatableRead || !options.ReadOnly {
			t.Fatal("context query did not request repeatable-read read-only transaction")
		}
		return operation(ctx, db)
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Load(context.Background(), contextmgr.Query{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0003", RunID: "run_0003", BaseContextVersion: 2})
	if err != nil || value.ContextVersion != 2 || value.ActiveRunID != "run_0003" || len(value.History) != 2 || value.Current.Content != "current" {
		t.Fatalf("Load=%+v error=%v", value, err)
	}
	script.mu.Lock()
	defer script.mu.Unlock()
	if len(script.seen) != 2 || !strings.Contains(script.seen[0].query, "t.status='RUNNING'") ||
		!strings.Contains(script.seen[1].query, "t.status='COMPLETED'") ||
		!strings.Contains(script.seen[1].query, "m.id=selected.output_message_id") ||
		!strings.Contains(script.seen[1].query, "m.visibility='CONVERSATION'") {
		t.Fatalf("queries=%+v", script.seen)
	}
}

func TestContextSnapshotStoreNotFoundAndCorruptIdentity(t *testing.T) {
	query := contextmgr.Query{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0003", RunID: "run_0003", BaseContextVersion: 2}
	for _, test := range []struct {
		name string
		rows [][]driver.Value
		want error
	}{
		{"not found", nil, contextmgr.ErrNotFound},
		{"wrong base", [][]driver.Value{{"ten_0001", "ses_0001", int64(2), "run_0003", "trn_0003", int64(3), "run_0003", int64(1), "msg_0005", "trn_0003", "USER", "current"}}, contextmgr.ErrCorruptSource},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := &conversationScript{queries: []conversationQuery{{rows: test.rows}}}
			db := openConversationDB(t, script)
			store, _ := NewContextSnapshotStore(func(ctx context.Context, _ *sql.TxOptions, operation func(context.Context, DBTX) error) error {
				return operation(ctx, db)
			})
			_, err := store.Load(context.Background(), query)
			if !errors.Is(err, test.want) {
				t.Fatalf("Load error=%v want=%v", err, test.want)
			}
		})
	}
}
