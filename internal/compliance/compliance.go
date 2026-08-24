// Package compliance coordinates tenant-safe retention and deletion workflows.
package compliance

import (
	"context"
	"crypto/sha256"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrInvalid    = errors.New("invalid compliance operation")
	ErrNotFound   = errors.New("compliance resource not found")
	ErrConflict   = errors.New("compliance operation conflict")
	ErrLegalHold  = errors.New("session is under legal hold")
	ErrLeaseLost  = errors.New("compliance worker lease lost")
	jobPattern    = regexp.MustCompile(`^del_[A-Za-z0-9_-]{4,124}$`)
	holdPattern   = regexp.MustCompile(`^hld_[A-Za-z0-9_-]{4,124}$`)
	eventPattern  = regexp.MustCompile(`^cev_[A-Za-z0-9_-]{4,124}$`)
	workerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	codePattern   = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

type TargetKind string

const (
	TargetObject TargetKind = "OBJECT"
	TargetRedis  TargetKind = "REDIS"
	TargetIndex  TargetKind = "INDEX"
)

type Target struct {
	Kind    TargetKind
	Locator string
}

type Phase string

const (
	PhasePending         Phase = "PENDING"
	PhaseExternalDeleted Phase = "EXTERNAL_DELETED"
	PhasePostgresDeleted Phase = "POSTGRES_DELETED"
	PhaseCompleted       Phase = "COMPLETED"
)

type Status string

const (
	StatusPending   Status = "PENDING"
	StatusRunning   Status = "RUNNING"
	StatusRetry     Status = "RETRY"
	StatusCompleted Status = "COMPLETED"
)

type Request struct {
	TenantID          domain.TenantID
	JobID             string
	SessionID         domain.SessionID
	ActorSHA256       [sha256.Size]byte
	PolicyVersion     string
	RequestedAt       time.Time
	EvidenceRetention time.Duration
	EvidenceID        string
}

type Job struct {
	TenantID          domain.TenantID
	ID                string
	SessionID         domain.SessionID
	ActorSHA256       [sha256.Size]byte
	PolicyVersion     string
	Targets           []Target
	Phase             Phase
	Status            Status
	AttemptCount      uint64
	LeaseOwner        string
	LeaseToken        uint64
	LeaseExpiresAt    time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	CompletedAt       time.Time
	EvidenceRetention time.Duration
}

type Lease struct {
	TenantID  domain.TenantID
	JobID     string
	Owner     string
	Token     uint64
	ExpiresAt time.Time
}

type Hold struct {
	TenantID   domain.TenantID
	SessionID  domain.SessionID
	ID         string
	ReasonCode string
	CreatedAt  time.Time
}

type EvidenceType string

const (
	EvidenceRequested       EvidenceType = "REQUESTED"
	EvidenceExternalDeleted EvidenceType = "EXTERNAL_DELETED"
	EvidencePostgresDeleted EvidenceType = "POSTGRES_DELETED"
	EvidenceFailed          EvidenceType = "FAILED"
	EvidenceCompleted       EvidenceType = "COMPLETED"
)

type Evidence struct {
	TenantID      domain.TenantID
	ID            string
	JobID         string
	SessionID     domain.SessionID
	Type          EvidenceType
	Phase         Phase
	FailureClass  string
	ActorSHA256   [sha256.Size]byte
	PolicyVersion string
	TargetCount   int
	OccurredAt    time.Time
	ExpiresAt     time.Time
}

type Cursor struct {
	OccurredAt time.Time
	ID         string
}

type EvidencePage struct {
	Items      []Evidence
	NextCursor *Cursor
}

type Store interface {
	Request(context.Context, Request) (Job, error)
	Claim(context.Context, domain.TenantID, string, string, time.Time, time.Duration) (Job, error)
	CheckpointExternal(context.Context, Lease, string, time.Time) error
	DeleteSession(context.Context, Lease, string, time.Time) error
	Complete(context.Context, Lease, string, time.Time) error
	Fail(context.Context, Lease, string, string, time.Time) error
	AddHold(context.Context, Hold) error
	ReleaseHold(context.Context, domain.TenantID, domain.SessionID, string, time.Time) error
	ListEvidence(context.Context, domain.TenantID, *Cursor, int) (EvidencePage, error)
	PurgeEvidence(context.Context, domain.TenantID, time.Time, int) (int, error)
}

type Cleaner interface {
	Delete(context.Context, Target) error
}

type CleanerFunc func(context.Context, Target) error

func (f CleanerFunc) Delete(ctx context.Context, target Target) error { return f(ctx, target) }

type IDGenerator interface {
	NewEvidenceID() (string, error)
}

type IDGeneratorFunc func() (string, error)

func (f IDGeneratorFunc) NewEvidenceID() (string, error) { return f() }

type Service struct {
	store         Store
	cleaners      map[TargetKind]Cleaner
	ids           IDGenerator
	leaseDuration time.Duration
}

func NewService(store Store, cleaners map[TargetKind]Cleaner, ids IDGenerator, leaseDuration time.Duration) (*Service, error) {
	if store == nil || ids == nil || leaseDuration < time.Second || leaseDuration > time.Hour || len(cleaners) != 3 {
		return nil, ErrInvalid
	}
	copyCleaners := make(map[TargetKind]Cleaner, 3)
	for _, kind := range []TargetKind{TargetObject, TargetRedis, TargetIndex} {
		if cleaners[kind] == nil {
			return nil, ErrInvalid
		}
		copyCleaners[kind] = cleaners[kind]
	}
	return &Service{store: store, cleaners: copyCleaners, ids: ids, leaseDuration: leaseDuration}, nil
}

func (s *Service) Request(ctx context.Context, request Request) (Job, error) {
	if !validRequest(ctx, request) {
		return Job{}, ErrInvalid
	}
	evidenceID, err := s.newEvidenceID()
	if err != nil {
		return Job{}, err
	}
	request.EvidenceID = evidenceID
	return s.store.Request(ctx, request)
}

func (s *Service) Process(ctx context.Context, tenant domain.TenantID, jobID string, worker string, now time.Time) error {
	if !validContext(ctx) || !validTenant(tenant) || !jobPattern.MatchString(jobID) ||
		!workerPattern.MatchString(worker) || now.IsZero() {
		return ErrInvalid
	}
	job, err := s.store.Claim(ctx, tenant, jobID, worker, now.UTC(), s.leaseDuration)
	if err != nil {
		return err
	}
	lease := Lease{TenantID: tenant, JobID: jobID, Owner: worker, Token: job.LeaseToken, ExpiresAt: job.LeaseExpiresAt}
	if job.Phase == PhasePending {
		for _, target := range job.Targets {
			cleaner, ok := s.cleaners[target.Kind]
			if !ok || !validTarget(target) {
				return ErrInvalid
			}
			if err := cleaner.Delete(ctx, target); err != nil {
				evidenceID, idErr := s.newEvidenceID()
				if idErr != nil {
					return errors.Join(err, idErr)
				}
				return errors.Join(err, s.store.Fail(ctx, lease, evidenceID, "external_delete", now.UTC()))
			}
		}
		evidenceID, idErr := s.newEvidenceID()
		if idErr != nil {
			return idErr
		}
		if err := s.store.CheckpointExternal(ctx, lease, evidenceID, now.UTC()); err != nil {
			return err
		}
		job.Phase = PhaseExternalDeleted
	}
	if job.Phase == PhaseExternalDeleted {
		evidenceID, idErr := s.newEvidenceID()
		if idErr != nil {
			return idErr
		}
		if err := s.store.DeleteSession(ctx, lease, evidenceID, now.UTC()); err != nil {
			failureID, failureIDErr := s.newEvidenceID()
			if failureIDErr != nil {
				return errors.Join(err, failureIDErr)
			}
			return errors.Join(err, s.store.Fail(ctx, lease, failureID, "postgres_delete", now.UTC()))
		}
		job.Phase = PhasePostgresDeleted
	}
	if job.Phase == PhasePostgresDeleted {
		evidenceID, idErr := s.newEvidenceID()
		if idErr != nil {
			return idErr
		}
		return s.store.Complete(ctx, lease, evidenceID, now.UTC())
	}
	if job.Phase == PhaseCompleted {
		return nil
	}
	return ErrConflict
}

func (s *Service) AddHold(ctx context.Context, hold Hold) error {
	if !validContext(ctx) || !validTenant(hold.TenantID) || !validSession(hold.SessionID) ||
		!holdPattern.MatchString(hold.ID) || !codePattern.MatchString(hold.ReasonCode) || hold.CreatedAt.IsZero() {
		return ErrInvalid
	}
	hold.CreatedAt = hold.CreatedAt.UTC()
	return s.store.AddHold(ctx, hold)
}

func (s *Service) ReleaseHold(ctx context.Context, tenant domain.TenantID, session domain.SessionID, holdID string, at time.Time) error {
	if !validContext(ctx) || !validTenant(tenant) || !validSession(session) || !holdPattern.MatchString(holdID) || at.IsZero() {
		return ErrInvalid
	}
	return s.store.ReleaseHold(ctx, tenant, session, holdID, at.UTC())
}

func (s *Service) ListEvidence(ctx context.Context, tenant domain.TenantID, after *Cursor, limit int) (EvidencePage, error) {
	if !validContext(ctx) || !validTenant(tenant) || limit < 1 || limit > 200 ||
		(after != nil && (after.OccurredAt.IsZero() || !eventPattern.MatchString(after.ID))) {
		return EvidencePage{}, ErrInvalid
	}
	return s.store.ListEvidence(ctx, tenant, after, limit)
}

// PurgeEvidence removes expired evidence only for completed deletion jobs.
func (s *Service) PurgeEvidence(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) (int, error) {
	if !validContext(ctx) || !validTenant(tenant) || before.IsZero() || limit < 1 || limit > 1000 {
		return 0, ErrInvalid
	}
	return s.store.PurgeEvidence(ctx, tenant, before.UTC(), limit)
}

func (s *Service) newEvidenceID() (string, error) {
	value, err := s.ids.NewEvidenceID()
	if err != nil {
		return "", err
	}
	if !eventPattern.MatchString(value) {
		return "", ErrInvalid
	}
	return value, nil
}

func validRequest(ctx context.Context, value Request) bool {
	return validContext(ctx) && validTenant(value.TenantID) && validSession(value.SessionID) &&
		jobPattern.MatchString(value.JobID) && value.ActorSHA256 != [sha256.Size]byte{} &&
		codePattern.MatchString(value.PolicyVersion) && !value.RequestedAt.IsZero() &&
		value.EvidenceRetention >= 24*time.Hour && value.EvidenceRetention <= 7*365*24*time.Hour
}

func validTarget(value Target) bool {
	if value.Kind != TargetObject && value.Kind != TargetRedis && value.Kind != TargetIndex {
		return false
	}
	return value.Locator != "" && len(value.Locator) <= 512 && strings.TrimSpace(value.Locator) == value.Locator &&
		!strings.ContainsAny(value.Locator, "\r\n\x00")
}

func validLease(value Lease, now time.Time) bool {
	return validTenant(value.TenantID) && jobPattern.MatchString(value.JobID) && workerPattern.MatchString(value.Owner) &&
		value.Token > 0 && value.ExpiresAt.After(now)
}

func validContext(ctx context.Context) bool { return ctx != nil && ctx.Err() == nil }

func validTenant(value domain.TenantID) bool {
	_, err := domain.ParseTenantID(value.String())
	return err == nil
}

func validSession(value domain.SessionID) bool {
	_, err := domain.ParseSessionID(value.String())
	return err == nil
}
