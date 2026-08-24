package retrieval

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

import "github.com/agenticrag/agenticrag/internal/domain"

type auditStoreStub struct {
	value AuditRecord
	err   error
	calls int
}

func (s *auditStoreStub) SaveAudit(_ context.Context, value AuditRecord) error {
	s.calls++
	s.value = value
	return s.err
}
func TestAuditServiceRetentionValidationAndDefensiveCopy(t *testing.T) {
	store := &auditStoreStub{}
	policy := RetentionPolicyFunc(func(context.Context, domain.TenantID) (time.Duration, error) { return 30 * 24 * time.Hour, nil })
	service, _ := NewAuditService(store, policy)
	at := time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return at }
	record := validAuditRecord()
	score := *record.Candidates[0].RerankScore
	if err := service.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	record.Candidates[0].RerankScore = &score
	*record.Candidates[0].RerankScore = 99
	if store.calls != 1 || store.value.CreatedAt != at || store.value.ExpiresAt != at.Add(30*24*time.Hour) || *store.value.Candidates[0].RerankScore == 99 {
		t.Fatalf("stored=%+v", store.value)
	}
}
func TestAuditValidationBoundaries(t *testing.T) {
	base := validAuditRecord()
	base.CreatedAt = time.Now().UTC()
	base.ExpiresAt = base.CreatedAt.Add(time.Hour)
	tests := []func(*AuditRecord){func(v *AuditRecord) { v.ID = "bad" }, func(v *AuditRecord) { v.OriginalQuery = " " }, func(v *AuditRecord) { v.DegradedRoute = "OTHER" }, func(v *AuditRecord) { v.Candidates[0].Ordinal = 2 }, func(v *AuditRecord) { v.Candidates[0].VectorRank = 0; v.Candidates[0].BM25Rank = 0 }, func(v *AuditRecord) { v.Candidates[0].FusedScore = math.NaN() }, func(v *AuditRecord) { v.Candidates[0].ArtifactID = "" }, func(v *AuditRecord) { v.Candidates[0].ContentHash = [sha256.Size]byte{} }, func(v *AuditRecord) { v.Candidates[1].ArtifactID = "art_0002" }, func(v *AuditRecord) { v.Candidates[1].ChunkID = v.Candidates[0].ChunkID }}
	for index, mutate := range tests {
		value := base
		value.Candidates = cloneAuditCandidates(base.Candidates)
		mutate(&value)
		if err := ValidateAudit(value); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d=%v", index, err)
		}
	}
}
func TestAuditServiceRejectsRetentionAndStoreError(t *testing.T) {
	store := &auditStoreStub{err: errors.New("database")}
	policy := RetentionPolicyFunc(func(context.Context, domain.TenantID) (time.Duration, error) { return time.Minute, nil })
	service, _ := NewAuditService(store, policy)
	if err := service.Save(context.Background(), validAuditRecord()); !errors.Is(err, ErrInvalid) || store.calls != 0 {
		t.Fatalf("retention=%v calls=%d", err, store.calls)
	}
	policy = func(context.Context, domain.TenantID) (time.Duration, error) { return 24 * time.Hour, nil }
	service, _ = NewAuditService(store, policy)
	if err := service.Save(context.Background(), validAuditRecord()); err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("store=%v", err)
	}
	if _, err := NewAuditService(nil, policy); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil store accepted")
	}
}
func validAuditRecord() AuditRecord {
	score := 0.8
	hash := sha256.Sum256([]byte("snapshot"))
	return AuditRecord{TenantID: "ten_0001", ID: "ret_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", KnowledgeBaseID: "kbs_0001", IndexVersionID: "ver_0001", OriginalQuery: "它怎么样？", RetrievalQuery: "Product X 性能", DegradedRoute: "BM25", Candidates: []AuditCandidate{{Ordinal: 1, DocumentID: "doc_0001", ChunkID: "chk_0001", VectorRank: 1, BM25Rank: 2, FusedScore: 0.9, RerankScore: &score, Selected: true, ArtifactID: "art_0001", ContentHash: hash}, {Ordinal: 2, DocumentID: "doc_0002", ChunkID: "chk_0002", VectorRank: 2, BM25Rank: 1, FusedScore: 0.7}}}
}
