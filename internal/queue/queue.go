// Package queue defines replaceable at-least-once Run dispatch contracts.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrInvalidArgument = errors.New("queue invalid argument")
	ErrNotFound        = errors.New("queue message not found")
	ErrConflict        = errors.New("queue delivery identity conflict")
	ErrFenced          = errors.New("queue lease fenced")
	ErrLeaseExpired    = errors.New("queue lease expired")
	ErrTenantCapacity  = errors.New("tenant queue capacity exceeded")
	ErrGlobalCapacity  = errors.New("global queue capacity exceeded")
)

type TraceContext struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
}

type Message struct {
	DeliveryID  string
	TenantID    domain.TenantID
	SessionID   domain.SessionID
	TurnID      domain.TurnID
	RunID       domain.RunID
	Payload     json.RawMessage
	Trace       TraceContext
	AvailableAt time.Time
	CreatedAt   time.Time
}

type Delivery struct {
	Message
	LeaseOwner    string
	LeaseToken    uint64
	LeaseExpires  time.Time
	DeliveryCount uint64
}

type ClaimCommand struct {
	TenantID domain.TenantID
	Owner    string
	Limit    int
	Duration time.Duration
	At       time.Time
}

type LeaseCommand struct {
	TenantID domain.TenantID
	RunID    domain.RunID
	Owner    string
	Token    uint64
	At       time.Time
}

type RenewCommand struct {
	LeaseCommand
	Duration time.Duration
}

type NackCommand struct {
	LeaseCommand
	RetryAfter time.Duration
	Reason     string
}

type CleanupCommand struct {
	TenantID domain.TenantID
	Cutoff   time.Time
	Limit    int
}

type RunQueue interface {
	Enqueue(context.Context, Message) error
	Claim(context.Context, ClaimCommand) ([]Delivery, error)
	Renew(context.Context, RenewCommand) (Delivery, error)
	Ack(context.Context, LeaseCommand) error
	Nack(context.Context, NackCommand) error
	Cleanup(context.Context, CleanupCommand) (int64, error)
}
