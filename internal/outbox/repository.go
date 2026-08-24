// Package outbox implements tenant-scoped transactional event delivery.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const (
	MinimumClaimDuration = 5 * time.Second
	MaximumClaimDuration = 5 * time.Minute
	MaximumPayloadSize   = 1 << 20
	MaximumErrorLength   = 2048
)

var (
	ErrInvalidArgument = errors.New("outbox invalid argument")
	ErrNotFound        = errors.New("outbox event not found")
	ErrClaimFenced     = errors.New("outbox claim fenced")
	ErrClaimExpired    = errors.New("outbox claim expired")
	resourceIDPattern  = regexp.MustCompile(`^evt_[A-Za-z0-9_-]{4,124}$`)
	namePattern        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,127}$`)
	aggregateIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{3,127}$`)
)

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Status string

const (
	StatusPending   Status = "PENDING"
	StatusPublished Status = "PUBLISHED"
	StatusDead      Status = "DEAD"
)

type Event struct {
	TenantID      domain.TenantID
	ID            string
	AggregateType string
	AggregateID   string
	Type          string
	Payload       json.RawMessage
	Status        Status
	AvailableAt   time.Time
	OccurredAt    time.Time
	AttemptCount  int
	ClaimOwner    string
	ClaimToken    uint64
	ClaimExpires  time.Time
	PublishedAt   time.Time
	LastError     string
}

type Repository struct{ db DBTX }

func NewRepository(db DBTX) (*Repository, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: database executor is nil", ErrInvalidArgument)
	}
	return &Repository{db: db}, nil
}

func (r *Repository) Enqueue(ctx context.Context, event Event) error {
	if err := validateNewEvent(event); err != nil {
		return err
	}
	payload := append(json.RawMessage(nil), event.Payload...)
	_, err := r.db.ExecContext(ctx, insertEventSQL, event.TenantID.String(), event.ID,
		event.AggregateType, event.AggregateID, event.Type, payload,
		event.AvailableAt.UTC(), event.OccurredAt.UTC())
	if err != nil {
		return fmt.Errorf("enqueue outbox event: %w", err)
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, tenantID domain.TenantID, eventID string) (Event, error) {
	if err := validateIdentity(tenantID, eventID); err != nil {
		return Event{}, err
	}
	event, err := scanEvent(r.db.QueryRowContext(ctx, selectEventSQL, tenantID.String(), eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("select outbox event: %w", err)
	}
	return event, nil
}

type ClaimCommand struct {
	TenantID domain.TenantID
	Owner    string
	Duration time.Duration
	Limit    int
}

type ReadyTenantsCommand struct{ Limit int }

// ReadyTenants returns a bounded stable set of tenants with claimable work.
// Relay instances may race this read safely; ClaimBatch performs the authority
// transition with SKIP LOCKED and a fenced lease.
func (r *Repository) ReadyTenants(ctx context.Context, command ReadyTenantsCommand) ([]domain.TenantID, error) {
	if command.Limit < 1 || command.Limit > 1000 {
		return nil, fmt.Errorf("%w: ready tenant limit 1..1000 is required", ErrInvalidArgument)
	}
	rows, err := r.db.QueryContext(ctx, readyTenantsSQL, command.Limit)
	if err != nil {
		return nil, fmt.Errorf("list ready outbox tenants: %w", err)
	}
	defer rows.Close()
	result := make([]domain.TenantID, 0, command.Limit)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		tenantID, err := domain.ParseTenantID(raw)
		if err != nil {
			return nil, fmt.Errorf("corrupt outbox tenant: %w", err)
		}
		result = append(result, tenantID)
	}
	return result, rows.Err()
}

func (r *Repository) ClaimBatch(ctx context.Context, command ClaimCommand) ([]Event, error) {
	if err := validateClaim(command); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, claimBatchSQL, command.TenantID.String(), command.Limit,
		command.Owner, command.Duration.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("claim outbox batch: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0, command.Limit)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("hydrate claimed outbox event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed outbox events: %w", err)
	}
	return events, nil
}

type Acknowledgement struct {
	TenantID domain.TenantID
	EventID  string
	Owner    string
	Token    uint64
}

func (r *Repository) MarkPublished(ctx context.Context, acknowledgement Acknowledgement) error {
	if err := validateAcknowledgement(acknowledgement); err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, markPublishedSQL, acknowledgement.TenantID.String(),
		acknowledgement.EventID, acknowledgement.Owner, acknowledgement.Token)
	if err != nil {
		return fmt.Errorf("mark outbox event published: %w", err)
	}
	return r.classifyMutation(ctx, acknowledgement, result)
}

type Failure struct {
	Acknowledgement
	RetryAfter time.Duration
	Cause      string
	Dead       bool
}

func (r *Repository) MarkFailed(ctx context.Context, failure Failure) error {
	if err := validateFailure(failure); err != nil {
		return err
	}
	status := StatusPending
	if failure.Dead {
		status = StatusDead
	}
	result, err := r.db.ExecContext(ctx, markFailedSQL, failure.TenantID.String(), failure.EventID,
		failure.Owner, failure.Token, status, failure.RetryAfter.Milliseconds(), failure.Cause)
	if err != nil {
		return fmt.Errorf("mark outbox event failed: %w", err)
	}
	return r.classifyMutation(ctx, failure.Acknowledgement, result)
}

type CleanupCommand struct {
	TenantID domain.TenantID
	Cutoff   time.Time
	Limit    int
}

func (r *Repository) CleanupPublished(ctx context.Context, command CleanupCommand) (int64, error) {
	if _, err := domain.ParseTenantID(command.TenantID.String()); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if command.Cutoff.IsZero() || command.Limit < 1 || command.Limit > 1000 {
		return 0, fmt.Errorf("%w: cleanup cutoff and limit 1..1000 are required", ErrInvalidArgument)
	}
	result, err := r.db.ExecContext(ctx, cleanupPublishedSQL, command.TenantID.String(), command.Cutoff.UTC(), command.Limit)
	if err != nil {
		return 0, fmt.Errorf("clean published outbox events: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleaned outbox events: %w", err)
	}
	return count, nil
}

func (r *Repository) classifyMutation(ctx context.Context, acknowledgement Acknowledgement, result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count outbox mutation: %w", err)
	}
	if count == 1 {
		return nil
	}
	var owner sql.NullString
	var token uint64
	var expires sql.NullTime
	var databaseTime time.Time
	err = r.db.QueryRowContext(ctx, selectClaimSQL, acknowledgement.TenantID.String(), acknowledgement.EventID).
		Scan(&owner, &token, &expires, &databaseTime)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("classify outbox claim: %w", err)
	}
	if owner.Valid && owner.String == acknowledgement.Owner && token == acknowledgement.Token &&
		expires.Valid && !expires.Time.After(databaseTime) {
		return ErrClaimExpired
	}
	return ErrClaimFenced
}

type rowScanner interface{ Scan(...any) error }

func scanEvent(scanner rowScanner) (Event, error) {
	var event Event
	var rawTenant, rawStatus string
	var payload []byte
	var owner, lastError sql.NullString
	var claimExpires, publishedAt sql.NullTime
	if err := scanner.Scan(&rawTenant, &event.ID, &event.AggregateType, &event.AggregateID, &event.Type,
		&payload, &rawStatus, &event.AvailableAt, &event.OccurredAt, &event.AttemptCount,
		&owner, &event.ClaimToken, &claimExpires, &publishedAt, &lastError); err != nil {
		return Event{}, err
	}
	tenantID, err := domain.ParseTenantID(rawTenant)
	if err != nil {
		return Event{}, err
	}
	event.TenantID = tenantID
	event.Status = Status(rawStatus)
	event.Payload = append(json.RawMessage(nil), payload...)
	if owner.Valid {
		event.ClaimOwner = owner.String
	}
	if claimExpires.Valid {
		event.ClaimExpires = claimExpires.Time.UTC()
	}
	if publishedAt.Valid {
		event.PublishedAt = publishedAt.Time.UTC()
	}
	if lastError.Valid {
		event.LastError = lastError.String
	}
	event.AvailableAt = event.AvailableAt.UTC()
	event.OccurredAt = event.OccurredAt.UTC()
	if err := validateStoredEvent(event); err != nil {
		return Event{}, err
	}
	return event, nil
}

func validateNewEvent(event Event) error {
	if err := validateIdentity(event.TenantID, event.ID); err != nil {
		return err
	}
	if !namePattern.MatchString(event.AggregateType) || !aggregateIDPattern.MatchString(event.AggregateID) || !namePattern.MatchString(event.Type) {
		return fmt.Errorf("%w: aggregate and event names are invalid", ErrInvalidArgument)
	}
	if event.AvailableAt.IsZero() || event.OccurredAt.IsZero() {
		return fmt.Errorf("%w: event times are required", ErrInvalidArgument)
	}
	if len(event.Payload) == 0 || len(event.Payload) > MaximumPayloadSize || !json.Valid(event.Payload) {
		return fmt.Errorf("%w: payload must be valid JSON no larger than 1 MiB", ErrInvalidArgument)
	}
	if event.Status != "" || event.AttemptCount != 0 || event.ClaimOwner != "" || event.ClaimToken != 0 ||
		!event.ClaimExpires.IsZero() || !event.PublishedAt.IsZero() || event.LastError != "" {
		return fmt.Errorf("%w: enqueue accepts only immutable event fields", ErrInvalidArgument)
	}
	return nil
}

func validateStoredEvent(event Event) error {
	if err := validateIdentity(event.TenantID, event.ID); err != nil {
		return err
	}
	if !namePattern.MatchString(event.AggregateType) || !aggregateIDPattern.MatchString(event.AggregateID) || !namePattern.MatchString(event.Type) ||
		len(event.Payload) == 0 || len(event.Payload) > MaximumPayloadSize || !json.Valid(event.Payload) || event.AttemptCount < 0 {
		return fmt.Errorf("%w: corrupt stored event", ErrInvalidArgument)
	}
	if event.Status != StatusPending && event.Status != StatusPublished && event.Status != StatusDead {
		return fmt.Errorf("%w: corrupt stored status", ErrInvalidArgument)
	}
	if (event.ClaimOwner == "") != event.ClaimExpires.IsZero() || (event.Status == StatusPublished) != !event.PublishedAt.IsZero() {
		return fmt.Errorf("%w: corrupt stored lifecycle", ErrInvalidArgument)
	}
	if event.Status != StatusPending && event.ClaimOwner != "" {
		return fmt.Errorf("%w: terminal event retains a claim", ErrInvalidArgument)
	}
	return nil
}

func validateIdentity(tenantID domain.TenantID, eventID string) error {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if !resourceIDPattern.MatchString(eventID) {
		return fmt.Errorf("%w: invalid event ID", ErrInvalidArgument)
	}
	return nil
}

func validateClaim(command ClaimCommand) error {
	if _, err := domain.ParseTenantID(command.TenantID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if err := validateOwner(command.Owner); err != nil {
		return err
	}
	if command.Duration < MinimumClaimDuration || command.Duration > MaximumClaimDuration || command.Limit < 1 || command.Limit > 100 {
		return fmt.Errorf("%w: claim duration 5s..5m and limit 1..100 are required", ErrInvalidArgument)
	}
	return nil
}

func validateAcknowledgement(value Acknowledgement) error {
	if err := validateIdentity(value.TenantID, value.EventID); err != nil {
		return err
	}
	if err := validateOwner(value.Owner); err != nil {
		return err
	}
	if value.Token == 0 {
		return fmt.Errorf("%w: positive token is required", ErrInvalidArgument)
	}
	return nil
}

func validateFailure(failure Failure) error {
	if err := validateAcknowledgement(failure.Acknowledgement); err != nil {
		return err
	}
	if strings.TrimSpace(failure.Cause) == "" || len(failure.Cause) > MaximumErrorLength {
		return fmt.Errorf("%w: failure cause must be 1..2048 characters", ErrInvalidArgument)
	}
	if failure.RetryAfter < 0 || failure.RetryAfter > 24*time.Hour || (!failure.Dead && failure.RetryAfter == 0) {
		return fmt.Errorf("%w: retry delay must be positive and no more than 24 hours", ErrInvalidArgument)
	}
	return nil
}

func validateOwner(owner string) error {
	if strings.TrimSpace(owner) != owner || len(owner) < 1 || len(owner) > 128 {
		return fmt.Errorf("%w: owner must be 1..128 non-space-trimmed characters", ErrInvalidArgument)
	}
	return nil
}

const eventColumns = `tenant_id,id,aggregate_type,aggregate_id,event_type,payload,status,
available_at,occurred_at,attempt_count,claim_owner,claim_token,claim_expires_at,published_at,last_error`

const claimedEventColumns = `event.tenant_id,event.id,event.aggregate_type,event.aggregate_id,event.event_type,event.payload,event.status,
event.available_at,event.occurred_at,event.attempt_count,event.claim_owner,event.claim_token,event.claim_expires_at,event.published_at,event.last_error`

const insertEventSQL = `INSERT INTO outbox_events(
tenant_id,id,aggregate_type,aggregate_id,event_type,payload,available_at,occurred_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)`

const selectEventSQL = `SELECT ` + eventColumns + ` FROM outbox_events WHERE tenant_id=$1 AND id=$2`

const claimBatchSQL = `WITH candidates AS (
 SELECT candidate.tenant_id,candidate.id FROM outbox_events AS candidate
 WHERE candidate.tenant_id=$1 AND candidate.status='PENDING' AND candidate.available_at <= CURRENT_TIMESTAMP
   AND (candidate.claim_expires_at IS NULL OR candidate.claim_expires_at <= CURRENT_TIMESTAMP)
   AND (candidate.event_type <> 'RunEvent.v1' OR NOT EXISTS (
     SELECT 1 FROM outbox_events AS earlier
     WHERE earlier.tenant_id=candidate.tenant_id
       AND earlier.aggregate_type=candidate.aggregate_type
       AND earlier.aggregate_id=candidate.aggregate_id
       AND earlier.event_type='RunEvent.v1' AND earlier.status='PENDING'
       AND (earlier.payload->>'sequence') ~ '^[0-9]+$'
       AND (candidate.payload->>'sequence') ~ '^[0-9]+$'
       AND (earlier.payload->>'sequence')::bigint < (candidate.payload->>'sequence')::bigint
   ))
 ORDER BY candidate.available_at,candidate.occurred_at,candidate.aggregate_id,
   CASE WHEN candidate.event_type='RunEvent.v1' AND (candidate.payload->>'sequence') ~ '^[0-9]+$'
     THEN (candidate.payload->>'sequence')::bigint ELSE 9223372036854775807 END,candidate.id
 FOR UPDATE SKIP LOCKED LIMIT $2
)
UPDATE outbox_events AS event
SET claim_owner=$3,claim_token=event.claim_token+1,
    claim_expires_at=CURRENT_TIMESTAMP + $4 * interval '1 millisecond',
    attempt_count=event.attempt_count+1
FROM candidates WHERE event.tenant_id=candidates.tenant_id AND event.id=candidates.id
RETURNING ` + claimedEventColumns

const readyTenantsSQL = `SELECT candidate.tenant_id FROM outbox_events AS candidate
WHERE candidate.status='PENDING' AND candidate.available_at <= CURRENT_TIMESTAMP
  AND (candidate.claim_expires_at IS NULL OR candidate.claim_expires_at <= CURRENT_TIMESTAMP)
  AND (candidate.event_type <> 'RunEvent.v1' OR NOT EXISTS (
    SELECT 1 FROM outbox_events AS earlier
    WHERE earlier.tenant_id=candidate.tenant_id
      AND earlier.aggregate_type=candidate.aggregate_type
      AND earlier.aggregate_id=candidate.aggregate_id
      AND earlier.event_type='RunEvent.v1' AND earlier.status='PENDING'
      AND (earlier.payload->>'sequence') ~ '^[0-9]+$'
      AND (candidate.payload->>'sequence') ~ '^[0-9]+$'
      AND (earlier.payload->>'sequence')::bigint < (candidate.payload->>'sequence')::bigint
  ))
GROUP BY candidate.tenant_id ORDER BY MIN(candidate.available_at),candidate.tenant_id LIMIT $1`

const markPublishedSQL = `UPDATE outbox_events SET status='PUBLISHED',published_at=CURRENT_TIMESTAMP,
claim_owner=NULL,claim_expires_at=NULL,last_error=NULL
WHERE tenant_id=$1 AND id=$2 AND status='PENDING' AND claim_owner=$3 AND claim_token=$4
  AND claim_expires_at > CURRENT_TIMESTAMP`

const markFailedSQL = `UPDATE outbox_events SET status=$5,
available_at=CURRENT_TIMESTAMP + $6 * interval '1 millisecond',last_error=$7,
claim_owner=NULL,claim_expires_at=NULL
WHERE tenant_id=$1 AND id=$2 AND status='PENDING' AND claim_owner=$3 AND claim_token=$4
  AND claim_expires_at > CURRENT_TIMESTAMP`

const selectClaimSQL = `SELECT claim_owner,claim_token,claim_expires_at,CURRENT_TIMESTAMP FROM outbox_events WHERE tenant_id=$1 AND id=$2`

const cleanupPublishedSQL = `WITH candidates AS (
 SELECT tenant_id,id FROM outbox_events
 WHERE tenant_id=$1 AND status='PUBLISHED' AND published_at < $2
 ORDER BY published_at,id FOR UPDATE SKIP LOCKED LIMIT $3
)
DELETE FROM outbox_events AS event USING candidates
WHERE event.tenant_id=candidates.tenant_id AND event.id=candidates.id`
