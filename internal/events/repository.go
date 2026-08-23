package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrNotFound       = errors.New("event Run not found")
	ErrCursorExpired  = errors.New("event cursor expired")
	ErrInvalidStorage = errors.New("invalid event storage input")
)

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Repository struct{ db DBTX }

func NewRepository(db DBTX) (*Repository, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: database executor is nil", ErrInvalidStorage)
	}
	return &Repository{db: db}, nil
}

type AppendCommand struct {
	TenantID   domain.TenantID
	SessionID  domain.SessionID
	TurnID     domain.TurnID
	RunID      domain.RunID
	Type       Type
	Payload    any
	OccurredAt time.Time
	Retention  time.Duration
}

func (r *Repository) Append(ctx context.Context, command AppendCommand) (Event, error) {
	if command.Retention < time.Hour || command.Retention > 30*24*time.Hour || command.OccurredAt.IsZero() {
		return Event{}, fmt.Errorf("%w: occurrence time and retention 1h..30d are required", ErrInvalidStorage)
	}
	if _, err := domain.ParseSessionID(command.SessionID.String()); err != nil {
		return Event{}, err
	}
	// Validate the public envelope before consuming a durable sequence. Sequence
	// gaps are legal after transaction rollback, but invalid caller input must
	// never mutate the stream ledger.
	if _, err := New(command.TenantID, command.TurnID, command.RunID, 1, command.Type, command.Payload, command.OccurredAt); err != nil {
		return Event{}, err
	}
	payload, err := json.Marshal(command.Payload)
	if err != nil {
		return Event{}, err
	}
	var sequence uint64
	err = r.db.QueryRowContext(ctx, appendEventSQL, command.TenantID.String(), command.RunID.String(),
		command.SessionID.String(), command.TurnID.String(), CurrentSchemaVersion, command.Type.String(),
		command.OccurredAt.UTC().Format(time.RFC3339Nano), string(payload), command.OccurredAt.UTC(),
		command.OccurredAt.Add(command.Retention).UTC()).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, fmt.Errorf("%w: stream identity mismatch or sequence exhausted", ErrInvalidStorage)
	}
	if err != nil {
		return Event{}, fmt.Errorf("allocate event sequence: %w", err)
	}
	event, err := New(command.TenantID, command.TurnID, command.RunID, sequence, command.Type, command.Payload, command.OccurredAt)
	if err != nil {
		return Event{}, err
	}
	return event, nil
}

type Bounds struct {
	SessionID    domain.SessionID
	TurnID       domain.TurnID
	RetainedFrom uint64
	HighWater    uint64
}

type PruneResult struct {
	Deleted      int64
	RetainedFrom uint64
}

// PruneExpired deletes a bounded prefix and atomically advances the explicit
// retention watermark. It is safe to retry and never crosses a tenant boundary.
func (r *Repository) PruneExpired(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, at time.Time, limit int) (PruneResult, error) {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return PruneResult{}, err
	}
	if _, err := domain.ParseRunID(runID.String()); err != nil {
		return PruneResult{}, err
	}
	if at.IsZero() || limit < 1 || limit > 10_000 {
		return PruneResult{}, fmt.Errorf("%w: prune time or limit", ErrInvalidStorage)
	}
	var result PruneResult
	err := r.db.QueryRowContext(ctx, pruneExpiredSQL, tenantID.String(), runID.String(), at.UTC(), limit).Scan(&result.Deleted, &result.RetainedFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return PruneResult{}, ErrNotFound
	}
	if err != nil {
		return PruneResult{}, fmt.Errorf("prune Run events: %w", err)
	}
	return result, nil
}

func (r *Repository) Bounds(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (Bounds, error) {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return Bounds{}, err
	}
	if _, err := domain.ParseRunID(runID.String()); err != nil {
		return Bounds{}, err
	}
	var result Bounds
	var sessionID, turnID string
	err := r.db.QueryRowContext(ctx, boundsSQL, tenantID.String(), runID.String()).Scan(
		&sessionID, &turnID, &result.RetainedFrom, &result.HighWater)
	if errors.Is(err, sql.ErrNoRows) {
		return Bounds{}, ErrNotFound
	}
	if err != nil {
		return Bounds{}, fmt.Errorf("read Run event bounds: %w", err)
	}
	var parseErr error
	if result.SessionID, parseErr = domain.ParseSessionID(sessionID); parseErr != nil {
		return Bounds{}, parseErr
	}
	if result.TurnID, parseErr = domain.ParseTurnID(turnID); parseErr != nil {
		return Bounds{}, parseErr
	}
	if result.RetainedFrom == 0 || result.HighWater > math.MaxInt64 || result.RetainedFrom > result.HighWater+1 {
		return Bounds{}, errors.New("stored event bounds are invalid")
	}
	return result, nil
}

func (r *Repository) Replay(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, after, through uint64, limit int) ([]Event, error) {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return nil, err
	}
	if _, err := domain.ParseRunID(runID.String()); err != nil {
		return nil, err
	}
	if after > through || through > math.MaxInt64 || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: replay range or limit", ErrInvalidStorage)
	}
	rows, err := r.db.QueryContext(ctx, replaySQL, tenantID.String(), runID.String(), after, through, limit)
	if err != nil {
		return nil, fmt.Errorf("replay Run events: %w", err)
	}
	defer rows.Close()
	result := make([]Event, 0, limit)
	for rows.Next() {
		var sequence uint64
		var data []byte
		if err := rows.Scan(&sequence, &data); err != nil {
			return nil, err
		}
		event, err := DecodeStrict(tenantID, data)
		if err != nil {
			return nil, fmt.Errorf("hydrate Run event sequence %d: %w", sequence, err)
		}
		if event.RunID() != runID || event.Sequence() != sequence {
			return nil, fmt.Errorf("hydrate Run event sequence %d: stored identity mismatch", sequence)
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

const appendEventSQL = `WITH allocated AS (
    INSERT INTO run_event_streams(tenant_id,run_id,session_id,turn_id,next_sequence,retained_from,created_at,updated_at)
    VALUES($1,$2,$3,$4,2,1,$9,$9)
    ON CONFLICT (tenant_id,run_id) DO UPDATE SET
        next_sequence=run_event_streams.next_sequence+1,updated_at=GREATEST(run_event_streams.updated_at,EXCLUDED.updated_at)
    WHERE run_event_streams.session_id=EXCLUDED.session_id AND run_event_streams.turn_id=EXCLUDED.turn_id
        AND run_event_streams.next_sequence < 9223372036854775807
    RETURNING next_sequence-1 AS sequence
), inserted AS (
    INSERT INTO run_events(tenant_id,run_id,sequence,turn_id,schema_version,event_type,event_data,occurred_at,expires_at)
    SELECT $1,$2,allocated.sequence,$4,$5,$6,jsonb_build_object(
        'schema_version',$5,'type',$6,'turn_id',$4,'run_id',$2,'sequence',allocated.sequence,
        'occurred_at',$7,'payload',$8::jsonb),$9,$10
    FROM allocated RETURNING sequence
)
SELECT sequence FROM inserted`

const boundsSQL = `SELECT r.session_id,r.turn_id,COALESCE(s.retained_from,1),COALESCE(s.next_sequence-1,0)
FROM runs r LEFT JOIN run_event_streams s ON s.tenant_id=r.tenant_id AND s.run_id=r.id
WHERE r.tenant_id=$1 AND r.id=$2`

const replaySQL = `SELECT sequence,event_data FROM run_events
WHERE tenant_id=$1 AND run_id=$2 AND sequence>$3 AND sequence<=$4
ORDER BY sequence LIMIT $5`

const pruneExpiredSQL = `WITH candidates AS (
    SELECT sequence FROM run_events
    WHERE tenant_id=$1 AND run_id=$2 AND expires_at<=$3
    ORDER BY sequence LIMIT $4
), deleted AS (
    DELETE FROM run_events event USING candidates candidate
    WHERE event.tenant_id=$1 AND event.run_id=$2 AND event.sequence=candidate.sequence
    RETURNING event.sequence
), new_floor AS (
    SELECT COALESCE(MIN(event.sequence) FILTER (WHERE candidate.sequence IS NULL),stream.next_sequence) AS retained_from
    FROM run_event_streams stream
    LEFT JOIN run_events event ON event.tenant_id=stream.tenant_id AND event.run_id=stream.run_id
    LEFT JOIN candidates candidate ON candidate.sequence=event.sequence
    WHERE stream.tenant_id=$1 AND stream.run_id=$2
    GROUP BY stream.next_sequence
)
UPDATE run_event_streams stream SET
    retained_from=GREATEST(stream.retained_from,new_floor.retained_from),
    updated_at=GREATEST(stream.updated_at,$3)
FROM new_floor WHERE stream.tenant_id=$1 AND stream.run_id=$2
RETURNING (SELECT count(*) FROM deleted),stream.retained_from`
