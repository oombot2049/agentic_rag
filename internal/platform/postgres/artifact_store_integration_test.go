package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/artifact"
	artifactS3 "github.com/agenticrag/agenticrag/internal/artifact/s3"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestArtifactStorePostgresAndMinIOContract(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	migrator, err := NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "minio/minio:RELEASE.2025-09-07T16-13-09Z",
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"MINIO_ROOT_USER": "artifact-access", "MINIO_ROOT_PASSWORD": "artifact-secret-123",
			},
			Cmd:        []string{"server", "/data", "--address", ":9000"},
			WaitingFor: wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, container) })
	endpoint, err := container.Endpoint(ctx, "http")
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifactS3.New(artifactS3.Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "agenticrag-artifacts",
		AccessKey: "artifact-access", SecretKey: "artifact-secret-123"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	repository, err := artifact.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, err := artifact.NewService(repository, store)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("artifact-stream-block"), 1<<17)
	metadata, err := service.Upload(ctx, artifact.Upload{TenantID: "ten_0001", ID: "art_0001", Size: int64(len(payload)),
		MediaType: "application/octet-stream", EncryptionMode: artifact.EncryptionClient, EncryptionKey: "client-key-1", TTL: 24 * time.Hour,
		Body: bytes.NewReader(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SHA256 != sha256.Sum256(payload) || metadata.Size != int64(len(payload)) {
		t.Fatalf("metadata = %+v", metadata)
	}
	loaded, reader, err := service.Download(ctx, "ten_0001", "art_0001")
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(downloaded, payload) || loaded.SHA256 != metadata.SHA256 {
		t.Fatalf("download bytes=%d read=%v close=%v", len(downloaded), err, closeErr)
	}
	if _, _, err := service.Download(ctx, "ten_9999", "art_0001"); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("cross-tenant error = %v", err)
	}
	if _, err := service.Upload(ctx, artifact.Upload{TenantID: "ten_0001", ID: "art_0001", Size: 3,
		MediaType: "text/plain", EncryptionMode: artifact.EncryptionClient, EncryptionKey: "client-key-1", TTL: time.Hour, Body: bytes.NewReader([]byte("new"))}); !errors.Is(err, artifact.ErrAlreadyExists) {
		t.Fatalf("duplicate upload error = %v", err)
	}
	var payloadColumns int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name='artifact_metadata'
		AND column_name IN ('payload','content','body','data')`).Scan(&payloadColumns); err != nil {
		t.Fatal(err)
	}
	if payloadColumns != 0 {
		t.Fatalf("Artifact payload columns = %d", payloadColumns)
	}
}
