package embedding

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

type providerFake struct {
	calls   []BatchRequest
	failAt  int
	invalid string
}

func (p *providerFake) Embed(_ context.Context, request BatchRequest) (BatchResult, error) {
	p.calls = append(p.calls, request)
	if p.failAt > 0 && len(p.calls) == p.failAt {
		return BatchResult{}, errors.New("provider unavailable")
	}
	vectors := make([][]float32, len(request.Inputs))
	for index, text := range request.Inputs {
		vectors[index] = []float32{float32(len([]rune(text))), float32(index + 1)}
	}
	if p.invalid == "count" {
		vectors = nil
	}
	if p.invalid == "dimension" && len(vectors) > 0 {
		vectors[0] = []float32{1}
	}
	if p.invalid == "nan" && len(vectors) > 0 {
		vectors[0][0] = float32(math.NaN())
	}
	return BatchResult{Vectors: vectors, Usage: Usage{InputTokens: uint64(len(request.Inputs) * 3), CostMicros: uint64(len(request.Inputs) * 7)}, ProviderRequestID: fmt.Sprintf("provider-%d", len(p.calls))}, nil
}

type limiterFake struct {
	units []int
	err   error
}

func (l *limiterFake) Acquire(_ context.Context, _ domain.TenantID, units int) error {
	l.units = append(l.units, units)
	return l.err
}

type memoryStore struct {
	checkpoint uint64
	vectors    map[CacheKey]Vector
	commits    map[uint64]Commit
	failBatch  int
}

func (s *memoryStore) Checkpoint(context.Context, domain.TenantID, string) (uint64, error) {
	return s.checkpoint, nil
}
func (s *memoryStore) Lookup(_ context.Context, keys []CacheKey) (map[CacheKey]Vector, error) {
	result := map[CacheKey]Vector{}
	for _, key := range keys {
		if value, ok := s.vectors[key]; ok {
			value.Values = append([]float32(nil), value.Values...)
			result[key] = value
		}
	}
	return result, nil
}
func (s *memoryStore) CommitBatch(_ context.Context, _ domain.TenantID, value Commit) error {
	if s.failBatch >= 0 && int(value.BatchNumber) == s.failBatch {
		return errors.New("commit unavailable")
	}
	if existing, ok := s.commits[value.BatchNumber]; ok {
		if !reflect.DeepEqual(existing, value) {
			return errors.New("commit conflict")
		}
		return nil
	}
	if value.NextBatch != value.BatchNumber+1 || value.BatchNumber != s.checkpoint {
		return errors.New("checkpoint conflict")
	}
	next := make(map[CacheKey]Vector, len(s.vectors)+len(value.Keys))
	for key, vector := range s.vectors {
		next[key] = vector
	}
	for index, key := range value.Keys {
		next[key] = value.Vectors[index]
	}
	s.vectors = next
	s.commits[value.BatchNumber] = value
	s.checkpoint = value.NextBatch
	return nil
}
func TestPipelineBatchesCachesCostsAndPreservesOrder(t *testing.T) {
	provider := &providerFake{}
	limiter := &limiterFake{}
	store := &memoryStore{vectors: map[CacheKey]Vector{}, commits: map[uint64]Commit{}, failBatch: -1}
	pipeline, _ := NewPipeline(provider, limiter, store, Config{Model: "embed-large", ModelVersion: "2026-08", BatchSize: 2, Dimension: 2})
	inputs := embeddingInputs("a", "bb", "ccc", "dddd", "eeeee")
	firstKey := CacheKey{"ten_0001", "embed-large", "2026-08", inputs[0].ContentHash}
	store.vectors[firstKey] = Vector{[]float32{1, 99}, "embed-large", "2026-08"}
	vectors, err := pipeline.Run(context.Background(), "ten_0001", "emb_0001", inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.calls) != 3 || !reflect.DeepEqual(limiter.units, []int{1, 2, 1}) || store.checkpoint != 3 || len(store.commits) != 3 {
		t.Fatalf("calls=%d units=%v checkpoint=%d", len(provider.calls), limiter.units, store.checkpoint)
	}
	if provider.calls[0].IdempotencyKey != "emb_0001:000000" || provider.calls[1].IdempotencyKey != "emb_0001:000001" || vectors[0].Values[1] != 99 || vectors[4].Values[0] != 5 {
		t.Fatalf("requests=%+v vectors=%+v", provider.calls, vectors)
	}
	var tokens, cost uint64
	for _, commit := range store.commits {
		tokens += commit.Usage.InputTokens
		cost += commit.Usage.CostMicros
	}
	if tokens != 12 || cost != 28 {
		t.Fatalf("usage=%d/%d", tokens, cost)
	}
	providerCalls := len(provider.calls)
	again, err := pipeline.Run(context.Background(), "ten_0001", "emb_0001", inputs)
	if err != nil || len(provider.calls) != providerCalls || !reflect.DeepEqual(vectors, again) {
		t.Fatal("completed replay called provider or changed output")
	}
}
func TestPipelineResumesAtDurableBatch(t *testing.T) {
	provider := &providerFake{failAt: 2}
	limiter := &limiterFake{}
	store := &memoryStore{vectors: map[CacheKey]Vector{}, commits: map[uint64]Commit{}, failBatch: -1}
	pipeline, _ := NewPipeline(provider, limiter, store, Config{"model", "v1", 2, 2})
	inputs := embeddingInputs("a", "bb", "ccc")
	if _, err := pipeline.Run(context.Background(), "ten_0001", "emb_0001", inputs); err == nil || store.checkpoint != 1 {
		t.Fatalf("first error=%v checkpoint=%d", err, store.checkpoint)
	}
	provider.failAt = 0
	result, err := pipeline.Run(context.Background(), "ten_0001", "emb_0001", inputs)
	if err != nil || len(result) != 3 || store.checkpoint != 2 {
		t.Fatalf("resume=%v result=%d checkpoint=%d", err, len(result), store.checkpoint)
	}
	if provider.calls[2].IdempotencyKey != "emb_0001:000001" {
		t.Fatalf("unstable retry key=%s", provider.calls[2].IdempotencyKey)
	}
}
func TestPipelineRejectsProviderCorruptionWithoutProgress(t *testing.T) {
	for _, invalid := range []string{"count", "dimension", "nan"} {
		t.Run(invalid, func(t *testing.T) {
			provider := &providerFake{invalid: invalid}
			store := &memoryStore{vectors: map[CacheKey]Vector{}, commits: map[uint64]Commit{}, failBatch: -1}
			pipeline, _ := NewPipeline(provider, &limiterFake{}, store, Config{"model", "v1", 2, 2})
			if _, err := pipeline.Run(context.Background(), "ten_0001", "emb_0001", embeddingInputs("a")); !errors.Is(err, ErrInvalid) || store.checkpoint != 0 || len(store.vectors) != 0 {
				t.Fatalf("error=%v store=%+v", err, store)
			}
		})
	}
}
func TestCacheKeysIsolateTenantAndModelVersion(t *testing.T) {
	hash := sha256.Sum256([]byte("same"))
	left := CacheKey{"ten_0001", "model", "v1", hash}
	for _, right := range []CacheKey{{"ten_0002", "model", "v1", hash}, {"ten_0001", "model", "v2", hash}, {"ten_0001", "other", "v1", hash}} {
		if left == right {
			t.Fatal("cache scope collapsed")
		}
	}
}
func TestPipelineValidatesConfigurationAndInput(t *testing.T) {
	provider := &providerFake{}
	limiter := &limiterFake{}
	store := &memoryStore{}
	for _, config := range []Config{{"", "v1", 1, 2}, {"m", "", 1, 2}, {"m", "v1", 0, 2}, {"m", "v1", 257, 2}, {"m", "v1", 1, 0}} {
		if _, err := NewPipeline(provider, limiter, store, config); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid config accepted")
		}
	}
	pipeline, _ := NewPipeline(provider, limiter, &memoryStore{vectors: map[CacheKey]Vector{}, commits: map[uint64]Commit{}, failBatch: -1}, Config{"m", "v1", 1, 2})
	bad := embeddingInputs("a")
	bad[0].ContentHash = sha256.Sum256([]byte("other"))
	if _, err := pipeline.Run(context.Background(), "ten_0001", "emb_0001", bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad hash accepted")
	}
}
func embeddingInputs(values ...string) []Input {
	result := make([]Input, len(values))
	for index, value := range values {
		id, _ := knowledge.ParseChunkID(fmt.Sprintf("chk_%04d", index+1))
		result[index] = Input{id, value, sha256.Sum256([]byte(value))}
	}
	return result
}
