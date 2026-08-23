package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/contextmgr"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestContextSnapshotPostgresExcludesFailedAndCandidateHistory(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, err := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)
	err = pool.WithinTx(ctx, nil, func(ctx context.Context, tx postgres.DBTX) error {
		statements := []string{
			`INSERT INTO sessions(tenant_id,id,context_version,row_version,next_turn_sequence,committed_turn_sequence,active_run_id,head_turn_id,created_at,updated_at)
VALUES('ten_0001','ses_0001',1,4,4,1,'run_0004','trn_0001',$1,$1)`,
			`INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,selected_run_id,created_at,updated_at) VALUES
('ten_0001','trn_0001','ses_0001',1,0,'msg_0001','COMPLETED','run_0001',$1,$1),
('ten_0001','trn_0002','ses_0001',2,1,'msg_0004','FAILED',NULL,$1,$1),
('ten_0001','trn_0003','ses_0001',3,1,'msg_0005','RUNNING',NULL,$1,$1)`,
			`INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,output_message_id,created_at,updated_at) VALUES
('ten_0001','run_0001','ses_0001','trn_0001',1,0,'SELECTED','msg_0002',$1,$1),
('ten_0001','run_0002','ses_0001','trn_0001',2,1,'SUPERSEDED','msg_0003',$1,$1),
('ten_0001','run_0003','ses_0001','trn_0002',1,1,'FAILED',NULL,$1,$1),
('ten_0001','run_0004','ses_0001','trn_0003',1,1,'QUEUED',NULL,$1,$1)`,
			`INSERT INTO messages(tenant_id,id,session_id,turn_id,run_id,role,status,visibility,content,created_at) VALUES
('ten_0001','msg_0001','ses_0001','trn_0001',NULL,'USER','COMPLETED','CONVERSATION','completed question',$1),
('ten_0001','msg_0002','ses_0001','trn_0001','run_0001','ASSISTANT','COMPLETED','CONVERSATION','selected answer',$1),
('ten_0001','msg_0003','ses_0001','trn_0001','run_0002','ASSISTANT','COMPLETED','CANDIDATE','superseded answer',$1),
('ten_0001','msg_0004','ses_0001','trn_0002',NULL,'USER','COMPLETED','CONVERSATION','failed question',$1),
('ten_0001','msg_0005','ses_0001','trn_0003',NULL,'USER','COMPLETED','CONVERSATION','current question',$1)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement, created); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := NewContextSnapshotStore(func(ctx context.Context, options *sql.TxOptions, operation func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return operation(ctx, tx) })
	})
	builder, _ := contextmgr.New(store)
	snapshot, err := builder.Build(ctx, contextmgr.BuildRequest{Query: contextmgr.Query{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0003", RunID: "run_0004", BaseContextVersion: 1}, SystemInstructions: []string{"policy"}})
	if err != nil {
		t.Fatal(err)
	}
	entries := snapshot.Entries()
	if len(entries) != 4 || entries[1].Content != "completed question" || entries[2].Content != "selected answer" || entries[3].Content != "current question" {
		t.Fatalf("snapshot entries=%+v", entries)
	}
}
