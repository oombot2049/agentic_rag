package queue

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const (
	MinimumLeaseDuration = 5 * time.Second
	MaximumLeaseDuration = 5 * time.Minute
	MaximumPayloadSize   = 64 << 10
	MaximumErrorLength   = 2048
)

var deliveryIDPattern = regexp.MustCompile(`^evt_[A-Za-z0-9_-]{4,124}$`)

func validateMessage(message Message) error {
	if !deliveryIDPattern.MatchString(message.DeliveryID) {
		return fmt.Errorf("%w: delivery ID", ErrInvalidArgument)
	}
	if _, err := domain.ParseTenantID(message.TenantID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if _, err := domain.ParseSessionID(message.SessionID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if _, err := domain.ParseTurnID(message.TurnID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if _, err := domain.ParseRunID(message.RunID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	trimmed := bytes.TrimSpace(message.Payload)
	if len(trimmed) < 2 || len(trimmed) > MaximumPayloadSize || !json.Valid(trimmed) || trimmed[0] != '{' {
		return fmt.Errorf("%w: payload must be a bounded JSON object", ErrInvalidArgument)
	}
	if message.CreatedAt.IsZero() || message.AvailableAt.IsZero() || message.AvailableAt.Before(message.CreatedAt) {
		return fmt.Errorf("%w: message timestamps", ErrInvalidArgument)
	}
	if err := validateTrace(message.Trace); err != nil {
		return err
	}
	return nil
}

func validateTrace(trace TraceContext) error {
	if trace.TraceParent == "" {
		if trace.TraceState != "" {
			return fmt.Errorf("%w: tracestate requires traceparent", ErrInvalidArgument)
		}
		return nil
	}
	parts := strings.Split(trace.TraceParent, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 ||
		strings.ToLower(trace.TraceParent) != trace.TraceParent || !validHex(parts[1]) || !validHex(parts[2]) || !validHex(parts[3]) ||
		allZero(parts[1]) || allZero(parts[2]) {
		return fmt.Errorf("%w: traceparent is not canonical W3C version 00", ErrInvalidArgument)
	}
	if len(trace.TraceState) > 512 || strings.IndexFunc(trace.TraceState, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: tracestate is invalid", ErrInvalidArgument)
	}
	return nil
}

// NewTraceContext validates and returns immutable W3C propagation metadata.
func NewTraceContext(traceParent, traceState string) (TraceContext, error) {
	value := TraceContext{TraceParent: traceParent, TraceState: traceState}
	if err := validateTrace(value); err != nil {
		return TraceContext{}, err
	}
	return value, nil
}

func validHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func allZero(value string) bool { return strings.Trim(value, "0") == "" }

func validOwner(value string) bool {
	return validSafeText(value, 256)
}

func validSafeText(value string, maximum int) bool {
	return len(value) >= 1 && len(value) <= maximum && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validateClaim(command ClaimCommand) error {
	if _, err := domain.ParseTenantID(command.TenantID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if !validOwner(command.Owner) || command.Limit < 1 || command.Limit > 100 || command.At.IsZero() ||
		command.Duration < MinimumLeaseDuration || command.Duration > MaximumLeaseDuration {
		return fmt.Errorf("%w: claim owner, limit, time or duration", ErrInvalidArgument)
	}
	return nil
}

func validateLease(command LeaseCommand) error {
	if _, err := domain.ParseTenantID(command.TenantID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if _, err := domain.ParseRunID(command.RunID.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if !validOwner(command.Owner) || command.Token == 0 || command.At.IsZero() {
		return fmt.Errorf("%w: lease authority or time", ErrInvalidArgument)
	}
	return nil
}
