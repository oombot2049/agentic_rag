package domain

import "time"

// Session is the linear-conversation aggregate and concurrency boundary.
type Session struct {
	id                    SessionID
	tenantID              TenantID
	contextVersion        uint64
	rowVersion            uint64
	nextTurnSequence      uint64
	committedTurnSequence uint64
	activeRunID           RunID
	headTurnID            TurnID
	createdAt             time.Time
	updatedAt             time.Time
}

// NewSession creates an empty Session. Row versions begin at one; sequence zero
// means no Turn has yet been committed.
func NewSession(id SessionID, tenantID TenantID, createdAt time.Time) (*Session, error) {
	return RestoreSession(SessionSnapshot{
		ID: id, TenantID: tenantID, RowVersion: 1, NextTurnSequence: 1,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	})
}

// SessionSnapshot is a persistence-neutral reconstitution value.
type SessionSnapshot struct {
	ID                    SessionID
	TenantID              TenantID
	ContextVersion        uint64
	RowVersion            uint64
	NextTurnSequence      uint64
	CommittedTurnSequence uint64
	ActiveRunID           RunID
	HeadTurnID            TurnID
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// RestoreSession validates and reconstitutes a Session without database types.
func RestoreSession(snapshot SessionSnapshot) (*Session, error) {
	if err := validateSessionID(snapshot.ID); err != nil {
		return nil, err
	}
	if err := validateTenantID(snapshot.TenantID); err != nil {
		return nil, err
	}
	if err := validateNonNegativeInt64("context_version", snapshot.ContextVersion); err != nil {
		return nil, err
	}
	if err := validatePositiveInt64("row_version", snapshot.RowVersion); err != nil {
		return nil, err
	}
	if err := validatePositiveInt64("next_turn_sequence", snapshot.NextTurnSequence); err != nil {
		return nil, err
	}
	if err := validateNonNegativeInt64("committed_turn_sequence", snapshot.CommittedTurnSequence); err != nil {
		return nil, err
	}
	if snapshot.NextTurnSequence <= snapshot.CommittedTurnSequence {
		return nil, invalid("next_turn_sequence", "must be positive and greater than committed sequence")
	}
	if snapshot.ActiveRunID != "" {
		if err := validateRunID(snapshot.ActiveRunID); err != nil {
			return nil, err
		}
	}
	if snapshot.HeadTurnID != "" {
		if err := validateTurnID(snapshot.HeadTurnID); err != nil {
			return nil, err
		}
	}
	if snapshot.CommittedTurnSequence == 0 && snapshot.HeadTurnID != "" ||
		snapshot.CommittedTurnSequence > 0 && snapshot.HeadTurnID == "" {
		return nil, invalid("head_turn_id", "must agree with committed turn sequence")
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
	return &Session{
		id: snapshot.ID, tenantID: snapshot.TenantID,
		contextVersion: snapshot.ContextVersion, rowVersion: snapshot.RowVersion,
		nextTurnSequence: snapshot.NextTurnSequence, committedTurnSequence: snapshot.CommittedTurnSequence,
		activeRunID: snapshot.ActiveRunID, headTurnID: snapshot.HeadTurnID,
		createdAt: createdAt, updatedAt: updatedAt,
	}, nil
}

// ReserveTurnRun checks context staleness before busy state and reserves a new Turn sequence.
func (s *Session) ReserveTurnRun(expectedContextVersion uint64, runID RunID, at time.Time) (uint64, error) {
	if expectedContextVersion != s.contextVersion {
		return 0, ErrVersionConflict
	}
	if s.activeRunID != "" {
		return 0, ErrSessionBusy
	}
	if err := validateRunID(runID); err != nil {
		return 0, err
	}
	if s.rowVersion == maxSignedInt64 || s.nextTurnSequence == maxSignedInt64 {
		return 0, invalid("session_version", "cannot advance beyond signed 64-bit range")
	}
	updatedAt, err := s.validMutationTime(at)
	if err != nil {
		return 0, err
	}
	sequence := s.nextTurnSequence
	s.nextTurnSequence++
	s.activeRunID = runID
	s.rowVersion++
	s.updatedAt = updatedAt
	return sequence, nil
}

// ReserveRegeneration reserves a new Run for the current committed head without
// consuming a new Turn sequence.
func (s *Session) ReserveRegeneration(expectedContextVersion uint64, turn *Turn, run *Run, at time.Time) error {
	if expectedContextVersion != s.contextVersion {
		return ErrVersionConflict
	}
	if s.activeRunID != "" {
		return ErrSessionBusy
	}
	if turn == nil || run == nil || turn.tenantID != s.tenantID || run.tenantID != s.tenantID ||
		turn.sessionID != s.id || run.sessionID != s.id || run.turnID != turn.id {
		return invalid("run", "turn and run must belong to the session tenant and session")
	}
	if s.headTurnID != turn.id || s.committedTurnSequence != turn.sequence {
		return invalid("turn", "regeneration is only allowed for the committed head")
	}
	if turn.status == TurnStatusRunning || run.status != RunStatusQueued ||
		run.runNo != uint64(len(turn.runIDs)+1) || run.baseContextVersion != s.contextVersion {
		return invalid("run", "must be the next queued candidate at the current context version")
	}
	if s.rowVersion == maxSignedInt64 {
		return invalid("row_version", "cannot advance beyond signed 64-bit range")
	}
	updatedAt, err := s.validMutationTime(at)
	if err != nil {
		return err
	}
	if _, err = turn.validMutationTime(at); err != nil {
		return err
	}
	s.activeRunID = run.id
	s.rowVersion++
	s.updatedAt = updatedAt
	turn.runIDs = append(turn.runIDs, run.id)
	turn.status = TurnStatusRunning
	turn.updatedAt = updatedAt
	return nil
}

// CommitRun atomically mutates the in-memory final-commit participants after a
// successful assistant candidate has been produced.
func (s *Session) CommitRun(turn *Turn, run *Run, message *Message, at time.Time) error {
	if err := s.validateOwnedRun(turn, run); err != nil {
		return err
	}
	if run.status != RunStatusSucceeded || turn.status != TurnStatusRunning {
		return invalid("run", "commit requires a succeeded run and running turn")
	}
	if run.outputMessageID == "" {
		return invalid("output_message_id", "is required for final commit")
	}
	if message == nil || message.tenantID != s.tenantID || message.sessionID != s.id ||
		message.turnID != turn.id || message.runID != run.id || message.id != run.outputMessageID ||
		message.role != MessageRoleAssistant || message.visibility != MessageVisibilityCandidate {
		return invalid("message", "must be the run's owned assistant candidate")
	}
	isNewTurn := turn.sequence > s.committedTurnSequence && turn.sequence < s.nextTurnSequence
	isRegeneration := turn.sequence == s.committedTurnSequence && turn.id == s.headTurnID
	if !isNewTurn && !isRegeneration {
		return invalid("turn_sequence", "is neither the active new turn nor committed head regeneration")
	}
	if s.contextVersion == maxSignedInt64 || s.rowVersion == maxSignedInt64 {
		return invalid("session_version", "cannot advance beyond signed 64-bit range")
	}
	updatedAt, err := s.validMutationTime(at)
	if err != nil {
		return err
	}
	if _, err = turn.validMutationTime(at); err != nil {
		return err
	}
	if _, err = run.validMutationTime(at); err != nil {
		return err
	}
	message.visibility = MessageVisibilityConversation
	turn.status = TurnStatusCompleted
	turn.selectedRunID = run.id
	turn.updatedAt = updatedAt
	run.status = RunStatusSelected
	run.updatedAt = updatedAt
	s.contextVersion++
	s.committedTurnSequence = turn.sequence
	s.headTurnID = turn.id
	s.activeRunID = ""
	s.rowVersion++
	s.updatedAt = updatedAt
	return nil
}

// ReleaseRun clears control ownership after a terminal unsuccessful Run.
func (s *Session) ReleaseRun(run *Run, at time.Time) error {
	if run == nil || run.tenantID != s.tenantID || run.sessionID != s.id {
		return invalid("run", "does not belong to session tenant and session")
	}
	if s.activeRunID != run.id {
		return ErrRunFenced
	}
	if run.status != RunStatusFailed && run.status != RunStatusCancelled && run.status != RunStatusTimedOut {
		return invalid("run", "release requires an unsuccessful terminal run")
	}
	if s.rowVersion == maxSignedInt64 {
		return invalid("row_version", "cannot advance beyond signed 64-bit range")
	}
	updatedAt, err := s.validMutationTime(at)
	if err != nil {
		return err
	}
	s.activeRunID = ""
	s.rowVersion++
	s.updatedAt = updatedAt
	return nil
}

func (s *Session) validateOwnedRun(turn *Turn, run *Run) error {
	if turn == nil || run == nil || turn.tenantID != s.tenantID || run.tenantID != s.tenantID ||
		turn.sessionID != s.id || run.sessionID != s.id || run.turnID != turn.id {
		return invalid("run", "turn and run must belong to the session tenant and session")
	}
	if s.activeRunID != run.id {
		return ErrRunFenced
	}
	if !turn.hasRun(run.id) {
		return invalid("run_id", "is not registered by the turn")
	}
	return nil
}

func (s *Session) validMutationTime(at time.Time) (time.Time, error) {
	normalized, err := normalizeTime("updated_at", at)
	if err != nil {
		return time.Time{}, err
	}
	if normalized.Before(s.updatedAt) {
		return time.Time{}, invalid("updated_at", "must not move backwards")
	}
	return normalized, nil
}

func (s *Session) ID() SessionID                 { return s.id }
func (s *Session) TenantID() TenantID            { return s.tenantID }
func (s *Session) ContextVersion() uint64        { return s.contextVersion }
func (s *Session) RowVersion() uint64            { return s.rowVersion }
func (s *Session) NextTurnSequence() uint64      { return s.nextTurnSequence }
func (s *Session) CommittedTurnSequence() uint64 { return s.committedTurnSequence }
func (s *Session) ActiveRunID() (RunID, bool)    { return s.activeRunID, s.activeRunID != "" }
func (s *Session) HeadTurnID() (TurnID, bool)    { return s.headTurnID, s.headTurnID != "" }
func (s *Session) CreatedAt() time.Time          { return s.createdAt }
func (s *Session) UpdatedAt() time.Time          { return s.updatedAt }
