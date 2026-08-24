package audit

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
)

func TestServiceAppendQueriesAndDefensiveCopies(t *testing.T) {
	store := NewMemoryStore()
	service, err := NewService(store, RetentionPolicyFunc(func(context.Context, domain.TenantID) (time.Duration, error) {
		return 30 * 24 * time.Hour, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return at }
	first := validRecord()
	if err = service.Append(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	first.Attempts[0].FailureClass = "mutated"
	first.Artifact.MediaType = "text/html"
	got, err := service.GetByToolCall(context.Background(), "ten_0001", "tcl_0001")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != at || got.ExpiresAt != at.Add(30*24*time.Hour) || got.Attempts[0].FailureClass != "" || got.Artifact.MediaType != "application/json" {
		t.Fatalf("stored record=%+v", got)
	}
	second := validRecord()
	second.ID, second.ToolCallID = "aud_0002", "tcl_0002"
	service.now = func() time.Time { return at.Add(time.Second) }
	if err = service.Append(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	page, err := service.ListByRun(context.Background(), "ten_0001", "run_0001", nil, 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil || page.Items[0].ID != "aud_0001" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	page, err = service.ListByRun(context.Background(), "ten_0001", "run_0001", page.NextCursor, 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor != nil || page.Items[0].ID != "aud_0002" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err = service.GetByToolCall(context.Background(), "ten_9999", "tcl_0001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross tenant=%v", err)
	}
	if err = service.Append(context.Background(), second); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate=%v", err)
	}
	sameID := validRecord()
	sameID.ToolCallID = "tcl_0003"
	if err = service.Append(context.Background(), sameID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate audit ID=%v", err)
	}
}

func TestValidateRejectsUnsafeAndInconsistentShapes(t *testing.T) {
	base := validRecord()
	base.CreatedAt = base.EndedAt.Add(time.Second)
	base.ExpiresAt = base.CreatedAt.Add(time.Hour)
	tests := []func(*Record){
		func(v *Record) { v.ID = "bad" },
		func(v *Record) { v.CallerSubject = "user\nsecret" },
		func(v *Record) { v.ToolName = "Bad Tool" },
		func(v *Record) { v.PolicyDecision = "MAYBE" },
		func(v *Record) { v.ArgumentsSHA256 = [sha256.Size]byte{} },
		func(v *Record) { v.ResultSHA256 = [sha256.Size]byte{} },
		func(v *Record) { v.Artifact.SHA256 = sha256.Sum256([]byte("other")) },
		func(v *Record) { v.Artifact.MediaType = "bad\nvalue" },
		func(v *Record) { v.BusinessImpactCode = "free form" },
		func(v *Record) { v.Attempts[0].AttemptNo = 2 },
		func(v *Record) { v.Attempts[0].Status = domain.AttemptStatusFailed },
		func(v *Record) { v.Attempts[0].EndedAt = v.StartedAt.Add(-time.Second) },
		func(v *Record) { v.Attempts[0].StartedAt = v.StartedAt.Add(time.Millisecond) },
		func(v *Record) { v.Attempts[0].EndedAt = v.EndedAt.Add(-time.Millisecond) },
		func(v *Record) { v.ExpiresAt = v.CreatedAt.Add(time.Minute) },
	}
	for i, mutate := range tests {
		value := cloneRecord(base)
		mutate(&value)
		if err := Validate(value); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestDeniedFailedAndUnknownShapes(t *testing.T) {
	denied := validRecord()
	denied.PolicyDecision, denied.Outcome, denied.BusinessImpactCode = DecisionDeny, OutcomeDenied, "policy.denied"
	denied.Attempts, denied.Artifact = nil, nil
	denied.ResultSHA256, denied.ResultSizeBytes = [sha256.Size]byte{}, 0
	denied.EndedAt = denied.StartedAt
	denied.CreatedAt, denied.ExpiresAt = denied.EndedAt.Add(time.Second), denied.EndedAt.Add(time.Hour+time.Second)
	if err := Validate(denied); err != nil {
		t.Fatal(err)
	}
	failed := validRecord()
	failed.Outcome, failed.Artifact = OutcomeFailed, nil
	failed.ResultSHA256, failed.ResultSizeBytes = [sha256.Size]byte{}, 0
	failed.Attempts[0].Status, failed.Attempts[0].FailureClass = domain.AttemptStatusFailed, "provider_error"
	failed.CreatedAt, failed.ExpiresAt = failed.EndedAt.Add(time.Second), failed.EndedAt.Add(time.Hour+time.Second)
	if err := Validate(failed); err != nil {
		t.Fatal(err)
	}
	failed.Outcome, failed.Attempts[0].Status = OutcomeUnknown, domain.AttemptStatusUnknown
	if err := Validate(failed); err != nil {
		t.Fatal(err)
	}
}

func TestServiceInputRetentionPolicyAndErrors(t *testing.T) {
	if _, err := NewService(nil, RetentionPolicyFunc(func(context.Context, domain.TenantID) (time.Duration, error) { return time.Hour, nil })); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	policyErr := errors.New("policy")
	service, _ := NewService(store, RetentionPolicyFunc(func(context.Context, domain.TenantID) (time.Duration, error) { return 0, policyErr }))
	if err := service.Append(context.Background(), validRecord()); !errors.Is(err, policyErr) {
		t.Fatal(err)
	}
	service, _ = NewService(store, RetentionPolicyFunc(func(context.Context, domain.TenantID) (time.Duration, error) { return time.Minute, nil }))
	if err := service.Append(context.Background(), validRecord()); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.GetByToolCall(canceled, "ten_0001", "tcl_0001"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := service.ListByRun(context.Background(), "ten_0001", "run_0001", nil, 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func validRecord() Record {
	started := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	resultHash := sha256.Sum256([]byte(`{"answer":42}`))
	return Record{
		TenantID: "ten_0001", ID: "aud_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", StepID: "stp_0001",
		ToolCallID: "tcl_0001", CallerSubject: "usr:alice", ToolName: "knowledge.search", ToolVersion: "1.0.0",
		Effect: tools.EffectReadOnly, PolicyDecision: DecisionAllow, ArgumentsSHA256: sha256.Sum256([]byte("args")), Outcome: OutcomeSucceeded,
		ResultSHA256: resultHash, ResultSizeBytes: 13, BusinessImpactCode: "knowledge.read",
		Artifact:  &ArtifactReference{ID: "art_0001", SHA256: resultHash, SizeBytes: 13, MediaType: "application/json"},
		Attempts:  []Attempt{{AttemptID: "att_0001", AttemptNo: 1, Status: domain.AttemptStatusSucceeded, StartedAt: started, EndedAt: started.Add(time.Second)}},
		StartedAt: started, EndedAt: started.Add(time.Second), CreatedAt: started.Add(2 * time.Second), ExpiresAt: started.Add(time.Hour + 2*time.Second),
	}
}
