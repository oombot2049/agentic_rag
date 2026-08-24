package repository

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/turn"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestFinalCommitConcurrentReplayAndStaleWorkerFencing(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	seedFinalCommit(t, ctx, pool, at)
	store, _ := NewFinalCommitStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	service, _ := turn.NewFinalCommitService(store)
	request := turn.FinalRequest{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", MessageID: "msg_0002", Content: "atomic answer",
		ExpectedContextVersion: 0, ExpectedSessionRowVersion: 2, Authority: turn.FinalAuthority{Owner: "worker-new", Token: 2, ExecutionVersion: 3}, At: at.Add(time.Minute), EventRetention: 24 * time.Hour}
	results := make(chan turn.FinalResult, 2)
	failures := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			value, err := service.Commit(ctx, request)
			if err != nil {
				failures <- err
			} else {
				results <- value
			}
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Errorf("concurrent final commit: %v", err)
	}
	replayed := 0
	count := 0
	for result := range results {
		count++
		if result.Replayed {
			replayed++
		}
		if result.MessageID != "msg_0002" || result.ContextVersion != 1 || result.SessionRowVersion != 3 || result.EventSequence != 1 {
			t.Errorf("result=%+v", result)
		}
	}
	if count != 2 || replayed != 1 {
		t.Fatalf("count=%d replayed=%d", count, replayed)
	}
	var sessionContext, rowVersion uint64
	var active sql.NullString
	var turnStatus, selected, runStatus, output, visibility, content string
	var eventCount, outboxCount int
	if err := pool.QueryRowContext(ctx, `SELECT context_version,row_version,active_run_id FROM sessions WHERE tenant_id='ten_0001' AND id='ses_0001'`).Scan(&sessionContext, &rowVersion, &active); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRowContext(ctx, `SELECT status,selected_run_id FROM turns WHERE tenant_id='ten_0001' AND id='trn_0001'`).Scan(&turnStatus, &selected)
	_ = pool.QueryRowContext(ctx, `SELECT status,output_message_id FROM runs WHERE tenant_id='ten_0001' AND id='run_0001'`).Scan(&runStatus, &output)
	_ = pool.QueryRowContext(ctx, `SELECT visibility,content FROM messages WHERE tenant_id='ten_0001' AND id='msg_0002'`).Scan(&visibility, &content)
	_ = pool.QueryRowContext(ctx, `SELECT count(*) FROM run_events WHERE tenant_id='ten_0001' AND run_id='run_0001' AND event_type='run.completed'`).Scan(&eventCount)
	_ = pool.QueryRowContext(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id='ten_0001' AND aggregate_id='run_0001' AND event_type='RunEvent.v1'`).Scan(&outboxCount)
	if sessionContext != 1 || rowVersion != 3 || active.Valid || turnStatus != "COMPLETED" || selected != "run_0001" || runStatus != "SELECTED" || output != "msg_0002" || visibility != "CONVERSATION" || content != "atomic answer" || eventCount != 1 || outboxCount != 1 {
		t.Fatalf("durable state ctx=%d row=%d active=%v turn=%s selected=%s run=%s output=%s message=%s/%s events=%d outbox=%d", sessionContext, rowVersion, active, turnStatus, selected, runStatus, output, visibility, content, eventCount, outboxCount)
	}

	stale := request
	stale.MessageID = "msg_0003"
	stale.Content = "stale overwrite"
	stale.Authority = turn.FinalAuthority{Owner: "worker-old", Token: 1, ExecutionVersion: 2}
	if _, err := service.Commit(ctx, stale); !errors.Is(err, turn.ErrFinalConflict) {
		t.Fatalf("stale worker error=%v", err)
	}
}

func TestFinalCommitFailureRollsBackEveryParticipant(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	seedFinalCommit(t, ctx, pool, at)
	sentinel := errors.New("injected before commit")
	store, _ := NewFinalCommitStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error {
			if err := work(ctx, tx); err != nil {
				return err
			}
			return sentinel
		})
	})
	request := turn.FinalRequest{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", MessageID: "msg_0002", Content: "must roll back", ExpectedSessionRowVersion: 2, Authority: turn.FinalAuthority{Owner: "worker-new", Token: 2, ExecutionVersion: 3}, At: at.Add(time.Minute), EventRetention: 24 * time.Hour}
	if _, err := store.CommitFinal(ctx, request); !errors.Is(err, sentinel) {
		t.Fatalf("error=%v", err)
	}
	var messages, events, outboxEvents int
	var contextVersion uint64
	var status string
	_ = pool.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE id='msg_0002'`).Scan(&messages)
	_ = pool.QueryRowContext(ctx, `SELECT count(*) FROM run_events WHERE run_id='run_0001'`).Scan(&events)
	_ = pool.QueryRowContext(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id='run_0001'`).Scan(&outboxEvents)
	_ = pool.QueryRowContext(ctx, `SELECT context_version FROM sessions WHERE id='ses_0001'`).Scan(&contextVersion)
	_ = pool.QueryRowContext(ctx, `SELECT status FROM runs WHERE id='run_0001'`).Scan(&status)
	if messages != 0 || events != 0 || outboxEvents != 0 || contextVersion != 0 || status != "RUNNING" {
		t.Fatalf("partial commit messages=%d events=%d outbox=%d context=%d status=%s", messages, events, outboxEvents, contextVersion, status)
	}
}

func seedFinalCommit(t *testing.T, ctx context.Context, pool *postgres.Pool, at time.Time) {
	t.Helper()
	err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx postgres.DBTX) error {
		statements := []string{
			`INSERT INTO sessions(tenant_id,id,context_version,row_version,next_turn_sequence,committed_turn_sequence,active_run_id,created_at,updated_at) VALUES('ten_0001','ses_0001',0,2,2,0,'run_0001',$1,$1)`,
			`INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at) VALUES('ten_0001','trn_0001','ses_0001',1,0,'msg_0001','RUNNING',$1,$1)`,
			`INSERT INTO messages(tenant_id,id,session_id,turn_id,role,visibility,content,created_at) VALUES('ten_0001','msg_0001','ses_0001','trn_0001','USER','CONVERSATION','question',$1)`,
			`INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at,lease_owner,lease_token,lease_expires_at,execution_version) VALUES('ten_0001','run_0001','ses_0001','trn_0001',1,0,'RUNNING',$1,$1,'worker-new',2,$2,3)`,
		}
		for index, statement := range statements {
			var err error
			if index == 3 {
				_, err = tx.ExecContext(ctx, statement, at, at.Add(time.Hour))
			} else {
				_, err = tx.ExecContext(ctx, statement, at)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
