// Package domain contains the database-independent AgenticRAG domain model.
package domain

import (
	"errors"
	"fmt"
)

var (
	// ErrValidation identifies an invalid value or broken aggregate invariant.
	ErrValidation = errors.New("domain validation failed")
	// ErrInvalidTransition identifies a transition forbidden by a state machine.
	ErrInvalidTransition = errors.New("invalid domain state transition")
	// ErrVersionConflict identifies a stale conversation-context expectation.
	ErrVersionConflict = errors.New("session context version conflict")
	// ErrSessionBusy identifies a session that already has an active run.
	ErrSessionBusy = errors.New("session already has an active run")
	// ErrRunFenced identifies a command issued by a run that no longer owns a session.
	ErrRunFenced = errors.New("run does not own session")
)

// ValidationError describes a domain invariant violation without transport concerns.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ErrValidation.Error()
	}
	return fmt.Sprintf("%s: %s: %s", ErrValidation, e.Field, e.Reason)
}

func (e *ValidationError) Unwrap() error { return ErrValidation }

// TransitionError describes a rejected state-machine edge.
type TransitionError struct {
	Aggregate string
	From      string
	To        string
}

func (e *TransitionError) Error() string {
	if e == nil {
		return ErrInvalidTransition.Error()
	}
	return fmt.Sprintf("%s: %s %s -> %s", ErrInvalidTransition, e.Aggregate, e.From, e.To)
}

func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

func invalid(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}

func invalidTransition(aggregate string, from, to fmt.Stringer) error {
	return &TransitionError{Aggregate: aggregate, From: from.String(), To: to.String()}
}
