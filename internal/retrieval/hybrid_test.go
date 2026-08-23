package retrieval

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

type resolverStub struct {
	version knowledge.VersionID
	err     error
}

func (r resolverStub) ResolveActive(context.Context, domain.TenantID, knowledge.KnowledgeBaseID) (knowledge.VersionID, error) {
	return r.version, r.err
}

type searchSpy struct {
	mu       sync.Mutex
	requests []SearchRequest
	values   []Candidate
	err      error
	wait     bool
}

func (s *searchSpy) Search(ctx context.Context, request SearchRequest) ([]Candidate, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	if s.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]Candidate(nil), s.values...), s.err
}
func TestHybridRetrieverMandatoryFiltersFusionAndImmutability(t *testing.T) {
	acl := retrievalACL(t)
	vector := &searchSpy{values: []Candidate{candidate("chk_0001", 3, acl), candidate("chk_0002", 2, acl)}}
	bm25 := &searchSpy{values: []Candidate{candidate("chk_0002", 8, acl), candidate("chk_0003", 7, acl)}}
	retriever, _ := NewHybridRetriever(resolverStub{"ver_0001", nil}, vector, bm25, hybridConfig())
	request := hybridRequest()
	originalPrincipals := append([]Principal(nil), request.Principals...)
	originalMetadata := cloneMetadata(request.Metadata)
	result, err := retriever.Retrieve(context.Background(), request)
	if err != nil || result.Degraded || result.VersionID != "ver_0001" || len(result.Hits) != 3 || result.Hits[0].Candidate.ChunkID != "chk_0002" || result.Hits[0].VectorRank != 2 || result.Hits[0].BM25Rank != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	spies := []*searchSpy{vector, bm25}
	for _, spy := range spies {
		if len(spy.requests) != 1 {
			t.Fatal("route not called")
		}
		filter := spy.requests[0].Filter
		if filter.TenantID != "ten_0001" || filter.KnowledgeBaseID != "kbs_0001" || filter.VersionID != "ver_0001" || len(filter.Principals) != 1 || filter.Metadata["region"] != "cn" || spy.requests[0].TopK != 3 {
			t.Fatalf("filter=%+v", filter)
		}
		spy.requests[0].Filter.Principals[0].ID = "mutated"
		spy.requests[0].Filter.Metadata["region"] = "us"
	}
	if !reflect.DeepEqual(request.Principals, originalPrincipals) || !reflect.DeepEqual(request.Metadata, originalMetadata) {
		t.Fatal("caller input mutated")
	}
}
func TestHybridRetrieverAuthorizationViolationFailsWholeRoute(t *testing.T) {
	acl := retrievalACL(t)
	foreign := candidate("chk_0001", 1, acl)
	foreign.TenantID = "ten_9999"
	vector := &searchSpy{values: []Candidate{foreign}}
	bm25 := &searchSpy{values: []Candidate{candidate("chk_0002", 1, acl)}}
	retriever, _ := NewHybridRetriever(resolverStub{"ver_0001", nil}, vector, bm25, hybridConfig())
	result, err := retriever.Retrieve(context.Background(), hybridRequest())
	if err != nil || !result.Degraded || result.DegradedRoute != "VECTOR" || len(result.Hits) != 1 || result.Hits[0].Candidate.ChunkID != "chk_0002" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	bm25.values = []Candidate{foreign}
	if _, err = retriever.Retrieve(context.Background(), hybridRequest()); !errors.Is(err, ErrRetrievalFailed) {
		t.Fatalf("two violations=%v", err)
	}
}
func TestHybridRetrieverTimeoutDegradesAndParentCancellationWins(t *testing.T) {
	acl := retrievalACL(t)
	vector := &searchSpy{wait: true}
	bm25 := &searchSpy{values: []Candidate{candidate("chk_0001", 1, acl)}}
	config := hybridConfig()
	config.RouteTimeout = 20 * time.Millisecond
	retriever, _ := NewHybridRetriever(resolverStub{"ver_0001", nil}, vector, bm25, config)
	result, err := retriever.Retrieve(context.Background(), hybridRequest())
	if err != nil || !result.Degraded || result.DegradedRoute != "VECTOR" || len(result.Hits) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = retriever.Retrieve(ctx, hybridRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}
func TestHybridRetrieverStableTiesAndInvalidInputs(t *testing.T) {
	acl := retrievalACL(t)
	vector := &searchSpy{values: []Candidate{candidate("chk_0002", 1, acl), candidate("chk_0001", 1, acl)}}
	empty := &searchSpy{}
	retriever, _ := NewHybridRetriever(resolverStub{"ver_0001", nil}, vector, empty, hybridConfig())
	result, err := retriever.Retrieve(context.Background(), hybridRequest())
	if err != nil || result.Hits[0].Candidate.ChunkID != "chk_0002" {
		t.Fatalf("rank order lost: %+v", result)
	}
	request := hybridRequest()
	request.Principals = nil
	if _, err = retriever.Retrieve(context.Background(), request); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing ACL principals accepted")
	}
	if _, err := NewHybridRetriever(nil, vector, empty, hybridConfig()); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil resolver accepted")
	}
}
func retrievalACL(t *testing.T) knowledge.ACL {
	t.Helper()
	acl, err := knowledge.NewACL("ten_0001", 1, []knowledge.Grant{{Kind: knowledge.PrincipalUser, PrincipalID: "alice", Permission: knowledge.PermissionRead}})
	if err != nil {
		t.Fatal(err)
	}
	return acl
}
func candidate(id string, score float64, acl knowledge.ACL) Candidate {
	return Candidate{TenantID: "ten_0001", KnowledgeBaseID: "kbs_0001", VersionID: "ver_0001", DocumentID: "doc_0001", ChunkID: knowledge.ChunkID(id), Content: "content", ACL: acl, Score: score, Metadata: map[string]string{"region": "cn"}}
}
func hybridRequest() HybridRequest {
	return HybridRequest{TenantID: "ten_0001", KnowledgeBaseID: "kbs_0001", Principals: []Principal{{Kind: knowledge.PrincipalUser, ID: "alice"}}, Metadata: map[string]string{"region": "cn"}, Query: "database replication", Vector: []float32{1, 2}, TopK: 3}
}
func hybridConfig() HybridConfig {
	return HybridConfig{RouteTimeout: time.Second, RRFConstant: 60, VectorWeight: 1, BM25Weight: 1, MaximumTopK: 100}
}
