package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/approval"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/tools"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestApprovalRepositoryReleasesLeaseAndRequeuesExactlyOnce(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	seedFinalCommit(t, ctx, pool, at)
	if _, err := pool.ExecContext(ctx, `INSERT INTO steps(tenant_id,id,run_id,step_no,kind,status,created_at,updated_at,row_version) VALUES('ten_0001','stp_0001','run_0001',1,'TOOL','RUNNING',$1,$1,1)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO attempts(tenant_id,id,step_id,attempt_no,status,started_at,row_version) VALUES('ten_0001','att_0001','stp_0001',1,'STARTED',$1,1)`, at); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(`{"target":"record"}`))
	key := sha256.Sum256([]byte("invocation"))
	if _, err := pool.ExecContext(ctx, `INSERT INTO tool_calls(tenant_id,id,run_id,step_id,tool_name,tool_version,effect,scope_level,scope_session_id,scope_run_id,arguments_sha256,invocation_key,status,created_at,updated_at) VALUES('ten_0001','tcl_0001','run_0001','stp_0001','record.delete','1.0.0','DESTRUCTIVE','RUN','ses_0001','run_0001',$1,$2,'PREPARED',$3,$3)`, hash[:], "tik_"+base64.RawURLEncoding.EncodeToString(key[:]), at); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO tool_call_attempts(tenant_id,call_id,attempt_id,attempt_no,status,started_at) VALUES('ten_0001','tcl_0001','att_0001',1,'STARTED',$1)`, at); err != nil {
		t.Fatal(err)
	}
	store, _ := NewApprovalStore(pool, func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	nonces, _ := approval.NewNonceDeriver([]byte("01234567890123456789012345678901"))
	service, _ := approval.NewService(store, nonces)
	created, err := service.Request(ctx, approval.Request{TenantID: "ten_0001", ID: "apr_0001", RequesterSubject: "requester",
		ApproverSubject: "approver", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", StepID: "stp_0001",
		AttemptID: "att_0001", ToolCallID: "tcl_0001", ToolName: "record.delete", ToolVersion: "1.0.0",
		Effect: tools.EffectDestructive, ArgumentsHash: hash, At: at, TTL: time.Hour, WorkerOwner: "worker-new", LeaseToken: 2, ExecutionVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	var status string
	var owner sql.NullString
	var version uint64
	_ = pool.QueryRowContext(ctx, `SELECT status,lease_owner,execution_version FROM runs WHERE tenant_id='ten_0001' AND id='run_0001'`).Scan(&status, &owner, &version)
	if status != "WAITING_APPROVAL" || owner.Valid || version != 4 {
		t.Fatalf("waiting run=%s owner=%v version=%d", status, owner, version)
	}
	decision := approval.Decision{TenantID: "ten_0001", ApprovalID: "apr_0001", ActorSubject: "approver", Nonce: created.Nonce,
		ExpectedRowVersion: 1, Approve: true, At: at.Add(time.Minute)}
	if _, err = service.Decide(ctx, decision); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Decide(ctx, decision); err != nil {
		t.Fatalf("replay=%v", err)
	}
	var events int
	_ = pool.QueryRowContext(ctx, `SELECT status,execution_version FROM runs WHERE tenant_id='ten_0001' AND id='run_0001'`).Scan(&status, &version)
	_ = pool.QueryRowContext(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id='ten_0001' AND event_type='RunApprovalGranted.v1'`).Scan(&events)
	if status != "QUEUED" || version != 5 || events != 1 {
		t.Fatalf("resumed status=%s version=%d events=%d", status, version, events)
	}
}
