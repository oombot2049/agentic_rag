package postgres

import (
	"context"
	"testing"
	"time"

	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestCoreSchemaMigrationRoundTripAndConstraints(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	migrator, err := NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("initial Up: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("idempotent Up: %v", err)
	}

	assertCoreSchemaShape(t, ctx, pool)
	insertCompleteAggregate(t, ctx, pool, "ten_0001")
	insertInitialAggregate(t, ctx, pool, "ten_0002")

	var tenantRows int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE id='ses_0001'`).Scan(&tenantRows); err != nil {
		t.Fatal(err)
	}
	if tenantRows != 2 {
		t.Fatalf("same ID tenant rows = %d, want 2", tenantRows)
	}

	if _, err := pool.ExecContext(ctx, `
		INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at)
		VALUES('ten_9999','trn_9999','ses_0001',1,0,'msg_9999','RUNNING',now(),now())`); err == nil {
		t.Fatal("cross-tenant parent reference was accepted")
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO attempts(tenant_id,id,step_id,attempt_no,status,started_at,ended_at)
		VALUES('ten_0001','att_0002','stp_0001',1,'FAILED',now(),now())`); err == nil {
		t.Fatal("duplicate attempt number was accepted")
	}

	var state string
	if err := pool.QueryRowContext(ctx, `SELECT status FROM runs WHERE tenant_id='ten_0001' AND id='run_0001'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "SELECTED" {
		t.Fatalf("committed run status = %s", state)
	}

	if err := migrator.Down(ctx, 5); err != nil {
		t.Fatalf("Down: %v", err)
	}
	for _, table := range []string{"sessions", "turns", "messages", "runs", "steps", "attempts", "outbox_events", "artifact_metadata"} {
		var exists bool
		if err := pool.QueryRowContext(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("table %s remains after Down", table)
		}
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	assertCoreSchemaShape(t, ctx, pool)
}

func assertCoreSchemaShape(t *testing.T, ctx context.Context, pool *Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema='public' AND table_name IN ('sessions','turns','messages','runs','steps','attempts')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("core table count = %d", count)
	}
	if err := pool.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name IN ('sessions','turns','messages','runs','steps','attempts')
		AND column_name='tenant_id' AND is_nullable='NO'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("mandatory tenant column count = %d", count)
	}
	if err := pool.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name IN ('sessions','turns','messages','runs','steps','attempts')
		AND column_name LIKE '%\_at' ESCAPE '\' AND data_type <> 'timestamp with time zone'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("non-timestamptz instant columns = %d", count)
	}
	if err := pool.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE conname IN ('messages_run_fk','turns_user_message_fk','turns_selected_run_fk',
		'runs_output_message_fk','sessions_active_run_fk','sessions_head_turn_fk') AND condeferrable AND condeferred`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("deferred aggregate FK count = %d", count)
	}
	var exists bool
	if err := pool.QueryRowContext(ctx, `SELECT to_regclass('public.tool_approvals') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("tool_approvals remains in the current schema")
	}
	if err := pool.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name='tool_audits' AND column_name='approval_id'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("tool_audits.approval_id remains in the current schema")
	}
	if err := pool.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE conname IN ('tool_calls_effect_valid','tool_audits_effect')
		AND pg_get_constraintdef(oid) LIKE '%effect = ''READ_ONLY''%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("read-only tool effect constraints = %d, want 2", count)
	}
}

func insertInitialAggregate(t *testing.T, ctx context.Context, pool *Pool, tenantID string) {
	t.Helper()
	now := time.Now().UTC()
	err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES($1,'ses_0001',$2,$2)`, tenantID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at)
			VALUES($1,'trn_0001','ses_0001',1,0,'msg_0001','RUNNING',$2,$2)`, tenantID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages(tenant_id,id,session_id,turn_id,role,visibility,content,created_at)
			VALUES($1,'msg_0001','ses_0001','trn_0001','USER','CONVERSATION','question',$2)`, tenantID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at)
			VALUES($1,'run_0001','ses_0001','trn_0001',1,0,'QUEUED',$2,$2)`, tenantID, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET active_run_id='run_0001',row_version=2,updated_at=$2 WHERE tenant_id=$1 AND id='ses_0001'`, tenantID, now)
		return err
	})
	if err != nil {
		t.Fatalf("insert initial aggregate for %s: %v", tenantID, err)
	}
}

func insertCompleteAggregate(t *testing.T, ctx context.Context, pool *Pool, tenantID string) {
	t.Helper()
	insertInitialAggregate(t, ctx, pool, tenantID)
	now := time.Now().UTC()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO steps(tenant_id,id,run_id,step_no,kind,status,created_at,updated_at)
		VALUES($1,'stp_0001','run_0001',1,'GENERATION','SUCCEEDED',$2,$2)`, tenantID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO attempts(tenant_id,id,step_id,attempt_no,status,started_at,ended_at)
		VALUES($1,'att_0001','stp_0001',1,'SUCCEEDED',$2,$2)`, tenantID, now); err != nil {
		t.Fatal(err)
	}
	err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages(tenant_id,id,session_id,turn_id,run_id,role,visibility,content,created_at)
			VALUES($1,'msg_0002','ses_0001','trn_0001','run_0001','ASSISTANT','CANDIDATE','answer',$2)`, tenantID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='SELECTED',output_message_id='msg_0002',updated_at=$2 WHERE tenant_id=$1 AND id='run_0001'`, tenantID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET status='COMPLETED',selected_run_id='run_0001',updated_at=$2 WHERE tenant_id=$1 AND id='trn_0001'`, tenantID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET visibility='CONVERSATION' WHERE tenant_id=$1 AND id='msg_0002'`, tenantID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET context_version=1,row_version=3,committed_turn_sequence=1,
			head_turn_id='trn_0001',active_run_id=NULL,updated_at=$2 WHERE tenant_id=$1 AND id='ses_0001'`, tenantID, now)
		return err
	})
	if err != nil {
		t.Fatalf("final commit aggregate for %s: %v", tenantID, err)
	}
}
