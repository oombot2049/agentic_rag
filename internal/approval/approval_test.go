package approval

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/tools"
)

var approvalTime = time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)

func TestApprovalExactReplayApproveAndQuery(t *testing.T) {
	service := testService(t)
	request := validRequest()
	created, err := service.Request(context.Background(), request)
	if err != nil || created.Approval.Status != StatusWaiting || created.Nonce == "" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	replay, err := service.Request(context.Background(), request)
	if err != nil || replay.Nonce != created.Nonce || replay.Approval.ID != created.Approval.ID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	approved, err := service.Decide(context.Background(), Decision{TenantID: request.TenantID, ApprovalID: request.ID,
		ActorSubject: request.ApproverSubject, Nonce: created.Nonce, ExpectedRowVersion: 1,
		Approve: true, At: approvalTime.Add(time.Minute)})
	if err != nil || approved.Status != StatusApproved || approved.RowVersion != 2 {
		t.Fatalf("approved=%+v err=%v", approved, err)
	}
	exact, err := service.Decide(context.Background(), Decision{TenantID: request.TenantID, ApprovalID: request.ID,
		ActorSubject: request.ApproverSubject, Nonce: created.Nonce, ExpectedRowVersion: 1,
		Approve: true, At: approvalTime.Add(2 * time.Minute)})
	if err != nil || exact.Status != StatusApproved {
		t.Fatalf("decision replay=%+v err=%v", exact, err)
	}
	loaded, err := service.Get(context.Background(), request.TenantID, request.ID)
	if err != nil || loaded.DecisionActor != request.ApproverSubject {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestApprovalRejectExpiryAndFailClosedDecisions(t *testing.T) {
	tests := []struct {
		name       string
		decision   func(Created, Request) Decision
		wantStatus Status
		wantErr    error
	}{
		{"reject", func(c Created, r Request) Decision { return decisionFor(c, r, false, approvalTime.Add(time.Minute)) }, StatusRejected, nil},
		{"expiry boundary", func(c Created, r Request) Decision { return decisionFor(c, r, true, c.Approval.ExpiresAt) }, StatusExpired, nil},
		{"wrong actor", func(c Created, r Request) Decision {
			d := decisionFor(c, r, true, approvalTime.Add(time.Minute))
			d.ActorSubject = "other"
			return d
		}, "", ErrDenied},
		{"wrong nonce", func(c Created, r Request) Decision {
			d := decisionFor(c, r, true, approvalTime.Add(time.Minute))
			d.Nonce = "apn_wrong"
			return d
		}, "", ErrDenied},
		{"stale", func(c Created, r Request) Decision {
			d := decisionFor(c, r, true, approvalTime.Add(time.Minute))
			d.ExpectedRowVersion = 9
			return d
		}, "", ErrStale},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := testService(t)
			request := validRequest()
			created, _ := service.Request(context.Background(), request)
			result, err := service.Decide(context.Background(), test.decision(created, request))
			if !errors.Is(err, test.wantErr) || (test.wantErr == nil && result.Status != test.wantStatus) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestApprovalRequestMutationTenantIsolationAndValidation(t *testing.T) {
	service := testService(t)
	request := validRequest()
	created, _ := service.Request(context.Background(), request)
	mutated := request
	mutated.ToolVersion = "2.0.0"
	if _, err := service.Request(context.Background(), mutated); !errors.Is(err, ErrConflict) {
		t.Fatalf("mutation=%v", err)
	}
	if _, err := service.Get(context.Background(), "ten_9999", created.Approval.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign tenant=%v", err)
	}
	invalid := request
	invalid.RequesterSubject = invalid.ApproverSubject
	if _, err := service.Request(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self approval=%v", err)
	}
	invalid = request
	invalid.TTL = time.Second
	if _, err := service.Request(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("TTL=%v", err)
	}
	if _, err := NewNonceDeriver([]byte("short")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("secret=%v", err)
	}
}

func TestApprovalConcurrentDecisionHasOneTerminalWinner(t *testing.T) {
	service := testService(t)
	request := validRequest()
	created, _ := service.Request(context.Background(), request)
	var wait sync.WaitGroup
	results := make(chan Status, 32)
	for index := range 32 {
		wait.Add(1)
		go func(approve bool) {
			defer wait.Done()
			value, err := service.Decide(context.Background(), decisionFor(created, request, approve, approvalTime.Add(time.Minute)))
			if err == nil {
				results <- value.Status
			}
		}(index%2 == 0)
	}
	wait.Wait()
	close(results)
	var winner Status
	for status := range results {
		if winner == "" {
			winner = status
		}
		if status != winner {
			t.Fatalf("multiple terminal outcomes: %s/%s", winner, status)
		}
	}
	if winner != StatusApproved && winner != StatusRejected {
		t.Fatalf("winner=%s", winner)
	}
}

func validRequest() Request {
	hash := sha256.Sum256([]byte(`{"target":"record"}`))
	return Request{TenantID: "ten_0001", ID: "apr_0001", RequesterSubject: "user-requester", ApproverSubject: "user-approver",
		SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", StepID: "stp_0001", AttemptID: "att_0001",
		ToolCallID: "tcl_0001", ToolName: "record.delete", ToolVersion: "1.0.0", Effect: tools.EffectDestructive,
		ArgumentsHash: hash, At: approvalTime, TTL: time.Hour,
		WorkerOwner: "worker-01", LeaseToken: 7, ExecutionVersion: 3}
}
func decisionFor(created Created, request Request, approve bool, at time.Time) Decision {
	return Decision{TenantID: request.TenantID, ApprovalID: request.ID, ActorSubject: request.ApproverSubject,
		Nonce: created.Nonce, ExpectedRowVersion: created.Approval.RowVersion, Approve: approve, At: at}
}
func testService(t *testing.T) *Service {
	t.Helper()
	nonces, _ := NewNonceDeriver([]byte("01234567890123456789012345678901"))
	service, err := NewService(NewMemoryStore(), nonces)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
