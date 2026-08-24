package agent

import (
	"encoding/json"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type BeginStepCommand struct {
	Step    *domain.Step
	Attempt *domain.Attempt
	Payload json.RawMessage
}

type RetryAttemptCommand struct {
	Previous       ActiveStep
	PreviousStatus domain.AttemptStatus
	Attempt        *domain.Attempt
	At             time.Time
	Payload        json.RawMessage
}

type CompleteStepCommand struct {
	Active  ActiveStep
	At      time.Time
	Payload json.RawMessage
}

type FailStepCommand struct {
	Active        ActiveStep
	AttemptStatus domain.AttemptStatus
	At            time.Time
	Payload       json.RawMessage
}

type CompleteLoopCommand struct {
	StepNo  uint64
	Payload json.RawMessage
}
