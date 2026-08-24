package redis

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/quota"
	goredis "github.com/redis/go-redis/v9"
)

func TestQuotaStoreKeysShareClusterSlot(t *testing.T) {
	keys, err := NewKeyBuilder("test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewQuotaStore(&Client{client: goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})}, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.client.Close() })
	values, err := store.scopeKeys("ten_0001", "qrs_valid")
	if err != nil || len(values) != 3 {
		t.Fatalf("scopeKeys() = %v, %v", values, err)
	}
	for _, value := range values {
		if !strings.Contains(value, "{ten_0001}") {
			t.Fatalf("key %q does not share tenant hash tag", value)
		}
	}
	if _, err := store.scopeKeys("bad:scope", "qrs_valid"); err == nil {
		t.Fatal("scopeKeys() accepted unsafe scope")
	}
	if _, err := NewQuotaStore(nil, keys); !errors.Is(err, quota.ErrInvalid) {
		t.Fatalf("NewQuotaStore(nil) error = %v", err)
	}
}

func TestParseQuotaResults(t *testing.T) {
	tests := []struct {
		name    string
		result  []any
		allowed bool
		reason  quota.Reason
		retry   time.Duration
		wantErr error
	}{
		{name: "allowed", result: []any{int64(1), int64(0)}, allowed: true},
		{name: "qps", result: []any{int64(0), int64(1), int64(25)}, reason: quota.ReasonQPS, retry: 25 * time.Millisecond},
		{name: "active", result: []any{int64(0), int64(2), int64(1000)}, reason: quota.ReasonActiveRuns, retry: time.Second},
		{name: "tokens", result: []any{int64(0), int64(3), int64(0)}, reason: quota.ReasonTokens},
		{name: "cost strings", result: []any{"0", "4", "0"}, reason: quota.ReasonCost},
		{name: "conflict", result: []any{int64(-1), int64(0)}, wantErr: quota.ErrConflict},
		{name: "invalid", result: []any{int64(-2), int64(0)}, wantErr: quota.ErrInvalid},
		{name: "unknown reason", result: []any{int64(0), int64(9), int64(0)}, wantErr: errors.New("error")},
		{name: "incomplete", result: nil, wantErr: errors.New("error")},
		{name: "malformed", result: []any{true}, wantErr: errors.New("error")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := parseReserveDecision(test.result)
			if test.wantErr != nil {
				if err == nil {
					t.Fatalf("parseReserveDecision() = %+v, nil", decision)
				}
				if test.wantErr == quota.ErrConflict || test.wantErr == quota.ErrInvalid {
					if !errors.Is(err, test.wantErr) {
						t.Fatalf("error = %v, want %v", err, test.wantErr)
					}
				}
				return
			}
			if err != nil || decision.Allowed != test.allowed || decision.Reason != test.reason || decision.RetryAfter != test.retry {
				t.Fatalf("parseReserveDecision() = %+v, %v", decision, err)
			}
		})
	}

	for code, want := range map[int64]error{
		1: nil, -1: quota.ErrConflict, -2: quota.ErrNotFound, -3: quota.ErrInvalid,
	} {
		err := parseMutationResult([]any{code})
		if !errors.Is(err, want) {
			t.Fatalf("parseMutationResult(%d) = %v, want %v", code, err, want)
		}
	}
	if err := parseMutationResult([]any{int64(99)}); err == nil {
		t.Fatal("parseMutationResult() accepted unknown code")
	}
}
