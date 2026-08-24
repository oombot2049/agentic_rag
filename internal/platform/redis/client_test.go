package redis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
)

func TestClientRejectsInvalidTTLAndEmptyKeysWithoutNetwork(t *testing.T) {
	client := &Client{maxTTL: time.Hour}
	key := Key{value: "ar:test:{ten_1}:cache:item"}
	for _, ttl := range []time.Duration{0, -time.Second, time.Hour + time.Nanosecond} {
		if err := client.Set(context.Background(), key, []byte("x"), ttl); !errors.Is(err, ErrInvalidTTL) {
			t.Fatalf("Set ttl %s error = %v", ttl, err)
		}
	}
	if err := client.Set(context.Background(), Key{}, nil, time.Minute); err == nil {
		t.Fatal("empty Set key accepted")
	}
	if _, err := client.Get(context.Background(), Key{}); err == nil {
		t.Fatal("empty Get key accepted")
	}
	if _, err := client.Delete(context.Background(), key, Key{}); err == nil {
		t.Fatal("empty Delete key accepted")
	}
	if count, err := client.Delete(context.Background()); err != nil || count != 0 {
		t.Fatalf("Delete() = %d, %v", count, err)
	}
}

func TestOpenErrorDoesNotLeakPassword(t *testing.T) {
	marker := "redis-super-secret"
	_, err := Open(context.Background(), config.RedisConfig{
		Address: "127.0.0.1:1", Password: config.NewSecret(marker),
		DialTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond, PoolTimeout: 100 * time.Millisecond,
		HealthTimeout: 100 * time.Millisecond, PoolSize: 1, MaxTTL: time.Hour,
	})
	if err == nil {
		t.Fatal("Open() error = nil")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("password leaked: %q", err)
	}
}
