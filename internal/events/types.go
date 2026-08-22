// Package events defines durable, public, versioned Run progress events.
package events

type Type string

const (
	RunQueued          Type = "run.queued"
	RunStarted         Type = "run.started"
	StepStarted        Type = "step.started"
	RetrievalCompleted Type = "retrieval.completed"
	ToolStarted        Type = "tool.started"
	ToolCompleted      Type = "tool.completed"
	AssistantDelta     Type = "assistant.delta"
	StepCompleted      Type = "step.completed"
	RunCancelRequested Type = "run.cancel_requested"
	RunCompleted       Type = "run.completed"
	RunFailed          Type = "run.failed"
	RunCancelled       Type = "run.cancelled"
	RunTimedOut        Type = "run.timed_out"
)

var knownTypes = map[Type]struct{}{
	RunQueued: {}, RunStarted: {}, StepStarted: {}, RetrievalCompleted: {},
	ToolStarted: {}, ToolCompleted: {}, AssistantDelta: {}, StepCompleted: {},
	RunCancelRequested: {}, RunCompleted: {}, RunFailed: {}, RunCancelled: {}, RunTimedOut: {},
}

func (t Type) Known() bool {
	_, ok := knownTypes[t]
	return ok
}

func (t Type) String() string { return string(t) }
