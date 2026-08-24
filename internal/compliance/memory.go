package compliance

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type memorySession struct {
	targets []Target
	exists  bool
	jobID   string
	hold    *Hold
}

// MemoryStore is a deterministic compliance authority for tests and local use.
type MemoryStore struct {
	mu          sync.Mutex
	sessions    map[string]*memorySession
	jobs        map[string]Job
	evidence    []Evidence
	evidenceIDs map[string]struct{}
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: make(map[string]*memorySession), jobs: make(map[string]Job), evidenceIDs: make(map[string]struct{})}
}

func (s *MemoryStore) SeedSession(tenant domain.TenantID, session domain.SessionID, targets []Target) error {
	if !validTenant(tenant) || !validSession(session) {
		return ErrInvalid
	}
	copyTargets, err := validateTargets(targets)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionKey(tenant, session)
	if current := s.sessions[key]; current != nil && current.exists {
		return ErrConflict
	}
	s.sessions[key] = &memorySession{targets: copyTargets, exists: true}
	return nil
}

func (s *MemoryStore) Request(_ context.Context, request Request) (Job, error) {
	if !validRequest(context.Background(), request) || !eventPattern.MatchString(request.EvidenceID) {
		return Job{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := jobKey(request.TenantID, request.JobID)
	if existing, ok := s.jobs[key]; ok {
		if existing.SessionID != request.SessionID || existing.ActorSHA256 != request.ActorSHA256 ||
			existing.PolicyVersion != request.PolicyVersion || !existing.CreatedAt.Equal(request.RequestedAt.UTC()) ||
			existing.EvidenceRetention != request.EvidenceRetention {
			return Job{}, ErrConflict
		}
		return cloneJob(existing), nil
	}
	session := s.sessions[sessionKey(request.TenantID, request.SessionID)]
	if session == nil || !session.exists {
		return Job{}, ErrNotFound
	}
	if session.hold != nil {
		return Job{}, ErrLegalHold
	}
	if session.jobID != "" && session.jobID != request.JobID {
		return Job{}, ErrConflict
	}
	if err := s.evidenceAvailable(request.TenantID, request.EvidenceID); err != nil {
		return Job{}, err
	}
	job := Job{
		TenantID: request.TenantID, ID: request.JobID, SessionID: request.SessionID,
		ActorSHA256: request.ActorSHA256, PolicyVersion: request.PolicyVersion,
		Targets: cloneTargets(session.targets), Phase: PhasePending, Status: StatusPending,
		CreatedAt: request.RequestedAt.UTC(), UpdatedAt: request.RequestedAt.UTC(),
		EvidenceRetention: request.EvidenceRetention,
	}
	session.jobID = request.JobID
	s.jobs[key] = job
	if err := s.appendEvidence(job, request.EvidenceID, EvidenceRequested, "", request.RequestedAt.UTC()); err != nil {
		return Job{}, err
	}
	return cloneJob(job), nil
}

func (s *MemoryStore) Claim(ctx context.Context, tenant domain.TenantID, jobID, owner string, now time.Time, duration time.Duration) (Job, error) {
	if !validContext(ctx) || !validTenant(tenant) || !jobPattern.MatchString(jobID) || !workerPattern.MatchString(owner) ||
		now.IsZero() || duration < time.Second || duration > time.Hour {
		return Job{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := jobKey(tenant, jobID)
	job, ok := s.jobs[key]
	if !ok {
		return Job{}, ErrNotFound
	}
	if job.Status == StatusCompleted {
		return cloneJob(job), nil
	}
	if job.Status == StatusRunning && job.LeaseExpiresAt.After(now) {
		if job.LeaseOwner != owner {
			return Job{}, ErrConflict
		}
		return cloneJob(job), nil
	}
	job.Status = StatusRunning
	job.AttemptCount++
	job.LeaseOwner = owner
	job.LeaseToken++
	job.LeaseExpiresAt = now.Add(duration)
	job.UpdatedAt = now
	s.jobs[key] = job
	return cloneJob(job), nil
}

func (s *MemoryStore) CheckpointExternal(_ context.Context, lease Lease, evidenceID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, key, err := s.leasedJob(lease, now)
	if err != nil {
		return err
	}
	if job.Phase != PhasePending {
		return nil
	}
	if !eventPattern.MatchString(evidenceID) {
		return ErrInvalid
	}
	if err := s.evidenceAvailable(job.TenantID, evidenceID); err != nil {
		return err
	}
	job.Phase = PhaseExternalDeleted
	job.UpdatedAt = now
	s.jobs[key] = job
	return s.appendEvidence(job, evidenceID, EvidenceExternalDeleted, "", now)
}

func (s *MemoryStore) DeleteSession(_ context.Context, lease Lease, evidenceID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, key, err := s.leasedJob(lease, now)
	if err != nil {
		return err
	}
	if job.Phase == PhasePostgresDeleted || job.Phase == PhaseCompleted {
		return nil
	}
	if job.Phase != PhaseExternalDeleted || !eventPattern.MatchString(evidenceID) {
		return ErrConflict
	}
	if err := s.evidenceAvailable(job.TenantID, evidenceID); err != nil {
		return err
	}
	session := s.sessions[sessionKey(job.TenantID, job.SessionID)]
	if session != nil && session.hold != nil {
		return ErrLegalHold
	}
	if session != nil {
		session.exists = false
	}
	job.Phase = PhasePostgresDeleted
	job.UpdatedAt = now
	s.jobs[key] = job
	return s.appendEvidence(job, evidenceID, EvidencePostgresDeleted, "", now)
}

func (s *MemoryStore) Complete(_ context.Context, lease Lease, evidenceID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, key, err := s.leasedJob(lease, now)
	if err != nil {
		return err
	}
	if job.Status == StatusCompleted {
		return nil
	}
	if job.Phase != PhasePostgresDeleted || !eventPattern.MatchString(evidenceID) {
		return ErrConflict
	}
	if err := s.evidenceAvailable(job.TenantID, evidenceID); err != nil {
		return err
	}
	job.Phase = PhaseCompleted
	job.Status = StatusCompleted
	job.CompletedAt = now
	job.UpdatedAt = now
	job.LeaseOwner = ""
	job.LeaseExpiresAt = time.Time{}
	s.jobs[key] = job
	return s.appendEvidence(job, evidenceID, EvidenceCompleted, "", now)
}

func (s *MemoryStore) Fail(_ context.Context, lease Lease, evidenceID, failureClass string, now time.Time) error {
	if !eventPattern.MatchString(evidenceID) || !codePattern.MatchString(failureClass) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, key, err := s.leasedJob(lease, now)
	if err != nil {
		return err
	}
	if err := s.evidenceAvailable(job.TenantID, evidenceID); err != nil {
		return err
	}
	job.Status = StatusRetry
	job.UpdatedAt = now
	job.LeaseOwner = ""
	job.LeaseExpiresAt = time.Time{}
	s.jobs[key] = job
	return s.appendEvidence(job, evidenceID, EvidenceFailed, failureClass, now)
}

func (s *MemoryStore) AddHold(_ context.Context, hold Hold) error {
	if !validTenant(hold.TenantID) || !validSession(hold.SessionID) || !holdPattern.MatchString(hold.ID) ||
		!codePattern.MatchString(hold.ReasonCode) || hold.CreatedAt.IsZero() {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[sessionKey(hold.TenantID, hold.SessionID)]
	if session == nil || !session.exists {
		return ErrNotFound
	}
	if session.jobID != "" {
		return ErrConflict
	}
	if session.hold != nil {
		normalized := hold
		normalized.CreatedAt = normalized.CreatedAt.UTC()
		if *session.hold == normalized {
			return nil
		}
		return ErrConflict
	}
	copyHold := hold
	copyHold.CreatedAt = copyHold.CreatedAt.UTC()
	session.hold = &copyHold
	return nil
}

func (s *MemoryStore) ReleaseHold(_ context.Context, tenant domain.TenantID, sessionID domain.SessionID, holdID string, _ time.Time) error {
	if !validTenant(tenant) || !validSession(sessionID) || !holdPattern.MatchString(holdID) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[sessionKey(tenant, sessionID)]
	if session == nil || session.hold == nil {
		return ErrNotFound
	}
	if session.hold.ID != holdID {
		return ErrConflict
	}
	session.hold = nil
	return nil
}

func (s *MemoryStore) ListEvidence(_ context.Context, tenant domain.TenantID, after *Cursor, limit int) (EvidencePage, error) {
	if !validTenant(tenant) || limit < 1 || limit > 200 || (after != nil && (after.OccurredAt.IsZero() || !eventPattern.MatchString(after.ID))) {
		return EvidencePage{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Evidence, 0, limit+1)
	for _, item := range s.evidence {
		if item.TenantID != tenant || (after != nil && !cursorAfter(item, *after)) {
			continue
		}
		items = append(items, item)
	}
	slices.SortFunc(items, func(left, right Evidence) int {
		if value := left.OccurredAt.Compare(right.OccurredAt); value != 0 {
			return value
		}
		return strings.Compare(left.ID, right.ID)
	})
	page := EvidencePage{}
	if len(items) > limit {
		cursor := Cursor{OccurredAt: items[limit-1].OccurredAt, ID: items[limit-1].ID}
		page.NextCursor = &cursor
		items = items[:limit]
	}
	page.Items = append([]Evidence(nil), items...)
	return page, nil
}

func (s *MemoryStore) PurgeEvidence(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) (int, error) {
	if !validContext(ctx) || !validTenant(tenant) || before.IsZero() || limit < 1 || limit > 1000 {
		return 0, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	kept := make([]Evidence, 0, len(s.evidence))
	for _, item := range s.evidence {
		job := s.jobs[jobKey(item.TenantID, item.JobID)]
		if removed < limit && item.TenantID == tenant && !item.ExpiresAt.After(before) && job.Status == StatusCompleted {
			delete(s.evidenceIDs, item.TenantID.String()+"\x00"+item.ID)
			removed++
			continue
		}
		kept = append(kept, item)
	}
	s.evidence = kept
	return removed, nil
}

func (s *MemoryStore) leasedJob(lease Lease, now time.Time) (Job, string, error) {
	if !validLease(lease, now) {
		return Job{}, "", ErrLeaseLost
	}
	key := jobKey(lease.TenantID, lease.JobID)
	job, ok := s.jobs[key]
	if !ok {
		return Job{}, "", ErrNotFound
	}
	if job.Status != StatusRunning || job.LeaseOwner != lease.Owner || job.LeaseToken != lease.Token ||
		job.LeaseExpiresAt != lease.ExpiresAt || !job.LeaseExpiresAt.After(now) {
		return Job{}, "", ErrLeaseLost
	}
	return job, key, nil
}

func (s *MemoryStore) appendEvidence(job Job, id string, kind EvidenceType, failure string, at time.Time) error {
	key := job.TenantID.String() + "\x00" + id
	s.evidenceIDs[key] = struct{}{}
	s.evidence = append(s.evidence, Evidence{
		TenantID: job.TenantID, ID: id, JobID: job.ID, SessionID: job.SessionID,
		Type: kind, Phase: job.Phase, FailureClass: failure, ActorSHA256: job.ActorSHA256,
		PolicyVersion: job.PolicyVersion, TargetCount: len(job.Targets), OccurredAt: at,
		ExpiresAt: at.Add(job.EvidenceRetention),
	})
	return nil
}

func (s *MemoryStore) evidenceAvailable(tenant domain.TenantID, id string) error {
	if !eventPattern.MatchString(id) {
		return ErrInvalid
	}
	if _, exists := s.evidenceIDs[tenant.String()+"\x00"+id]; exists {
		return ErrConflict
	}
	return nil
}

func validateTargets(values []Target) ([]Target, error) {
	if len(values) > 10_000 {
		return nil, ErrInvalid
	}
	result := cloneTargets(values)
	for _, value := range result {
		if !validTarget(value) {
			return nil, ErrInvalid
		}
	}
	slices.SortFunc(result, func(left, right Target) int {
		if left.Kind != right.Kind {
			return strings.Compare(string(left.Kind), string(right.Kind))
		}
		return strings.Compare(left.Locator, right.Locator)
	})
	result = slices.Compact(result)
	return result, nil
}

func cloneJob(value Job) Job {
	value.Targets = cloneTargets(value.Targets)
	return value
}

func cloneTargets(values []Target) []Target { return append([]Target(nil), values...) }

func cursorAfter(item Evidence, cursor Cursor) bool {
	return item.OccurredAt.After(cursor.OccurredAt) || (item.OccurredAt.Equal(cursor.OccurredAt) && item.ID > cursor.ID)
}

func jobKey(tenant domain.TenantID, jobID string) string { return tenant.String() + "\x00" + jobID }

func sessionKey(tenant domain.TenantID, session domain.SessionID) string {
	return tenant.String() + "\x00" + session.String()
}

var _ Store = (*MemoryStore)(nil)
