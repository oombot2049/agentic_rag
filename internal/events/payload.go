package events

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const maximumDeltaBytes = 8192

var (
	toolCallIDPattern = regexp.MustCompile(`^tcl_[A-Za-z0-9_-]{4,124}$`)
	toolNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	reasonCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

type EmptyPayload struct{}
type RunQueuedPayload struct {
	MaxSteps int `json:"max_steps"`
}
type StepStartedPayload struct {
	StepID domain.StepID   `json:"step_id"`
	StepNo uint64          `json:"step_no"`
	Kind   domain.StepKind `json:"kind"`
}
type RetrievalCompletedPayload struct {
	StepID      domain.StepID `json:"step_id"`
	ResultCount int           `json:"result_count"`
}
type ToolStartedPayload struct {
	StepID     domain.StepID `json:"step_id"`
	ToolCallID string        `json:"tool_call_id"`
	ToolName   string        `json:"tool_name"`
}
type ToolCompletedPayload struct {
	StepID     domain.StepID `json:"step_id"`
	ToolCallID string        `json:"tool_call_id"`
	Succeeded  bool          `json:"succeeded"`
}
type AssistantDeltaPayload struct {
	Text string `json:"text"`
}
type StepCompletedPayload struct {
	StepID domain.StepID     `json:"step_id"`
	StepNo uint64            `json:"step_no"`
	Status domain.StepStatus `json:"status"`
}
type RunCompletedPayload struct {
	MessageID domain.MessageID `json:"message_id"`
}
type RunReasonPayload struct {
	ReasonCode string `json:"reason_code"`
}

func validatePayload(eventType Type, payload any) error {
	switch eventType {
	case RunQueued:
		value, ok := payload.(RunQueuedPayload)
		if !ok || value.MaxSteps < 1 || value.MaxSteps > 20 {
			return errors.New("run.queued requires max_steps 1..20")
		}
	case RunStarted, RunCancelRequested, RunCancelled:
		if _, ok := payload.(EmptyPayload); !ok {
			return fmt.Errorf("%s requires an empty payload", eventType)
		}
	case StepStarted:
		value, ok := payload.(StepStartedPayload)
		if !ok || invalidStep(value.StepID, value.StepNo) || !validStepKind(value.Kind) {
			return errors.New("step.started payload is invalid")
		}
	case RetrievalCompleted:
		value, ok := payload.(RetrievalCompletedPayload)
		if !ok || invalidStep(value.StepID, 1) || value.ResultCount < 0 || value.ResultCount > 10000 {
			return errors.New("retrieval.completed payload is invalid")
		}
	case ToolStarted:
		value, ok := payload.(ToolStartedPayload)
		if !ok || invalidStep(value.StepID, 1) || !toolCallIDPattern.MatchString(value.ToolCallID) || !toolNamePattern.MatchString(value.ToolName) {
			return errors.New("tool.started payload is invalid")
		}
	case ToolCompleted:
		value, ok := payload.(ToolCompletedPayload)
		if !ok || invalidStep(value.StepID, 1) || !toolCallIDPattern.MatchString(value.ToolCallID) {
			return errors.New("tool.completed payload is invalid")
		}
	case AssistantDelta:
		value, ok := payload.(AssistantDeltaPayload)
		if !ok || len(value.Text) == 0 || len(value.Text) > maximumDeltaBytes {
			return errors.New("assistant.delta text is empty or oversized")
		}
	case StepCompleted:
		value, ok := payload.(StepCompletedPayload)
		if !ok || invalidStep(value.StepID, value.StepNo) || !value.Status.Terminal() {
			return errors.New("step.completed payload is invalid")
		}
	case RunCompleted:
		value, ok := payload.(RunCompletedPayload)
		if !ok {
			return errors.New("run.completed payload type is invalid")
		}
		if _, err := domain.ParseMessageID(value.MessageID.String()); err != nil {
			return err
		}
	case RunFailed, RunTimedOut:
		value, ok := payload.(RunReasonPayload)
		if !ok || !reasonCodePattern.MatchString(value.ReasonCode) {
			return fmt.Errorf("%s safe reason code is invalid", eventType)
		}
	default:
		return errors.New("unknown event types cannot be emitted")
	}
	return nil
}

func invalidStep(id domain.StepID, number uint64) bool {
	_, err := domain.ParseStepID(id.String())
	return err != nil || number == 0 || number > math.MaxInt64
}

func validStepKind(kind domain.StepKind) bool {
	switch kind {
	case domain.StepKindContext, domain.StepKindRetrieval, domain.StepKindTool, domain.StepKindGeneration:
		return true
	default:
		return false
	}
}

func validTypeName(value Type) bool {
	if len(value) < 3 || len(value) > 128 || strings.HasPrefix(value.String(), ".") || strings.HasSuffix(value.String(), ".") {
		return false
	}
	for _, part := range strings.Split(value.String(), ".") {
		if part == "" || part[0] < 'a' || part[0] > 'z' {
			return false
		}
		for _, character := range part {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_') {
				return false
			}
		}
	}
	return true
}
