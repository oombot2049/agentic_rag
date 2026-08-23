package agent

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/repository"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestPostgresStoreAtomicStepBoundaries(t *testing.T) {
	pool := agentTestPool(t)
	ctx := context.Background()
	migrator, err := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	seedAgentRun(t, pool)
	execution, _ := repository.NewExecution(pool)
	acquired, err := execution.AcquireLease(ctx, repository.AcquireLeaseCommand{TenantID: "ten_0001", RunID: "run_0001",
		Owner: "worker-integration", ExpectedExecutionVersion: 1, At: agentTestTime, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	authority := repository.WorkerAuthority{TenantID: "ten_0001", RunID: "run_0001", Owner: acquired.Lease.Owner,
		Token: acquired.Lease.Token, ExpectedExecutionVersion: acquired.ExecutionVersion, At: agentTestTime.Add(time.Second)}
	store, _ := NewPostgresStore(pool)
	step, _ := domain.NewStep("stp_0001", "ten_0001", "run_0001", 1, domain.StepKindTool, authority.At)
	attempt, _ := domain.NewAttempt("att_0001", "ten_0001", "stp_0001", 1, authority.At)

	err = store.BeginStep(ctx, authority, BeginStepCommand{Step: step, Attempt: attempt, Payload: []byte(`{`)})
	if !errors.Is(err, repository.ErrInvalidArgument) {
		t.Fatalf("invalid checkpoint error=%v", err)
	}
	assertAgentRowCount(t, pool, "steps", 0)
	assertAgentRowCount(t, pool, "attempts", 0)

	executing := executingCheckpoint(t, true, 1)
	payload, _ := executing.Encode(12)
	if err := store.BeginStep(ctx, authority, BeginStepCommand{Step: step, Attempt: attempt, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	assertAgentRowCount(t, pool, "steps", 1)
	assertAgentRowCount(t, pool, "attempts", 1)

	ready := Checkpoint{SchemaVersion: 1, Phase: PhaseReady, NextStepNo: 2, State: []byte(`{"answer":"ok"}`)}
	readyPayload, _ := ready.Encode(12)
	if err := store.CompleteStep(ctx, authority, CompleteStepCommand{Active: *executing.Active,
		At: authority.At.Add(time.Second), Payload: readyPayload}); err != nil {
		t.Fatal(err)
	}
	var stepStatus, attemptStatus string
	if err := pool.QueryRowContext(ctx, `SELECT s.status,a.status FROM steps s JOIN attempts a
ON a.tenant_id=s.tenant_id AND a.step_id=s.id WHERE s.tenant_id='ten_0001' AND s.id='stp_0001'`).Scan(&stepStatus, &attemptStatus); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := execution.LoadCheckpoint(ctx, "ten_0001", "run_0001")
	decoded, decodeErr := DecodeCheckpoint(checkpoint.Payload, 12)
	if err != nil || decodeErr != nil || stepStatus != "SUCCEEDED" || attemptStatus != "SUCCEEDED" || checkpoint.StepNo != 1 ||
		decoded.Phase != PhaseReady || decoded.NextStepNo != 2 {
		t.Fatalf("step=%s attempt=%s checkpoint=%+v error=%v", stepStatus, attemptStatus, checkpoint, err)
	}
}

func seedAgentRun(t *testing.T, pool *postgres.Pool) {
	t.Helper()
	ctx := context.Background()
	err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx postgres.DBTX) error {
		statements := []string{
			`INSERT INTO sessions(tenant_id,id,context_version,row_version,next_turn_sequence,committed_turn_sequence,created_at,updated_at)
VALUES('ten_0001','ses_0001',0,1,2,0,$1,$1)`,
			`INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at)
VALUES('ten_0001','trn_0001','ses_0001',1,0,'msg_0001','RUNNING',$1,$1)`,
			`INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at)
VALUES('ten_0001','run_0001','ses_0001','trn_0001',1,0,'QUEUED',$1,$1)`,
			`INSERT INTO messages(tenant_id,id,session_id,turn_id,role,status,visibility,content,created_at)
VALUES('ten_0001','msg_0001','ses_0001','trn_0001','USER','COMPLETED','CONVERSATION','question',$1)`,
			`UPDATE sessions SET active_run_id='run_0001',head_turn_id='trn_0001' WHERE tenant_id='ten_0001' AND id='ses_0001'`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement, agentTestTime); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertAgentRowCount(t *testing.T, pool *postgres.Pool, table string, want int) {
	t.Helper()
	query := "SELECT count(*) FROM " + table // table is a test-owned constant.
	var got int
	if err := pool.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count=%d want=%d", table, got, want)
	}
}

func agentTestPool(t *testing.T) *postgres.Pool {
	t.Helper()
	if os.Getenv("AGENTICRAG_REQUIRE_DOCKER") != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, "postgres:18.6-alpine",
		tcpostgres.WithDatabase("agenticrag"), tcpostgres.WithUsername("agenticrag"),
		tcpostgres.WithPassword("integration-secret"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, container) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.LoadSources(config.MapSource{
		"AGENTICRAG_POSTGRES_DSN": dsn, "AGENTICRAG_POSTGRES_MAX_OPEN_CONNS": "10", "AGENTICRAG_POSTGRES_MAX_IDLE_CONNS": "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Open(ctx, configuration.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}
