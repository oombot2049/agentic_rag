package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestIdempotencyConcurrentClaimReplayAndReclaim(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	migrator, err := NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	repository, err := idempotency.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	scope := idempotency.Scope{
		TenantID: domain.TenantID("ten_0001"), AuthenticatedSubjectID: "user-42",
		Operation: "CREATE_TURN", Key: "idem_0123456789abcdef",
	}
	fingerprint, err := idempotency.NewFingerprint(idempotency.Request{
		Operation: scope.Operation, PathParameters: map[string]string{"session_id": "ses_0001"},
		Body: map[string]any{"expected_context_version": 0, "max_steps": 12},
	})
	if err != nil {
		t.Fatal(err)
	}

	const claimers = 40
	start := make(chan struct{})
	results := make(chan idempotency.ClaimResult, claimers)
	errorsFound := make(chan error, claimers)
	var wait sync.WaitGroup
	for range claimers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := repository.Claim(ctx, scope, fingerprint, testIdempotencyTime, idempotency.MinimumRetention)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent Claim: %v", err)
	}
	decisions := map[idempotency.Decision]int{}
	for result := range results {
		decisions[result.Decision]++
	}
	if decisions[idempotency.DecisionAcquired] != 1 || decisions[idempotency.DecisionInProgress] != claimers-1 {
		t.Fatalf("concurrent decisions = %v", decisions)
	}

	response := idempotency.Response{
		StatusCode: 202, Body: json.RawMessage(`{"run_id":"run_0001","turn_id":"trn_0001"}`),
		Location: "/v1/runs/run_0001", ResourceType: "RUN", ResourceID: "run_0001",
	}
	if err := repository.CompleteSucceeded(ctx, scope, fingerprint, response, testIdempotencyTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	replay, err := repository.Claim(ctx, scope, fingerprint, testIdempotencyTime.Add(2*time.Second), idempotency.MinimumRetention)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Decision != idempotency.DecisionReplaySucceeded || replay.Record.Response == nil || replay.Record.Response.StatusCode != 202 || replay.Record.Response.ResourceID != "run_0001" {
		t.Fatalf("successful replay = %+v", replay)
	}
	if !jsonEqual(replay.Record.Response.Body, response.Body) {
		t.Fatalf("replay body = %s", replay.Record.Response.Body)
	}

	otherHash, err := idempotency.NewFingerprint(idempotency.Request{Operation: scope.Operation, Body: map[string]any{"different": true}})
	if err != nil {
		t.Fatal(err)
	}
	reused, err := repository.Claim(ctx, scope, otherHash, testIdempotencyTime.Add(3*time.Second), idempotency.MinimumRetention)
	if err != nil {
		t.Fatal(err)
	}
	if reused.Decision != idempotency.DecisionKeyReused {
		t.Fatalf("different hash decision = %s", reused.Decision)
	}
	if err := repository.CompleteSucceeded(ctx, scope, fingerprint, response, testIdempotencyTime.Add(4*time.Second)); !errors.Is(err, idempotency.ErrClaimLost) {
		t.Fatalf("repeat completion error = %v", err)
	}

	failureScope := scope
	failureScope.Key = "idem_failure_0123456789"
	failureHash, err := idempotency.NewFingerprint(idempotency.Request{Operation: scope.Operation, Body: map[string]any{"failure": true}})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := repository.Claim(ctx, failureScope, failureHash, testIdempotencyTime, idempotency.MinimumRetention); err != nil || result.Decision != idempotency.DecisionAcquired {
		t.Fatalf("failure claim = %+v/%v", result, err)
	}
	failureResponse := idempotency.Response{StatusCode: 409, Body: json.RawMessage(`{"error":{"code":"SESSION_BUSY"}}`)}
	if err := repository.CompleteFailedFinal(ctx, failureScope, failureHash, failureResponse, testIdempotencyTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if result, err := repository.Claim(ctx, failureScope, failureHash, testIdempotencyTime.Add(2*time.Second), idempotency.MinimumRetention); err != nil || result.Decision != idempotency.DecisionReplayFailedFinal {
		t.Fatalf("failure replay = %+v/%v", result, err)
	}

	expiredScope := scope
	expiredScope.Key = "idem_expired_0123456789"
	if result, err := repository.Claim(ctx, expiredScope, fingerprint, testIdempotencyTime, idempotency.MinimumRetention); err != nil || result.Decision != idempotency.DecisionAcquired {
		t.Fatalf("initial expiring claim = %+v/%v", result, err)
	}
	reclaimed, err := repository.Claim(ctx, expiredScope, otherHash, testIdempotencyTime.Add(idempotency.MinimumRetention), idempotency.MinimumRetention)
	if err != nil || reclaimed.Decision != idempotency.DecisionAcquired || reclaimed.Record.Fingerprint != otherHash {
		t.Fatalf("reclaimed = %+v/%v", reclaimed, err)
	}
}

var testIdempotencyTime = time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)

func jsonEqual(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		deepEqualJSON(leftValue, rightValue)
}

func deepEqualJSON(left, right any) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}
