package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/retrieval"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestRetrievalAuditRepositoryRoundTripTenantIsolationAndSnapshotFailure(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
	seedFinalCommit(t, ctx, pool, at)
	hash := sha256.Sum256([]byte("snapshot"))
	if _, err := pool.ExecContext(ctx, `INSERT INTO artifact_metadata(tenant_id,id,object_key,content_sha256,size_bytes,media_type,encryption_mode,created_at,expires_at) VALUES('ten_0001','art_0001','tenants/ten_0001/artifacts/art_0001',$1,8,'text/plain','SSE-S3',$2,$3)`, hash[:], at, at.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	store, _ := NewRetrievalAuditStore(pool, func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	score := 0.8
	record := retrieval.AuditRecord{TenantID: "ten_0001", ID: "ret_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", KnowledgeBaseID: "kbs_0001", IndexVersionID: "ver_0001", OriginalQuery: "original", RetrievalQuery: "rewritten", DegradedRoute: "BM25", CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour), Candidates: []retrieval.AuditCandidate{{Ordinal: 1, DocumentID: "doc_0001", ChunkID: "chk_0001", VectorRank: 1, FusedScore: .9, RerankScore: &score, Selected: true, ArtifactID: "art_0001", ContentHash: hash}, {Ordinal: 2, DocumentID: "doc_0002", ChunkID: "chk_0002", BM25Rank: 1, FusedScore: .7}}}
	if err := store.SaveAudit(ctx, record); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetAudit(ctx, "ten_0001", "ret_0001")
	if err != nil || len(loaded.Candidates) != 2 || loaded.Candidates[0].ArtifactID != "art_0001" || loaded.Candidates[0].ContentHash != hash || *loaded.Candidates[0].RerankScore != score {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if _, err = store.GetAudit(ctx, "ten_9999", "ret_0001"); !errors.Is(err, retrieval.ErrAuditNotFound) {
		t.Fatalf("foreign read=%v", err)
	}
	bad := record
	bad.ID = "ret_0002"
	bad.Candidates = retrievalClone(record.Candidates)
	bad.Candidates[0].ContentHash = sha256.Sum256([]byte("wrong"))
	if err = store.SaveAudit(ctx, bad); !errors.Is(err, retrieval.ErrAuditSnapshot) {
		t.Fatalf("bad snapshot=%v", err)
	}
	var count int
	if err = pool.QueryRowContext(ctx, `SELECT count(*) FROM retrieval_audits WHERE tenant_id='ten_0001' AND id='ret_0002'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial audit count=%d err=%v", count, err)
	}
}
func retrievalClone(values []retrieval.AuditCandidate) []retrieval.AuditCandidate {
	return append([]retrieval.AuditCandidate(nil), values...)
}
