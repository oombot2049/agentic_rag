package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const (
	MinimumLeaseDuration  = 5 * time.Second
	MaximumLeaseDuration  = 5 * time.Minute
	maximumCheckpointSize = 1 << 20
)

var (
	ErrLeaseHeld          = errors.New("run lease is held")
	ErrLeaseFenced        = errors.New("run lease is fenced")
	ErrLeaseExpired       = errors.New("run lease is expired")
	ErrRowConflict        = errors.New("execution row version conflict")
	ErrCheckpointConflict = errors.New("checkpoint cannot move backwards")
)

type Execution struct{ db DBTX }

func NewExecution(db DBTX) (*Execution, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: database executor is nil", ErrInvalidArgument)
	}
	return &Execution{db: db}, nil
}

type Lease struct {
	Owner     string
	Token     uint64
	ExpiresAt time.Time
}

type RunRecord struct {
	Run              *domain.Run
	MaxSteps         int
	ExecutionVersion uint64
	Lease            *Lease
}

func (r *Execution) InsertRun(ctx context.Context, value *domain.Run) error {
	return r.InsertRunWithOptions(ctx, value, 12)
}

// InsertRunWithOptions persists the normalized execution policy alongside the Run.
func (r *Execution) InsertRunWithOptions(ctx context.Context, value *domain.Run, maxSteps int) error {
	if value == nil {
		return fmt.Errorf("%w: run is nil", ErrInvalidArgument)
	}
	if maxSteps < 1 || maxSteps > 20 {
		return fmt.Errorf("%w: max steps must be between 1 and 20", ErrInvalidArgument)
	}
	outputMessageID, hasOutput := value.OutputMessageID()
	_, err := r.db.ExecContext(ctx, insertRunSQL,
		value.TenantID().String(), value.ID().String(), value.SessionID().String(), value.TurnID().String(),
		value.RunNo(), value.BaseContextVersion(), value.Status().String(), optionalString(outputMessageID.String(), hasOutput),
		value.CreatedAt(), value.UpdatedAt(), maxSteps,
	)
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

func (r *Execution) GetRun(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (RunRecord, error) {
	if err := validateTenantAndRun(tenantID, runID); err != nil {
		return RunRecord{}, err
	}
	record, err := scanRunRecord(r.db.QueryRowContext(ctx, selectRunSQL, tenantID.String(), runID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, ErrNotFound
	}
	if err != nil {
		return RunRecord{}, fmt.Errorf("select run: %w", err)
	}
	return record, nil
}

type AcquireLeaseCommand struct {
	TenantID                 domain.TenantID
	RunID                    domain.RunID
	Owner                    string
	ExpectedExecutionVersion uint64
	At                       time.Time
	Duration                 time.Duration
}

func (r *Execution) AcquireLease(ctx context.Context, command AcquireLeaseCommand) (RunRecord, error) {
	if err := validateAcquire(command); err != nil {
		return RunRecord{}, err
	}
	expiresAt := command.At.UTC().Add(command.Duration)
	record, err := scanRunRecord(r.db.QueryRowContext(ctx, acquireLeaseSQL,
		command.TenantID.String(), command.RunID.String(), command.Owner,
		command.ExpectedExecutionVersion, command.At.UTC(), expiresAt))
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, fmt.Errorf("acquire run lease: %w", err)
	}
	current, loadErr := r.GetRun(ctx, command.TenantID, command.RunID)
	if loadErr != nil {
		return RunRecord{}, loadErr
	}
	if current.ExecutionVersion != command.ExpectedExecutionVersion {
		return RunRecord{}, ErrRowConflict
	}
	if current.Lease != nil && current.Lease.ExpiresAt.After(command.At.UTC()) {
		return RunRecord{}, ErrLeaseHeld
	}
	return RunRecord{}, ErrRowConflict
}

type LeaseMutationCommand struct {
	TenantID                 domain.TenantID
	RunID                    domain.RunID
	Owner                    string
	Token                    uint64
	ExpectedExecutionVersion uint64
	At                       time.Time
	Duration                 time.Duration
}

func (r *Execution) RenewLease(ctx context.Context, command LeaseMutationCommand) (RunRecord, error) {
	if err := validateLeaseMutation(command, true); err != nil {
		return RunRecord{}, err
	}
	record, err := scanRunRecord(r.db.QueryRowContext(ctx, renewLeaseSQL,
		command.TenantID.String(), command.RunID.String(), command.Owner, command.Token,
		command.ExpectedExecutionVersion, command.At.UTC(), command.At.UTC().Add(command.Duration)))
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, fmt.Errorf("renew run lease: %w", err)
	}
	return RunRecord{}, r.classifyLeaseMutation(ctx, command)
}

func (r *Execution) ReleaseLease(ctx context.Context, command LeaseMutationCommand) (RunRecord, error) {
	if err := validateLeaseMutation(command, false); err != nil {
		return RunRecord{}, err
	}
	record, err := scanRunRecord(r.db.QueryRowContext(ctx, releaseLeaseSQL,
		command.TenantID.String(), command.RunID.String(), command.Owner, command.Token,
		command.ExpectedExecutionVersion, command.At.UTC()))
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, fmt.Errorf("release run lease: %w", err)
	}
	return RunRecord{}, r.classifyLeaseMutation(ctx, command)
}

func (r *Execution) classifyLeaseMutation(ctx context.Context, command LeaseMutationCommand) error {
	current, err := r.GetRun(ctx, command.TenantID, command.RunID)
	if err != nil {
		return err
	}
	if current.Lease == nil || current.Lease.Owner != command.Owner || current.Lease.Token != command.Token {
		return ErrLeaseFenced
	}
	if !current.Lease.ExpiresAt.After(command.At.UTC()) {
		return ErrLeaseExpired
	}
	if current.ExecutionVersion != command.ExpectedExecutionVersion {
		return ErrRowConflict
	}
	return ErrRowConflict
}

type WorkerAuthority struct {
	TenantID                 domain.TenantID
	RunID                    domain.RunID
	Owner                    string
	Token                    uint64
	ExpectedExecutionVersion uint64
	At                       time.Time
}

func (r *Execution) InsertStep(ctx context.Context, authority WorkerAuthority, step *domain.Step) error {
	if err := validateAuthority(authority); err != nil {
		return err
	}
	if step == nil || step.TenantID() != authority.TenantID || step.RunID() != authority.RunID {
		return fmt.Errorf("%w: step ownership", ErrInvalidArgument)
	}
	result, err := r.db.ExecContext(ctx, insertStepSQL,
		authority.TenantID.String(), step.ID().String(), authority.RunID.String(), step.StepNo(), string(step.Kind()), step.Status().String(),
		step.CreatedAt(), step.UpdatedAt(), authority.Owner, authority.Token, authority.ExpectedExecutionVersion, authority.At.UTC())
	if err != nil {
		return fmt.Errorf("insert step: %w", err)
	}
	return requireOneAffected(result)
}

type StepTransitionCommand struct {
	Authority          WorkerAuthority
	StepID             domain.StepID
	ExpectedRowVersion uint64
	Target             domain.StepStatus
	At                 time.Time
}

type StepRecord struct {
	Step       *domain.Step
	RowVersion uint64
}

func (r *Execution) GetStep(ctx context.Context, tenantID domain.TenantID, stepID domain.StepID) (StepRecord, error) {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return StepRecord{}, err
	}
	if _, err := domain.ParseStepID(stepID.String()); err != nil {
		return StepRecord{}, err
	}
	record, err := scanStepRecord(r.db.QueryRowContext(ctx, selectStepSQL, tenantID.String(), stepID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return StepRecord{}, ErrNotFound
	}
	if err != nil {
		return StepRecord{}, fmt.Errorf("select step: %w", err)
	}
	rows, err := r.db.QueryContext(ctx, selectStepAttemptsSQL, tenantID.String(), stepID.String())
	if err != nil {
		return StepRecord{}, fmt.Errorf("select step attempts: %w", err)
	}
	defer rows.Close()
	var attemptIDs []domain.AttemptID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return StepRecord{}, err
		}
		id, err := domain.ParseAttemptID(raw)
		if err != nil {
			return StepRecord{}, err
		}
		attemptIDs = append(attemptIDs, id)
	}
	if err := rows.Err(); err != nil {
		return StepRecord{}, err
	}
	snapshot := domain.StepSnapshot{
		ID: record.Step.ID(), TenantID: record.Step.TenantID(), RunID: record.Step.RunID(), StepNo: record.Step.StepNo(),
		Kind: record.Step.Kind(), Status: record.Step.Status(), AttemptIDs: attemptIDs,
		CreatedAt: record.Step.CreatedAt(), UpdatedAt: record.Step.UpdatedAt(),
	}
	record.Step, err = domain.RestoreStep(snapshot)
	return record, err
}

func (r *Execution) TransitionStep(ctx context.Context, command StepTransitionCommand) (StepRecord, error) {
	if err := validateAuthority(command.Authority); err != nil {
		return StepRecord{}, err
	}
	if _, err := domain.ParseStepID(command.StepID.String()); err != nil {
		return StepRecord{}, err
	}
	if command.ExpectedRowVersion == 0 || command.At.IsZero() {
		return StepRecord{}, fmt.Errorf("%w: step version/time", ErrInvalidArgument)
	}
	record, err := scanStepRecord(r.db.QueryRowContext(ctx, transitionStepSQL,
		command.Authority.TenantID.String(), command.StepID.String(), command.Authority.RunID.String(),
		command.ExpectedRowVersion, command.Target.String(), command.At.UTC(), command.Authority.Owner,
		command.Authority.Token, command.Authority.ExpectedExecutionVersion, command.At.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return StepRecord{}, ErrRowConflict
	}
	if err != nil {
		return StepRecord{}, fmt.Errorf("transition step: %w", err)
	}
	return record, nil
}

func (r *Execution) InsertAttempt(ctx context.Context, authority WorkerAuthority, attempt *domain.Attempt) error {
	if err := validateAuthority(authority); err != nil {
		return err
	}
	if attempt == nil || attempt.TenantID() != authority.TenantID {
		return fmt.Errorf("%w: attempt ownership", ErrInvalidArgument)
	}
	result, err := r.db.ExecContext(ctx, insertAttemptSQL,
		authority.TenantID.String(), attempt.ID().String(), attempt.StepID().String(), attempt.AttemptNo(),
		attempt.Status().String(), attempt.StartedAt(), authority.RunID.String(), authority.Owner, authority.Token,
		authority.ExpectedExecutionVersion, authority.At.UTC())
	if err != nil {
		return fmt.Errorf("insert attempt: %w", err)
	}
	return requireOneAffected(result)
}

type AttemptFinishCommand struct {
	Authority          WorkerAuthority
	AttemptID          domain.AttemptID
	ExpectedRowVersion uint64
	Target             domain.AttemptStatus
	At                 time.Time
}

type AttemptRecord struct {
	Attempt    *domain.Attempt
	RowVersion uint64
}

func (r *Execution) GetAttempt(ctx context.Context, tenantID domain.TenantID, attemptID domain.AttemptID) (AttemptRecord, error) {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return AttemptRecord{}, err
	}
	if _, err := domain.ParseAttemptID(attemptID.String()); err != nil {
		return AttemptRecord{}, err
	}
	record, err := scanAttemptRecord(r.db.QueryRowContext(ctx, selectAttemptSQL, tenantID.String(), attemptID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return AttemptRecord{}, ErrNotFound
	}
	if err != nil {
		return AttemptRecord{}, fmt.Errorf("select attempt: %w", err)
	}
	return record, nil
}

func (r *Execution) FinishAttempt(ctx context.Context, command AttemptFinishCommand) (AttemptRecord, error) {
	if err := validateAuthority(command.Authority); err != nil {
		return AttemptRecord{}, err
	}
	if _, err := domain.ParseAttemptID(command.AttemptID.String()); err != nil {
		return AttemptRecord{}, err
	}
	if command.ExpectedRowVersion == 0 || command.At.IsZero() || !command.Target.Terminal() {
		return AttemptRecord{}, fmt.Errorf("%w: attempt finish", ErrInvalidArgument)
	}
	record, err := scanAttemptRecord(r.db.QueryRowContext(ctx, finishAttemptSQL,
		command.Authority.TenantID.String(), command.AttemptID.String(), command.Authority.RunID.String(),
		command.ExpectedRowVersion, command.Target.String(), command.At.UTC(), command.Authority.Owner,
		command.Authority.Token, command.Authority.ExpectedExecutionVersion, command.At.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return AttemptRecord{}, ErrRowConflict
	}
	if err != nil {
		return AttemptRecord{}, fmt.Errorf("finish attempt: %w", err)
	}
	return record, nil
}

type Checkpoint struct {
	TenantID   domain.TenantID
	RunID      domain.RunID
	LeaseToken uint64
	StepNo     uint64
	Payload    json.RawMessage
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (r *Execution) SaveCheckpoint(ctx context.Context, authority WorkerAuthority, stepNo uint64, payload json.RawMessage) error {
	if err := validateAuthority(authority); err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > maximumCheckpointSize || !json.Valid(payload) {
		return fmt.Errorf("%w: checkpoint payload", ErrInvalidArgument)
	}
	result, err := r.db.ExecContext(ctx, saveCheckpointSQL,
		authority.TenantID.String(), authority.RunID.String(), authority.Token, stepNo, string(payload),
		authority.At.UTC(), authority.Owner, authority.ExpectedExecutionVersion)
	if err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	current, loadErr := r.LoadCheckpoint(ctx, authority.TenantID, authority.RunID)
	if loadErr == nil && current.StepNo > stepNo {
		return ErrCheckpointConflict
	}
	return ErrLeaseFenced
}

func (r *Execution) LoadCheckpoint(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (Checkpoint, error) {
	if err := validateTenantAndRun(tenantID, runID); err != nil {
		return Checkpoint{}, err
	}
	var result Checkpoint
	var rawTenant, rawRun string
	err := r.db.QueryRowContext(ctx, loadCheckpointSQL, tenantID.String(), runID.String()).Scan(
		&rawTenant, &rawRun, &result.LeaseToken, &result.StepNo, &result.Payload, &result.CreatedAt, &result.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, ErrNotFound
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("load checkpoint: %w", err)
	}
	var parseErr error
	if result.TenantID, parseErr = domain.ParseTenantID(rawTenant); parseErr != nil {
		return Checkpoint{}, parseErr
	}
	if result.RunID, parseErr = domain.ParseRunID(rawRun); parseErr != nil {
		return Checkpoint{}, parseErr
	}
	result.Payload = append(json.RawMessage(nil), result.Payload...)
	result.CreatedAt, result.UpdatedAt = result.CreatedAt.UTC(), result.UpdatedAt.UTC()
	return result, nil
}

func requireOneAffected(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLeaseFenced
	}
	return nil
}

func validateAcquire(command AcquireLeaseCommand) error {
	if err := validateTenantAndRun(command.TenantID, command.RunID); err != nil {
		return err
	}
	if invalidWorker(command.Owner) || command.ExpectedExecutionVersion == 0 || command.At.IsZero() || command.Duration < MinimumLeaseDuration || command.Duration > MaximumLeaseDuration {
		return fmt.Errorf("%w: lease acquisition", ErrInvalidArgument)
	}
	return nil
}

func validateLeaseMutation(command LeaseMutationCommand, requireDuration bool) error {
	if err := validateTenantAndRun(command.TenantID, command.RunID); err != nil {
		return err
	}
	if invalidWorker(command.Owner) || command.Token == 0 || command.ExpectedExecutionVersion == 0 || command.At.IsZero() {
		return fmt.Errorf("%w: lease mutation", ErrInvalidArgument)
	}
	if requireDuration && (command.Duration < MinimumLeaseDuration || command.Duration > MaximumLeaseDuration) {
		return fmt.Errorf("%w: lease duration", ErrInvalidArgument)
	}
	return nil
}

func validateAuthority(authority WorkerAuthority) error {
	return validateLeaseMutation(LeaseMutationCommand{
		TenantID: authority.TenantID, RunID: authority.RunID, Owner: authority.Owner, Token: authority.Token,
		ExpectedExecutionVersion: authority.ExpectedExecutionVersion, At: authority.At,
	}, false)
}

func validateTenantAndRun(tenantID domain.TenantID, runID domain.RunID) error {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return err
	}
	_, err := domain.ParseRunID(runID.String())
	return err
}

func invalidWorker(value string) bool {
	return len(value) < 1 || len(value) > 256 || strings.IndexFunc(value, unicode.IsControl) >= 0
}

type executionRowScanner interface{ Scan(...any) error }

func scanRunRecord(row executionRowScanner) (RunRecord, error) {
	var snapshot domain.RunSnapshot
	var tenant, run, session, turn, status string
	var output, owner sql.NullString
	var token uint64
	var expires sql.NullTime
	var result RunRecord
	err := row.Scan(&tenant, &run, &session, &turn, &snapshot.RunNo, &snapshot.BaseContextVersion,
		&status, &output, &snapshot.CreatedAt, &snapshot.UpdatedAt, &result.MaxSteps,
		&result.ExecutionVersion, &owner, &token, &expires)
	if err != nil {
		return RunRecord{}, err
	}
	if result.MaxSteps < 1 || result.MaxSteps > 20 {
		return RunRecord{}, errors.New("stored max_steps is invalid")
	}
	var parseErr error
	if snapshot.TenantID, parseErr = domain.ParseTenantID(tenant); parseErr != nil {
		return RunRecord{}, parseErr
	}
	if snapshot.ID, parseErr = domain.ParseRunID(run); parseErr != nil {
		return RunRecord{}, parseErr
	}
	if snapshot.SessionID, parseErr = domain.ParseSessionID(session); parseErr != nil {
		return RunRecord{}, parseErr
	}
	if snapshot.TurnID, parseErr = domain.ParseTurnID(turn); parseErr != nil {
		return RunRecord{}, parseErr
	}
	if output.Valid {
		if snapshot.OutputMessageID, parseErr = domain.ParseMessageID(output.String); parseErr != nil {
			return RunRecord{}, parseErr
		}
	}
	snapshot.Status = domain.RunStatus(status)
	result.Run, parseErr = domain.RestoreRun(snapshot)
	if parseErr != nil {
		return RunRecord{}, parseErr
	}
	if owner.Valid {
		if !expires.Valid || token == 0 {
			return RunRecord{}, errors.New("stored lease shape is invalid")
		}
		result.Lease = &Lease{Owner: owner.String, Token: token, ExpiresAt: expires.Time.UTC()}
	}
	return result, nil
}

func scanStepRecord(row executionRowScanner) (StepRecord, error) {
	var snapshot domain.StepSnapshot
	var tenant, step, run, kind, status string
	var result StepRecord
	err := row.Scan(&tenant, &step, &run, &snapshot.StepNo, &kind, &status, &snapshot.CreatedAt, &snapshot.UpdatedAt, &result.RowVersion)
	if err != nil {
		return StepRecord{}, err
	}
	var parseErr error
	if snapshot.TenantID, parseErr = domain.ParseTenantID(tenant); parseErr != nil {
		return StepRecord{}, parseErr
	}
	if snapshot.ID, parseErr = domain.ParseStepID(step); parseErr != nil {
		return StepRecord{}, parseErr
	}
	if snapshot.RunID, parseErr = domain.ParseRunID(run); parseErr != nil {
		return StepRecord{}, parseErr
	}
	snapshot.Kind, snapshot.Status = domain.StepKind(kind), domain.StepStatus(status)
	result.Step, parseErr = domain.RestoreStep(snapshot)
	return result, parseErr
}

func scanAttemptRecord(row executionRowScanner) (AttemptRecord, error) {
	var snapshot domain.AttemptSnapshot
	var tenant, attempt, step, status string
	var ended sql.NullTime
	var result AttemptRecord
	err := row.Scan(&tenant, &attempt, &step, &snapshot.AttemptNo, &status, &snapshot.StartedAt, &ended, &result.RowVersion)
	if err != nil {
		return AttemptRecord{}, err
	}
	var parseErr error
	if snapshot.TenantID, parseErr = domain.ParseTenantID(tenant); parseErr != nil {
		return AttemptRecord{}, parseErr
	}
	if snapshot.ID, parseErr = domain.ParseAttemptID(attempt); parseErr != nil {
		return AttemptRecord{}, parseErr
	}
	if snapshot.StepID, parseErr = domain.ParseStepID(step); parseErr != nil {
		return AttemptRecord{}, parseErr
	}
	snapshot.Status = domain.AttemptStatus(status)
	if ended.Valid {
		snapshot.EndedAt = ended.Time
	}
	result.Attempt, parseErr = domain.RestoreAttempt(snapshot)
	return result, parseErr
}

const runColumns = `tenant_id,id,session_id,turn_id,run_no,base_context_version,status,
    output_message_id,created_at,updated_at,max_steps,execution_version,lease_owner,lease_token,lease_expires_at`
const insertRunSQL = `INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,
    status,output_message_id,created_at,updated_at,max_steps) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
const selectRunSQL = `SELECT ` + runColumns + ` FROM runs WHERE tenant_id=$1 AND id=$2`
const acquireLeaseSQL = `UPDATE runs SET lease_owner=$3,lease_token=lease_token+1,lease_expires_at=$6,
    execution_version=execution_version+1,updated_at=$5
WHERE tenant_id=$1 AND id=$2 AND execution_version=$4 AND (lease_owner IS NULL OR lease_expires_at <= $5)
    AND status IN ('QUEUED','RUNNING','CANCEL_REQUESTED','SUCCEEDED') AND execution_version < 9223372036854775807
RETURNING ` + runColumns
const renewLeaseSQL = `UPDATE runs SET lease_expires_at=$7,execution_version=execution_version+1,updated_at=$6
WHERE tenant_id=$1 AND id=$2 AND lease_owner=$3 AND lease_token=$4 AND execution_version=$5
    AND lease_expires_at > $6 AND execution_version < 9223372036854775807
RETURNING ` + runColumns
const releaseLeaseSQL = `UPDATE runs SET lease_owner=NULL,lease_expires_at=NULL,execution_version=execution_version+1,updated_at=$6
WHERE tenant_id=$1 AND id=$2 AND lease_owner=$3 AND lease_token=$4 AND execution_version=$5
    AND lease_expires_at > $6 AND execution_version < 9223372036854775807
RETURNING ` + runColumns

const insertStepSQL = `INSERT INTO steps(tenant_id,id,run_id,step_no,kind,status,created_at,updated_at)
SELECT $1,$2,$3,$4,$5,$6,$7,$8 FROM runs
WHERE tenant_id=$1 AND id=$3 AND lease_owner=$9 AND lease_token=$10 AND execution_version=$11 AND lease_expires_at > $12`
const selectStepSQL = `SELECT tenant_id,id,run_id,step_no,kind,status,created_at,updated_at,row_version
FROM steps WHERE tenant_id=$1 AND id=$2`
const selectStepAttemptsSQL = `SELECT id FROM attempts WHERE tenant_id=$1 AND step_id=$2 ORDER BY attempt_no`
const transitionStepSQL = `UPDATE steps SET status=$5,row_version=row_version+1,updated_at=$6
WHERE tenant_id=$1 AND id=$2 AND run_id=$3 AND row_version=$4 AND row_version < 9223372036854775807
    AND ((status='PENDING' AND $5 IN ('RUNNING','SKIPPED')) OR (status='RUNNING' AND $5 IN ('SUCCEEDED','FAILED','CANCELLED')))
    AND EXISTS (SELECT 1 FROM runs WHERE tenant_id=$1 AND id=$3 AND lease_owner=$7 AND lease_token=$8
        AND execution_version=$9 AND lease_expires_at > $10)
RETURNING tenant_id,id,run_id,step_no,kind,status,created_at,updated_at,row_version`
const insertAttemptSQL = `INSERT INTO attempts(tenant_id,id,step_id,attempt_no,status,started_at)
SELECT $1,$2,$3,$4,$5,$6 FROM steps s
WHERE s.tenant_id=$1 AND s.id=$3 AND s.run_id=$7 AND s.status='RUNNING'
    AND NOT EXISTS (SELECT 1 FROM attempts a WHERE a.tenant_id=$1 AND a.step_id=$3 AND a.status='STARTED')
    AND EXISTS (SELECT 1 FROM runs r WHERE r.tenant_id=$1 AND r.id=$7 AND r.lease_owner=$8
        AND r.lease_token=$9 AND r.execution_version=$10 AND r.lease_expires_at > $11)`
const selectAttemptSQL = `SELECT tenant_id,id,step_id,attempt_no,status,started_at,ended_at,row_version
FROM attempts WHERE tenant_id=$1 AND id=$2`
const finishAttemptSQL = `UPDATE attempts a SET status=$5,ended_at=$6,row_version=row_version+1
WHERE a.tenant_id=$1 AND a.id=$2 AND a.row_version=$4 AND a.status='STARTED' AND a.row_version < 9223372036854775807
    AND EXISTS (SELECT 1 FROM steps s JOIN runs r ON r.tenant_id=s.tenant_id AND r.id=s.run_id
        WHERE s.tenant_id=$1 AND s.id=a.step_id AND r.id=$3 AND r.lease_owner=$7 AND r.lease_token=$8
            AND r.execution_version=$9 AND r.lease_expires_at > $10)
RETURNING a.tenant_id,a.id,a.step_id,a.attempt_no,a.status,a.started_at,a.ended_at,a.row_version`
const saveCheckpointSQL = `INSERT INTO run_checkpoints(tenant_id,run_id,lease_token,step_no,payload,created_at,updated_at)
SELECT $1,$2,$3,$4,$5::jsonb,$6,$6 FROM runs
WHERE tenant_id=$1 AND id=$2 AND lease_owner=$7 AND lease_token=$3 AND execution_version=$8 AND lease_expires_at > $6
ON CONFLICT (tenant_id,run_id) DO UPDATE SET lease_token=EXCLUDED.lease_token,step_no=EXCLUDED.step_no,
    payload=EXCLUDED.payload,updated_at=EXCLUDED.updated_at
WHERE run_checkpoints.step_no <= EXCLUDED.step_no`
const loadCheckpointSQL = `SELECT tenant_id,run_id,lease_token,step_no,payload,created_at,updated_at
FROM run_checkpoints WHERE tenant_id=$1 AND run_id=$2`
