// Package session provides tenant-scoped Session persistence and concurrency control.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrNotFound        = errors.New("session not found")
	ErrVersionConflict = errors.New("session context version conflict")
	ErrBusy            = errors.New("session already has an active run")
	ErrCASConflict     = errors.New("session row version conflict")
	ErrRunFenced       = errors.New("run no longer owns session")
)

type DBTX interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Repository struct{ db DBTX }

func NewRepository(db DBTX) (*Repository, error) {
	if db == nil {
		return nil, errors.New("session repository requires database executor")
	}
	return &Repository{db: db}, nil
}

type ReserveTurnCommand struct {
	TenantID               domain.TenantID
	SessionID              domain.SessionID
	RunID                  domain.RunID
	ExpectedContextVersion uint64
	ExpectedRowVersion     uint64
	At                     time.Time
}

type Reservation struct {
	TurnSequence uint64
	Session      *domain.Session
}

func (r *Repository) Load(ctx context.Context, tenantID domain.TenantID, sessionID domain.SessionID) (*domain.Session, error) {
	if err := validateIdentity(tenantID, sessionID); err != nil {
		return nil, err
	}
	result, err := scanSession(r.db.QueryRowContext(ctx, loadSQL, tenantID.String(), sessionID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	return result, nil
}

func (r *Repository) ReserveTurn(ctx context.Context, command ReserveTurnCommand) (Reservation, error) {
	if err := validateReserve(command); err != nil {
		return Reservation{}, err
	}
	session, err := scanSession(r.db.QueryRowContext(ctx, reserveTurnSQL,
		command.TenantID.String(), command.SessionID.String(), command.RunID.String(),
		command.ExpectedContextVersion, command.ExpectedRowVersion, command.At.UTC()))
	if err == nil {
		return Reservation{TurnSequence: session.NextTurnSequence() - 1, Session: session}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, fmt.Errorf("reserve session turn: %w", err)
	}
	return Reservation{}, r.classifyReservation(ctx, command)
}

type ReserveRegenerationCommand struct {
	TenantID               domain.TenantID
	SessionID              domain.SessionID
	TurnID                 domain.TurnID
	RunID                  domain.RunID
	ExpectedContextVersion uint64
	ExpectedRowVersion     uint64
	At                     time.Time
}

func (r *Repository) ReserveRegeneration(ctx context.Context, command ReserveRegenerationCommand) (*domain.Session, error) {
	if err := validateRegeneration(command); err != nil {
		return nil, err
	}
	result, err := scanSession(r.db.QueryRowContext(ctx, reserveRegenerationSQL,
		command.TenantID.String(), command.SessionID.String(), command.TurnID.String(), command.RunID.String(),
		command.ExpectedContextVersion, command.ExpectedRowVersion, command.At.UTC()))
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reserve session regeneration: %w", err)
	}
	reserve := ReserveTurnCommand{
		TenantID: command.TenantID, SessionID: command.SessionID, RunID: command.RunID,
		ExpectedContextVersion: command.ExpectedContextVersion, ExpectedRowVersion: command.ExpectedRowVersion, At: command.At,
	}
	return nil, r.classifyReservation(ctx, reserve)
}

type CommitCommand struct {
	TenantID           domain.TenantID
	SessionID          domain.SessionID
	TurnID             domain.TurnID
	RunID              domain.RunID
	TurnSequence       uint64
	ExpectedRowVersion uint64
	At                 time.Time
}

func (r *Repository) CommitRun(ctx context.Context, command CommitCommand) (*domain.Session, error) {
	if err := validateCommit(command); err != nil {
		return nil, err
	}
	result, err := scanSession(r.db.QueryRowContext(ctx, commitSQL,
		command.TenantID.String(), command.SessionID.String(), command.TurnID.String(), command.RunID.String(),
		command.TurnSequence, command.ExpectedRowVersion, command.At.UTC()))
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("commit session run: %w", err)
	}
	return nil, r.classifyWorker(ctx, command.TenantID, command.SessionID, command.RunID, command.ExpectedRowVersion)
}

type ReleaseCommand struct {
	TenantID           domain.TenantID
	SessionID          domain.SessionID
	RunID              domain.RunID
	ExpectedRowVersion uint64
	At                 time.Time
}

func (r *Repository) ReleaseRun(ctx context.Context, command ReleaseCommand) (*domain.Session, error) {
	if err := validateRelease(command); err != nil {
		return nil, err
	}
	result, err := scanSession(r.db.QueryRowContext(ctx, releaseSQL,
		command.TenantID.String(), command.SessionID.String(), command.RunID.String(),
		command.ExpectedRowVersion, command.At.UTC()))
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("release session run: %w", err)
	}
	return nil, r.classifyWorker(ctx, command.TenantID, command.SessionID, command.RunID, command.ExpectedRowVersion)
}

func (r *Repository) classifyReservation(ctx context.Context, command ReserveTurnCommand) error {
	current, err := r.Load(ctx, command.TenantID, command.SessionID)
	if err != nil {
		return err
	}
	if current.ContextVersion() != command.ExpectedContextVersion {
		return ErrVersionConflict
	}
	if _, active := current.ActiveRunID(); active {
		return ErrBusy
	}
	if current.RowVersion() != command.ExpectedRowVersion {
		return ErrCASConflict
	}
	return ErrCASConflict
}

func (r *Repository) classifyWorker(ctx context.Context, tenantID domain.TenantID, sessionID domain.SessionID, runID domain.RunID, expectedRow uint64) error {
	current, err := r.Load(ctx, tenantID, sessionID)
	if err != nil {
		return err
	}
	active, exists := current.ActiveRunID()
	if !exists || active != runID {
		return ErrRunFenced
	}
	if current.RowVersion() != expectedRow {
		return ErrCASConflict
	}
	return ErrCASConflict
}

type rowScanner interface{ Scan(...any) error }

func scanSession(row rowScanner) (*domain.Session, error) {
	var snapshot domain.SessionSnapshot
	var tenantID, sessionID string
	var activeRunID, headTurnID sql.NullString
	err := row.Scan(
		&tenantID, &sessionID, &snapshot.ContextVersion, &snapshot.RowVersion,
		&snapshot.NextTurnSequence, &snapshot.CommittedTurnSequence,
		&activeRunID, &headTurnID, &snapshot.CreatedAt, &snapshot.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	parsedTenant, err := domain.ParseTenantID(tenantID)
	if err != nil {
		return nil, err
	}
	parsedSession, err := domain.ParseSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	snapshot.TenantID, snapshot.ID = parsedTenant, parsedSession
	if activeRunID.Valid {
		snapshot.ActiveRunID, err = domain.ParseRunID(activeRunID.String)
		if err != nil {
			return nil, err
		}
	}
	if headTurnID.Valid {
		snapshot.HeadTurnID, err = domain.ParseTurnID(headTurnID.String)
		if err != nil {
			return nil, err
		}
	}
	return domain.RestoreSession(snapshot)
}

func validateIdentity(tenantID domain.TenantID, sessionID domain.SessionID) error {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return err
	}
	if _, err := domain.ParseSessionID(sessionID.String()); err != nil {
		return err
	}
	return nil
}

func validateReserve(command ReserveTurnCommand) error {
	if err := validateIdentity(command.TenantID, command.SessionID); err != nil {
		return err
	}
	if _, err := domain.ParseRunID(command.RunID.String()); err != nil {
		return err
	}
	if command.ExpectedRowVersion == 0 {
		return errors.New("expected row version must be positive")
	}
	if command.At.IsZero() {
		return errors.New("reservation time must not be zero")
	}
	return nil
}

func validateRegeneration(command ReserveRegenerationCommand) error {
	if err := validateReserve(ReserveTurnCommand{TenantID: command.TenantID, SessionID: command.SessionID, RunID: command.RunID, ExpectedRowVersion: command.ExpectedRowVersion, At: command.At}); err != nil {
		return err
	}
	_, err := domain.ParseTurnID(command.TurnID.String())
	return err
}

func validateCommit(command CommitCommand) error {
	if err := validateIdentity(command.TenantID, command.SessionID); err != nil {
		return err
	}
	if _, err := domain.ParseTurnID(command.TurnID.String()); err != nil {
		return err
	}
	if _, err := domain.ParseRunID(command.RunID.String()); err != nil {
		return err
	}
	if command.TurnSequence == 0 || command.ExpectedRowVersion == 0 {
		return errors.New("turn sequence and expected row version must be positive")
	}
	if command.At.IsZero() {
		return errors.New("commit time must not be zero")
	}
	return nil
}

func validateRelease(command ReleaseCommand) error {
	if err := validateIdentity(command.TenantID, command.SessionID); err != nil {
		return err
	}
	if _, err := domain.ParseRunID(command.RunID.String()); err != nil {
		return err
	}
	if command.ExpectedRowVersion == 0 {
		return errors.New("expected row version must be positive")
	}
	if command.At.IsZero() {
		return errors.New("release time must not be zero")
	}
	return nil
}

const sessionColumns = `tenant_id,id,context_version,row_version,next_turn_sequence,
    committed_turn_sequence,active_run_id,head_turn_id,created_at,updated_at`

const loadSQL = `SELECT ` + sessionColumns + ` FROM sessions WHERE tenant_id=$1 AND id=$2`

const reserveTurnSQL = `UPDATE sessions SET active_run_id=$3,next_turn_sequence=next_turn_sequence+1,
    row_version=row_version+1,updated_at=$6
WHERE tenant_id=$1 AND id=$2 AND context_version=$4 AND row_version=$5 AND active_run_id IS NULL
    AND next_turn_sequence < 9223372036854775807 AND row_version < 9223372036854775807
    AND updated_at <= $6
RETURNING ` + sessionColumns

const reserveRegenerationSQL = `UPDATE sessions SET active_run_id=$4,row_version=row_version+1,updated_at=$7
WHERE tenant_id=$1 AND id=$2 AND head_turn_id=$3 AND context_version=$5 AND row_version=$6
    AND active_run_id IS NULL AND row_version < 9223372036854775807 AND updated_at <= $7
RETURNING ` + sessionColumns

const commitSQL = `UPDATE sessions SET context_version=context_version+1,row_version=row_version+1,
    committed_turn_sequence=$5,head_turn_id=$3,active_run_id=NULL,updated_at=$7
WHERE tenant_id=$1 AND id=$2 AND active_run_id=$4 AND row_version=$6
    AND context_version < 9223372036854775807 AND row_version < 9223372036854775807
    AND (($5 > committed_turn_sequence AND $5 < next_turn_sequence) OR ($5 = committed_turn_sequence AND head_turn_id=$3))
    AND updated_at <= $7
RETURNING ` + sessionColumns

const releaseSQL = `UPDATE sessions SET active_run_id=NULL,row_version=row_version+1,updated_at=$5
WHERE tenant_id=$1 AND id=$2 AND active_run_id=$3 AND row_version=$4
    AND row_version < 9223372036854775807 AND updated_at <= $5
RETURNING ` + sessionColumns
