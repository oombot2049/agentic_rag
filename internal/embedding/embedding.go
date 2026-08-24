// Package embedding implements recoverable, tenant-scoped batch embedding.
package embedding

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

var ErrInvalid = errors.New("invalid embedding input")
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

type Input struct {
	ChunkID     knowledge.ChunkID
	Content     string
	ContentHash [sha256.Size]byte
}
type CacheKey struct {
	TenantID            domain.TenantID
	Model, ModelVersion string
	ContentHash         [sha256.Size]byte
}
type Vector struct {
	Values              []float32
	Model, ModelVersion string
}
type Usage struct {
	InputTokens uint64
	CostMicros  uint64
}
type BatchRequest struct {
	IdempotencyKey, Model, ModelVersion string
	Inputs                              []string
}
type BatchResult struct {
	Vectors           [][]float32
	Usage             Usage
	ProviderRequestID string
}
type Provider interface {
	Embed(context.Context, BatchRequest) (BatchResult, error)
}
type Limiter interface {
	Acquire(context.Context, domain.TenantID, int) error
}
type Commit struct {
	JobID                  string
	BatchNumber, NextBatch uint64
	Keys                   []CacheKey
	Vectors                []Vector
	Usage                  Usage
	ProviderRequestID      string
}
type Store interface {
	Checkpoint(context.Context, domain.TenantID, string) (uint64, error)
	Lookup(context.Context, []CacheKey) (map[CacheKey]Vector, error)
	CommitBatch(context.Context, domain.TenantID, Commit) error
}
type Config struct {
	Model, ModelVersion  string
	BatchSize, Dimension int
}
type Pipeline struct {
	provider Provider
	limiter  Limiter
	store    Store
	config   Config
}

func NewPipeline(provider Provider, limiter Limiter, store Store, config Config) (*Pipeline, error) {
	if provider == nil || limiter == nil || store == nil || !namePattern.MatchString(config.Model) || !namePattern.MatchString(config.ModelVersion) || config.BatchSize < 1 || config.BatchSize > 256 || config.Dimension < 1 || config.Dimension > 65536 {
		return nil, ErrInvalid
	}
	return &Pipeline{provider: provider, limiter: limiter, store: store, config: config}, nil
}
func (p *Pipeline) Run(ctx context.Context, tenantID domain.TenantID, jobID string, inputs []Input) ([]Vector, error) {
	if ctx == nil || !validJob(jobID) || len(inputs) == 0 || len(inputs) > 1_000_000 {
		return nil, ErrInvalid
	}
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return nil, err
	}
	keys := make([]CacheKey, len(inputs))
	seen := make(map[knowledge.ChunkID]struct{}, len(inputs))
	for index, input := range inputs {
		if _, err := knowledge.ParseChunkID(input.ChunkID.String()); err != nil {
			return nil, err
		}
		if _, ok := seen[input.ChunkID]; ok {
			return nil, fmt.Errorf("%w: duplicate Chunk", ErrInvalid)
		}
		seen[input.ChunkID] = struct{}{}
		if strings.TrimSpace(input.Content) == "" || sha256.Sum256([]byte(input.Content)) != input.ContentHash {
			return nil, fmt.Errorf("%w: content hash", ErrInvalid)
		}
		keys[index] = CacheKey{tenantID, p.config.Model, p.config.ModelVersion, input.ContentHash}
	}
	totalBatches := (len(inputs) + p.config.BatchSize - 1) / p.config.BatchSize
	next, err := p.store.Checkpoint(ctx, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	if next > uint64(totalBatches) {
		return nil, fmt.Errorf("%w: checkpoint", ErrInvalid)
	}
	for batch := int(next); batch < totalBatches; batch++ {
		start := batch * p.config.BatchSize
		end := min(start+p.config.BatchSize, len(inputs))
		batchKeys := keys[start:end]
		cached, err := p.store.Lookup(ctx, batchKeys)
		if err != nil {
			return nil, err
		}
		missingKeys := make([]CacheKey, 0)
		texts := make([]string, 0)
		for offset, key := range batchKeys {
			if vector, ok := cached[key]; ok {
				if err := p.validateVector(vector); err != nil {
					return nil, err
				}
				continue
			}
			missingKeys = append(missingKeys, key)
			texts = append(texts, inputs[start+offset].Content)
		}
		commit := Commit{JobID: jobID, BatchNumber: uint64(batch), NextBatch: uint64(batch + 1)}
		if len(texts) > 0 {
			if err = p.limiter.Acquire(ctx, tenantID, len(texts)); err != nil {
				return nil, err
			}
			requestKey := fmt.Sprintf("%s:%06d", jobID, batch)
			result, embedErr := p.provider.Embed(ctx, BatchRequest{requestKey, p.config.Model, p.config.ModelVersion, append([]string(nil), texts...)})
			if embedErr != nil {
				return nil, embedErr
			}
			if len(result.Vectors) != len(texts) || result.ProviderRequestID == "" {
				return nil, fmt.Errorf("%w: provider result", ErrInvalid)
			}
			commit.Keys = missingKeys
			commit.Vectors = make([]Vector, len(texts))
			for index, values := range result.Vectors {
				vector := Vector{append([]float32(nil), values...), p.config.Model, p.config.ModelVersion}
				if err = p.validateVector(vector); err != nil {
					return nil, err
				}
				commit.Vectors[index] = vector
			}
			commit.Usage = result.Usage
			commit.ProviderRequestID = result.ProviderRequestID
		}
		if err = p.store.CommitBatch(ctx, tenantID, commit); err != nil {
			return nil, err
		}
	}
	stored, err := p.store.Lookup(ctx, keys)
	if err != nil {
		return nil, err
	}
	result := make([]Vector, len(keys))
	for index, key := range keys {
		vector, ok := stored[key]
		if !ok {
			return nil, errors.New("committed embedding is missing")
		}
		if err = p.validateVector(vector); err != nil {
			return nil, err
		}
		vector.Values = append([]float32(nil), vector.Values...)
		result[index] = vector
	}
	return result, nil
}
func (p *Pipeline) validateVector(value Vector) error {
	if value.Model != p.config.Model || value.ModelVersion != p.config.ModelVersion || len(value.Values) != p.config.Dimension {
		return fmt.Errorf("%w: vector provenance or dimension", ErrInvalid)
	}
	for _, number := range value.Values {
		if math.IsNaN(float64(number)) || math.IsInf(float64(number), 0) {
			return fmt.Errorf("%w: non-finite vector", ErrInvalid)
		}
	}
	return nil
}
func validJob(value string) bool {
	return len(value) >= 8 && len(value) <= 128 && strings.HasPrefix(value, "emb_")
}
