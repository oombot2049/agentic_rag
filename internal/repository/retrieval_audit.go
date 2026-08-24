package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
	"github.com/agenticrag/agenticrag/internal/retrieval"
)

type RetrievalAuditStore struct {
	withinTx WithinTxFunc
	db       DBTX
}

func NewRetrievalAuditStore(db DBTX, withinTx WithinTxFunc) (*RetrievalAuditStore, error) {
	if db == nil || withinTx == nil {
		return nil, fmt.Errorf("%w: audit database and transaction runner", ErrInvalidArgument)
	}
	return &RetrievalAuditStore{withinTx, db}, nil
}
func (s *RetrievalAuditStore) SaveAudit(ctx context.Context, value retrieval.AuditRecord) error {
	if err := retrieval.ValidateAudit(value); err != nil {
		return err
	}
	return s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		selected := 0
		for _, candidate := range value.Candidates {
			if !candidate.Selected {
				continue
			}
			selected++
			var marker int
			err := tx.QueryRowContext(ctx, verifyAuditArtifactSQL, value.TenantID.String(), candidate.ArtifactID, candidate.ContentHash[:], value.ExpiresAt).Scan(&marker)
			if errors.Is(err, sql.ErrNoRows) {
				return retrieval.ErrAuditSnapshot
			}
			if err != nil {
				return fmt.Errorf("verify retrieval Artifact: %w", err)
			}
		}
		_, err := tx.ExecContext(ctx, insertRetrievalAuditSQL, value.TenantID.String(), value.ID, value.SessionID.String(), value.TurnID.String(), value.RunID.String(), value.KnowledgeBaseID.String(), value.IndexVersionID.String(), value.OriginalQuery, value.RetrievalQuery, nullableAuditRoute(value.DegradedRoute), len(value.Candidates), selected, value.CreatedAt, value.ExpiresAt)
		if err != nil {
			return fmt.Errorf("insert retrieval audit: %w", err)
		}
		for _, candidate := range value.Candidates {
			var rerank any
			if candidate.RerankScore != nil {
				rerank = *candidate.RerankScore
			}
			var artifactID, hash any
			if candidate.Selected {
				artifactID = candidate.ArtifactID
				hash = candidate.ContentHash[:]
			}
			if _, err = tx.ExecContext(ctx, insertRetrievalCandidateSQL, value.TenantID.String(), value.ID, candidate.Ordinal, candidate.DocumentID.String(), candidate.ChunkID.String(), candidate.VectorRank, candidate.BM25Rank, candidate.FusedScore, rerank, candidate.Selected, artifactID, hash); err != nil {
				return fmt.Errorf("insert retrieval candidate: %w", err)
			}
		}
		return nil
	})
}
func (s *RetrievalAuditStore) GetAudit(ctx context.Context, tenantID string, auditID string) (retrieval.AuditRecord, error) {
	var value retrieval.AuditRecord
	if _, err := domain.ParseTenantID(tenantID); err != nil {
		return value, err
	}
	if len(auditID) < 8 || len(auditID) > 128 || !strings.HasPrefix(auditID, "ret_") {
		return value, ErrInvalidArgument
	}
	var tenant, sessionID, turnID, runID, kbID, versionID string
	var degraded sql.NullString
	var candidateCount, selectedCount int
	err := s.db.QueryRowContext(ctx, selectRetrievalAuditSQL, tenantID, auditID).Scan(&tenant, &value.ID, &sessionID, &turnID, &runID, &kbID, &versionID, &value.OriginalQuery, &value.RetrievalQuery, &degraded, &candidateCount, &selectedCount, &value.CreatedAt, &value.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return value, retrieval.ErrAuditNotFound
	}
	if err != nil {
		return value, err
	}
	if err = parseAuditHeader(&value, tenant, sessionID, turnID, runID, kbID, versionID); err != nil {
		return value, err
	}
	value.DegradedRoute = degraded.String
	rows, err := s.db.QueryContext(ctx, selectRetrievalCandidatesSQL, tenantID, auditID)
	if err != nil {
		return value, err
	}
	defer rows.Close()
	for rows.Next() {
		var candidate retrieval.AuditCandidate
		var rerank sql.NullFloat64
		var artifact sql.NullString
		var hash []byte
		if err = rows.Scan(&candidate.Ordinal, &candidate.DocumentID, &candidate.ChunkID, &candidate.VectorRank, &candidate.BM25Rank, &candidate.FusedScore, &rerank, &candidate.Selected, &artifact, &hash); err != nil {
			return value, err
		}
		if rerank.Valid {
			score := rerank.Float64
			candidate.RerankScore = &score
		}
		if candidate.Selected {
			if len(hash) != 32 {
				return value, retrieval.ErrAuditSnapshot
			}
			candidate.ArtifactID = artifact.String
			copy(candidate.ContentHash[:], hash)
		}
		value.Candidates = append(value.Candidates, candidate)
	}
	if err = rows.Err(); err != nil {
		return value, err
	}
	if len(value.Candidates) != candidateCount {
		return value, errors.New("stored retrieval candidate count mismatch")
	}
	actualSelected := 0
	for _, candidate := range value.Candidates {
		if candidate.Selected {
			actualSelected++
		}
	}
	if actualSelected != selectedCount {
		return value, errors.New("stored retrieval selected count mismatch")
	}
	if err = retrieval.ValidateAudit(value); err != nil {
		return value, fmt.Errorf("hydrate retrieval audit: %w", err)
	}
	return value, nil
}
func parseAuditHeader(value *retrieval.AuditRecord, tenant, session, turn, run, kb, version string) error {
	var err error
	if value.TenantID, err = domain.ParseTenantID(tenant); err != nil {
		return err
	}
	if value.SessionID, err = domain.ParseSessionID(session); err != nil {
		return err
	}
	if value.TurnID, err = domain.ParseTurnID(turn); err != nil {
		return err
	}
	if value.RunID, err = domain.ParseRunID(run); err != nil {
		return err
	}
	if value.KnowledgeBaseID, err = knowledge.ParseKnowledgeBaseID(kb); err != nil {
		return err
	}
	value.IndexVersionID, err = knowledge.ParseVersionID(version)
	return err
}
func nullableAuditRoute(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const verifyAuditArtifactSQL = `SELECT 1 FROM artifact_metadata WHERE tenant_id=$1 AND id=$2 AND content_sha256=$3 AND expires_at >= $4`
const insertRetrievalAuditSQL = `INSERT INTO retrieval_audits(tenant_id,id,session_id,turn_id,run_id,knowledge_base_id,index_version_id,original_query,retrieval_query,degraded_route,candidate_count,selected_count,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`
const insertRetrievalCandidateSQL = `INSERT INTO retrieval_audit_candidates(tenant_id,retrieval_id,ordinal,document_id,chunk_id,vector_rank,bm25_rank,fused_score,rerank_score,selected,artifact_id,content_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
const selectRetrievalAuditSQL = `SELECT tenant_id,id,session_id,turn_id,run_id,knowledge_base_id,index_version_id,original_query,retrieval_query,degraded_route,candidate_count,selected_count,created_at,expires_at FROM retrieval_audits WHERE tenant_id=$1 AND id=$2`
const selectRetrievalCandidatesSQL = `SELECT ordinal,document_id,chunk_id,vector_rank,bm25_rank,fused_score,rerank_score,selected,artifact_id,content_sha256 FROM retrieval_audit_candidates WHERE tenant_id=$1 AND retrieval_id=$2 ORDER BY ordinal`

var _ retrieval.AuditStore = (*RetrievalAuditStore)(nil)
