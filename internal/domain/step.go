package domain

import "time"

type StepKind string

const (
	StepKindContext    StepKind = "CONTEXT"
	StepKindRetrieval  StepKind = "RETRIEVAL"
	StepKindTool       StepKind = "TOOL"
	StepKindGeneration StepKind = "GENERATION"
)

func (k StepKind) valid() bool {
	return k == StepKindContext || k == StepKindRetrieval || k == StepKindTool || k == StepKindGeneration
}

type StepStatus string

const (
	StepStatusPending   StepStatus = "PENDING"
	StepStatusRunning   StepStatus = "RUNNING"
	StepStatusSucceeded StepStatus = "SUCCEEDED"
	StepStatusFailed    StepStatus = "FAILED"
	StepStatusCancelled StepStatus = "CANCELLED"
	StepStatusSkipped   StepStatus = "SKIPPED"
)

func (s StepStatus) String() string { return string(s) }

func (s StepStatus) valid() bool {
	return s == StepStatusPending || s == StepStatusRunning || s == StepStatusSucceeded ||
		s == StepStatusFailed || s == StepStatusCancelled || s == StepStatusSkipped
}

func (s StepStatus) Terminal() bool {
	return s != StepStatusPending && s != StepStatusRunning && s.valid()
}

// Step is one ordered logical action in a Run.
type Step struct {
	id         StepID
	tenantID   TenantID
	runID      RunID
	stepNo     uint64
	kind       StepKind
	status     StepStatus
	attemptIDs []AttemptID
	createdAt  time.Time
	updatedAt  time.Time
}

type StepSnapshot struct {
	ID         StepID
	TenantID   TenantID
	RunID      RunID
	StepNo     uint64
	Kind       StepKind
	Status     StepStatus
	AttemptIDs []AttemptID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func NewStep(id StepID, tenantID TenantID, runID RunID, stepNo uint64, kind StepKind, createdAt time.Time) (*Step, error) {
	return RestoreStep(StepSnapshot{
		ID: id, TenantID: tenantID, RunID: runID, StepNo: stepNo, Kind: kind,
		Status: StepStatusPending, CreatedAt: createdAt, UpdatedAt: createdAt,
	})
}

func RestoreStep(snapshot StepSnapshot) (*Step, error) {
	if err := validateStepID(snapshot.ID); err != nil {
		return nil, err
	}
	if err := validateTenantID(snapshot.TenantID); err != nil {
		return nil, err
	}
	if err := validateRunID(snapshot.RunID); err != nil {
		return nil, err
	}
	if err := validatePositiveInt64("step_no", snapshot.StepNo); err != nil {
		return nil, err
	}
	if !snapshot.Kind.valid() {
		return nil, invalid("step_kind", "is unknown")
	}
	if !snapshot.Status.valid() {
		return nil, invalid("step_status", "is unknown")
	}
	seen := make(map[AttemptID]struct{}, len(snapshot.AttemptIDs))
	for _, attemptID := range snapshot.AttemptIDs {
		if err := validateAttemptID(attemptID); err != nil {
			return nil, err
		}
		if _, exists := seen[attemptID]; exists {
			return nil, invalid("attempt_ids", "must be unique")
		}
		seen[attemptID] = struct{}{}
	}
	createdAt, err := normalizeTime("created_at", snapshot.CreatedAt)
	if err != nil {
		return nil, err
	}
	updatedAt, err := normalizeTime("updated_at", snapshot.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if updatedAt.Before(createdAt) {
		return nil, invalid("updated_at", "must not precede created_at")
	}
	return &Step{
		id: snapshot.ID, tenantID: snapshot.TenantID, runID: snapshot.RunID,
		stepNo: snapshot.StepNo, kind: snapshot.Kind, status: snapshot.Status,
		attemptIDs: copySlice(snapshot.AttemptIDs), createdAt: createdAt, updatedAt: updatedAt,
	}, nil
}

func (s *Step) Start(at time.Time) error  { return s.transition(StepStatusRunning, at) }
func (s *Step) Skip(at time.Time) error   { return s.transition(StepStatusSkipped, at) }
func (s *Step) Cancel(at time.Time) error { return s.transition(StepStatusCancelled, at) }

func (s *Step) AddAttempt(attempt *Attempt, at time.Time) error {
	if s.status != StepStatusRunning {
		return invalid("attempt", "can only be appended to a running step")
	}
	if attempt == nil || attempt.tenantID != s.tenantID || attempt.stepID != s.id {
		return invalid("attempt", "does not belong to step tenant and step")
	}
	if attempt.status != AttemptStatusStarted || attempt.attemptNo != uint64(len(s.attemptIDs)+1) {
		return invalid("attempt", "must be the next started attempt")
	}
	for _, id := range s.attemptIDs {
		if id == attempt.id {
			return invalid("attempt_id", "is already registered")
		}
	}
	updatedAt, err := s.validMutationTime(at)
	if err != nil {
		return err
	}
	s.attemptIDs = append(s.attemptIDs, attempt.id)
	s.updatedAt = updatedAt
	return nil
}

func (s *Step) Succeed(last *Attempt, at time.Time) error {
	if err := s.validateLastAttempt(last, AttemptStatusSucceeded); err != nil {
		return err
	}
	return s.transition(StepStatusSucceeded, at)
}

func (s *Step) Fail(last *Attempt, at time.Time) error {
	if last == nil || (last.status != AttemptStatusFailed && last.status != AttemptStatusUnknown) {
		return invalid("attempt", "last attempt must have failed or unknown outcome")
	}
	if err := s.validateLastAttemptIdentity(last); err != nil {
		return err
	}
	return s.transition(StepStatusFailed, at)
}

func (s *Step) validateLastAttempt(last *Attempt, expected AttemptStatus) error {
	if last == nil || last.status != expected {
		return invalid("attempt", "last attempt has an invalid outcome")
	}
	return s.validateLastAttemptIdentity(last)
}

func (s *Step) validateLastAttemptIdentity(last *Attempt) error {
	if last.tenantID != s.tenantID || last.stepID != s.id || len(s.attemptIDs) == 0 ||
		s.attemptIDs[len(s.attemptIDs)-1] != last.id {
		return invalid("attempt", "must be the last attempt owned by the step")
	}
	return nil
}

func (s *Step) transition(target StepStatus, at time.Time) error {
	if !validStepTransition(s.status, target) {
		return invalidTransition("step", s.status, target)
	}
	updatedAt, err := s.validMutationTime(at)
	if err != nil {
		return err
	}
	s.status = target
	s.updatedAt = updatedAt
	return nil
}

func validStepTransition(from, to StepStatus) bool {
	return from == StepStatusPending && (to == StepStatusRunning || to == StepStatusSkipped) ||
		from == StepStatusRunning && (to == StepStatusSucceeded || to == StepStatusFailed || to == StepStatusCancelled)
}

func (s *Step) validMutationTime(at time.Time) (time.Time, error) {
	normalized, err := normalizeTime("updated_at", at)
	if err != nil {
		return time.Time{}, err
	}
	if normalized.Before(s.updatedAt) {
		return time.Time{}, invalid("updated_at", "must not move backwards")
	}
	return normalized, nil
}

func (s *Step) ID() StepID              { return s.id }
func (s *Step) TenantID() TenantID      { return s.tenantID }
func (s *Step) RunID() RunID            { return s.runID }
func (s *Step) StepNo() uint64          { return s.stepNo }
func (s *Step) Kind() StepKind          { return s.kind }
func (s *Step) Status() StepStatus      { return s.status }
func (s *Step) Terminal() bool          { return s.status.Terminal() }
func (s *Step) AttemptIDs() []AttemptID { return copySlice(s.attemptIDs) }
func (s *Step) CreatedAt() time.Time    { return s.createdAt }
func (s *Step) UpdatedAt() time.Time    { return s.updatedAt }
