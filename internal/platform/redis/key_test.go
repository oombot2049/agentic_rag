package redis

import (
	"math/rand"
	"strings"
	"testing"
)

func TestKeyBuilderProducesTenantScopedKeys(t *testing.T) {
	builder, err := NewKeyBuilder("production")
	if err != nil {
		t.Fatal(err)
	}
	first, err := builder.Build("ten_01", NamespaceEvents, "run_01", "stream")
	if err != nil {
		t.Fatal(err)
	}
	second, err := builder.Build("ten_02", NamespaceEvents, "run_01", "stream")
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "ar:production:{ten_01}:events:run_01:stream" {
		t.Fatalf("key = %q", first)
	}
	if first == second || !strings.Contains(second.String(), "{ten_02}") {
		t.Fatalf("tenant collision: %q %q", first, second)
	}
}

func TestKeyBuilderRejectsInjectionAndMissingScope(t *testing.T) {
	tests := []struct {
		name, environment, tenant string
		namespace                 Namespace
		segments                  []string
	}{
		{"bad environment", "prod:other", "ten_1", NamespaceCache, []string{"x"}},
		{"empty tenant", "prod", "", NamespaceCache, []string{"x"}},
		{"tenant delimiter", "prod", "ten}:cache:{other", NamespaceCache, []string{"x"}},
		{"unknown namespace", "prod", "ten_1", Namespace("raw"), []string{"x"}},
		{"no logical key", "prod", "ten_1", NamespaceCache, nil},
		{"wildcard", "prod", "ten_1", NamespaceCache, []string{"*"}},
		{"colon", "prod", "ten_1", NamespaceCache, []string{"a:b"}},
		{"whitespace", "prod", "ten_1", NamespaceCache, []string{"a b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder, err := NewKeyBuilder(tt.environment)
			if err == nil {
				_, err = builder.Build(tt.tenant, tt.namespace, tt.segments...)
			}
			if err == nil {
				t.Fatal("error = nil")
			}
		})
	}
}

func TestRandomTenantKeysNeverCollide(t *testing.T) {
	builder, _ := NewKeyBuilder("test")
	random := rand.New(rand.NewSource(42))
	seen := make(map[string]string, 5000)
	for i := 0; i < 5000; i++ {
		tenant := "ten_" + randomToken(random, 20)
		key, err := builder.Build(tenant, NamespaceCache, "session_"+randomToken(random, 20))
		if err != nil {
			t.Fatal(err)
		}
		if prior, exists := seen[key.String()]; exists && prior != tenant {
			t.Fatalf("cross-tenant collision: %s and %s", prior, tenant)
		}
		seen[key.String()] = tenant
	}
}

func randomToken(random *rand.Rand, length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = alphabet[random.Intn(len(alphabet))]
	}
	return string(result)
}

func FuzzKeyBuilder(f *testing.F) {
	f.Add("ten_1", "session_1")
	f.Add("{other}", "x:y")
	builder, _ := NewKeyBuilder("test")
	f.Fuzz(func(t *testing.T, tenant, logical string) {
		key, err := builder.Build(tenant, NamespaceCache, logical)
		if err != nil {
			return
		}
		if !strings.HasPrefix(key.String(), "ar:test:{"+tenant+"}:cache:") {
			t.Fatalf("scope lost: %q", key)
		}
		if strings.Count(key.String(), ":") != 4 {
			t.Fatalf("delimiter injection: %q", key)
		}
	})
}
