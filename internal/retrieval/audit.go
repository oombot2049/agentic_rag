package retrieval

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

var (
	ErrAuditNotFound = errors.New("retrieval audit not found")
	ErrAuditSnapshot = errors.New("retrieval audit snapshot is missing, mismatched or expires too early")
)

type AuditCandidate struct {
	Ordinal              uint32
	DocumentID           knowledge.DocumentID
	ChunkID              knowledge.ChunkID
	VectorRank, BM25Rank int
	FusedScore           float64
	RerankScore          *float64
	Selected             bool
	ArtifactID           string
	ContentHash          [sha256.Size]byte
}
type AuditRecord struct {
	TenantID                                     domain.TenantID
	ID                                           string
	SessionID                                    domain.SessionID
	TurnID                                       domain.TurnID
	RunID                                        domain.RunID
	KnowledgeBaseID                              knowledge.KnowledgeBaseID
	IndexVersionID                               knowledge.VersionID
	OriginalQuery, RetrievalQuery, DegradedRoute string
	Candidates                                   []AuditCandidate
	CreatedAt, ExpiresAt                         time.Time
}
type AuditStore interface {
	SaveAudit(context.Context, AuditRecord) error
}
type RetentionPolicy interface {
	Retention(context.Context, domain.TenantID) (time.Duration, error)
}
type RetentionPolicyFunc func(context.Context, domain.TenantID) (time.Duration, error)

func (f RetentionPolicyFunc) Retention(ctx context.Context, tenant domain.TenantID) (time.Duration, error) {
	return f(ctx, tenant)
}

type AuditService struct {
	store  AuditStore
	policy RetentionPolicy
	now    func() time.Time
}

func NewAuditService(store AuditStore, policy RetentionPolicy) (*AuditService, error) {
	if store == nil || policy == nil {
		return nil, ErrInvalid
	}
	return &AuditService{store: store, policy: policy, now: time.Now}, nil
}
func (s *AuditService) Save(ctx context.Context, record AuditRecord) error {
	if ctx == nil {
		return ErrInvalid
	}
	retention, err := s.policy.Retention(ctx, record.TenantID)
	if err != nil {
		return err
	}
	if retention < time.Hour || retention > 365*24*time.Hour {
		return ErrInvalid
	}
	record.CreatedAt = s.now().UTC()
	record.ExpiresAt = record.CreatedAt.Add(retention)
	record.Candidates = cloneAuditCandidates(record.Candidates)
	if err = ValidateAudit(record); err != nil {
		return err
	}
	return s.store.SaveAudit(ctx, record)
}
func ValidateAudit(value AuditRecord) error {
	if _, err := domain.ParseTenantID(value.TenantID.String()); err != nil {
		return err
	}
	if !validPrefixedID(value.ID, "ret_") {
		return ErrInvalid
	}
	if _, err := domain.ParseSessionID(value.SessionID.String()); err != nil {
		return err
	}
	if _, err := domain.ParseTurnID(value.TurnID.String()); err != nil {
		return err
	}
	if _, err := domain.ParseRunID(value.RunID.String()); err != nil {
		return err
	}
	if _, err := knowledge.ParseKnowledgeBaseID(value.KnowledgeBaseID.String()); err != nil {
		return err
	}
	if _, err := knowledge.ParseVersionID(value.IndexVersionID.String()); err != nil {
		return err
	}
	if !validAuditText(value.OriginalQuery, 4096) || !validAuditText(value.RetrievalQuery, 4096) || value.CreatedAt.IsZero() || value.ExpiresAt.Sub(value.CreatedAt) < time.Hour || value.ExpiresAt.Sub(value.CreatedAt) > 365*24*time.Hour || len(value.Candidates) < 1 || len(value.Candidates) > 1000 {
		return ErrInvalid
	}
	if value.DegradedRoute != "" && value.DegradedRoute != "VECTOR" && value.DegradedRoute != "BM25" {
		return ErrInvalid
	}
	seen := map[knowledge.ChunkID]struct{}{}
	selected := 0
	for index, candidate := range value.Candidates {
		if candidate.Ordinal != uint32(index+1) {
			return ErrInvalid
		}
		if _, err := knowledge.ParseDocumentID(candidate.DocumentID.String()); err != nil {
			return err
		}
		if _, err := knowledge.ParseChunkID(candidate.ChunkID.String()); err != nil {
			return err
		}
		if _, ok := seen[candidate.ChunkID]; ok {
			return ErrInvalid
		}
		seen[candidate.ChunkID] = struct{}{}
		if candidate.VectorRank < 0 || candidate.BM25Rank < 0 || candidate.VectorRank == 0 && candidate.BM25Rank == 0 || math.IsNaN(candidate.FusedScore) || math.IsInf(candidate.FusedScore, 0) {
			return ErrInvalid
		}
		if candidate.RerankScore != nil && (math.IsNaN(*candidate.RerankScore) || math.IsInf(*candidate.RerankScore, 0)) {
			return ErrInvalid
		}
		if candidate.Selected {
			selected++
			if !validPrefixedID(candidate.ArtifactID, "art_") || candidate.ContentHash == [sha256.Size]byte{} {
				return ErrInvalid
			}
		} else if candidate.ArtifactID != "" || candidate.ContentHash != [sha256.Size]byte{} {
			return ErrInvalid
		}
	}
	if selected == 0 {
		return ErrInvalid
	}
	return nil
}
func cloneAuditCandidates(values []AuditCandidate) []AuditCandidate {
	result := append([]AuditCandidate(nil), values...)
	for index := range result {
		if result[index].RerankScore != nil {
			score := *result[index].RerankScore
			result[index].RerankScore = &score
		}
	}
	return result
}
func validPrefixedID(value, prefix string) bool {
	if len(value) < 8 || len(value) > 128 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}
func validAuditText(value string, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}
