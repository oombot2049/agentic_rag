// Package builtin provides safe reference adapters for the Tool Gateway.
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strings"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
	"github.com/agenticrag/agenticrag/internal/tools"
	"github.com/agenticrag/agenticrag/internal/tools/executor"
)

const (
	KnowledgeSearchName = "knowledge.search"
	Version             = "1.0.0"
)

var (
	ErrInvalid = errors.New("invalid built-in Tool request")
	ErrBackend = errors.New("built-in Tool backend failed")
	resourceID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{3,127}$`)
	artifactID = regexp.MustCompile(`^art_[A-Za-z0-9_-]{4,124}$`)
)

type KnowledgeItem struct {
	DocumentID string  `json:"document_id"`
	ChunkID    string  `json:"chunk_id"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
}

func Definitions() []tools.Definition {
	return []tools.Definition{knowledgeDefinition()}
}

func Adapters(searcher knowledge.QueryService) ([]executor.Adapter, error) {
	knowledge, err := NewKnowledgeSearch(searcher)
	if err != nil {
		return nil, err
	}
	return []executor.Adapter{knowledge}, nil
}

type KnowledgeSearch struct{ searcher knowledge.QueryService }

func NewKnowledgeSearch(searcher knowledge.QueryService) (*KnowledgeSearch, error) {
	if searcher == nil {
		return nil, ErrInvalid
	}
	return &KnowledgeSearch{searcher: searcher}, nil
}

func (*KnowledgeSearch) Name() string    { return KnowledgeSearchName }
func (*KnowledgeSearch) Version() string { return Version }
func (*KnowledgeSearch) Capabilities() executor.Capabilities {
	return executor.Capabilities{Network: executor.NetworkNone}
}

func (a *KnowledgeSearch) Execute(ctx context.Context, request executor.AdapterRequest) (io.ReadCloser, error) {
	if a == nil || a.searcher == nil || !validAdapterRequest(ctx, request, KnowledgeSearchName) {
		return nil, ErrInvalid
	}
	var input struct {
		Query string `json:"query"`
		TopK  *int   `json:"top_k"`
	}
	if strictDecode(request.Arguments, &input) != nil || !validText(input.Query, 2048, false) || input.TopK != nil && (*input.TopK < 1 || *input.TopK > 20) {
		return nil, ErrInvalid
	}
	topK := 5
	if input.TopK != nil {
		topK = *input.TopK
	}
	result, err := a.searcher.Retrieve(ctx, knowledge.QueryRequest{TenantID: request.TenantID, RunID: request.Scope.RunID,
		Query: input.Query, TopK: topK})
	if err != nil {
		return nil, ErrBackend
	}
	items := make([]KnowledgeItem, len(result.Evidence))
	for index, evidence := range result.Evidence {
		if !artifactID.MatchString(evidence.ArtifactID) {
			return nil, ErrBackend
		}
		items[index] = KnowledgeItem{DocumentID: evidence.DocumentID, ChunkID: evidence.ChunkID,
			Content: evidence.Content, Score: evidence.Score}
	}
	if len(items) > topK {
		return nil, ErrBackend
	}
	for _, item := range items {
		if !resourceID.MatchString(item.DocumentID) || !resourceID.MatchString(item.ChunkID) || !validText(item.Content, 8192, true) || math.IsNaN(item.Score) || math.IsInf(item.Score, 0) {
			return nil, ErrBackend
		}
	}
	return encode(map[string]any{"items": items})
}

func validAdapterRequest(ctx context.Context, request executor.AdapterRequest, name string) bool {
	if ctx == nil || ctx.Err() != nil || request.Name != name || request.Version != Version || request.Scope.TenantID != request.TenantID ||
		request.Scope.SessionID == "" || request.Scope.RunID == "" {
		return false
	}
	if _, err := domain.ParseTenantID(request.TenantID.String()); err != nil {
		return false
	}
	if _, err := domain.ParseSessionID(request.Scope.SessionID.String()); err != nil {
		return false
	}
	_, err := domain.ParseRunID(request.Scope.RunID.String())
	return err == nil
}

func strictDecode(input json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ErrInvalid
	}
	return nil
}

func validText(value string, maximum int, allowNewlines bool) bool {
	if len(value) == 0 || len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsControl(r) && (!allowNewlines || r != '\n' && r != '\r' && r != '\t')
	}) < 0
}

func encode(value any) (io.ReadCloser, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, ErrBackend
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func knowledgeDefinition() tools.Definition {
	return tools.Definition{Name: KnowledgeSearchName, Version: Version, Description: "Search tenant-authorized knowledge snapshots.",
		RequiredPermissions: []string{"knowledge:search"}, Scope: tools.ScopeRun, Effect: tools.EffectReadOnly,
		InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1,"maxLength":2048},"top_k":{"type":"integer","minimum":1,"maximum":20}}}`),
		OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["items"],"properties":{"items":{"type":"array","maxItems":20,"items":{"type":"object","additionalProperties":false,"required":["document_id","chunk_id","content","score"],"properties":{"document_id":{"type":"string"},"chunk_id":{"type":"string"},"content":{"type":"string","maxLength":8192},"score":{"type":"number"}}}}}}`)}
}
