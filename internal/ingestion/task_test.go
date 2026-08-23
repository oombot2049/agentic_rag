package ingestion

import (
	"errors"
	"testing"
	"time"
)

var ingestionTime = time.Date(2026, 8, 23, 6, 0, 0, 0, time.UTC)

func TestTaskRetryLifecycleAndAttemptBound(t *testing.T) {
	task, err := NewTask("ing_0001", 2, ingestionTime)
	if err != nil {
		t.Fatal(err)
	}
	if err = task.Start(ingestionTime); err != nil {
		t.Fatal(err)
	}
	if err = task.ScheduleRetry("SOURCE_TIMEOUT", time.Second, ingestionTime); err != nil {
		t.Fatal(err)
	}
	if err = task.Ready(ingestionTime.Add(time.Millisecond)); !errors.Is(err, ErrTransition) {
		t.Fatal("early retry accepted")
	}
	if err = task.Ready(ingestionTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = task.Start(ingestionTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = task.ScheduleRetry("AGAIN", time.Second, ingestionTime.Add(time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatal("attempt overflow accepted")
	}
	if err = task.Fail("ATTEMPTS_EXHAUSTED", ingestionTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if task.Status() != TaskFailed || task.Attempt() != 2 || task.MaxAttempts() != 2 || task.RowVersion() != 6 || task.ReasonCode() != "ATTEMPTS_EXHAUSTED" || task.ID() != "ing_0001" {
		t.Fatalf("task=%+v", task)
	}
	if err = task.Start(ingestionTime.Add(3 * time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatal("terminal reopened")
	}
}
func TestTaskValidationAndCompletion(t *testing.T) {
	for _, test := range []struct {
		id       string
		attempts uint32
		at       time.Time
	}{{"bad", 1, ingestionTime}, {"ing_0001", 0, ingestionTime}, {"ing_0001", 21, ingestionTime}, {"ing_0001", 1, time.Time{}}} {
		if _, err := NewTask(test.id, test.attempts, test.at); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %+v: %v", test, err)
		}
	}
	task, _ := NewTask("ing_0001", 1, ingestionTime)
	if err := task.Complete(ingestionTime); !errors.Is(err, ErrTransition) {
		t.Fatal("queued completed")
	}
	_ = task.Start(ingestionTime)
	if err := task.Complete(ingestionTime); err != nil {
		t.Fatal(err)
	}
	if task.Status() != TaskCompleted || !task.NotBefore().IsZero() {
		t.Fatal("not completed")
	}
}
func TestTransientPreservesCause(t *testing.T) {
	sentinel := errors.New("network")
	wrapped := Transient(sentinel)
	if !errors.Is(wrapped, sentinel) || !retryable(wrapped) || Transient(nil) != nil {
		t.Fatal("transient classification failed")
	}
}
