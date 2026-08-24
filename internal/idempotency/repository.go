package idempotency

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const (
	MinimumRetention = 24 * time.Hour
	maxResponseBytes = 1 << 20
)

var (
	ErrInvalidInput = errors.New("invalid idempotency input")
	ErrClaimLost    = errors.New("idempotency claim is no longer owned")
	operationFormat = regexp.MustCompile(`^[A-Z][A-Z0-9_.:-]{0,127}$`)
)

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Scope struct {
	TenantID               domain.TenantID
	AuthenticatedSubjectID string
	Operation              string
	Key                    string
}

type Status string

const (
	StatusInProgress  Status = "IN_PROGRESS"
	StatusSucceeded   Status = "SUCCEEDED"
	StatusFailedFinal Status = "FAILED_FINAL"
)

type Decision string

const (
	DecisionAcquired          Decision = "ACQUIRED"
	DecisionInProgress        Decision = "IN_PROGRESS"
	DecisionReplaySucceeded   Decision = "REPLAY_SUCCEEDED"
	DecisionReplayFailedFinal Decision = "REPLAY_FAILED_FINAL"
	DecisionKeyReused         Decision = "KEY_REUSED"
)

type Response struct {
	StatusCode   int
	Body         json.RawMessage
	Location     string
	ResourceType string
	ResourceID   string
}

type Record struct {
	Scope       Scope
	Fingerprint Fingerprint
	Status      Status
	Response    *Response
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
}

type ClaimResult struct {
	Decision Decision
	Record   Record
}

type Repository struct{ db DBTX }

func NewRepository(db DBTX) (*Repository, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: database executor is nil", ErrInvalidInput)
	}
	return &Repository{db: db}, nil
}

func (r *Repository) Claim(ctx context.Context, scope Scope, fingerprint Fingerprint, now time.Time, retention time.Duration) (ClaimResult, error) {
	if err := validateClaim(scope, fingerprint, now, retention); err != nil {
		return ClaimResult{}, err
	}
	now = now.UTC()
	expiresAt := now.Add(retention)
	row := r.db.QueryRowContext(ctx, claimSQL,
		scope.TenantID.String(), scope.AuthenticatedSubjectID, scope.Operation, scope.Key,
		fingerprint[:], now, expiresAt,
	)
	record, err := scanRecord(row)
	if err == nil {
		return ClaimResult{Decision: DecisionAcquired, Record: record}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ClaimResult{}, fmt.Errorf("claim idempotency record: %w", err)
	}
	record, err = r.load(ctx, scope)
	if err != nil {
		return ClaimResult{}, err
	}
	return ClaimResult{Decision: classify(record, fingerprint), Record: record}, nil
}

func (r *Repository) CompleteSucceeded(ctx context.Context, scope Scope, fingerprint Fingerprint, response Response, now time.Time) error {
	return r.complete(ctx, scope, fingerprint, StatusSucceeded, response, now)
}

func (r *Repository) CompleteFailedFinal(ctx context.Context, scope Scope, fingerprint Fingerprint, response Response, now time.Time) error {
	return r.complete(ctx, scope, fingerprint, StatusFailedFinal, response, now)
}

func (r *Repository) complete(ctx context.Context, scope Scope, fingerprint Fingerprint, status Status, response Response, now time.Time) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	if fingerprint == (Fingerprint{}) {
		return fmt.Errorf("%w: fingerprint is zero", ErrInvalidInput)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: completion time is zero", ErrInvalidInput)
	}
	if err := validateResponse(status, response); err != nil {
		return err
	}
	location, resourceType, resourceID := nullable(response.Location), nullable(response.ResourceType), nullable(response.ResourceID)
	result, err := r.db.ExecContext(ctx, completeSQL,
		scope.TenantID.String(), scope.AuthenticatedSubjectID, scope.Operation, scope.Key,
		fingerprint[:], string(response.Body), response.StatusCode, location, resourceType, resourceID,
		string(status), now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("complete idempotency record: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read idempotency completion result: %w", err)
	}
	if affected != 1 {
		return ErrClaimLost
	}
	return nil
}

func (r *Repository) load(ctx context.Context, scope Scope) (Record, error) {
	record, err := scanRecord(r.db.QueryRowContext(ctx, loadSQL,
		scope.TenantID.String(), scope.AuthenticatedSubjectID, scope.Operation, scope.Key))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrClaimLost
		}
		return Record{}, fmt.Errorf("load idempotency record: %w", err)
	}
	return record, nil
}

type rowScanner interface{ Scan(...any) error }

func scanRecord(row rowScanner) (Record, error) {
	var record Record
	var tenantID, status string
	var hash []byte
	var responseStatus sql.NullInt64
	var responseBody []byte
	var location, resourceType, resourceID sql.NullString
	err := row.Scan(
		&tenantID, &record.Scope.AuthenticatedSubjectID, &record.Scope.Operation, &record.Scope.Key,
		&hash, &status, &responseStatus, &responseBody, &location, &resourceType, &resourceID,
		&record.CreatedAt, &record.UpdatedAt, &record.ExpiresAt,
	)
	if err != nil {
		return Record{}, err
	}
	parsedTenant, err := domain.ParseTenantID(tenantID)
	if err != nil {
		return Record{}, err
	}
	fingerprint, err := fingerprintFromBytes(hash)
	if err != nil {
		return Record{}, err
	}
	record.Scope.TenantID = parsedTenant
	record.Fingerprint = fingerprint
	record.Status = Status(status)
	if record.Status != StatusInProgress {
		record.Response = &Response{
			StatusCode: int(responseStatus.Int64), Body: append(json.RawMessage(nil), responseBody...),
			Location: location.String, ResourceType: resourceType.String, ResourceID: resourceID.String,
		}
	}
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	record.ExpiresAt = record.ExpiresAt.UTC()
	return record, nil
}

func classify(record Record, incoming Fingerprint) Decision {
	if subtle.ConstantTimeCompare(record.Fingerprint[:], incoming[:]) != 1 {
		return DecisionKeyReused
	}
	switch record.Status {
	case StatusInProgress:
		return DecisionInProgress
	case StatusSucceeded:
		return DecisionReplaySucceeded
	case StatusFailedFinal:
		return DecisionReplayFailedFinal
	default:
		return DecisionKeyReused
	}
}

func validateClaim(scope Scope, fingerprint Fingerprint, now time.Time, retention time.Duration) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	if fingerprint == (Fingerprint{}) {
		return fmt.Errorf("%w: fingerprint is zero", ErrInvalidInput)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: claim time is zero", ErrInvalidInput)
	}
	if retention < MinimumRetention {
		return fmt.Errorf("%w: retention must be at least 24 hours", ErrInvalidInput)
	}
	if now.Add(retention).Before(now) {
		return fmt.Errorf("%w: retention overflows time", ErrInvalidInput)
	}
	return nil
}

func validateScope(scope Scope) error {
	if _, err := domain.ParseTenantID(scope.TenantID.String()); err != nil {
		return fmt.Errorf("%w: tenant: %v", ErrInvalidInput, err)
	}
	if invalidBoundedText(scope.AuthenticatedSubjectID, 1, 256) {
		return fmt.Errorf("%w: authenticated subject", ErrInvalidInput)
	}
	if err := validateOperation(scope.Operation); err != nil {
		return err
	}
	if invalidBoundedText(scope.Key, 16, 128) {
		return fmt.Errorf("%w: idempotency key", ErrInvalidInput)
	}
	return nil
}

func validateOperation(operation string) error {
	if !operationFormat.MatchString(operation) {
		return fmt.Errorf("%w: operation", ErrInvalidInput)
	}
	return nil
}

func invalidBoundedText(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return true
	}
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func validateResponse(status Status, response Response) error {
	if len(response.Body) == 0 || len(response.Body) > maxResponseBytes || !json.Valid(response.Body) {
		return fmt.Errorf("%w: response body", ErrInvalidInput)
	}
	if status == StatusSucceeded && (response.StatusCode < 200 || response.StatusCode > 299) {
		return fmt.Errorf("%w: successful response status", ErrInvalidInput)
	}
	if status == StatusFailedFinal && (response.StatusCode < 400 || response.StatusCode > 599) {
		return fmt.Errorf("%w: final-error response status", ErrInvalidInput)
	}
	if invalidOptionalText(response.Location, 2048) {
		return fmt.Errorf("%w: response location", ErrInvalidInput)
	}
	if (response.ResourceType == "") != (response.ResourceID == "") {
		return fmt.Errorf("%w: resource type and ID must be paired", ErrInvalidInput)
	}
	if invalidOptionalText(response.ResourceType, 128) || invalidOptionalText(response.ResourceID, 128) {
		return fmt.Errorf("%w: resource reference", ErrInvalidInput)
	}
	return nil
}

func invalidOptionalText(value string, maximum int) bool {
	return len(value) > maximum || strings.IndexFunc(value, unicode.IsControl) >= 0
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const claimSQL = `
INSERT INTO idempotency_records(
    tenant_id,authenticated_subject_id,operation,idempotency_key,request_hash,status,
    created_at,updated_at,expires_at
) VALUES($1,$2,$3,$4,$5,'IN_PROGRESS',$6,$6,$7)
ON CONFLICT (tenant_id,authenticated_subject_id,operation,idempotency_key)
DO UPDATE SET request_hash=EXCLUDED.request_hash,status='IN_PROGRESS',
    response_status=NULL,response_body=NULL,response_location=NULL,resource_type=NULL,resource_id=NULL,
    created_at=EXCLUDED.created_at,updated_at=EXCLUDED.updated_at,expires_at=EXCLUDED.expires_at
WHERE idempotency_records.expires_at <= EXCLUDED.created_at
RETURNING tenant_id,authenticated_subject_id,operation,idempotency_key,request_hash,status,
    response_status,response_body,response_location,resource_type,resource_id,created_at,updated_at,expires_at`

const loadSQL = `
SELECT tenant_id,authenticated_subject_id,operation,idempotency_key,request_hash,status,
    response_status,response_body,response_location,resource_type,resource_id,created_at,updated_at,expires_at
FROM idempotency_records
WHERE tenant_id=$1 AND authenticated_subject_id=$2 AND operation=$3 AND idempotency_key=$4`

const completeSQL = `
UPDATE idempotency_records SET status=$11,response_body=$6::jsonb,response_status=$7,
    response_location=$8,resource_type=$9,resource_id=$10,updated_at=$12
WHERE tenant_id=$1 AND authenticated_subject_id=$2 AND operation=$3 AND idempotency_key=$4
    AND request_hash=$5 AND status='IN_PROGRESS' AND expires_at > $12`
