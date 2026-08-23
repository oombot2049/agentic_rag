// Package audit records immutable, redacted evidence for Tool Calls.
package audit

import (
	"context"
	"crypto/sha256"
	"errors"
	"regexp"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
)

var (
	ErrInvalid  = errors.New("invalid tool audit")
	ErrNotFound = errors.New("tool audit not found")
	ErrConflict = errors.New("tool audit already exists")
	ErrArtifact = errors.New("tool audit Artifact is missing, mismatched or expires too early")
)

var (
	auditIDPattern    = regexp.MustCompile(`^aud_[A-Za-z0-9_-]{4,124}$`)
	toolCallPattern   = regexp.MustCompile(`^tcl_[A-Za-z0-9_-]{4,124}$`)
	artifactPattern   = regexp.MustCompile(`^art_[A-Za-z0-9_-]{4,124}$`)
	approvalPattern   = regexp.MustCompile(`^apr_[A-Za-z0-9_-]{4,124}$`)
	tokenPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@/-]{0,255}$`)
	impactCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

type PolicyDecision string

const (
	DecisionAllow           PolicyDecision = "ALLOW"
	DecisionDeny            PolicyDecision = "DENY"
	DecisionRequireApproval PolicyDecision = "REQUIRE_APPROVAL"
)

type Outcome string

const (
	OutcomeSucceeded Outcome = "SUCCEEDED"
	OutcomeFailed    Outcome = "FAILED"
	OutcomeUnknown   Outcome = "UNKNOWN"
	OutcomeDenied    Outcome = "DENIED"
)

type Attempt struct {
	AttemptID    domain.AttemptID
	AttemptNo    uint64
	Status       domain.AttemptStatus
	FailureClass string
	StartedAt    time.Time
	EndedAt      time.Time
}

type ArtifactReference struct {
	ID        string
	SHA256    [sha256.Size]byte
	SizeBytes int64
	MediaType string
}

type Record struct {
	TenantID           domain.TenantID
	ID                 string
	SessionID          domain.SessionID
	TurnID             domain.TurnID
	RunID              domain.RunID
	StepID             domain.StepID
	ToolCallID         string
	CallerSubject      string
	ToolName           string
	ToolVersion        string
	Effect             tools.Effect
	PolicyDecision     PolicyDecision
	ApprovalID         string
	ArgumentsSHA256    [sha256.Size]byte
	Outcome            Outcome
	ResultSHA256       [sha256.Size]byte
	ResultSizeBytes    int64
	BusinessImpactCode string
	Artifact           *ArtifactReference
	Attempts           []Attempt
	StartedAt          time.Time
	EndedAt            time.Time
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

type Cursor struct {
	CreatedAt time.Time
	ID        string
}

type Page struct {
	Items      []Record
	NextCursor *Cursor
}

type Store interface {
	Append(context.Context, Record) error
	GetByToolCall(context.Context, domain.TenantID, string) (Record, error)
	ListByRun(context.Context, domain.TenantID, domain.RunID, *Cursor, int) (Page, error)
}

type RetentionPolicy interface {
	Retention(context.Context, domain.TenantID) (time.Duration, error)
}

type RetentionPolicyFunc func(context.Context, domain.TenantID) (time.Duration, error)

func (f RetentionPolicyFunc) Retention(ctx context.Context, tenant domain.TenantID) (time.Duration, error) {
	return f(ctx, tenant)
}

type Service struct {
	store  Store
	policy RetentionPolicy
	now    func() time.Time
}

func NewService(store Store, policy RetentionPolicy) (*Service, error) {
	if store == nil || policy == nil {
		return nil, ErrInvalid
	}
	return &Service{store: store, policy: policy, now: time.Now}, nil
}

func (s *Service) Append(ctx context.Context, value Record) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrInvalid
	}
	retention, err := s.policy.Retention(ctx, value.TenantID)
	if err != nil {
		return err
	}
	if retention < time.Hour || retention > 365*24*time.Hour {
		return ErrInvalid
	}
	value.CreatedAt = s.now().UTC()
	value.ExpiresAt = value.CreatedAt.Add(retention)
	value = cloneRecord(value)
	if err := Validate(value); err != nil {
		return err
	}
	return s.store.Append(ctx, value)
}

func (s *Service) GetByToolCall(ctx context.Context, tenant domain.TenantID, callID string) (Record, error) {
	if !validContextTenant(ctx, tenant) || !toolCallPattern.MatchString(callID) {
		return Record{}, ErrInvalid
	}
	value, err := s.store.GetByToolCall(ctx, tenant, callID)
	return cloneRecord(value), err
}

func (s *Service) ListByRun(ctx context.Context, tenant domain.TenantID, run domain.RunID, after *Cursor, limit int) (Page, error) {
	if !validContextTenant(ctx, tenant) || !validRun(run) || limit < 1 || limit > 200 || !validCursor(after) {
		return Page{}, ErrInvalid
	}
	page, err := s.store.ListByRun(ctx, tenant, run, after, limit)
	for i := range page.Items {
		page.Items[i] = cloneRecord(page.Items[i])
	}
	if page.NextCursor != nil {
		cursor := *page.NextCursor
		page.NextCursor = &cursor
	}
	return page, err
}

func Validate(v Record) error {
	if !validTenant(v.TenantID) || !auditIDPattern.MatchString(v.ID) || !validSession(v.SessionID) ||
		!validTurn(v.TurnID) || !validRun(v.RunID) || !validStep(v.StepID) || !toolCallPattern.MatchString(v.ToolCallID) ||
		!tokenPattern.MatchString(v.CallerSubject) || !toolNamePattern.MatchString(v.ToolName) ||
		!toolVersionPattern.MatchString(v.ToolVersion) || !validEffect(v.Effect) || !validDecision(v.PolicyDecision) ||
		v.ArgumentsSHA256 == [sha256.Size]byte{} || !validOutcome(v.Outcome) ||
		!impactCodePattern.MatchString(v.BusinessImpactCode) || v.StartedAt.IsZero() || v.EndedAt.Before(v.StartedAt) ||
		v.CreatedAt.IsZero() || v.CreatedAt.Before(v.EndedAt) || v.ExpiresAt.Sub(v.CreatedAt) < time.Hour ||
		v.ExpiresAt.Sub(v.CreatedAt) > 365*24*time.Hour || !validApproval(v) || !validResult(v) || !validAttempts(v) {
		return ErrInvalid
	}
	return nil
}

var (
	toolNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_-]*)*$`)
	toolVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	failurePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	mediaTypePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}$`)
)

func validApproval(v Record) bool {
	if v.PolicyDecision == DecisionRequireApproval {
		return approvalPattern.MatchString(v.ApprovalID)
	}
	return v.ApprovalID == ""
}

func validResult(v Record) bool {
	if v.Outcome == OutcomeSucceeded {
		if v.ResultSHA256 == [sha256.Size]byte{} || v.ResultSizeBytes < 0 {
			return false
		}
	} else if v.ResultSHA256 != [sha256.Size]byte{} || v.ResultSizeBytes != 0 || v.Artifact != nil {
		return false
	}
	if v.Artifact == nil {
		return true
	}
	return artifactPattern.MatchString(v.Artifact.ID) && v.Artifact.SHA256 == v.ResultSHA256 &&
		v.Artifact.SizeBytes == v.ResultSizeBytes && v.Artifact.SizeBytes > 0 && mediaTypePattern.MatchString(v.Artifact.MediaType)
}

func validAttempts(v Record) bool {
	if v.Outcome == OutcomeDenied {
		return len(v.Attempts) == 0 && v.PolicyDecision == DecisionDeny && v.StartedAt.Equal(v.EndedAt)
	}
	if v.PolicyDecision == DecisionDeny || len(v.Attempts) == 0 || len(v.Attempts) > 100 {
		return false
	}
	seen := make(map[domain.AttemptID]struct{}, len(v.Attempts))
	for i, attempt := range v.Attempts {
		if _, err := domain.ParseAttemptID(attempt.AttemptID.String()); err != nil || attempt.AttemptNo != uint64(i+1) ||
			attempt.StartedAt.Before(v.StartedAt) || attempt.EndedAt.Before(attempt.StartedAt) || attempt.EndedAt.After(v.EndedAt) {
			return false
		}
		if _, ok := seen[attempt.AttemptID]; ok {
			return false
		}
		seen[attempt.AttemptID] = struct{}{}
		if i == 0 && !attempt.StartedAt.Equal(v.StartedAt) || i > 0 && attempt.StartedAt.Before(v.Attempts[i-1].EndedAt) {
			return false
		}
		if attempt.Status == domain.AttemptStatusSucceeded {
			if attempt.FailureClass != "" {
				return false
			}
		} else if attempt.Status == domain.AttemptStatusFailed || attempt.Status == domain.AttemptStatusUnknown {
			if !failurePattern.MatchString(attempt.FailureClass) {
				return false
			}
		} else {
			return false
		}
	}
	if !v.Attempts[len(v.Attempts)-1].EndedAt.Equal(v.EndedAt) {
		return false
	}
	last := v.Attempts[len(v.Attempts)-1].Status
	return (v.Outcome == OutcomeSucceeded && last == domain.AttemptStatusSucceeded) ||
		(v.Outcome == OutcomeFailed && last == domain.AttemptStatusFailed) ||
		(v.Outcome == OutcomeUnknown && last == domain.AttemptStatusUnknown)
}

func validDecision(v PolicyDecision) bool {
	return v == DecisionAllow || v == DecisionDeny || v == DecisionRequireApproval
}
func validOutcome(v Outcome) bool {
	return v == OutcomeSucceeded || v == OutcomeFailed || v == OutcomeUnknown || v == OutcomeDenied
}
func validEffect(v tools.Effect) bool {
	return v == tools.EffectReadOnly || v == tools.EffectIdempotentWrite || v == tools.EffectDestructive
}
func validTenant(v domain.TenantID) bool {
	_, err := domain.ParseTenantID(v.String())
	return err == nil
}
func validSession(v domain.SessionID) bool {
	_, err := domain.ParseSessionID(v.String())
	return err == nil
}
func validTurn(v domain.TurnID) bool { _, err := domain.ParseTurnID(v.String()); return err == nil }
func validRun(v domain.RunID) bool   { _, err := domain.ParseRunID(v.String()); return err == nil }
func validStep(v domain.StepID) bool { _, err := domain.ParseStepID(v.String()); return err == nil }
func validContextTenant(ctx context.Context, tenant domain.TenantID) bool {
	return ctx != nil && ctx.Err() == nil && validTenant(tenant)
}
func validCursor(v *Cursor) bool {
	return v == nil || !v.CreatedAt.IsZero() && auditIDPattern.MatchString(v.ID)
}

func cloneRecord(v Record) Record {
	v.Attempts = append([]Attempt(nil), v.Attempts...)
	if v.Artifact != nil {
		artifact := *v.Artifact
		v.Artifact = &artifact
	}
	return v
}
