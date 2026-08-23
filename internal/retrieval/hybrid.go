package retrieval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

var (
	ErrRetrievalFailed  = errors.New("all retrieval routes failed")
	ErrBackendViolation = errors.New("retrieval backend authorization contract violation")
)

type Principal struct {
	Kind knowledge.PrincipalKind
	ID   string
}
type AuthorizedFilter struct {
	TenantID        domain.TenantID
	KnowledgeBaseID knowledge.KnowledgeBaseID
	VersionID       knowledge.VersionID
	Principals      []Principal
	Metadata        map[string]string
}
type SearchRequest struct {
	Filter AuthorizedFilter
	Query  string
	Vector []float32
	TopK   int
}
type Candidate struct {
	TenantID        domain.TenantID
	KnowledgeBaseID knowledge.KnowledgeBaseID
	VersionID       knowledge.VersionID
	DocumentID      knowledge.DocumentID
	ChunkID         knowledge.ChunkID
	Content         string
	ACL             knowledge.ACL
	Score           float64
	Metadata        map[string]string
}
type Searcher interface {
	Search(context.Context, SearchRequest) ([]Candidate, error)
}
type SearcherFunc func(context.Context, SearchRequest) ([]Candidate, error)

func (f SearcherFunc) Search(ctx context.Context, request SearchRequest) ([]Candidate, error) {
	return f(ctx, request)
}

type ActiveResolver interface {
	ResolveActive(context.Context, domain.TenantID, knowledge.KnowledgeBaseID) (knowledge.VersionID, error)
}
type HybridRequest struct {
	TenantID        domain.TenantID
	KnowledgeBaseID knowledge.KnowledgeBaseID
	Principals      []Principal
	Metadata        map[string]string
	Query           string
	Vector          []float32
	TopK            int
}
type Hit struct {
	Candidate            Candidate
	FusedScore           float64
	VectorRank, BM25Rank int
}
type HybridResult struct {
	Hits          []Hit
	VersionID     knowledge.VersionID
	Degraded      bool
	DegradedRoute string
}
type HybridConfig struct {
	RouteTimeout                          time.Duration
	RRFConstant, VectorWeight, BM25Weight float64
	MaximumTopK                           int
}
type HybridRetriever struct {
	resolver     ActiveResolver
	vector, bm25 Searcher
	config       HybridConfig
}

func NewHybridRetriever(resolver ActiveResolver, vector, bm25 Searcher, config HybridConfig) (*HybridRetriever, error) {
	if resolver == nil || vector == nil || bm25 == nil || config.RouteTimeout <= 0 || config.RouteTimeout > time.Minute || config.RRFConstant <= 0 || config.VectorWeight <= 0 || config.BM25Weight <= 0 || config.MaximumTopK < 1 || config.MaximumTopK > 1000 {
		return nil, ErrInvalid
	}
	return &HybridRetriever{resolver, vector, bm25, config}, nil
}

type routeResult struct {
	name   string
	values []Candidate
	err    error
}

func (r *HybridRetriever) Retrieve(ctx context.Context, request HybridRequest) (HybridResult, error) {
	if ctx == nil || !validHybridRequest(request, r.config.MaximumTopK) {
		return HybridResult{}, ErrInvalid
	}
	version, err := r.resolver.ResolveActive(ctx, request.TenantID, request.KnowledgeBaseID)
	if err != nil {
		return HybridResult{}, err
	}
	if _, err = knowledge.ParseVersionID(version.String()); err != nil {
		return HybridResult{}, ErrBackendViolation
	}
	filter := AuthorizedFilter{request.TenantID, request.KnowledgeBaseID, version, append([]Principal(nil), request.Principals...), cloneMetadata(request.Metadata)}
	base := SearchRequest{Filter: filter, Query: request.Query, Vector: append([]float32(nil), request.Vector...), TopK: request.TopK}
	results := make(chan routeResult, 2)
	for _, route := range []struct {
		name     string
		searcher Searcher
	}{{"VECTOR", r.vector}, {"BM25", r.bm25}} {
		go func(name string, searcher Searcher) {
			routeCtx, cancel := context.WithTimeout(ctx, r.config.RouteTimeout)
			defer cancel()
			values, routeErr := searcher.Search(routeCtx, cloneSearchRequest(base))
			if routeErr == nil {
				routeErr = validateCandidates(filter, values, request.TopK)
			}
			results <- routeResult{name, values, routeErr}
		}(route.name, route.searcher)
	}
	responses := []routeResult{<-results, <-results}
	if err = ctx.Err(); err != nil {
		return HybridResult{}, err
	}
	var vectorValues, bm25Values []Candidate
	failed := []string{}
	for _, response := range responses {
		if response.err != nil {
			failed = append(failed, response.name)
			continue
		}
		if response.name == "VECTOR" {
			vectorValues = response.values
		} else {
			bm25Values = response.values
		}
	}
	if len(failed) == 2 {
		return HybridResult{}, fmt.Errorf("%w: VECTOR_AND_BM25", ErrRetrievalFailed)
	}
	hits := fuse(vectorValues, bm25Values, r.config)
	result := HybridResult{Hits: hits, VersionID: version}
	if len(result.Hits) > request.TopK {
		result.Hits = result.Hits[:request.TopK]
	}
	if len(failed) == 1 {
		result.Degraded = true
		result.DegradedRoute = failed[0]
	}
	return result, nil
}
func validHybridRequest(value HybridRequest, maximum int) bool {
	if _, err := domain.ParseTenantID(value.TenantID.String()); err != nil {
		return false
	}
	if _, err := knowledge.ParseKnowledgeBaseID(value.KnowledgeBaseID.String()); err != nil {
		return false
	}
	if !validText(value.Query, maxQueryBytes) || value.TopK < 1 || value.TopK > maximum || len(value.Vector) == 0 || len(value.Vector) > 65536 || len(value.Principals) == 0 || len(value.Principals) > 256 || len(value.Metadata) > 32 {
		return false
	}
	seen := map[string]struct{}{}
	for _, principal := range value.Principals {
		key := string(principal.Kind) + "\x00" + principal.ID
		validKind := principal.Kind == knowledge.PrincipalUser || principal.Kind == knowledge.PrincipalGroup || principal.Kind == knowledge.PrincipalRole
		if _, ok := seen[key]; ok || !validKind || principal.ID == "" || len(principal.ID) > 128 || strings.TrimSpace(principal.ID) != principal.ID {
			return false
		}
		seen[key] = struct{}{}
	}
	for key, item := range value.Metadata {
		if len(key) < 1 || len(key) > 64 || len(item) > 256 || strings.TrimSpace(key) != key || strings.TrimSpace(item) != item {
			return false
		}
	}
	for _, number := range value.Vector {
		if math.IsNaN(float64(number)) || math.IsInf(float64(number), 0) {
			return false
		}
	}
	return true
}
func validateCandidates(filter AuthorizedFilter, values []Candidate, topK int) error {
	if len(values) > topK {
		return ErrBackendViolation
	}
	seen := map[knowledge.ChunkID]struct{}{}
	for _, value := range values {
		if value.TenantID != filter.TenantID || value.KnowledgeBaseID != filter.KnowledgeBaseID || value.VersionID != filter.VersionID || value.ACL.TenantID() != filter.TenantID || math.IsNaN(value.Score) || math.IsInf(value.Score, 0) {
			return ErrBackendViolation
		}
		if _, err := knowledge.ParseDocumentID(value.DocumentID.String()); err != nil {
			return ErrBackendViolation
		}
		if _, err := knowledge.ParseChunkID(value.ChunkID.String()); err != nil {
			return ErrBackendViolation
		}
		for key, expected := range filter.Metadata {
			if value.Metadata[key] != expected {
				return ErrBackendViolation
			}
		}
		if _, ok := seen[value.ChunkID]; ok {
			return ErrBackendViolation
		}
		seen[value.ChunkID] = struct{}{}
		allowed := false
		for _, principal := range filter.Principals {
			if value.ACL.Allows(filter.TenantID, principal.Kind, principal.ID, knowledge.PermissionRead) {
				allowed = true
				break
			}
		}
		if !allowed {
			return ErrBackendViolation
		}
	}
	return nil
}
func fuse(vector, bm25 []Candidate, config HybridConfig) []Hit {
	byID := map[knowledge.ChunkID]*Hit{}
	add := func(values []Candidate, isVector bool, weight float64) {
		for index, value := range values {
			hit := byID[value.ChunkID]
			if hit == nil {
				copy := value
				copy.Metadata = cloneMetadata(value.Metadata)
				hit = &Hit{Candidate: copy}
				byID[value.ChunkID] = hit
			}
			rank := index + 1
			hit.FusedScore += weight / (config.RRFConstant + float64(rank))
			if isVector {
				hit.VectorRank = rank
			} else {
				hit.BM25Rank = rank
			}
		}
	}
	add(vector, true, config.VectorWeight)
	add(bm25, false, config.BM25Weight)
	result := make([]Hit, 0, len(byID))
	for _, hit := range byID {
		result = append(result, *hit)
	}
	slices.SortFunc(result, func(left, right Hit) int {
		if left.FusedScore > right.FusedScore {
			return -1
		}
		if left.FusedScore < right.FusedScore {
			return 1
		}
		return strings.Compare(left.Candidate.ChunkID.String(), right.Candidate.ChunkID.String())
	})
	return result
}
func cloneMetadata(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
func cloneSearchRequest(value SearchRequest) SearchRequest {
	value.Filter.Principals = append([]Principal(nil), value.Filter.Principals...)
	value.Filter.Metadata = cloneMetadata(value.Filter.Metadata)
	value.Vector = append([]float32(nil), value.Vector...)
	return value
}
