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
	"github.com/agenticrag/agenticrag/internal/tools"
	"github.com/agenticrag/agenticrag/internal/tools/executor"
)

const (
	KnowledgeSearchName = "knowledge.search"
	TicketCreateName    = "support.ticket.create"
	Version             = "1.0.0"
)

var (
	ErrInvalid = errors.New("invalid built-in Tool request")
	ErrBackend = errors.New("built-in Tool backend failed")
	resourceID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{3,127}$`)
	ticketID   = regexp.MustCompile(`^tic_[A-Za-z0-9_-]{4,124}$`)
)

type KnowledgeQuery struct {
	TenantID  domain.TenantID
	SessionID domain.SessionID
	RunID     domain.RunID
	Query     string
	TopK      int
}

type KnowledgeItem struct {
	DocumentID string  `json:"document_id"`
	ChunkID    string  `json:"chunk_id"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
}

type KnowledgeSearcher interface {
	Search(context.Context, KnowledgeQuery) ([]KnowledgeItem, error)
}

type TicketCommand struct {
	TenantID       domain.TenantID
	SessionID      domain.SessionID
	RunID          domain.RunID
	IdempotencyKey string
	Title          string
	Description    string
	Priority       string
}

type TicketResult struct {
	TicketID string `json:"ticket_id"`
	Status   string `json:"status"`
}

// TicketCreator must atomically bind one IdempotencyKey to one request/result.
type TicketCreator interface {
	Create(context.Context, TicketCommand) (TicketResult, error)
}

func Definitions() []tools.Definition {
	return []tools.Definition{knowledgeDefinition(), ticketDefinition()}
}

func Adapters(searcher KnowledgeSearcher, creator TicketCreator) ([]executor.Adapter, error) {
	knowledge, err := NewKnowledgeSearch(searcher)
	if err != nil {
		return nil, err
	}
	ticket, err := NewTicketCreate(creator)
	if err != nil {
		return nil, err
	}
	return []executor.Adapter{knowledge, ticket}, nil
}

type KnowledgeSearch struct{ searcher KnowledgeSearcher }

func NewKnowledgeSearch(searcher KnowledgeSearcher) (*KnowledgeSearch, error) {
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
	items, err := a.searcher.Search(ctx, KnowledgeQuery{TenantID: request.TenantID, SessionID: request.Scope.SessionID, RunID: request.Scope.RunID, Query: input.Query, TopK: topK})
	if err != nil {
		return nil, ErrBackend
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

type TicketCreate struct{ creator TicketCreator }

func NewTicketCreate(creator TicketCreator) (*TicketCreate, error) {
	if creator == nil {
		return nil, ErrInvalid
	}
	return &TicketCreate{creator: creator}, nil
}

func (*TicketCreate) Name() string    { return TicketCreateName }
func (*TicketCreate) Version() string { return Version }
func (*TicketCreate) Capabilities() executor.Capabilities {
	return executor.Capabilities{Network: executor.NetworkNone}
}

func (a *TicketCreate) Execute(ctx context.Context, request executor.AdapterRequest) (io.ReadCloser, error) {
	if a == nil || a.creator == nil || !validAdapterRequest(ctx, request, TicketCreateName) || !validInvocationKey(request.InvocationKey) {
		return nil, ErrInvalid
	}
	var input struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Priority    string `json:"priority"`
	}
	if strictDecode(request.Arguments, &input) != nil || !validText(input.Title, 200, false) || !validText(input.Description, 4000, true) ||
		(input.Priority != "LOW" && input.Priority != "NORMAL" && input.Priority != "HIGH") {
		return nil, ErrInvalid
	}
	result, err := a.creator.Create(ctx, TicketCommand{TenantID: request.TenantID, SessionID: request.Scope.SessionID, RunID: request.Scope.RunID,
		IdempotencyKey: request.InvocationKey, Title: input.Title, Description: input.Description, Priority: input.Priority})
	if err != nil {
		return nil, ErrBackend
	}
	if !ticketID.MatchString(result.TicketID) || (result.Status != "OPEN" && result.Status != "QUEUED") {
		return nil, ErrBackend
	}
	return encode(result)
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

func validInvocationKey(value string) bool {
	return len(value) == 47 && strings.HasPrefix(value, "tik_") && strings.IndexFunc(value[4:], func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
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

func ticketDefinition() tools.Definition {
	return tools.Definition{Name: TicketCreateName, Version: Version, Description: "Create one support ticket with a durable business idempotency key.",
		RequiredPermissions: []string{"support:ticket:create"}, Scope: tools.ScopeRun, Effect: tools.EffectIdempotentWrite,
		InputSchema:  json.RawMessage(`{"type":"object","additionalProperties":false,"required":["title","description","priority"],"properties":{"title":{"type":"string","minLength":1,"maxLength":200},"description":{"type":"string","minLength":1,"maxLength":4000},"priority":{"type":"string","enum":["LOW","NORMAL","HIGH"]}}}`),
		OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["ticket_id","status"],"properties":{"ticket_id":{"type":"string","pattern":"^tic_[A-Za-z0-9_-]{4,124}$"},"status":{"type":"string","enum":["OPEN","QUEUED"]}}}`)}
}
