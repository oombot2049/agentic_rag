package domain

import (
	"strings"
	"time"
)

type MessageRole string

const (
	MessageRoleUser      MessageRole = "USER"
	MessageRoleAssistant MessageRole = "ASSISTANT"
	MessageRoleSystem    MessageRole = "SYSTEM"
	MessageRoleTool      MessageRole = "TOOL"
)

func (r MessageRole) valid() bool {
	return r == MessageRoleUser || r == MessageRoleAssistant || r == MessageRoleSystem || r == MessageRoleTool
}

type MessageVisibility string

const (
	MessageVisibilityConversation MessageVisibility = "CONVERSATION"
	MessageVisibilityCandidate    MessageVisibility = "CANDIDATE"
	MessageVisibilityInternal     MessageVisibility = "INTERNAL"
)

func (v MessageVisibility) String() string { return string(v) }

func (v MessageVisibility) valid() bool {
	return v == MessageVisibilityConversation || v == MessageVisibilityCandidate || v == MessageVisibilityInternal
}

// Message is a completed durable content item. Streaming deltas are Run events.
type Message struct {
	id         MessageID
	tenantID   TenantID
	sessionID  SessionID
	turnID     TurnID
	runID      RunID
	role       MessageRole
	visibility MessageVisibility
	content    string
	createdAt  time.Time
}

type MessageSnapshot struct {
	ID         MessageID
	TenantID   TenantID
	SessionID  SessionID
	TurnID     TurnID
	RunID      RunID
	Role       MessageRole
	Visibility MessageVisibility
	Content    string
	CreatedAt  time.Time
}

func NewUserMessage(id MessageID, tenantID TenantID, sessionID SessionID, turnID TurnID, content string, createdAt time.Time) (*Message, error) {
	return RestoreMessage(MessageSnapshot{
		ID: id, TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
		Role: MessageRoleUser, Visibility: MessageVisibilityConversation,
		Content: content, CreatedAt: createdAt,
	})
}

func NewAssistantCandidate(id MessageID, tenantID TenantID, sessionID SessionID, turnID TurnID, runID RunID, content string, createdAt time.Time) (*Message, error) {
	return RestoreMessage(MessageSnapshot{
		ID: id, TenantID: tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID,
		Role: MessageRoleAssistant, Visibility: MessageVisibilityCandidate,
		Content: content, CreatedAt: createdAt,
	})
}

func RestoreMessage(snapshot MessageSnapshot) (*Message, error) {
	if err := validateMessageID(snapshot.ID); err != nil {
		return nil, err
	}
	if err := validateTenantID(snapshot.TenantID); err != nil {
		return nil, err
	}
	if err := validateSessionID(snapshot.SessionID); err != nil {
		return nil, err
	}
	if err := validateTurnID(snapshot.TurnID); err != nil {
		return nil, err
	}
	if !snapshot.Role.valid() {
		return nil, invalid("message_role", "is unknown")
	}
	if !snapshot.Visibility.valid() {
		return nil, invalid("message_visibility", "is unknown")
	}
	if strings.TrimSpace(snapshot.Content) == "" {
		return nil, invalid("content", "must not be blank")
	}
	if snapshot.RunID != "" {
		if err := validateRunID(snapshot.RunID); err != nil {
			return nil, err
		}
	}
	if snapshot.Role == MessageRoleUser && (snapshot.RunID != "" || snapshot.Visibility != MessageVisibilityConversation) {
		return nil, invalid("message", "user input must be visible conversation content without a run")
	}
	if snapshot.Role == MessageRoleAssistant && snapshot.RunID == "" {
		return nil, invalid("run_id", "is required for assistant content")
	}
	if snapshot.Visibility == MessageVisibilityCandidate && snapshot.Role != MessageRoleAssistant {
		return nil, invalid("message_visibility", "candidate visibility is only valid for assistant content")
	}
	createdAt, err := normalizeTime("created_at", snapshot.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &Message{
		id: snapshot.ID, tenantID: snapshot.TenantID, sessionID: snapshot.SessionID,
		turnID: snapshot.TurnID, runID: snapshot.RunID, role: snapshot.Role,
		visibility: snapshot.Visibility, content: snapshot.Content, createdAt: createdAt,
	}, nil
}

// PromoteToConversation performs the one allowed Message metadata transition.
func (m *Message) PromoteToConversation() error {
	if m.role != MessageRoleAssistant || m.visibility != MessageVisibilityCandidate {
		return invalidTransition("message visibility", m.visibility, MessageVisibilityConversation)
	}
	m.visibility = MessageVisibilityConversation
	return nil
}

func (m *Message) ID() MessageID                 { return m.id }
func (m *Message) TenantID() TenantID            { return m.tenantID }
func (m *Message) SessionID() SessionID          { return m.sessionID }
func (m *Message) TurnID() TurnID                { return m.turnID }
func (m *Message) RunID() (RunID, bool)          { return m.runID, m.runID != "" }
func (m *Message) Role() MessageRole             { return m.role }
func (m *Message) Visibility() MessageVisibility { return m.visibility }
func (m *Message) Content() string               { return m.content }
func (m *Message) CreatedAt() time.Time          { return m.createdAt }
