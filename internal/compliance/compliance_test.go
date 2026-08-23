package compliance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var complianceTime = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func TestDeletionRetriesAcrossAllTargetsAndPreservesEvidence(t *testing.T) {
	store := NewMemoryStore()
	targets := []Target{
		{Kind: TargetObject, Locator: "tenants/ten_0001/artifacts/art_0001"},
		{Kind: TargetRedis, Locator: "ar:test:{ten_0001}:cache:session:ses_0001"},
		{Kind: TargetIndex, Locator: "session/ten_0001/ses_0001"},
		{Kind: TargetObject, Locator: "tenants/ten_0001/artifacts/art_0001"},
	}
	if err := store.SeedSession("ten_0001", "ses_0001", targets); err != nil {
		t.Fatal(err)
	}
	cleaners := newCleanerSet(targets)
	cleaners[TargetObject].failures = 1
	service := newTestService(t, store, cleaners)
	job, err := service.Request(context.Background(), deletionRequest("ten_0001", "ses_0001", "del_0001"))
	if err != nil || len(job.Targets) != 3 {
		t.Fatalf("Request() = %+v, %v", job, err)
	}
	providerFailure := errors.New("provider secret failure")
	cleaners[TargetObject].failure = providerFailure
	if err := service.Process(context.Background(), "ten_0001", "del_0001", "worker-a", complianceTime); !errors.Is(err, providerFailure) {
		t.Fatalf("first Process() error = %v", err)
	}
	if err := service.Process(context.Background(), "ten_0001", "del_0001", "worker-b", complianceTime.Add(time.Second)); err != nil {
		t.Fatalf("retry Process() error = %v", err)
	}
	if err := service.Process(context.Background(), "ten_0001", "del_0001", "worker-c", complianceTime.Add(2*time.Second)); err != nil {
		t.Fatalf("completed replay error = %v", err)
	}
	for kind, cleaner := range cleaners {
		if remaining := cleaner.remaining(); remaining != 0 {
			t.Errorf("%s remaining targets = %d", kind, remaining)
		}
	}
	if _, err := service.Request(context.Background(), deletionRequest("ten_0001", "ses_0001", "del_other")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted Session request error = %v", err)
	}
	page, err := service.ListEvidence(context.Background(), "ten_0001", nil, 2)
	if err != nil || len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("first evidence page = %+v, %v", page, err)
	}
	second, err := service.ListEvidence(context.Background(), "ten_0001", page.NextCursor, 10)
	if err != nil || len(second.Items) != 3 {
		t.Fatalf("second evidence page = %+v, %v", second, err)
	}
	all := append(page.Items, second.Items...)
	want := []EvidenceType{EvidenceRequested, EvidenceFailed, EvidenceExternalDeleted, EvidencePostgresDeleted, EvidenceCompleted}
	for index, item := range all {
		if item.Type != want[index] || item.ActorSHA256 == [sha256.Size]byte{} || !item.ExpiresAt.Equal(item.OccurredAt.Add(30*24*time.Hour)) {
			t.Errorf("evidence[%d] = %+v", index, item)
		}
	}
}

func TestLegalHoldAndDeletionAreMutuallyExclusive(t *testing.T) {
	store := NewMemoryStore()
	if err := store.SeedSession("ten_0001", "ses_0001", nil); err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, store, newCleanerSet(nil))
	hold := Hold{TenantID: "ten_0001", SessionID: "ses_0001", ID: "hld_0001", ReasonCode: "litigation", CreatedAt: complianceTime}
	if err := service.AddHold(context.Background(), hold); err != nil {
		t.Fatal(err)
	}
	if err := service.AddHold(context.Background(), hold); err != nil {
		t.Fatalf("hold replay error = %v", err)
	}
	if _, err := service.Request(context.Background(), deletionRequest("ten_0001", "ses_0001", "del_0001")); !errors.Is(err, ErrLegalHold) {
		t.Fatalf("held Request() error = %v", err)
	}
	if err := service.ReleaseHold(context.Background(), "ten_0001", "ses_0001", "hld_0001", complianceTime); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Request(context.Background(), deletionRequest("ten_0001", "ses_0001", "del_0001")); err != nil {
		t.Fatal(err)
	}
	if err := service.AddHold(context.Background(), Hold{TenantID: "ten_0001", SessionID: "ses_0001", ID: "hld_0002", ReasonCode: "investigation", CreatedAt: complianceTime}); !errors.Is(err, ErrConflict) {
		t.Fatalf("late AddHold() error = %v", err)
	}
}

func TestLeaseTakeoverFencesStaleWorker(t *testing.T) {
	store := NewMemoryStore()
	if err := store.SeedSession("ten_0001", "ses_0001", nil); err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, store, newCleanerSet(nil))
	if _, err := service.Request(context.Background(), deletionRequest("ten_0001", "ses_0001", "del_0001")); err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(context.Background(), "ten_0001", "del_0001", "worker-a", complianceTime, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(context.Background(), "ten_0001", "del_0001", "worker-b", complianceTime, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent Claim() error = %v", err)
	}
	second, err := store.Claim(context.Background(), "ten_0001", "del_0001", "worker-b", complianceTime.Add(time.Minute), time.Minute)
	if err != nil || second.LeaseToken <= first.LeaseToken {
		t.Fatalf("takeover Claim() = %+v, %v", second, err)
	}
	oldLease := Lease{"ten_0001", "del_0001", "worker-a", first.LeaseToken, first.LeaseExpiresAt}
	if err := store.CheckpointExternal(context.Background(), oldLease, "cev_stale", complianceTime.Add(time.Minute)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale CheckpointExternal() error = %v", err)
	}
	newLease := Lease{"ten_0001", "del_0001", "worker-b", second.LeaseToken, second.LeaseExpiresAt}
	if err := store.CheckpointExternal(context.Background(), newLease, "cev_current", complianceTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestRequestReplayConflictAndTenantIsolation(t *testing.T) {
	store := NewMemoryStore()
	for _, tenant := range []domain.TenantID{"ten_0001", "ten_0002"} {
		if err := store.SeedSession(tenant, "ses_0001", nil); err != nil {
			t.Fatal(err)
		}
	}
	service := newTestService(t, store, newCleanerSet(nil))
	first := deletionRequest("ten_0001", "ses_0001", "del_same")
	job, err := service.Request(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Request(context.Background(), first)
	if err != nil || replay.ID != job.ID {
		t.Fatalf("Request() replay = %+v, %v", replay, err)
	}
	changed := first
	changed.PolicyVersion = "policy-v2"
	if _, err := service.Request(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed Request() error = %v", err)
	}
	changed = first
	changed.EvidenceRetention = 31 * 24 * time.Hour
	if _, err := service.Request(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retention Request() error = %v", err)
	}
	if _, err := service.Request(context.Background(), deletionRequest("ten_0002", "ses_0001", "del_same")); err != nil {
		t.Fatalf("other tenant Request() error = %v", err)
	}
	page, err := service.ListEvidence(context.Background(), "ten_0002", nil, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].TenantID != "ten_0002" {
		t.Fatalf("tenant evidence = %+v, %v", page, err)
	}
}

func TestEvidenceRetentionPurgesOnlyCompletedJobs(t *testing.T) {
	store := NewMemoryStore()
	for _, session := range []domain.SessionID{"ses_done", "ses_pending"} {
		if err := store.SeedSession("ten_0001", session, nil); err != nil {
			t.Fatal(err)
		}
	}
	service := newTestService(t, store, newCleanerSet(nil))
	if _, err := service.Request(context.Background(), deletionRequest("ten_0001", "ses_done", "del_done")); err != nil {
		t.Fatal(err)
	}
	if err := service.Process(context.Background(), "ten_0001", "del_done", "worker-a", complianceTime); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Request(context.Background(), deletionRequest("ten_0001", "ses_pending", "del_pending")); err != nil {
		t.Fatal(err)
	}
	removed, err := service.PurgeEvidence(context.Background(), "ten_0001", complianceTime.Add(31*24*time.Hour), 100)
	if err != nil || removed != 4 {
		t.Fatalf("PurgeEvidence() = %d, %v", removed, err)
	}
	page, err := service.ListEvidence(context.Background(), "ten_0001", nil, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].JobID != "del_pending" {
		t.Fatalf("remaining evidence = %+v, %v", page, err)
	}
}

type recordingCleaner struct {
	mu       sync.Mutex
	values   map[string]struct{}
	failures int
	failure  error
}

func (c *recordingCleaner) Delete(_ context.Context, target Target) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures > 0 {
		c.failures--
		return c.failure
	}
	delete(c.values, target.Locator)
	return nil
}

func (c *recordingCleaner) remaining() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.values)
}

func newCleanerSet(targets []Target) map[TargetKind]*recordingCleaner {
	result := map[TargetKind]*recordingCleaner{
		TargetObject: {values: map[string]struct{}{}},
		TargetRedis:  {values: map[string]struct{}{}},
		TargetIndex:  {values: map[string]struct{}{}},
	}
	for _, target := range targets {
		result[target.Kind].values[target.Locator] = struct{}{}
	}
	return result
}

func newTestService(t *testing.T, store Store, values map[TargetKind]*recordingCleaner) *Service {
	t.Helper()
	cleaners := make(map[TargetKind]Cleaner, len(values))
	for kind, cleaner := range values {
		cleaners[kind] = cleaner
	}
	var sequence atomic.Uint64
	service, err := NewService(store, cleaners, IDGeneratorFunc(func() (string, error) {
		return fmt.Sprintf("cev_%04d", sequence.Add(1)), nil
	}), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func deletionRequest(tenant domain.TenantID, session domain.SessionID, jobID string) Request {
	return Request{
		TenantID: tenant, JobID: jobID, SessionID: session, ActorSHA256: sha256.Sum256([]byte("subject")),
		PolicyVersion: "policy-v1", RequestedAt: complianceTime, EvidenceRetention: 30 * 24 * time.Hour,
	}
}
