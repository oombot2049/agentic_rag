// Package contextmgr builds immutable linear Session context snapshots.
package contextmgr

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrInvalidArgument = errors.New("context builder invalid argument")
	ErrNotFound        = errors.New("context source not found")
	ErrFenced          = errors.New("context base version or active Run fenced")
	ErrCorruptSource   = errors.New("context source is inconsistent")
)

type Source string

const (
	SourceSystem      Source = "SYSTEM"
	SourceHistory     Source = "HISTORY"
	SourceMemory      Source = "MEMORY"
	SourceCurrentUser Source = "CURRENT_USER"
	SourceRAG         Source = "RAG"
)

const (
	PrioritySystem      = 500
	PriorityCurrentUser = 400
	PriorityHistory     = 300
	PriorityMemory      = 200
	PriorityRAG         = 100
	maximumInputBytes   = 64 << 10
)

type Query struct {
	TenantID           domain.TenantID
	SessionID          domain.SessionID
	TurnID             domain.TurnID
	RunID              domain.RunID
	BaseContextVersion uint64
}

type SourceMessage struct {
	MessageID    domain.MessageID
	TurnID       domain.TurnID
	RunID        domain.RunID
	TurnSequence uint64
	Role         domain.MessageRole
	Content      string
}

type Conversation struct {
	ContextVersion uint64
	ActiveRunID    domain.RunID
	Current        SourceMessage
	History        []SourceMessage
}

type Store interface {
	Load(context.Context, Query) (Conversation, error)
}

type Memory struct {
	ID      string
	Content string
}

type BuildRequest struct {
	Query
	SystemInstructions []string
	Memories           []Memory
}

type Entry struct {
	Source       Source             `json:"source"`
	Priority     int                `json:"priority"`
	Role         domain.MessageRole `json:"role"`
	MessageID    domain.MessageID   `json:"message_id,omitempty"`
	TurnID       domain.TurnID      `json:"turn_id,omitempty"`
	RunID        domain.RunID       `json:"run_id,omitempty"`
	TurnSequence uint64             `json:"turn_sequence,omitempty"`
	ReferenceID  string             `json:"reference_id,omitempty"`
	Content      string             `json:"content"`
}

type Snapshot struct {
	tenantID           domain.TenantID
	sessionID          domain.SessionID
	turnID             domain.TurnID
	runID              domain.RunID
	baseContextVersion uint64
	entries            []Entry
	digest             [sha256.Size]byte
}

func (s Snapshot) TenantID() domain.TenantID   { return s.tenantID }
func (s Snapshot) SessionID() domain.SessionID { return s.sessionID }
func (s Snapshot) TurnID() domain.TurnID       { return s.turnID }
func (s Snapshot) RunID() domain.RunID         { return s.runID }
func (s Snapshot) BaseContextVersion() uint64  { return s.baseContextVersion }
func (s Snapshot) Digest() [sha256.Size]byte   { return s.digest }
func (s Snapshot) Entries() []Entry            { return append([]Entry(nil), s.entries...) }

type Builder struct{ store Store }

func New(store Store) (*Builder, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is nil", ErrInvalidArgument)
	}
	return &Builder{store: store}, nil
}

func (b *Builder) Build(ctx context.Context, request BuildRequest) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	if err := validateQuery(request.Query); err != nil {
		return Snapshot{}, err
	}
	if err := validateExternalInputs(request.SystemInstructions, request.Memories); err != nil {
		return Snapshot{}, err
	}
	conversation, err := b.store.Load(ctx, request.Query)
	if err != nil {
		return Snapshot{}, err
	}
	if conversation.ContextVersion != request.BaseContextVersion || conversation.ActiveRunID != request.RunID {
		return Snapshot{}, ErrFenced
	}
	if err := validateConversation(request.Query, conversation); err != nil {
		return Snapshot{}, err
	}
	entries := make([]Entry, 0, len(request.SystemInstructions)+len(conversation.History)+len(request.Memories)+1)
	for index, instruction := range request.SystemInstructions {
		entries = append(entries, Entry{Source: SourceSystem, Priority: PrioritySystem, Role: domain.MessageRoleSystem,
			ReferenceID: fmt.Sprintf("system-%03d", index+1), Content: instruction})
	}
	for _, message := range conversation.History {
		entries = append(entries, sourceEntry(SourceHistory, PriorityHistory, message))
	}
	for _, memory := range request.Memories {
		entries = append(entries, Entry{Source: SourceMemory, Priority: PriorityMemory, Role: domain.MessageRoleSystem,
			ReferenceID: memory.ID, Content: memory.Content})
	}
	entries = append(entries, sourceEntry(SourceCurrentUser, PriorityCurrentUser, conversation.Current))
	canonical := struct {
		TenantID  string  `json:"tenant_id"`
		SessionID string  `json:"session_id"`
		TurnID    string  `json:"turn_id"`
		RunID     string  `json:"run_id"`
		Version   uint64  `json:"base_context_version"`
		Entries   []Entry `json:"entries"`
	}{request.TenantID.String(), request.SessionID.String(), request.TurnID.String(), request.RunID.String(), request.BaseContextVersion, entries}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return Snapshot{}, fmt.Errorf("canonicalize context snapshot: %w", err)
	}
	return Snapshot{tenantID: request.TenantID, sessionID: request.SessionID, turnID: request.TurnID, runID: request.RunID,
		baseContextVersion: request.BaseContextVersion, entries: append([]Entry(nil), entries...), digest: sha256.Sum256(encoded)}, nil
}

func sourceEntry(source Source, priority int, message SourceMessage) Entry {
	return Entry{Source: source, Priority: priority, Role: message.Role, MessageID: message.MessageID, TurnID: message.TurnID,
		RunID: message.RunID, TurnSequence: message.TurnSequence, Content: message.Content}
}

func validateQuery(query Query) error {
	if _, err := domain.ParseTenantID(query.TenantID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if _, err := domain.ParseSessionID(query.SessionID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if _, err := domain.ParseTurnID(query.TurnID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if _, err := domain.ParseRunID(query.RunID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return nil
}

func validateExternalInputs(system []string, memories []Memory) error {
	if len(system) > 32 || len(memories) > 256 {
		return fmt.Errorf("%w: too many context inputs", ErrInvalidArgument)
	}
	seen := make(map[string]struct{}, len(memories))
	for _, value := range system {
		if !validContent(value) {
			return fmt.Errorf("%w: system instruction", ErrInvalidArgument)
		}
	}
	for _, memory := range memories {
		if strings.TrimSpace(memory.ID) != memory.ID || memory.ID == "" || len(memory.ID) > 256 || !validContent(memory.Content) {
			return fmt.Errorf("%w: memory", ErrInvalidArgument)
		}
		if _, exists := seen[memory.ID]; exists {
			return fmt.Errorf("%w: duplicate memory", ErrInvalidArgument)
		}
		seen[memory.ID] = struct{}{}
	}
	return nil
}

func validContent(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && len(value) <= maximumInputBytes
}

func validateConversation(query Query, value Conversation) error {
	if value.Current.MessageID == "" || value.Current.TurnID != query.TurnID || value.Current.RunID != "" ||
		value.Current.Role != domain.MessageRoleUser || value.Current.TurnSequence == 0 || !validContent(value.Current.Content) {
		return ErrCorruptSource
	}
	var lastSequence uint64
	roles := make(map[uint64]uint8)
	messageIDs := map[domain.MessageID]struct{}{value.Current.MessageID: {}}
	for _, message := range value.History {
		if message.TurnID == query.TurnID || message.MessageID == "" || message.TurnSequence == 0 ||
			message.TurnSequence >= value.Current.TurnSequence || !validContent(message.Content) {
			return ErrCorruptSource
		}
		if _, duplicate := messageIDs[message.MessageID]; duplicate {
			return ErrCorruptSource
		}
		messageIDs[message.MessageID] = struct{}{}
		if message.TurnSequence < lastSequence {
			return ErrCorruptSource
		}
		lastSequence = message.TurnSequence
		switch message.Role {
		case domain.MessageRoleUser:
			if message.RunID != "" || roles[message.TurnSequence] != 0 {
				return ErrCorruptSource
			}
			roles[message.TurnSequence] |= 1
		case domain.MessageRoleAssistant:
			if message.RunID == "" || roles[message.TurnSequence] != 1 {
				return ErrCorruptSource
			}
			roles[message.TurnSequence] |= 2
		default:
			return ErrCorruptSource
		}
	}
	for _, shape := range roles {
		if shape != 3 {
			return ErrCorruptSource
		}
	}
	return nil
}
