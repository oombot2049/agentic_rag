package knowledge

import (
	"context"
	"encoding/json"

	"github.com/agenticrag/agenticrag/internal/domain"
)

// QueryRequest contains caller-selected retrieval inputs plus authority copied
// from the current fenced execution. Implementations must still enforce ACLs.
type QueryRequest struct {
	TenantID         domain.TenantID
	RunID            domain.RunID
	Query            string
	TopK             int
	KnowledgeBaseIDs []string
	Filters          json.RawMessage
}

type QueryEvidence struct {
	ArtifactID string
	DocumentID string
	ChunkID    string
	Content    string
	Score      float64
}

type QueryResult struct {
	Evidence []QueryEvidence
}

// QueryService is the single knowledge-retrieval core shared by deterministic
// RAG and the ReAct knowledge.search adapter.
type QueryService interface {
	Retrieve(context.Context, QueryRequest) (QueryResult, error)
}
