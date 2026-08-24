// Package ingestion implements retry-safe asynchronous document ingestion.
package ingestion

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

var (
	ErrInvalid    = errors.New("invalid ingestion input")
	ErrTransition = errors.New("invalid ingestion task transition")
)
var reasonPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

type TaskStatus string

const (
	TaskQueued    TaskStatus = "QUEUED"
	TaskRunning   TaskStatus = "RUNNING"
	TaskRetryWait TaskStatus = "RETRY_WAIT"
	TaskCompleted TaskStatus = "COMPLETED"
	TaskFailed    TaskStatus = "FAILED"
)

type Task struct {
	id                               string
	status                           TaskStatus
	attempt, maxAttempts, rowVersion uint32
	notBefore                        time.Time
	reasonCode                       string
	createdAt, updatedAt             time.Time
}

func NewTask(id string, maxAttempts uint32, at time.Time) (*Task, error) {
	if !validTaskID(id) || maxAttempts < 1 || maxAttempts > 20 || at.IsZero() {
		return nil, fmt.Errorf("%w: task identity, attempts or time", ErrInvalid)
	}
	at = at.UTC()
	return &Task{id: id, status: TaskQueued, maxAttempts: maxAttempts, rowVersion: 1, createdAt: at, updatedAt: at}, nil
}
func (t *Task) Start(at time.Time) error {
	if t.status != TaskQueued || t.attempt >= t.maxAttempts {
		return ErrTransition
	}
	at, err := taskTime(t.updatedAt, at)
	if err != nil {
		return err
	}
	t.status = TaskRunning
	t.attempt++
	t.reasonCode = ""
	t.notBefore = time.Time{}
	t.rowVersion++
	t.updatedAt = at
	return nil
}
func (t *Task) ScheduleRetry(reason string, delay time.Duration, at time.Time) error {
	if t.status != TaskRunning || t.attempt >= t.maxAttempts || !reasonPattern.MatchString(reason) || delay <= 0 || delay > 24*time.Hour {
		return ErrTransition
	}
	at, err := taskTime(t.updatedAt, at)
	if err != nil {
		return err
	}
	t.status = TaskRetryWait
	t.reasonCode = reason
	t.notBefore = at.Add(delay)
	t.rowVersion++
	t.updatedAt = at
	return nil
}
func (t *Task) Ready(at time.Time) error {
	if t.status != TaskRetryWait {
		return ErrTransition
	}
	at, err := taskTime(t.updatedAt, at)
	if err != nil {
		return err
	}
	if at.Before(t.notBefore) {
		return ErrTransition
	}
	t.status = TaskQueued
	t.rowVersion++
	t.updatedAt = at
	return nil
}
func (t *Task) Complete(at time.Time) error {
	if t.status != TaskRunning {
		return ErrTransition
	}
	at, err := taskTime(t.updatedAt, at)
	if err != nil {
		return err
	}
	t.status = TaskCompleted
	t.reasonCode = ""
	t.rowVersion++
	t.updatedAt = at
	return nil
}
func (t *Task) Fail(reason string, at time.Time) error {
	if t.status != TaskRunning || !reasonPattern.MatchString(reason) {
		return ErrTransition
	}
	at, err := taskTime(t.updatedAt, at)
	if err != nil {
		return err
	}
	t.status = TaskFailed
	t.reasonCode = reason
	t.rowVersion++
	t.updatedAt = at
	return nil
}
func (t *Task) ID() string           { return t.id }
func (t *Task) Status() TaskStatus   { return t.status }
func (t *Task) Attempt() uint32      { return t.attempt }
func (t *Task) MaxAttempts() uint32  { return t.maxAttempts }
func (t *Task) RowVersion() uint32   { return t.rowVersion }
func (t *Task) NotBefore() time.Time { return t.notBefore }
func (t *Task) ReasonCode() string   { return t.reasonCode }
func validTaskID(value string) bool {
	if len(value) < 8 || len(value) > 128 || len(value) < 4 || value[:4] != "ing_" {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func taskTime(previous, value time.Time) (time.Time, error) {
	if value.IsZero() {
		return time.Time{}, ErrInvalid
	}
	value = value.UTC()
	if value.Before(previous) {
		return time.Time{}, ErrInvalid
	}
	return value, nil
}

type temporaryError struct{ cause error }

func (e temporaryError) Error() string   { return "temporary ingestion dependency failure" }
func (e temporaryError) Unwrap() error   { return e.cause }
func (e temporaryError) Temporary() bool { return true }
func Transient(cause error) error {
	if cause == nil {
		return nil
	}
	return temporaryError{cause: cause}
}
func retryable(err error) bool {
	var marker interface{ Temporary() bool }
	return errors.As(err, &marker) && marker.Temporary()
}
