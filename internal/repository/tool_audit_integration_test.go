package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/audit"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/tools"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestToolAuditRepositoryPersistsQueriesAndVerifiesArtifact(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	seedFinalCommit(t, ctx, pool, started)
	argumentsHash := sha256.Sum256([]byte(`{"query":"hello"}`))
	result := []byte(`{"answer":42}`)
	resultHash := sha256.Sum256(result)
	invocation := sha256.Sum256([]byte("audit invocation"))
	if _, err := pool.ExecContext(ctx, `INSERT INTO steps(tenant_id,id,run_id,step_no,kind,status,created_at,updated_at,row_version) VALUES('ten_0001','stp_0001','run_0001',1,'TOOL','SUCCEEDED',$1,$2,1)`, started, started.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO attempts(tenant_id,id,step_id,attempt_no,status,started_at,ended_at,row_version) VALUES('ten_0001','att_0001','stp_0001',1,'SUCCEEDED',$1,$2,1)`, started, started.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO tool_calls(tenant_id,id,run_id,step_id,tool_name,tool_version,effect,scope_level,scope_session_id,scope_run_id,arguments_sha256,invocation_key,status,result,created_at,updated_at) VALUES('ten_0001','tcl_0001','run_0001','stp_0001','knowledge.search','1.0.0','READ_ONLY','RUN','ses_0001','run_0001',$1,$2,'SUCCEEDED',$3,$4,$5)`, argumentsHash[:], "tik_"+base64.RawURLEncoding.EncodeToString(invocation[:]), result, started, started.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO tool_call_attempts(tenant_id,call_id,attempt_id,attempt_no,status,started_at,ended_at) VALUES('ten_0001','tcl_0001','att_0001',1,'SUCCEEDED',$1,$2)`, started, started.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO artifact_metadata(tenant_id,id,object_key,content_sha256,size_bytes,media_type,encryption_mode,created_at,expires_at) VALUES('ten_0001','art_0001','tenants/ten_0001/artifacts/art_0001',$1,$2,'application/json','SSE-S3',$3,$4)`, resultHash[:], len(result), started, started.Add(90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	store, err := NewToolAuditStore(pool, func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	if err != nil {
		t.Fatal(err)
	}
	record := audit.Record{TenantID: "ten_0001", ID: "aud_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", StepID: "stp_0001",
		ToolCallID: "tcl_0001", CallerSubject: "usr:alice", ToolName: "knowledge.search", ToolVersion: "1.0.0", Effect: tools.EffectReadOnly,
		PolicyDecision: audit.DecisionAllow, ArgumentsSHA256: argumentsHash, Outcome: audit.OutcomeSucceeded, ResultSHA256: resultHash,
		ResultSizeBytes: int64(len(result)), BusinessImpactCode: "knowledge.read", Artifact: &audit.ArtifactReference{ID: "art_0001", SHA256: resultHash, SizeBytes: int64(len(result)), MediaType: "application/json"},
		Attempts:  []audit.Attempt{{AttemptID: "att_0001", AttemptNo: 1, Status: domain.AttemptStatusSucceeded, StartedAt: started, EndedAt: started.Add(time.Second)}},
		StartedAt: started, EndedAt: started.Add(time.Second), CreatedAt: started.Add(2 * time.Second), ExpiresAt: started.Add(30 * 24 * time.Hour)}
	if err = store.Append(ctx, record); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetByToolCall(ctx, "ten_0001", "tcl_0001")
	if err != nil || got.CallerSubject != "usr:alice" || len(got.Attempts) != 1 || got.Artifact == nil || got.Artifact.SHA256 != resultHash {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	page, err := store.ListByRun(ctx, "ten_0001", "run_0001", nil, 10)
	if err != nil || len(page.Items) != 1 || len(page.Items[0].Attempts) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err = store.GetByToolCall(ctx, "ten_9999", "tcl_0001"); !errors.Is(err, audit.ErrNotFound) {
		t.Fatalf("foreign tenant=%v", err)
	}

	bad := record
	bad.ID = "aud_0002"
	badHash := sha256.Sum256([]byte("tampered"))
	bad.Artifact = &audit.ArtifactReference{ID: "art_0001", SHA256: badHash, SizeBytes: int64(len(result)), MediaType: "application/json"}
	bad.ResultSHA256 = badHash
	if err = store.Append(ctx, bad); !errors.Is(err, audit.ErrArtifact) {
		t.Fatalf("mismatched Artifact=%v", err)
	}
	var count int
	if err = pool.QueryRowContext(ctx, `SELECT count(*) FROM tool_audits WHERE tenant_id='ten_0001'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
