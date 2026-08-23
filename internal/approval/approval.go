// Package approval defines capabilities for policy-gated Tool execution.
package approval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
)

var (
	ErrInvalid  = errors.New("invalid approval input")
	ErrConflict = errors.New("approval conflict")
	ErrNotFound = errors.New("approval not found")
	ErrStale    = errors.New("approval stale writer")
	ErrDenied   = errors.New("approval decision denied")
)

const (
	MinimumTTL = time.Minute
	MaximumTTL = 7 * 24 * time.Hour
)

var approvalIDPattern = regexp.MustCompile(`^apr_[A-Za-z0-9_-]{4,124}$`)

type Status string

const (
	StatusWaiting  Status = "WAITING"
	StatusApproved Status = "APPROVED"
	StatusRejected Status = "REJECTED"
	StatusExpired  Status = "EXPIRED"
)

func (status Status) Terminal() bool {
	return status == StatusApproved || status == StatusRejected || status == StatusExpired
}

type Record struct {
	TenantID         domain.TenantID
	ID               string
	RequesterSubject string
	ApproverSubject  string
	SessionID        domain.SessionID
	TurnID           domain.TurnID
	RunID            domain.RunID
	StepID           domain.StepID
	AttemptID        domain.AttemptID
	ToolCallID       string
	ToolName         string
	ToolVersion      string
	Effect           tools.Effect
	ArgumentsHash    [sha256.Size]byte
	NonceHash        [sha256.Size]byte
	Status           Status
	DecisionActor    string
	RowVersion       uint64
	CreatedAt        time.Time
	ExpiresAt        time.Time
	DecidedAt        time.Time
	WorkerOwner      string
	LeaseToken       uint64
	ExecutionVersion uint64
}

type Request struct {
	TenantID         domain.TenantID
	ID               string
	RequesterSubject string
	ApproverSubject  string
	SessionID        domain.SessionID
	TurnID           domain.TurnID
	RunID            domain.RunID
	StepID           domain.StepID
	AttemptID        domain.AttemptID
	ToolCallID       string
	ToolName         string
	ToolVersion      string
	Effect           tools.Effect
	ArgumentsHash    [sha256.Size]byte
	At               time.Time
	TTL              time.Duration
	WorkerOwner      string
	LeaseToken       uint64
	ExecutionVersion uint64
}

type Created struct {
	Approval Record
	Nonce    string
}

type Decision struct {
	TenantID           domain.TenantID
	ApprovalID         string
	ActorSubject       string
	Nonce              string
	ExpectedRowVersion uint64
	Approve            bool
	At                 time.Time
}

type DecisionCommand struct {
	TenantID           domain.TenantID
	ApprovalID         string
	ActorSubject       string
	NonceHash          [sha256.Size]byte
	ExpectedRowVersion uint64
	Approve            bool
	At                 time.Time
}

type Store interface {
	Create(context.Context, Record) (Record, error)
	Decide(context.Context, DecisionCommand) (Record, error)
	Get(context.Context, domain.TenantID, string) (Record, error)
}

type NonceDeriver struct{ secret []byte }

func NewNonceDeriver(secret []byte) (*NonceDeriver, error) {
	if len(secret) < 32 {
		return nil, ErrInvalid
	}
	return &NonceDeriver{secret: append([]byte(nil), secret...)}, nil
}
func (d *NonceDeriver) derive(tenant domain.TenantID, approvalID string) (string, error) {
	if d == nil || len(d.secret) < 32 || !approvalIDPattern.MatchString(approvalID) {
		return "", ErrInvalid
	}
	mac := hmac.New(sha256.New, d.secret)
	_, _ = mac.Write([]byte(tenant.String() + "\x00" + approvalID))
	return "apn_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

type Service struct {
	store  Store
	nonces *NonceDeriver
}

func NewService(store Store, nonces *NonceDeriver) (*Service, error) {
	if store == nil || nonces == nil {
		return nil, ErrInvalid
	}
	return &Service{store: store, nonces: nonces}, nil
}

func (s *Service) Request(ctx context.Context, request Request) (Created, error) {
	if ctx == nil || ctx.Err() != nil || validateRequest(request) != nil {
		return Created{}, ErrInvalid
	}
	nonce, err := s.nonces.derive(request.TenantID, request.ID)
	if err != nil {
		return Created{}, err
	}
	record := Record{TenantID: request.TenantID, ID: request.ID, RequesterSubject: request.RequesterSubject,
		ApproverSubject: request.ApproverSubject, SessionID: request.SessionID, TurnID: request.TurnID,
		RunID: request.RunID, StepID: request.StepID, AttemptID: request.AttemptID, ToolCallID: request.ToolCallID,
		ToolName: request.ToolName, ToolVersion: request.ToolVersion, Effect: request.Effect,
		ArgumentsHash: request.ArgumentsHash, NonceHash: sha256.Sum256([]byte(nonce)), Status: StatusWaiting,
		RowVersion: 1, CreatedAt: request.At.UTC(), ExpiresAt: request.At.UTC().Add(request.TTL),
		WorkerOwner: request.WorkerOwner, LeaseToken: request.LeaseToken, ExecutionVersion: request.ExecutionVersion}
	stored, err := s.store.Create(ctx, record)
	if err != nil {
		return Created{}, err
	}
	return Created{Approval: stored, Nonce: nonce}, nil
}

func (s *Service) Decide(ctx context.Context, decision Decision) (Record, error) {
	if ctx == nil || ctx.Err() != nil || !approvalIDPattern.MatchString(decision.ApprovalID) ||
		invalidSubject(decision.ActorSubject) || decision.ExpectedRowVersion == 0 || decision.At.IsZero() ||
		!strings.HasPrefix(decision.Nonce, "apn_") {
		return Record{}, ErrInvalid
	}
	if _, err := domain.ParseTenantID(decision.TenantID.String()); err != nil {
		return Record{}, ErrInvalid
	}
	return s.store.Decide(ctx, DecisionCommand{TenantID: decision.TenantID, ApprovalID: decision.ApprovalID,
		ActorSubject: decision.ActorSubject, NonceHash: sha256.Sum256([]byte(decision.Nonce)),
		ExpectedRowVersion: decision.ExpectedRowVersion, Approve: decision.Approve, At: decision.At.UTC()})
}

func (s *Service) Get(ctx context.Context, tenant domain.TenantID, approvalID string) (Record, error) {
	if ctx == nil || ctx.Err() != nil || !approvalIDPattern.MatchString(approvalID) {
		return Record{}, ErrInvalid
	}
	if _, err := domain.ParseTenantID(tenant.String()); err != nil {
		return Record{}, ErrInvalid
	}
	return s.store.Get(ctx, tenant, approvalID)
}

func validateRequest(value Request) error {
	parsers := []error{}
	_, err := domain.ParseTenantID(value.TenantID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseSessionID(value.SessionID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseTurnID(value.TurnID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseRunID(value.RunID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseStepID(value.StepID.String())
	parsers = append(parsers, err)
	_, err = domain.ParseAttemptID(value.AttemptID.String())
	parsers = append(parsers, err)
	for _, parseErr := range parsers {
		if parseErr != nil {
			return ErrInvalid
		}
	}
	if !approvalIDPattern.MatchString(value.ID) || invalidSubject(value.RequesterSubject) ||
		invalidSubject(value.ApproverSubject) || value.RequesterSubject == value.ApproverSubject ||
		!regexp.MustCompile(`^tcl_[A-Za-z0-9_-]{4,124}$`).MatchString(value.ToolCallID) ||
		!regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_-]*)*$`).MatchString(value.ToolName) ||
		!regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(value.ToolVersion) ||
		(value.Effect != tools.EffectIdempotentWrite && value.Effect != tools.EffectDestructive) ||
		value.ArgumentsHash == [sha256.Size]byte{} || value.At.IsZero() || value.TTL < MinimumTTL || value.TTL > MaximumTTL {
		return ErrInvalid
	}
	if invalidSubject(value.WorkerOwner) || value.LeaseToken == 0 || value.ExecutionVersion == 0 {
		return ErrInvalid
	}
	return nil
}

func invalidSubject(value string) bool {
	return len(value) < 1 || len(value) > 256 || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0
}

func sameRequest(left, right Record) bool {
	return left.TenantID == right.TenantID && left.ID == right.ID && left.RequesterSubject == right.RequesterSubject &&
		left.ApproverSubject == right.ApproverSubject && left.SessionID == right.SessionID && left.TurnID == right.TurnID &&
		left.RunID == right.RunID && left.StepID == right.StepID && left.AttemptID == right.AttemptID &&
		left.ToolCallID == right.ToolCallID && left.ToolName == right.ToolName && left.ToolVersion == right.ToolVersion &&
		left.Effect == right.Effect && left.ArgumentsHash == right.ArgumentsHash && hmac.Equal(left.NonceHash[:], right.NonceHash[:]) &&
		left.CreatedAt.Equal(right.CreatedAt) && left.ExpiresAt.Equal(right.ExpiresAt) &&
		left.WorkerOwner == right.WorkerOwner && left.LeaseToken == right.LeaseToken && left.ExecutionVersion == right.ExecutionVersion
}
