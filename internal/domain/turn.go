package domain

import "time"

type TurnStatus string

const (
	TurnStatusRunning   TurnStatus = "RUNNING"
	TurnStatusCompleted TurnStatus = "COMPLETED"
	TurnStatusFailed    TurnStatus = "FAILED"
	TurnStatusCancelled TurnStatus = "CANCELLED"
)

func (s TurnStatus) String() string { return string(s) }

func (s TurnStatus) valid() bool {
	return s == TurnStatusRunning || s == TurnStatusCompleted || s == TurnStatusFailed || s == TurnStatusCancelled
}

// Turn is one logical user question and its append-only Run candidates.
type Turn struct {
	id                 TurnID
	tenantID           TenantID
	sessionID          SessionID
	sequence           uint64
	baseContextVersion uint64
	userMessageID      MessageID
	status             TurnStatus
	runIDs             []RunID
	selectedRunID      RunID
	createdAt          time.Time
	updatedAt          time.Time
}

type TurnSnapshot struct {
	ID                 TurnID
	TenantID           TenantID
	SessionID          SessionID
	Sequence           uint64
	BaseContextVersion uint64
	UserMessageID      MessageID
	Status             TurnStatus
	RunIDs             []RunID
	SelectedRunID      RunID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func NewTurn(
	id TurnID,
	tenantID TenantID,
	sessionID SessionID,
	sequence uint64,
	baseContextVersion uint64,
	userMessageID MessageID,
	initialRunID RunID,
	createdAt time.Time,
) (*Turn, error) {
	return RestoreTurn(TurnSnapshot{
		ID: id, TenantID: tenantID, SessionID: sessionID, Sequence: sequence,
		BaseContextVersion: baseContextVersion, UserMessageID: userMessageID,
		Status: TurnStatusRunning, RunIDs: []RunID{initialRunID},
		CreatedAt: createdAt, UpdatedAt: createdAt,
	})
}

func RestoreTurn(snapshot TurnSnapshot) (*Turn, error) {
	if err := validateTurnID(snapshot.ID); err != nil {
		return nil, err
	}
	if err := validateTenantID(snapshot.TenantID); err != nil {
		return nil, err
	}
	if err := validateSessionID(snapshot.SessionID); err != nil {
		return nil, err
	}
	if err := validatePositiveInt64("turn_sequence", snapshot.Sequence); err != nil {
		return nil, err
	}
	if err := validateNonNegativeInt64("base_context_version", snapshot.BaseContextVersion); err != nil {
		return nil, err
	}
	if err := validateMessageID(snapshot.UserMessageID); err != nil {
		return nil, err
	}
	if !snapshot.Status.valid() {
		return nil, invalid("turn_status", "is unknown")
	}
	if len(snapshot.RunIDs) == 0 {
		return nil, invalid("run_ids", "must contain at least one run")
	}
	seen := make(map[RunID]struct{}, len(snapshot.RunIDs))
	for _, runID := range snapshot.RunIDs {
		if err := validateRunID(runID); err != nil {
			return nil, err
		}
		if _, exists := seen[runID]; exists {
			return nil, invalid("run_ids", "must be unique")
		}
		seen[runID] = struct{}{}
	}
	if snapshot.SelectedRunID != "" {
		if _, exists := seen[snapshot.SelectedRunID]; !exists {
			return nil, invalid("selected_run_id", "must be owned by the turn")
		}
	}
	if snapshot.Status == TurnStatusCompleted && snapshot.SelectedRunID == "" {
		return nil, invalid("selected_run_id", "is required for a completed turn")
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
	return &Turn{
		id: snapshot.ID, tenantID: snapshot.TenantID, sessionID: snapshot.SessionID,
		sequence: snapshot.Sequence, baseContextVersion: snapshot.BaseContextVersion,
		userMessageID: snapshot.UserMessageID, status: snapshot.Status,
		runIDs: copySlice(snapshot.RunIDs), selectedRunID: snapshot.SelectedRunID,
		createdAt: createdAt, updatedAt: updatedAt,
	}, nil
}

// StartRun reopens a terminal Turn for an explicit full retry/regeneration.
func (t *Turn) StartRun(run *Run, at time.Time) error {
	if t.status == TurnStatusRunning {
		return invalidTransition("turn", t.status, TurnStatusRunning)
	}
	if err := t.validateRun(run); err != nil {
		return err
	}
	if t.hasRun(run.id) {
		return invalid("run_id", "is already registered")
	}
	if run.runNo != uint64(len(t.runIDs)+1) || run.status != RunStatusQueued {
		return invalid("run", "must be the next queued run candidate")
	}
	updatedAt, err := t.validMutationTime(at)
	if err != nil {
		return err
	}
	t.runIDs = append(t.runIDs, run.id)
	t.status = TurnStatusRunning
	t.updatedAt = updatedAt
	return nil
}

func (t *Turn) Complete(run *Run, at time.Time) error {
	if t.status != TurnStatusRunning {
		return invalidTransition("turn", t.status, TurnStatusCompleted)
	}
	if err := t.validateRun(run); err != nil {
		return err
	}
	if !t.hasRun(run.id) || run.status != RunStatusSucceeded {
		return invalid("run", "must be an owned succeeded candidate")
	}
	updatedAt, err := t.validMutationTime(at)
	if err != nil {
		return err
	}
	t.status = TurnStatusCompleted
	t.selectedRunID = run.id
	t.updatedAt = updatedAt
	return nil
}

func (t *Turn) Fail(at time.Time) error   { return t.finish(TurnStatusFailed, at) }
func (t *Turn) Cancel(at time.Time) error { return t.finish(TurnStatusCancelled, at) }

func (t *Turn) finish(target TurnStatus, at time.Time) error {
	if t.status != TurnStatusRunning {
		return invalidTransition("turn", t.status, target)
	}
	updatedAt, err := t.validMutationTime(at)
	if err != nil {
		return err
	}
	t.status = target
	t.updatedAt = updatedAt
	return nil
}

func (t *Turn) validateRun(run *Run) error {
	if run == nil || run.tenantID != t.tenantID || run.sessionID != t.sessionID || run.turnID != t.id {
		return invalid("run", "does not belong to turn tenant, session and turn")
	}
	return nil
}

func (t *Turn) validMutationTime(at time.Time) (time.Time, error) {
	normalized, err := normalizeTime("updated_at", at)
	if err != nil {
		return time.Time{}, err
	}
	if normalized.Before(t.updatedAt) {
		return time.Time{}, invalid("updated_at", "must not move backwards")
	}
	return normalized, nil
}

func (t *Turn) hasRun(runID RunID) bool {
	for _, candidate := range t.runIDs {
		if candidate == runID {
			return true
		}
	}
	return false
}

func (t *Turn) ID() TurnID                   { return t.id }
func (t *Turn) TenantID() TenantID           { return t.tenantID }
func (t *Turn) SessionID() SessionID         { return t.sessionID }
func (t *Turn) Sequence() uint64             { return t.sequence }
func (t *Turn) BaseContextVersion() uint64   { return t.baseContextVersion }
func (t *Turn) UserMessageID() MessageID     { return t.userMessageID }
func (t *Turn) Status() TurnStatus           { return t.status }
func (t *Turn) RunIDs() []RunID              { return copySlice(t.runIDs) }
func (t *Turn) SelectedRunID() (RunID, bool) { return t.selectedRunID, t.selectedRunID != "" }
func (t *Turn) CreatedAt() time.Time         { return t.createdAt }
func (t *Turn) UpdatedAt() time.Time         { return t.updatedAt }
