package compliance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	platformpostgres "github.com/agenticrag/agenticrag/internal/platform/postgres"
)

type WithinTxFunc func(context.Context, *sql.TxOptions, func(context.Context, platformpostgres.DBTX) error) error

// PostgresStore persists the deletion saga and surviving evidence.
type PostgresStore struct {
	db       platformpostgres.DBTX
	withinTx WithinTxFunc
}

func NewPostgresStore(db platformpostgres.DBTX, withinTx WithinTxFunc) (*PostgresStore, error) {
	if db == nil || withinTx == nil {
		return nil, ErrInvalid
	}
	return &PostgresStore{db: db, withinTx: withinTx}, nil
}

// RegisterTarget records a session-owned external resource before deletion.
func (s *PostgresStore) RegisterTarget(ctx context.Context, tenant domain.TenantID, session domain.SessionID, target Target, at time.Time) error {
	if !validContext(ctx) || !validTenant(tenant) || !validSession(session) || !validTarget(target) || at.IsZero() {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO compliance_external_targets(tenant_id,session_id,kind,locator,created_at)
VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, tenant.String(), session.String(), target.Kind, target.Locator, at.UTC())
	if err != nil {
		return fmt.Errorf("register compliance target: %w", err)
	}
	return nil
}

func (s *PostgresStore) Request(ctx context.Context, request Request) (Job, error) {
	if !validRequest(ctx, request) || !eventPattern.MatchString(request.EvidenceID) {
		return Job{}, ErrInvalid
	}
	var result Job
	err := s.withinTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable}, func(ctx context.Context, tx platformpostgres.DBTX) error {
		var advisoryLock any
		if err := tx.QueryRowContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2,0))`, request.TenantID.String(), request.SessionID.String()).Scan(&advisoryLock); err != nil {
			return err
		}
		existing, err := selectJob(ctx, tx, request.TenantID, request.JobID, false)
		if err == nil {
			if existing.SessionID != request.SessionID || existing.ActorSHA256 != request.ActorSHA256 ||
				existing.PolicyVersion != request.PolicyVersion || !existing.CreatedAt.Equal(request.RequestedAt.UTC()) ||
				existing.EvidenceRetention != request.EvidenceRetention {
				return ErrConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var marker int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, request.TenantID.String(), request.SessionID.String()).Scan(&marker); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM legal_holds WHERE tenant_id=$1 AND session_id=$2 AND released_at IS NULL LIMIT 1`, request.TenantID.String(), request.SessionID.String()).Scan(&marker); err == nil {
			return ErrLegalHold
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var otherID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM session_deletion_jobs WHERE tenant_id=$1 AND session_id=$2`, request.TenantID.String(), request.SessionID.String()).Scan(&otherID); err == nil {
			return ErrConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT kind,locator FROM compliance_external_targets WHERE tenant_id=$1 AND session_id=$2 ORDER BY kind,locator`, request.TenantID.String(), request.SessionID.String())
		if err != nil {
			return err
		}
		targets := make([]Target, 0)
		for rows.Next() {
			var target Target
			if err := rows.Scan(&target.Kind, &target.Locator); err != nil {
				_ = rows.Close()
				return err
			}
			targets = append(targets, target)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		targets, err = validateTargets(targets)
		if err != nil {
			return err
		}
		manifest, err := json.Marshal(targets)
		if err != nil {
			return err
		}
		result = Job{
			TenantID: request.TenantID, ID: request.JobID, SessionID: request.SessionID,
			ActorSHA256: request.ActorSHA256, PolicyVersion: request.PolicyVersion, Targets: targets,
			Phase: PhasePending, Status: StatusPending, CreatedAt: request.RequestedAt.UTC(), UpdatedAt: request.RequestedAt.UTC(),
			EvidenceRetention: request.EvidenceRetention,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_deletion_jobs(
tenant_id,id,session_id,actor_sha256,policy_version,manifest,phase,status,created_at,updated_at,evidence_retention_millis)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10)`, result.TenantID.String(), result.ID, result.SessionID.String(),
			result.ActorSHA256[:], result.PolicyVersion, manifest, result.Phase, result.Status, result.CreatedAt, result.EvidenceRetention.Milliseconds()); err != nil {
			return err
		}
		return insertEvidence(ctx, tx, result, request.EvidenceID, EvidenceRequested, "", request.RequestedAt.UTC())
	})
	if err != nil {
		return Job{}, fmt.Errorf("request session deletion: %w", err)
	}
	return cloneJob(result), nil
}

func (s *PostgresStore) Claim(ctx context.Context, tenant domain.TenantID, jobID, owner string, now time.Time, duration time.Duration) (Job, error) {
	if !validContext(ctx) || !validTenant(tenant) || !jobPattern.MatchString(jobID) || !workerPattern.MatchString(owner) ||
		now.IsZero() || duration < time.Second || duration > time.Hour {
		return Job{}, ErrInvalid
	}
	var result Job
	err := s.withinTx(ctx, nil, func(ctx context.Context, tx platformpostgres.DBTX) error {
		job, err := selectJob(ctx, tx, tenant, jobID, true)
		if err != nil {
			return err
		}
		if job.Status == StatusCompleted {
			result = job
			return nil
		}
		if job.Status == StatusRunning && job.LeaseExpiresAt.After(now) {
			if job.LeaseOwner != owner {
				return ErrConflict
			}
			result = job
			return nil
		}
		job.Status = StatusRunning
		job.AttemptCount++
		job.LeaseOwner = owner
		job.LeaseToken++
		job.LeaseExpiresAt = now.UTC().Add(duration)
		job.UpdatedAt = now.UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE session_deletion_jobs SET status=$3,attempt_count=$4,lease_owner=$5,
lease_token=$6,lease_expires_at=$7,updated_at=$8 WHERE tenant_id=$1 AND id=$2`, tenant.String(), jobID,
			job.Status, job.AttemptCount, job.LeaseOwner, job.LeaseToken, job.LeaseExpiresAt, job.UpdatedAt); err != nil {
			return err
		}
		result = job
		return nil
	})
	if err != nil {
		return Job{}, fmt.Errorf("claim session deletion: %w", err)
	}
	return cloneJob(result), nil
}

func (s *PostgresStore) CheckpointExternal(ctx context.Context, lease Lease, evidenceID string, now time.Time) error {
	return s.transition(ctx, lease, evidenceID, now, PhasePending, PhaseExternalDeleted, EvidenceExternalDeleted, "")
}

func (s *PostgresStore) DeleteSession(ctx context.Context, lease Lease, evidenceID string, now time.Time) error {
	if !eventPattern.MatchString(evidenceID) {
		return ErrInvalid
	}
	return s.withinTx(ctx, nil, func(ctx context.Context, tx platformpostgres.DBTX) error {
		job, err := selectJob(ctx, tx, lease.TenantID, lease.JobID, true)
		if err != nil {
			return err
		}
		if job.Phase == PhasePostgresDeleted || job.Phase == PhaseCompleted {
			return nil
		}
		if job.Phase != PhaseExternalDeleted || !leaseMatches(job, lease, now) {
			return ErrLeaseLost
		}
		var marker int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM legal_holds WHERE tenant_id=$1 AND session_id=$2 AND released_at IS NULL LIMIT 1`, job.TenantID.String(), job.SessionID.String()).Scan(&marker); err == nil {
			return ErrLegalHold
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE tenant_id=$1 AND id=$2`, job.TenantID.String(), job.SessionID.String()); err != nil {
			return err
		}
		for _, target := range job.Targets {
			if target.Kind == TargetObject {
				if _, err := tx.ExecContext(ctx, `DELETE FROM artifact_metadata WHERE tenant_id=$1 AND object_key=$2`, job.TenantID.String(), target.Locator); err != nil {
					return err
				}
			}
		}
		job.Phase = PhasePostgresDeleted
		job.UpdatedAt = now.UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE session_deletion_jobs SET phase=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2`, job.TenantID.String(), job.ID, job.Phase, job.UpdatedAt); err != nil {
			return err
		}
		return insertEvidence(ctx, tx, job, evidenceID, EvidencePostgresDeleted, "", now.UTC())
	})
}

func (s *PostgresStore) Complete(ctx context.Context, lease Lease, evidenceID string, now time.Time) error {
	if !eventPattern.MatchString(evidenceID) {
		return ErrInvalid
	}
	return s.withinTx(ctx, nil, func(ctx context.Context, tx platformpostgres.DBTX) error {
		job, err := selectJob(ctx, tx, lease.TenantID, lease.JobID, true)
		if err != nil {
			return err
		}
		if job.Status == StatusCompleted {
			return nil
		}
		if job.Phase != PhasePostgresDeleted || !leaseMatches(job, lease, now) {
			return ErrLeaseLost
		}
		job.Phase, job.Status = PhaseCompleted, StatusCompleted
		job.CompletedAt, job.UpdatedAt = now.UTC(), now.UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE session_deletion_jobs SET phase=$3,status=$4,completed_at=$5,updated_at=$5,
lease_owner=NULL,lease_expires_at=NULL WHERE tenant_id=$1 AND id=$2`, job.TenantID.String(), job.ID, job.Phase, job.Status, job.CompletedAt); err != nil {
			return err
		}
		return insertEvidence(ctx, tx, job, evidenceID, EvidenceCompleted, "", now.UTC())
	})
}

func (s *PostgresStore) Fail(ctx context.Context, lease Lease, evidenceID, failureClass string, now time.Time) error {
	if !eventPattern.MatchString(evidenceID) || !codePattern.MatchString(failureClass) {
		return ErrInvalid
	}
	return s.withinTx(ctx, nil, func(ctx context.Context, tx platformpostgres.DBTX) error {
		job, err := selectJob(ctx, tx, lease.TenantID, lease.JobID, true)
		if err != nil {
			return err
		}
		if !leaseMatches(job, lease, now) {
			return ErrLeaseLost
		}
		job.Status, job.UpdatedAt = StatusRetry, now.UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE session_deletion_jobs SET status=$3,updated_at=$4,lease_owner=NULL,
lease_expires_at=NULL WHERE tenant_id=$1 AND id=$2`, job.TenantID.String(), job.ID, job.Status, job.UpdatedAt); err != nil {
			return err
		}
		return insertEvidence(ctx, tx, job, evidenceID, EvidenceFailed, failureClass, now.UTC())
	})
}

func (s *PostgresStore) AddHold(ctx context.Context, hold Hold) error {
	if !validContext(ctx) || !validTenant(hold.TenantID) || !validSession(hold.SessionID) || !holdPattern.MatchString(hold.ID) ||
		!codePattern.MatchString(hold.ReasonCode) || hold.CreatedAt.IsZero() {
		return ErrInvalid
	}
	return s.withinTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable}, func(ctx context.Context, tx platformpostgres.DBTX) error {
		var advisoryLock any
		if err := tx.QueryRowContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2,0))`, hold.TenantID.String(), hold.SessionID.String()).Scan(&advisoryLock); err != nil {
			return err
		}
		var marker int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE tenant_id=$1 AND id=$2`, hold.TenantID.String(), hold.SessionID.String()).Scan(&marker); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM session_deletion_jobs WHERE tenant_id=$1 AND session_id=$2`, hold.TenantID.String(), hold.SessionID.String()).Scan(&marker); err == nil {
			return ErrConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var id, reason string
		var created time.Time
		err := tx.QueryRowContext(ctx, `SELECT id,reason_code,created_at FROM legal_holds WHERE tenant_id=$1 AND session_id=$2 AND released_at IS NULL`, hold.TenantID.String(), hold.SessionID.String()).Scan(&id, &reason, &created)
		if err == nil {
			if id == hold.ID && reason == hold.ReasonCode && created.Equal(hold.CreatedAt.UTC()) {
				return nil
			}
			return ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO legal_holds(tenant_id,session_id,id,reason_code,created_at) VALUES($1,$2,$3,$4,$5)`,
			hold.TenantID.String(), hold.SessionID.String(), hold.ID, hold.ReasonCode, hold.CreatedAt.UTC())
		return err
	})
}

func (s *PostgresStore) ReleaseHold(ctx context.Context, tenant domain.TenantID, session domain.SessionID, holdID string, at time.Time) error {
	if !validContext(ctx) || !validTenant(tenant) || !validSession(session) || !holdPattern.MatchString(holdID) || at.IsZero() {
		return ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `UPDATE legal_holds SET released_at=$4 WHERE tenant_id=$1 AND session_id=$2 AND id=$3 AND released_at IS NULL`, tenant.String(), session.String(), holdID, at.UTC())
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListEvidence(ctx context.Context, tenant domain.TenantID, after *Cursor, limit int) (EvidencePage, error) {
	if !validContext(ctx) || !validTenant(tenant) || limit < 1 || limit > 200 {
		return EvidencePage{}, ErrInvalid
	}
	var occurred time.Time
	var id string
	if after != nil {
		if after.OccurredAt.IsZero() || !eventPattern.MatchString(after.ID) {
			return EvidencePage{}, ErrInvalid
		}
		occurred, id = after.OccurredAt.UTC(), after.ID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id,id,job_id,session_id,event_type,phase,
COALESCE(failure_class,''),actor_sha256,policy_version,target_count,occurred_at,expires_at
FROM compliance_deletion_evidence WHERE tenant_id=$1 AND ($2::timestamptz IS NULL OR (occurred_at,id)>($2,$3))
ORDER BY occurred_at,id LIMIT $4`, tenant.String(), nullableTime(occurred), id, limit+1)
	if err != nil {
		return EvidencePage{}, err
	}
	defer rows.Close()
	items := make([]Evidence, 0, limit+1)
	for rows.Next() {
		var item Evidence
		var rawTenant, rawSession string
		var hash []byte
		if err := rows.Scan(&rawTenant, &item.ID, &item.JobID, &rawSession, &item.Type, &item.Phase,
			&item.FailureClass, &hash, &item.PolicyVersion, &item.TargetCount, &item.OccurredAt, &item.ExpiresAt); err != nil {
			return EvidencePage{}, err
		}
		item.TenantID = domain.TenantID(rawTenant)
		item.SessionID = domain.SessionID(rawSession)
		if len(hash) != sha256.Size {
			return EvidencePage{}, ErrInvalid
		}
		copy(item.ActorSHA256[:], hash)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return EvidencePage{}, err
	}
	page := EvidencePage{}
	if len(items) > limit {
		cursor := Cursor{OccurredAt: items[limit-1].OccurredAt, ID: items[limit-1].ID}
		page.NextCursor = &cursor
		items = items[:limit]
	}
	page.Items = items
	return page, nil
}

func (s *PostgresStore) PurgeEvidence(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) (int, error) {
	if !validContext(ctx) || !validTenant(tenant) || before.IsZero() || limit < 1 || limit > 1000 {
		return 0, ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `WITH expired AS (
SELECT e.tenant_id,e.id FROM compliance_deletion_evidence e
JOIN session_deletion_jobs j ON j.tenant_id=e.tenant_id AND j.id=e.job_id
WHERE e.tenant_id=$1 AND e.expires_at<=$2 AND j.status='COMPLETED'
ORDER BY e.expires_at,e.id LIMIT $3 FOR UPDATE OF e SKIP LOCKED)
DELETE FROM compliance_deletion_evidence e USING expired x
WHERE e.tenant_id=x.tenant_id AND e.id=x.id`, tenant.String(), before.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("purge compliance evidence: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (s *PostgresStore) transition(ctx context.Context, lease Lease, evidenceID string, now time.Time, from, to Phase, kind EvidenceType, failure string) error {
	if !eventPattern.MatchString(evidenceID) {
		return ErrInvalid
	}
	return s.withinTx(ctx, nil, func(ctx context.Context, tx platformpostgres.DBTX) error {
		job, err := selectJob(ctx, tx, lease.TenantID, lease.JobID, true)
		if err != nil {
			return err
		}
		if job.Phase != from {
			return nil
		}
		if !leaseMatches(job, lease, now) {
			return ErrLeaseLost
		}
		job.Phase, job.UpdatedAt = to, now.UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE session_deletion_jobs SET phase=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2`, job.TenantID.String(), job.ID, job.Phase, job.UpdatedAt); err != nil {
			return err
		}
		return insertEvidence(ctx, tx, job, evidenceID, kind, failure, now.UTC())
	})
}

type scanner interface{ Scan(...any) error }

func selectJob(ctx context.Context, db platformpostgres.DBTX, tenant domain.TenantID, jobID string, lock bool) (Job, error) {
	query := `SELECT tenant_id,id,session_id,actor_sha256,policy_version,manifest,phase,status,attempt_count,
COALESCE(lease_owner,''),lease_token,lease_expires_at,created_at,updated_at,completed_at,evidence_retention_millis
FROM session_deletion_jobs WHERE tenant_id=$1 AND id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanJob(db.QueryRowContext(ctx, query, tenant.String(), jobID))
}

func scanJob(row scanner) (Job, error) {
	var value Job
	var rawTenant, rawSession string
	var hash, manifest []byte
	var leaseExpiry, completed sql.NullTime
	var retentionMillis int64
	if err := row.Scan(&rawTenant, &value.ID, &rawSession, &hash, &value.PolicyVersion, &manifest, &value.Phase,
		&value.Status, &value.AttemptCount, &value.LeaseOwner, &value.LeaseToken, &leaseExpiry, &value.CreatedAt,
		&value.UpdatedAt, &completed, &retentionMillis); errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	} else if err != nil {
		return Job{}, err
	}
	value.TenantID, value.SessionID = domain.TenantID(rawTenant), domain.SessionID(rawSession)
	if len(hash) != sha256.Size || json.Unmarshal(manifest, &value.Targets) != nil {
		return Job{}, ErrInvalid
	}
	copy(value.ActorSHA256[:], hash)
	if leaseExpiry.Valid {
		value.LeaseExpiresAt = leaseExpiry.Time.UTC()
	}
	if completed.Valid {
		value.CompletedAt = completed.Time.UTC()
	}
	value.CreatedAt, value.UpdatedAt = value.CreatedAt.UTC(), value.UpdatedAt.UTC()
	value.EvidenceRetention = time.Duration(retentionMillis) * time.Millisecond
	if _, err := validateTargets(value.Targets); err != nil {
		return Job{}, err
	}
	return value, nil
}

func insertEvidence(ctx context.Context, db platformpostgres.DBTX, job Job, id string, kind EvidenceType, failure string, at time.Time) error {
	var nullableFailure any
	if failure != "" {
		nullableFailure = failure
	}
	_, err := db.ExecContext(ctx, `INSERT INTO compliance_deletion_evidence(
tenant_id,id,job_id,session_id,event_type,phase,failure_class,actor_sha256,policy_version,target_count,occurred_at,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, job.TenantID.String(), id, job.ID, job.SessionID.String(),
		kind, job.Phase, nullableFailure, job.ActorSHA256[:], job.PolicyVersion, len(job.Targets), at, at.Add(job.EvidenceRetention))
	return err
}

func leaseMatches(job Job, lease Lease, now time.Time) bool {
	return validLease(lease, now) && job.Status == StatusRunning && job.LeaseOwner == lease.Owner &&
		job.LeaseToken == lease.Token && job.LeaseExpiresAt.Equal(lease.ExpiresAt) && job.LeaseExpiresAt.After(now)
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

var _ Store = (*PostgresStore)(nil)
