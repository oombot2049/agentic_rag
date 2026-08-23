// Package agent coordinates a recoverable, fenced Agent execution loop.
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const checkpointSchemaVersion = 1

var (
	ErrInvalidArgument     = errors.New("agent invalid argument")
	ErrInvalidCheckpoint   = errors.New("agent invalid checkpoint")
	ErrStepLimit           = errors.New("agent step limit exceeded")
	ErrAttemptLimit        = errors.New("agent attempt limit exceeded")
	ErrStepFailed          = errors.New("agent step failed")
	ErrUnknownOutcome      = errors.New("agent external outcome unknown")
	ErrTransactionContract = errors.New("agent store transaction contract violated")
)

type Phase string

const (
	PhaseReady     Phase = "READY"
	PhaseExecuting Phase = "EXECUTING"
	PhaseCompleted Phase = "COMPLETED"
	PhaseFailed    Phase = "FAILED"
)

type Directive struct {
	Kind         domain.StepKind `json:"kind"`
	OperationKey string          `json:"operation_key"`
	Input        json.RawMessage `json:"input"`
	RetryUnknown bool            `json:"retry_unknown"`
}

type ActiveStep struct {
	StepID            domain.StepID    `json:"step_id"`
	StepNo            uint64           `json:"step_no"`
	StepRowVersion    uint64           `json:"step_row_version"`
	AttemptID         domain.AttemptID `json:"attempt_id"`
	AttemptNo         uint64           `json:"attempt_no"`
	AttemptRowVersion uint64           `json:"attempt_row_version"`
	Directive         Directive        `json:"directive"`
}

type Checkpoint struct {
	SchemaVersion int             `json:"schema_version"`
	Phase         Phase           `json:"phase"`
	NextStepNo    uint64          `json:"next_step_no"`
	State         json.RawMessage `json:"state"`
	Active        *ActiveStep     `json:"active,omitempty"`
	FailureClass  string          `json:"failure_class,omitempty"`
}

func initialCheckpoint() Checkpoint {
	return Checkpoint{SchemaVersion: checkpointSchemaVersion, Phase: PhaseReady, NextStepNo: 1, State: json.RawMessage(`{}`)}
}

func DecodeCheckpoint(payload json.RawMessage, maxSteps int) (Checkpoint, error) {
	if len(payload) == 0 {
		return initialCheckpoint(), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var value Checkpoint
	if err := decoder.Decode(&value); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: decode: %v", ErrInvalidCheckpoint, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Checkpoint{}, fmt.Errorf("%w: trailing value", ErrInvalidCheckpoint)
	}
	if err := value.validate(maxSteps); err != nil {
		return Checkpoint{}, err
	}
	value.State = cloneJSON(value.State)
	if value.Active != nil {
		copyActive := *value.Active
		copyActive.Directive.Input = cloneJSON(value.Active.Directive.Input)
		value.Active = &copyActive
	}
	return value, nil
}

func (c Checkpoint) Encode(maxSteps int) (json.RawMessage, error) {
	if err := c.validate(maxSteps); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrInvalidCheckpoint, err)
	}
	return payload, nil
}

func (c Checkpoint) validate(maxSteps int) error {
	if maxSteps < 1 || maxSteps > 20 || c.SchemaVersion != checkpointSchemaVersion || !validJSON(c.State) {
		return fmt.Errorf("%w: schema, max steps, or state", ErrInvalidCheckpoint)
	}
	if c.NextStepNo < 1 || c.NextStepNo > uint64(maxSteps+1) {
		return fmt.Errorf("%w: next step number", ErrInvalidCheckpoint)
	}
	switch c.Phase {
	case PhaseReady:
		if c.Active != nil || c.FailureClass != "" {
			return fmt.Errorf("%w: ready shape", ErrInvalidCheckpoint)
		}
	case PhaseExecuting:
		if c.Active == nil || c.FailureClass != "" || c.Active.StepNo != c.NextStepNo {
			return fmt.Errorf("%w: executing shape", ErrInvalidCheckpoint)
		}
		if err := c.Active.validate(); err != nil {
			return err
		}
	case PhaseCompleted:
		if c.Active != nil || c.FailureClass != "" {
			return fmt.Errorf("%w: completed shape", ErrInvalidCheckpoint)
		}
	case PhaseFailed:
		if c.Active != nil || !validFailureClass(c.FailureClass) {
			return fmt.Errorf("%w: failed shape", ErrInvalidCheckpoint)
		}
	default:
		return fmt.Errorf("%w: phase", ErrInvalidCheckpoint)
	}
	return nil
}

func (a ActiveStep) validate() error {
	if _, err := domain.ParseStepID(a.StepID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCheckpoint, err)
	}
	if _, err := domain.ParseAttemptID(a.AttemptID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCheckpoint, err)
	}
	if a.StepNo < 1 || a.StepRowVersion < 1 || a.AttemptNo < 1 || a.AttemptRowVersion < 1 {
		return fmt.Errorf("%w: active versions", ErrInvalidCheckpoint)
	}
	return validateDirective(a.Directive)
}

func validateDirective(value Directive) error {
	switch value.Kind {
	case domain.StepKindContext, domain.StepKindRetrieval, domain.StepKindTool, domain.StepKindGeneration:
	default:
		return fmt.Errorf("%w: directive kind", ErrInvalidArgument)
	}
	if !validOperationKey(value.OperationKey) || !validJSON(value.Input) {
		return fmt.Errorf("%w: directive operation key or input", ErrInvalidArgument)
	}
	return nil
}

func validOperationKey(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func validFailureClass(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func validJSON(value json.RawMessage) bool { return len(value) > 0 && json.Valid(value) }
func cloneJSON(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}
