//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/artifact"
	artifacts3 "github.com/agenticrag/agenticrag/internal/artifact/s3"
	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/domain"
	platformpostgres "github.com/agenticrag/agenticrag/internal/platform/postgres"
	platformredis "github.com/agenticrag/agenticrag/internal/platform/redis"
	"github.com/agenticrag/agenticrag/internal/queue"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	postgresImage = "postgres:18.6-alpine"
	valkeyImage   = "valkey/valkey:9.1.0-alpine"
	minioImage    = "minio/minio:RELEASE.2025-09-07T16-13-09Z"
	qdrantImage   = "qdrant/qdrant:v1.15.5"
)

var integrationTime = time.Date(2026, 8, 23, 16, 0, 0, 0, time.UTC)

func TestPostgreSQLAndQueueTenantIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, postgresImage, tcpostgres.WithDatabase("agenticrag"),
		tcpostgres.WithUsername("agenticrag"), tcpostgres.WithPassword("integration-secret"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	registerContainer(t, container)
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadSources(config.MapSource{"AGENTICRAG_POSTGRES_DSN": dsn})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := platformpostgres.Open(ctx, cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	migrator, err := platformpostgres.NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	seedQueueTenants(t, ctx, pool)
	adapter, err := queue.NewPostgresAdapter(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, queue.DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	}, queue.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for index, tenant := range []string{"ten_0001", "ten_0002"} {
		suffix := fmt.Sprintf("%04d", index+1)
		err := adapter.Enqueue(ctx, queue.Message{DeliveryID: "evt_" + suffix, TenantID: domainTenant(tenant),
			SessionID: domainSession("ses_" + suffix), TurnID: domainTurn("trn_" + suffix), RunID: domainRun("run_" + suffix),
			Payload: []byte(`{"type":"RunQueued.v1"}`), CreatedAt: integrationTime, AvailableAt: integrationTime})
		if err != nil {
			t.Fatal(err)
		}
	}
	deliveries, err := adapter.Claim(ctx, queue.ClaimCommand{TenantID: "ten_0001", Owner: "integration-worker", Limit: 10,
		Duration: time.Minute, At: integrationTime})
	if err != nil || len(deliveries) != 1 || deliveries[0].TenantID != "ten_0001" || deliveries[0].RunID != "run_0001" {
		t.Fatalf("tenant claim = %+v, %v", deliveries, err)
	}
	if err := adapter.Ack(ctx, queue.LeaseCommand{TenantID: "ten_0001", RunID: deliveries[0].RunID,
		Owner: deliveries[0].LeaseOwner, Token: deliveries[0].LeaseToken, At: integrationTime.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	var tenantTwoReady int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM run_queue_messages WHERE tenant_id='ten_0002' AND status='READY'`).Scan(&tenantTwoReady); err != nil || tenantTwoReady != 1 {
		t.Fatalf("tenant two ready=%d error=%v", tenantTwoReady, err)
	}
}

func TestValkeyTenantIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := testcontainers.Run(ctx, valkeyImage, testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")))
	if err != nil {
		t.Fatal(err)
	}
	registerContainer(t, container)
	endpoint, err := container.Endpoint(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadSources(config.MapSource{"AGENTICRAG_POSTGRES_DSN": "postgres://unused", "AGENTICRAG_REDIS_ADDRESS": endpoint})
	if err != nil {
		t.Fatal(err)
	}
	client, err := platformredis.Open(ctx, cfg.Redis)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	builder, err := platformredis.NewKeyBuilder("integration")
	if err != nil {
		t.Fatal(err)
	}
	one, _ := builder.Build("ten_0001", platformredis.NamespaceCache, "session", "ses_0001")
	two, _ := builder.Build("ten_0002", platformredis.NamespaceCache, "session", "ses_0001")
	if err := client.Set(ctx, one, []byte("tenant-one"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, two, []byte("tenant-two"), time.Minute); err != nil {
		t.Fatal(err)
	}
	first, firstErr := client.Get(ctx, one)
	second, secondErr := client.Get(ctx, two)
	if firstErr != nil || secondErr != nil || string(first) != "tenant-one" || string(second) != "tenant-two" || one.String() == two.String() {
		t.Fatalf("Valkey isolation first=%q/%v second=%q/%v", first, firstErr, second, secondErr)
	}
}

func TestMinIOS3AdapterTenantIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := testcontainers.Run(ctx, minioImage, testcontainers.WithExposedPorts("9000/tcp"),
		testcontainers.WithEnv(map[string]string{"MINIO_ROOT_USER": "integration-access", "MINIO_ROOT_PASSWORD": "integration-secret-key"}),
		testcontainers.WithCmd("server", "/data", "--address", ":9000"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/minio/health/live").WithPort("9000/tcp").WithStartupTimeout(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	registerContainer(t, container)
	endpoint, err := container.Endpoint(ctx, "http")
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifacts3.New(artifacts3.Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "agenticrag-artifacts",
		AccessKey: "integration-access", SecretKey: "integration-secret-key", Client: &http.Client{Timeout: 10 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	keys := []string{"tenants/ten_0001/artifacts/art_0001", "tenants/ten_0002/artifacts/art_0002"}
	for index, key := range keys {
		body := fmt.Sprintf("tenant-%d", index+1)
		if _, err := store.Put(ctx, key, strings.NewReader(body), int64(len(body)), artifact.PutOptions{
			MediaType: "text/plain", EncryptionMode: artifact.EncryptionSSES3, CreateOnly: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, keys[0]); err != artifact.ErrNotFound {
		t.Fatalf("deleted tenant object error=%v", err)
	}
	reader, err := store.Open(ctx, keys[1])
	if err != nil {
		t.Fatal(err)
	}
	remaining, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil || string(remaining) != "tenant-2" {
		t.Fatalf("remaining object=%q error=%v", remaining, readErr)
	}
}

func TestQdrantVectorTenantFilterContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := testcontainers.Run(ctx, qdrantImage, testcontainers.WithExposedPorts("6333/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/readyz").WithPort("6333/tcp").WithStartupTimeout(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	registerContainer(t, container)
	endpoint, err := container.Endpoint(ctx, "http")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	doJSON(t, ctx, client, http.MethodPut, endpoint+"/collections/agenticrag_integration", map[string]any{
		"vectors": map[string]any{"size": 3, "distance": "Cosine"},
	}, nil)
	doJSON(t, ctx, client, http.MethodPut, endpoint+"/collections/agenticrag_integration/points?wait=true", map[string]any{
		"points": []any{
			map[string]any{"id": 1, "vector": []float64{1, 0, 0}, "payload": map[string]any{"tenant_id": "ten_0001", "chunk_id": "chk_0001"}},
			map[string]any{"id": 2, "vector": []float64{1, 0, 0}, "payload": map[string]any{"tenant_id": "ten_0002", "chunk_id": "chk_0002"}},
		},
	}, nil)
	var result struct {
		Result struct {
			Points []struct {
				Payload map[string]any `json:"payload"`
			} `json:"points"`
		} `json:"result"`
	}
	doJSON(t, ctx, client, http.MethodPost, endpoint+"/collections/agenticrag_integration/points/query", map[string]any{
		"query": []float64{1, 0, 0}, "filter": map[string]any{"must": []any{
			map[string]any{"key": "tenant_id", "match": map[string]any{"value": "ten_0001"}},
		}}, "limit": 10, "with_payload": true,
	}, &result)
	if len(result.Result.Points) != 1 || result.Result.Points[0].Payload["tenant_id"] != "ten_0001" ||
		result.Result.Points[0].Payload["chunk_id"] != "chk_0001" {
		t.Fatalf("tenant-filtered vector result = %+v", result.Result.Points)
	}
}

func seedQueueTenants(t *testing.T, ctx context.Context, pool *platformpostgres.Pool) {
	t.Helper()
	err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx platformpostgres.DBTX) error {
		for index, tenant := range []string{"ten_0001", "ten_0002"} {
			suffix := fmt.Sprintf("%04d", index+1)
			sessionID, turnID, messageID, runID := "ses_"+suffix, "trn_"+suffix, "msg_"+suffix, "run_"+suffix
			if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES($1,$2,$3,$3)`, tenant, sessionID, integrationTime); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at) VALUES($1,$2,$3,1,0,$4,'RUNNING',$5,$5)`, tenant, turnID, sessionID, messageID, integrationTime); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO messages(tenant_id,id,session_id,turn_id,role,visibility,content,created_at) VALUES($1,$2,$3,$4,'USER','CONVERSATION','question',$5)`, tenant, messageID, sessionID, turnID, integrationTime); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at) VALUES($1,$2,$3,$4,1,0,'QUEUED',$5,$5)`, tenant, runID, sessionID, turnID, integrationTime); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func registerContainer(t *testing.T, container testcontainers.Container) {
	t.Helper()
	t.Cleanup(func() { testcontainers.CleanupContainer(t, container) })
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		logs, err := container.Logs(ctx)
		if err != nil {
			t.Logf("collect container logs: %v", err)
			return
		}
		defer logs.Close()
		content, err := io.ReadAll(io.LimitReader(logs, 256<<10))
		if err != nil {
			t.Logf("read container logs: %v", err)
			return
		}
		t.Logf("container logs (bounded):\n%s", content)
	})
}

func doJSON(t *testing.T, ctx context.Context, client *http.Client, method, url string, input any, output any) {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("%s %s status=%d body=%s", method, url, response.StatusCode, body)
	}
	if output != nil {
		if err := json.Unmarshal(body, output); err != nil {
			t.Fatalf("decode %s: %v body=%s", url, err, body)
		}
	}
}

// Typed aliases keep integration setup readable without weakening production parsing.
func domainTenant(value string) domain.TenantID   { return domain.TenantID(value) }
func domainSession(value string) domain.SessionID { return domain.SessionID(value) }
func domainTurn(value string) domain.TurnID       { return domain.TurnID(value) }
func domainRun(value string) domain.RunID         { return domain.RunID(value) }
