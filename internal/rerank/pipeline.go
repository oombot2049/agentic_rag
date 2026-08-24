// Package rerank builds stable token-bounded retrieval context.
package rerank

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
	observabilitytrace "github.com/agenticrag/agenticrag/internal/observability/trace"
	"go.opentelemetry.io/otel/attribute"
)

var ErrInvalid = errors.New("invalid rerank input")

type Candidate struct {
	TenantID    domain.TenantID
	DocumentID  knowledge.DocumentID
	ChunkID     knowledge.ChunkID
	Ordinal     uint64
	Content     string
	InitialRank int
}
type RerankDocument struct {
	ChunkID knowledge.ChunkID
	Content string
}
type Provider interface {
	Rerank(context.Context, string, []RerankDocument) ([]float64, error)
}
type TokenCounter interface {
	Count(context.Context, string) (int, error)
}
type Compressor interface {
	Compress(context.Context, string, int) (string, error)
}
type Item struct {
	DocumentID                knowledge.DocumentID
	ChunkIDs                  []knowledge.ChunkID
	FirstOrdinal, LastOrdinal uint64
	Content                   string
	Score                     float64
	Tokens                    int
	Compressed                bool
}
type Result struct {
	Items             []Item
	TotalTokens       int
	RerankFallback    bool
	DroppedDuplicates int
}
type Config struct {
	Timeout                        time.Duration
	TokenBudget, MaximumCandidates int
}
type Pipeline struct {
	provider   Provider
	counter    TokenCounter
	compressor Compressor
	config     Config
}

func NewPipeline(provider Provider, counter TokenCounter, compressor Compressor, config Config) (*Pipeline, error) {
	if provider == nil || counter == nil || compressor == nil || config.Timeout <= 0 || config.Timeout > time.Minute || config.TokenBudget < 1 || config.TokenBudget > 1_000_000 || config.MaximumCandidates < 1 || config.MaximumCandidates > 10_000 {
		return nil, ErrInvalid
	}
	return &Pipeline{provider, counter, compressor, config}, nil
}
func (p *Pipeline) Run(ctx context.Context, query string, input []Candidate) (result Result, resultErr error) {
	if ctx == nil || strings.TrimSpace(query) == "" || len(input) == 0 || len(input) > p.config.MaximumCandidates {
		return Result{}, ErrInvalid
	}
	ctx, span := observabilitytrace.StartRAG(ctx, "rerank")
	defer func() {
		span.SetAttributes(attribute.Int("agenticrag.rag.result_count", len(result.Items)), attribute.Bool("agenticrag.rag.fallback", result.RerankFallback))
		observabilitytrace.End(span, resultErr)
	}()
	candidates, dropped, err := deduplicate(input)
	if err != nil {
		return Result{}, err
	}
	documents := make([]RerankDocument, len(candidates))
	for index, value := range candidates {
		documents[index] = RerankDocument{value.ChunkID, value.Content}
	}
	rerankCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	scores, rerankErr := p.provider.Rerank(rerankCtx, query, append([]RerankDocument(nil), documents...))
	cancel()
	fallback := rerankErr != nil || len(scores) != len(candidates)
	if !fallback {
		for _, score := range scores {
			if math.IsNaN(score) || math.IsInf(score, 0) {
				fallback = true
				break
			}
		}
	}
	if fallback {
		scores = make([]float64, len(candidates))
		for index := range candidates {
			scores[index] = float64(len(candidates) - index)
		}
	}
	ranked := make([]scoredCandidate, len(candidates))
	for index := range candidates {
		ranked[index] = scoredCandidate{candidates[index], scores[index]}
	}
	slices.SortStableFunc(ranked, func(left, right scoredCandidate) int {
		if left.score > right.score {
			return -1
		}
		if left.score < right.score {
			return 1
		}
		if left.candidate.InitialRank < right.candidate.InitialRank {
			return -1
		}
		if left.candidate.InitialRank > right.candidate.InitialRank {
			return 1
		}
		return strings.Compare(left.candidate.ChunkID.String(), right.candidate.ChunkID.String())
	})
	items := merge(ranked)
	result = Result{RerankFallback: fallback, DroppedDuplicates: dropped}
	for _, item := range items {
		tokens, countErr := p.counter.Count(ctx, item.Content)
		if countErr != nil {
			return Result{}, countErr
		}
		if tokens < 1 {
			return Result{}, ErrInvalid
		}
		remaining := p.config.TokenBudget - result.TotalTokens
		if tokens <= remaining {
			item.Tokens = tokens
			result.TotalTokens += tokens
			result.Items = append(result.Items, item)
			continue
		}
		if remaining > 0 {
			compressed, compressErr := p.compressor.Compress(ctx, item.Content, remaining)
			if compressErr != nil {
				return Result{}, compressErr
			}
			if strings.TrimSpace(compressed) != "" {
				compressedTokens, recountErr := p.counter.Count(ctx, compressed)
				if recountErr != nil {
					return Result{}, recountErr
				}
				if compressedTokens > 0 && compressedTokens <= remaining {
					item.Content = compressed
					item.Tokens = compressedTokens
					item.Compressed = true
					result.TotalTokens += compressedTokens
					result.Items = append(result.Items, item)
				}
			}
		}
		break
	}
	return result, nil
}
func deduplicate(input []Candidate) ([]Candidate, int, error) {
	owned := append([]Candidate(nil), input...)
	slices.SortStableFunc(owned, func(a, b Candidate) int {
		if a.InitialRank < b.InitialRank {
			return -1
		}
		if a.InitialRank > b.InitialRank {
			return 1
		}
		return strings.Compare(a.ChunkID.String(), b.ChunkID.String())
	})
	seenIDs := map[knowledge.ChunkID]struct{}{}
	seenContent := map[[sha256.Size]byte]struct{}{}
	result := make([]Candidate, 0, len(input))
	var tenantID domain.TenantID
	for _, value := range owned {
		if _, err := domain.ParseTenantID(value.TenantID.String()); err != nil {
			return nil, 0, err
		}
		if tenantID == "" {
			tenantID = value.TenantID
		} else if tenantID != value.TenantID {
			return nil, 0, ErrInvalid
		}
		if _, err := knowledge.ParseDocumentID(value.DocumentID.String()); err != nil {
			return nil, 0, err
		}
		if _, err := knowledge.ParseChunkID(value.ChunkID.String()); err != nil {
			return nil, 0, err
		}
		if value.Ordinal == 0 || value.InitialRank < 1 || strings.TrimSpace(value.Content) == "" {
			return nil, 0, ErrInvalid
		}
		if _, ok := seenIDs[value.ChunkID]; ok {
			return nil, 0, ErrInvalid
		}
		seenIDs[value.ChunkID] = struct{}{}
		digest := sha256.Sum256([]byte(canonical(value.Content)))
		if _, ok := seenContent[digest]; ok {
			continue
		}
		seenContent[digest] = struct{}{}
		result = append(result, value)
	}
	return result, len(input) - len(result), nil
}

type scoredCandidate struct {
	candidate Candidate
	score     float64
}

func merge(ranked []scoredCandidate) []Item {
	result := make([]Item, 0, len(ranked))
	for _, value := range ranked {
		if len(result) > 0 {
			last := &result[len(result)-1]
			if last.DocumentID == value.candidate.DocumentID && last.LastOrdinal+1 == value.candidate.Ordinal {
				last.ChunkIDs = append(last.ChunkIDs, value.candidate.ChunkID)
				last.LastOrdinal = value.candidate.Ordinal
				last.Content += "\n" + value.candidate.Content
				continue
			}
		}
		result = append(result, Item{DocumentID: value.candidate.DocumentID, ChunkIDs: []knowledge.ChunkID{value.candidate.ChunkID}, FirstOrdinal: value.candidate.Ordinal, LastOrdinal: value.candidate.Ordinal, Content: value.candidate.Content, Score: value.score})
	}
	return result
}
func canonical(value string) string {
	return strings.ToLower(strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " "))
}

type PrefixCompressor struct{ counter TokenCounter }

func NewPrefixCompressor(counter TokenCounter) (*PrefixCompressor, error) {
	if counter == nil {
		return nil, ErrInvalid
	}
	return &PrefixCompressor{counter}, nil
}
func (c *PrefixCompressor) Compress(ctx context.Context, text string, maxTokens int) (string, error) {
	if ctx == nil || maxTokens < 1 {
		return "", ErrInvalid
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		candidate := strings.TrimSpace(string(runes[:middle]))
		tokens, err := c.counter.Count(ctx, candidate)
		if err != nil {
			return "", err
		}
		if tokens <= maxTokens {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return strings.TrimSpace(string(runes[:low])), nil
}
