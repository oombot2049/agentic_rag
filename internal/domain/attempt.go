package domain

import "time"

type AttemptStatus string

const (
	AttemptStatusStarted   AttemptStatus = "STARTED"
	AttemptStatusSucceeded AttemptStatus = "SUCCEEDED"
	AttemptStatusFailed    AttemptStatus = "FAILED"
	AttemptStatusUnknown   AttemptStatus = "UNKNOWN"
)

func (s AttemptStatus) String() string { return string(s) }

func (s AttemptStatus) valid() bool {
	return s == AttemptStatusStarted || s == AttemptStatusSucceeded || s == AttemptStatusFailed || s == AttemptStatusUnknown
}

func (s AttemptStatus) Terminal() bool { return s != AttemptStatusStarted && s.valid() }

// Attempt is one append-only physical execution try for a Step.
type Attempt struct {
	id        AttemptID
	tenantID  TenantID
	stepID    StepID
	attemptNo uint64
	status    AttemptStatus
	startedAt time.Time
	endedAt   time.Time
}

type AttemptSnapshot struct {
	ID        AttemptID
	TenantID  TenantID
	StepID    StepID
	AttemptNo uint64
	Status    AttemptStatus
	StartedAt time.Time
	EndedAt   time.Time
}

func NewAttempt(id AttemptID, tenantID TenantID, stepID StepID, attemptNo uint64, startedAt time.Time) (*Attempt, error) {
	return RestoreAttempt(AttemptSnapshot{
		ID: id, TenantID: tenantID, StepID: stepID, AttemptNo: attemptNo,
		Status: AttemptStatusStarted, StartedAt: startedAt,
	})
}

func RestoreAttempt(snapshot AttemptSnapshot) (*Attempt, error) {
	if err := validateAttemptID(snapshot.ID); err != nil {
		return nil, err
	}
	if err := validateTenantID(snapshot.TenantID); err != nil {
		return nil, err
	}
	if err := validateStepID(snapshot.StepID); err != nil {
		return nil, err
	}
	if err := validatePositiveInt64("attempt_no", snapshot.AttemptNo); err != nil {
		return nil, err
	}
	if !snapshot.Status.valid() {
		return nil, invalid("attempt_status", "is unknown")
	}
	startedAt, err := normalizeTime("started_at", snapshot.StartedAt)
	if err != nil {
		return nil, err
	}
	var endedAt time.Time
	if snapshot.Status.Terminal() {
		endedAt, err = normalizeTime("ended_at", snapshot.EndedAt)
		if err != nil {
			return nil, err
		}
		if endedAt.Before(startedAt) {
			return nil, invalid("ended_at", "must not precede started_at")
		}
	} else if !snapshot.EndedAt.IsZero() {
		return nil, invalid("ended_at", "must be zero while attempt is started")
	}
	return &Attempt{
		id: snapshot.ID, tenantID: snapshot.TenantID, stepID: snapshot.StepID,
		attemptNo: snapshot.AttemptNo, status: snapshot.Status,
		startedAt: startedAt, endedAt: endedAt,
	}, nil
}

func (a *Attempt) Succeed(at time.Time) error     { return a.finish(AttemptStatusSucceeded, at) }
func (a *Attempt) Fail(at time.Time) error        { return a.finish(AttemptStatusFailed, at) }
func (a *Attempt) MarkUnknown(at time.Time) error { return a.finish(AttemptStatusUnknown, at) }

func (a *Attempt) finish(target AttemptStatus, at time.Time) error {
	if a.status != AttemptStatusStarted {
		return invalidTransition("attempt", a.status, target)
	}
	endedAt, err := normalizeTime("ended_at", at)
	if err != nil {
		return err
	}
	if endedAt.Before(a.startedAt) {
		return invalid("ended_at", "must not precede started_at")
	}
	a.status = target
	a.endedAt = endedAt
	return nil
}

func (a *Attempt) ID() AttemptID              { return a.id }
func (a *Attempt) TenantID() TenantID         { return a.tenantID }
func (a *Attempt) StepID() StepID             { return a.stepID }
func (a *Attempt) AttemptNo() uint64          { return a.attemptNo }
func (a *Attempt) Status() AttemptStatus      { return a.status }
func (a *Attempt) Terminal() bool             { return a.status.Terminal() }
func (a *Attempt) StartedAt() time.Time       { return a.startedAt }
func (a *Attempt) EndedAt() (time.Time, bool) { return a.endedAt, !a.endedAt.IsZero() }
