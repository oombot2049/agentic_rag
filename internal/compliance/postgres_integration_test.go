package compliance

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestPostgresDeletionSagaPreservesEvidenceAfterCascade(t *testing.T) {
	pool := complianceTestPool(t)
	ctx := context.Background()
	migrator, err := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	seedComplianceSession(t, pool)
	store, err := NewPostgresStore(pool, pool.WithinTx)
	if err != nil {
		t.Fatal(err)
	}
	targets := []Target{
		{Kind: TargetObject, Locator: "tenants/ten_0001/artifacts/art_0001"},
		{Kind: TargetRedis, Locator: "ar:prod:{ten_0001}:cache:session:ses_0001"},
		{Kind: TargetIndex, Locator: "sessions/ten_0001/ses_0001"},
	}
	for _, target := range targets {
		if err := store.RegisterTarget(ctx, "ten_0001", "ses_0001", target, complianceTime); err != nil {
			t.Fatal(err)
		}
	}
	cleaners := newCleanerSet(targets)
	service := newTestService(t, store, cleaners)
	job, err := service.Request(ctx, deletionRequest("ten_0001", "ses_0001", "del_0001"))
	if err != nil || len(job.Targets) != len(targets) {
		t.Fatalf("Request() = %+v, %v", job, err)
	}
	if err := service.Process(ctx, "ten_0001", job.ID, "worker-integration", complianceTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "turns", "runs", "messages", "compliance_external_targets", "artifact_metadata"} {
		assertComplianceCount(t, pool, table, 0)
	}
	assertComplianceCount(t, pool, "session_deletion_jobs", 1)
	assertComplianceCount(t, pool, "compliance_deletion_evidence", 4)
	page, err := service.ListEvidence(ctx, "ten_0001", nil, 10)
	if err != nil || len(page.Items) != 4 || page.Items[3].Type != EvidenceCompleted {
		t.Fatalf("ListEvidence() = %+v, %v", page, err)
	}
	if err := service.Process(ctx, "ten_0001", job.ID, "worker-replay", complianceTime.Add(2*time.Second)); err != nil {
		t.Fatalf("completed replay: %v", err)
	}
	assertComplianceCount(t, pool, "compliance_deletion_evidence", 4)
	removed, err := service.PurgeEvidence(ctx, "ten_0001", complianceTime.Add(31*24*time.Hour), 2)
	if err != nil || removed != 2 {
		t.Fatalf("first PurgeEvidence() = %d, %v", removed, err)
	}
	removed, err = service.PurgeEvidence(ctx, "ten_0001", complianceTime.Add(31*24*time.Hour), 10)
	if err != nil || removed != 2 {
		t.Fatalf("second PurgeEvidence() = %d, %v", removed, err)
	}
	assertComplianceCount(t, pool, "compliance_deletion_evidence", 0)
}

func seedComplianceSession(t *testing.T, pool *postgres.Pool) {
	t.Helper()
	statements := []string{
		`INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES('ten_0001','ses_0001',$1,$1)`,
		`INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at)
VALUES('ten_0001','trn_0001','ses_0001',1,0,'msg_0001','RUNNING',$1,$1)`,
		`INSERT INTO messages(tenant_id,id,session_id,turn_id,role,visibility,content,created_at)
VALUES('ten_0001','msg_0001','ses_0001','trn_0001','USER','CONVERSATION','question',$1)`,
		`INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at)
VALUES('ten_0001','run_0001','ses_0001','trn_0001',1,0,'QUEUED',$1,$1)`,
		`INSERT INTO artifact_metadata(tenant_id,id,object_key,content_sha256,size_bytes,media_type,encryption_mode,created_at,expires_at)
VALUES('ten_0001','art_0001','tenants/ten_0001/artifacts/art_0001',decode(repeat('00',32),'hex'),1,'text/plain','SSE-S3',$1,$1+interval '1 day')`,
	}
	err := pool.WithinTx(context.Background(), nil, func(ctx context.Context, tx postgres.DBTX) error {
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement, complianceTime); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertComplianceCount(t *testing.T, pool *postgres.Pool, table string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count=%d want=%d", table, got, want)
	}
}

func complianceTestPool(t *testing.T) *postgres.Pool {
	t.Helper()
	if os.Getenv("AGENTICRAG_REQUIRE_DOCKER") != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, "postgres:18.6-alpine", tcpostgres.WithDatabase("agenticrag"),
		tcpostgres.WithUsername("agenticrag"), tcpostgres.WithPassword("integration-secret"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, container) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadSources(config.MapSource{"AGENTICRAG_POSTGRES_DSN": dsn})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Open(ctx, cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}
