package tools

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrLedgerInvalid  = errors.New("invalid tool ledger input")
	ErrLedgerConflict = errors.New("tool ledger conflict")
	ErrLedgerNotFound = errors.New("tool call not found")
	ErrLedgerStale    = errors.New("tool ledger stale writer")
)

var (
	toolCallIDPattern = regexp.MustCompile(`^tcl_[A-Za-z0-9_-]{4,124}$`)
	failurePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

const maximumToolResultBytes = 64 << 10

type CallStatus string

const (
	CallPrepared  CallStatus = "PREPARED"
	CallSucceeded CallStatus = "SUCCEEDED"
	CallFailed    CallStatus = "FAILED"
	CallUnknown   CallStatus = "UNKNOWN"
)

func (status CallStatus) Terminal() bool {
	return status == CallSucceeded || status == CallFailed
}

type AttemptRecord struct {
	AttemptID domain.AttemptID
	AttemptNo uint64
	Status    domain.AttemptStatus
	StartedAt time.Time
	EndedAt   time.Time
}

type CallRecord struct {
	TenantID      domain.TenantID
	CallID        string
	RunID         domain.RunID
	StepID        domain.StepID
	Name          string
	Version       string
	Effect        Effect
	ScopeLevel    ScopeLevel
	Scope         Scope
	ArgumentsHash [sha256.Size]byte
	InvocationKey string
	Status        CallStatus
	Result        json.RawMessage
	FailureClass  string
	Attempts      []AttemptRecord
	RowVersion    uint64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type BeginCommand struct {
	CallID     string
	RunID      domain.RunID
	StepID     domain.StepID
	AttemptID  domain.AttemptID
	AttemptNo  uint64
	Invocation Invocation
	At         time.Time
}

type Decision string

const (
	DecisionExecute       Decision = "EXECUTE"
	DecisionQueryResult   Decision = "QUERY_RESULT"
	DecisionReturnResult  Decision = "RETURN_RESULT"
	DecisionReturnFailure Decision = "RETURN_FAILURE"
)

type BeginResult struct {
	Call     CallRecord
	Decision Decision
}

type OutcomeCommand struct {
	TenantID           domain.TenantID
	CallID             string
	AttemptID          domain.AttemptID
	ExpectedRowVersion uint64
	Status             CallStatus
	Result             json.RawMessage
	FailureClass       string
	At                 time.Time
}

type LedgerStore interface {
	Begin(context.Context, CallRecord, AttemptRecord) (record CallRecord, created bool, err error)
	RecordOutcome(context.Context, OutcomeCommand) (CallRecord, error)
	Get(context.Context, domain.TenantID, string) (CallRecord, error)
}

type KeyDeriver interface {
	Derive(domain.TenantID, string) (string, error)
}

type HMACKeyDeriver struct{ secret []byte }

func NewHMACKeyDeriver(secret []byte) (*HMACKeyDeriver, error) {
	if len(secret) < 32 {
		return nil, ErrLedgerInvalid
	}
	return &HMACKeyDeriver{secret: append([]byte(nil), secret...)}, nil
}

func (d *HMACKeyDeriver) Derive(tenantID domain.TenantID, callID string) (string, error) {
	if d == nil || len(d.secret) < 32 || !toolCallIDPattern.MatchString(callID) {
		return "", ErrLedgerInvalid
	}
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return "", ErrLedgerInvalid
	}
	mac := hmac.New(sha256.New, d.secret)
	_, _ = mac.Write([]byte(tenantID.String() + "\x00" + callID))
	return "tik_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

type Ledger struct {
	store LedgerStore
	keys  KeyDeriver
}

func NewLedger(store LedgerStore, keys KeyDeriver) (*Ledger, error) {
	if store == nil || keys == nil {
		return nil, ErrLedgerInvalid
	}
	return &Ledger{store: store, keys: keys}, nil
}

func (l *Ledger) Begin(ctx context.Context, command BeginCommand) (BeginResult, error) {
	if ctx == nil || ctx.Err() != nil || !toolCallIDPattern.MatchString(command.CallID) ||
		command.AttemptNo == 0 || command.At.IsZero() {
		return BeginResult{}, ErrLedgerInvalid
	}
	if _, err := domain.ParseRunID(command.RunID.String()); err != nil {
		return BeginResult{}, ErrLedgerInvalid
	}
	if _, err := domain.ParseStepID(command.StepID.String()); err != nil {
		return BeginResult{}, ErrLedgerInvalid
	}
	if _, err := domain.ParseAttemptID(command.AttemptID.String()); err != nil {
		return BeginResult{}, ErrLedgerInvalid
	}
	if err := validateInvocation(command.Invocation); err != nil {
		return BeginResult{}, err
	}
	key, err := l.keys.Derive(command.Invocation.Scope.TenantID, command.CallID)
	if err != nil {
		return BeginResult{}, ErrLedgerInvalid
	}
	argumentsHash, err := canonicalArgumentsHash(command.Invocation.Arguments)
	if err != nil {
		return BeginResult{}, ErrLedgerInvalid
	}
	at := command.At.UTC()
	record := CallRecord{
		TenantID: command.Invocation.Scope.TenantID, CallID: command.CallID,
		RunID: command.RunID, StepID: command.StepID,
		Name: command.Invocation.Definition.Name, Version: command.Invocation.Definition.Version,
		Effect: command.Invocation.Definition.Effect, ScopeLevel: command.Invocation.Definition.Scope,
		Scope:         command.Invocation.Scope,
		ArgumentsHash: argumentsHash, InvocationKey: key,
		Status: CallPrepared, RowVersion: 1, CreatedAt: at, UpdatedAt: at,
	}
	attempt := AttemptRecord{AttemptID: command.AttemptID, AttemptNo: command.AttemptNo,
		Status: domain.AttemptStatusStarted, StartedAt: at}
	stored, created, err := l.store.Begin(ctx, record, attempt)
	if err != nil {
		return BeginResult{}, err
	}
	decision := DecisionQueryResult
	if created {
		decision = DecisionExecute
	} else if stored.Status == CallSucceeded {
		decision = DecisionReturnResult
	} else if stored.Status == CallFailed {
		decision = DecisionReturnFailure
	}
	return BeginResult{Call: cloneCallRecord(stored), Decision: decision}, nil
}

func canonicalArgumentsHash(arguments json.RawMessage) ([sha256.Size]byte, error) {
	var value any
	if err := json.Unmarshal(arguments, &value); err != nil {
		return [sha256.Size]byte{}, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

func (l *Ledger) RecordOutcome(ctx context.Context, command OutcomeCommand) (CallRecord, error) {
	if ctx == nil || ctx.Err() != nil || !validOutcome(command) {
		return CallRecord{}, ErrLedgerInvalid
	}
	record, err := l.store.RecordOutcome(ctx, command)
	return cloneCallRecord(record), err
}

func (l *Ledger) Get(ctx context.Context, tenantID domain.TenantID, callID string) (CallRecord, error) {
	if ctx == nil || ctx.Err() != nil || !toolCallIDPattern.MatchString(callID) {
		return CallRecord{}, ErrLedgerInvalid
	}
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return CallRecord{}, ErrLedgerInvalid
	}
	record, err := l.store.Get(ctx, tenantID, callID)
	return cloneCallRecord(record), err
}

func validateInvocation(invocation Invocation) error {
	if _, err := domain.ParseTenantID(invocation.Scope.TenantID.String()); err != nil {
		return ErrLedgerInvalid
	}
	if !namePattern.MatchString(invocation.Definition.Name) ||
		!versionPattern.MatchString(invocation.Definition.Version) ||
		!validEffect(invocation.Definition.Effect) ||
		!validScope(invocation.Definition.Scope, invocation.Scope) ||
		strictJSONObject(invocation.Arguments) != nil {
		return ErrLedgerInvalid
	}
	return nil
}

func validOutcome(command OutcomeCommand) bool {
	if _, err := domain.ParseTenantID(command.TenantID.String()); err != nil {
		return false
	}
	if !toolCallIDPattern.MatchString(command.CallID) || command.ExpectedRowVersion == 0 || command.At.IsZero() {
		return false
	}
	if _, err := domain.ParseAttemptID(command.AttemptID.String()); err != nil {
		return false
	}
	switch command.Status {
	case CallSucceeded:
		return command.FailureClass == "" && len(command.Result) <= maximumToolResultBytes && strictJSONObject(command.Result) == nil
	case CallFailed, CallUnknown:
		return len(command.Result) == 0 && failurePattern.MatchString(command.FailureClass)
	default:
		return false
	}
}

func sameImmutableCall(left, right CallRecord) bool {
	return left.TenantID == right.TenantID && left.CallID == right.CallID &&
		left.RunID == right.RunID && left.StepID == right.StepID && left.Name == right.Name &&
		left.Version == right.Version && left.Effect == right.Effect && left.ScopeLevel == right.ScopeLevel && left.Scope == right.Scope &&
		left.ArgumentsHash == right.ArgumentsHash && hmac.Equal([]byte(left.InvocationKey), []byte(right.InvocationKey))
}

func cloneCallRecord(value CallRecord) CallRecord {
	value.Result = append(json.RawMessage(nil), value.Result...)
	value.Attempts = append([]AttemptRecord(nil), value.Attempts...)
	return value
}
