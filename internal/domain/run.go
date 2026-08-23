package domain

import "time"

type RunStatus string

const (
	RunStatusQueued          RunStatus = "QUEUED"
	RunStatusRunning         RunStatus = "RUNNING"
	RunStatusWaitingApproval RunStatus = "WAITING_APPROVAL"
	RunStatusCancelRequested RunStatus = "CANCEL_REQUESTED"
	RunStatusSucceeded       RunStatus = "SUCCEEDED"
	RunStatusSelected        RunStatus = "SELECTED"
	RunStatusSuperseded      RunStatus = "SUPERSEDED"
	RunStatusFailed          RunStatus = "FAILED"
	RunStatusCancelled       RunStatus = "CANCELLED"
	RunStatusTimedOut        RunStatus = "TIMED_OUT"
)

func (s RunStatus) String() string { return string(s) }

func (s RunStatus) valid() bool {
	switch s {
	case RunStatusQueued, RunStatusRunning, RunStatusWaitingApproval, RunStatusCancelRequested, RunStatusSucceeded,
		RunStatusSelected, RunStatusSuperseded, RunStatusFailed, RunStatusCancelled, RunStatusTimedOut:
		return true
	default:
		return false
	}
}

func (s RunStatus) Terminal() bool {
	return s == RunStatusSelected || s == RunStatusSuperseded || s == RunStatusFailed ||
		s == RunStatusCancelled || s == RunStatusTimedOut
}

// Run is one complete Agent execution candidate for a Turn.
type Run struct {
	id                 RunID
	tenantID           TenantID
	sessionID          SessionID
	turnID             TurnID
	runNo              uint64
	baseContextVersion uint64
	status             RunStatus
	outputMessageID    MessageID
	createdAt          time.Time
	updatedAt          time.Time
}

type RunSnapshot struct {
	ID                 RunID
	TenantID           TenantID
	SessionID          SessionID
	TurnID             TurnID
	RunNo              uint64
	BaseContextVersion uint64
	Status             RunStatus
	OutputMessageID    MessageID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func NewRun(id RunID, tenantID TenantID, sessionID SessionID, turnID TurnID, runNo, baseVersion uint64, createdAt time.Time) (*Run, error) {
	return RestoreRun(RunSnapshot{
		ID: id, TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
		RunNo: runNo, BaseContextVersion: baseVersion, Status: RunStatusQueued,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	})
}

func RestoreRun(snapshot RunSnapshot) (*Run, error) {
	if err := validateRunID(snapshot.ID); err != nil {
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
	if err := validatePositiveInt64("run_no", snapshot.RunNo); err != nil {
		return nil, err
	}
	if err := validateNonNegativeInt64("base_context_version", snapshot.BaseContextVersion); err != nil {
		return nil, err
	}
	if !snapshot.Status.valid() {
		return nil, invalid("run_status", "is unknown")
	}
	if snapshot.OutputMessageID != "" {
		if err := validateMessageID(snapshot.OutputMessageID); err != nil {
			return nil, err
		}
	}
	if (snapshot.Status == RunStatusSucceeded || snapshot.Status == RunStatusSelected || snapshot.Status == RunStatusSuperseded) && snapshot.OutputMessageID == "" {
		return nil, invalid("output_message_id", "is required after run success")
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
	return &Run{
		id: snapshot.ID, tenantID: snapshot.TenantID, sessionID: snapshot.SessionID,
		turnID: snapshot.TurnID, runNo: snapshot.RunNo, baseContextVersion: snapshot.BaseContextVersion,
		status: snapshot.Status, outputMessageID: snapshot.OutputMessageID,
		createdAt: createdAt, updatedAt: updatedAt,
	}, nil
}

func (r *Run) Start(at time.Time) error               { return r.transition(RunStatusRunning, at) }
func (r *Run) WaitForApproval(at time.Time) error     { return r.transition(RunStatusWaitingApproval, at) }
func (r *Run) ResumeAfterApproval(at time.Time) error { return r.transition(RunStatusQueued, at) }
func (r *Run) RequestCancel(at time.Time) error       { return r.transition(RunStatusCancelRequested, at) }
func (r *Run) Cancel(at time.Time) error              { return r.transition(RunStatusCancelled, at) }
func (r *Run) Fail(at time.Time) error                { return r.transition(RunStatusFailed, at) }
func (r *Run) Timeout(at time.Time) error             { return r.transition(RunStatusTimedOut, at) }
func (r *Run) Select(at time.Time) error              { return r.transition(RunStatusSelected, at) }
func (r *Run) Supersede(at time.Time) error           { return r.transition(RunStatusSuperseded, at) }

func (r *Run) Succeed(outputMessageID MessageID, at time.Time) error {
	if r.status != RunStatusRunning {
		return invalidTransition("run", r.status, RunStatusSucceeded)
	}
	if err := validateMessageID(outputMessageID); err != nil {
		return err
	}
	updatedAt, err := r.validMutationTime(at)
	if err != nil {
		return err
	}
	r.status = RunStatusSucceeded
	r.outputMessageID = outputMessageID
	r.updatedAt = updatedAt
	return nil
}

func (r *Run) transition(target RunStatus, at time.Time) error {
	if !validRunTransition(r.status, target) {
		return invalidTransition("run", r.status, target)
	}
	updatedAt, err := r.validMutationTime(at)
	if err != nil {
		return err
	}
	r.status = target
	r.updatedAt = updatedAt
	return nil
}

func validRunTransition(from, to RunStatus) bool {
	switch from {
	case RunStatusQueued:
		return to == RunStatusRunning || to == RunStatusCancelled || to == RunStatusTimedOut
	case RunStatusRunning:
		return to == RunStatusWaitingApproval || to == RunStatusCancelRequested || to == RunStatusFailed || to == RunStatusTimedOut
	case RunStatusWaitingApproval:
		return to == RunStatusQueued || to == RunStatusFailed || to == RunStatusCancelled || to == RunStatusTimedOut
	case RunStatusCancelRequested:
		return to == RunStatusCancelled
	case RunStatusSucceeded:
		return to == RunStatusSelected || to == RunStatusSuperseded
	default:
		return false
	}
}

func (r *Run) validMutationTime(at time.Time) (time.Time, error) {
	normalized, err := normalizeTime("updated_at", at)
	if err != nil {
		return time.Time{}, err
	}
	if normalized.Before(r.updatedAt) {
		return time.Time{}, invalid("updated_at", "must not move backwards")
	}
	return normalized, nil
}

func (r *Run) ID() RunID                          { return r.id }
func (r *Run) TenantID() TenantID                 { return r.tenantID }
func (r *Run) SessionID() SessionID               { return r.sessionID }
func (r *Run) TurnID() TurnID                     { return r.turnID }
func (r *Run) RunNo() uint64                      { return r.runNo }
func (r *Run) BaseContextVersion() uint64         { return r.baseContextVersion }
func (r *Run) Status() RunStatus                  { return r.status }
func (r *Run) Terminal() bool                     { return r.status.Terminal() }
func (r *Run) OutputMessageID() (MessageID, bool) { return r.outputMessageID, r.outputMessageID != "" }
func (r *Run) CreatedAt() time.Time               { return r.createdAt }
func (r *Run) UpdatedAt() time.Time               { return r.updatedAt }
