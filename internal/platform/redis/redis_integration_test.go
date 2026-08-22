package redis

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestValkeyClientIntegration(t *testing.T) {
	if os.Getenv("AGENTICRAG_REQUIRE_DOCKER") != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := testcontainers.Run(ctx, "valkey/valkey:9.1.0-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, container)
	endpoint, err := container.Endpoint(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadSources(config.MapSource{
		"AGENTICRAG_POSTGRES_DSN":         "postgres://unused",
		"AGENTICRAG_REDIS_ADDRESS":        endpoint,
		"AGENTICRAG_REDIS_POOL_SIZE":      "8",
		"AGENTICRAG_REDIS_MIN_IDLE_CONNS": "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := Open(ctx, cfg.Redis)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
	builder, _ := NewKeyBuilder("test")
	key, _ := builder.Build("ten_1", NamespaceCache, "session_1")
	value := []byte("cached-value")
	if err := client.Set(ctx, key, value, 150*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	got, err := client.Get(ctx, key)
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("Get() = %q, %v", got, err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := client.Get(ctx, key); err != ErrMiss {
		t.Fatalf("expired Get error = %v", err)
	}
	if err := client.Set(ctx, key, value, time.Minute); err != nil {
		t.Fatal(err)
	}
	if count, err := client.Delete(ctx, key); err != nil || count != 1 {
		t.Fatalf("Delete = %d, %v", count, err)
	}
}
