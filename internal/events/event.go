package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const (
	CurrentSchemaVersion = uint16(1)
	maximumPayloadBytes  = 64 << 10
	maximumEnvelopeBytes = 128 << 10
)

type Event struct {
	schemaVersion uint16
	eventType     Type
	tenantID      domain.TenantID
	turnID        domain.TurnID
	runID         domain.RunID
	sequence      uint64
	occurredAt    time.Time
	payload       json.RawMessage
	known         bool
}

type wireEvent struct {
	SchemaVersion uint16          `json:"schema_version"`
	Type          Type            `json:"type"`
	TurnID        domain.TurnID   `json:"turn_id"`
	RunID         domain.RunID    `json:"run_id"`
	Sequence      uint64          `json:"sequence"`
	OccurredAt    string          `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

func New(tenantID domain.TenantID, turnID domain.TurnID, runID domain.RunID, sequence uint64, eventType Type, payload any, occurredAt time.Time) (Event, error) {
	if !eventType.Known() {
		return Event{}, errors.New("unknown event type cannot be emitted")
	}
	if err := validatePayload(eventType, payload); err != nil {
		return Event{}, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return restore(CurrentSchemaVersion, eventType, tenantID, turnID, runID, sequence, occurredAt, raw, true)
}

func DecodeStrict(tenantID domain.TenantID, data []byte) (Event, error) {
	return decode(tenantID, data, true)
}

func DecodeCompatible(tenantID domain.TenantID, data []byte) (Event, error) {
	return decode(tenantID, data, false)
}

func decode(tenantID domain.TenantID, data []byte, strict bool) (Event, error) {
	if len(data) == 0 || len(data) > maximumEnvelopeBytes || !json.Valid(data) {
		return Event{}, errors.New("event envelope is invalid or oversized")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Event{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	var wire wireEvent
	if err := decoder.Decode(&wire); err != nil {
		return Event{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Event{}, errors.New("event envelope contains trailing JSON")
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, wire.OccurredAt)
	if err != nil || !strings.HasSuffix(wire.OccurredAt, "Z") || occurredAt.UTC().Format(time.RFC3339Nano) != wire.OccurredAt {
		return Event{}, errors.New("occurred_at must be canonical UTC RFC3339")
	}
	known := wire.SchemaVersion == CurrentSchemaVersion && wire.Type.Known()
	if strict && !known {
		return Event{}, errors.New("strict decode requires current schema and known type")
	}
	if known {
		payload, err := decodeKnownPayload(wire.Type, wire.Payload)
		if err != nil {
			return Event{}, err
		}
		if err := validatePayload(wire.Type, payload); err != nil {
			return Event{}, err
		}
	}
	return restore(wire.SchemaVersion, wire.Type, tenantID, wire.TurnID, wire.RunID, wire.Sequence, occurredAt, wire.Payload, known)
}

func restore(version uint16, eventType Type, tenantID domain.TenantID, turnID domain.TurnID, runID domain.RunID, sequence uint64, occurredAt time.Time, payload json.RawMessage, known bool) (Event, error) {
	if version == 0 || !validTypeName(eventType) || sequence == 0 || sequence > math.MaxInt64 || occurredAt.IsZero() {
		return Event{}, errors.New("event envelope fields are invalid")
	}
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return Event{}, err
	}
	if _, err := domain.ParseTurnID(turnID.String()); err != nil {
		return Event{}, err
	}
	if _, err := domain.ParseRunID(runID.String()); err != nil {
		return Event{}, err
	}
	if len(payload) < 2 || len(payload) > maximumPayloadBytes || !json.Valid(payload) || bytes.TrimSpace(payload)[0] != '{' {
		return Event{}, errors.New("event payload must be a bounded JSON object")
	}
	return Event{schemaVersion: version, eventType: eventType, tenantID: tenantID, turnID: turnID,
		runID: runID, sequence: sequence, occurredAt: occurredAt.UTC(), payload: append(json.RawMessage(nil), payload...), known: known}, nil
}

func (e Event) MarshalJSON() ([]byte, error) {
	if _, err := restore(e.schemaVersion, e.eventType, e.tenantID, e.turnID, e.runID, e.sequence, e.occurredAt, e.payload, e.known); err != nil {
		return nil, err
	}
	return json.Marshal(wireEvent{SchemaVersion: e.schemaVersion, Type: e.eventType, TurnID: e.turnID,
		RunID: e.runID, Sequence: e.sequence, OccurredAt: e.occurredAt.Format(time.RFC3339Nano), Payload: e.payload})
}

func (e Event) SchemaVersion() uint16     { return e.schemaVersion }
func (e Event) Type() Type                { return e.eventType }
func (e Event) TenantID() domain.TenantID { return e.tenantID }
func (e Event) TurnID() domain.TurnID     { return e.turnID }
func (e Event) RunID() domain.RunID       { return e.runID }
func (e Event) Sequence() uint64          { return e.sequence }
func (e Event) OccurredAt() time.Time     { return e.occurredAt }
func (e Event) Known() bool               { return e.known }
func (e Event) SSEID() string             { return strconv.FormatUint(e.sequence, 10) }
func (e Event) Payload() json.RawMessage  { return append(json.RawMessage(nil), e.payload...) }

func decodeKnownPayload(eventType Type, raw []byte) (any, error) {
	var target any
	switch eventType {
	case RunQueued:
		target = &RunQueuedPayload{}
	case RunStarted, RunCancelRequested, RunCancelled:
		target = &EmptyPayload{}
	case StepStarted:
		target = &StepStartedPayload{}
	case RetrievalCompleted:
		target = &RetrievalCompletedPayload{}
	case ToolStarted:
		target = &ToolStartedPayload{}
	case ToolCompleted:
		target = &ToolCompletedPayload{}
	case AssistantDelta:
		target = &AssistantDeltaPayload{}
	case StepCompleted:
		target = &StepCompletedPayload{}
	case RunCompleted:
		target = &RunCompletedPayload{}
	case RunFailed, RunTimedOut:
		target = &RunReasonPayload{}
	default:
		return nil, errors.New("unknown event payload")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("payload contains trailing JSON")
	}
	switch value := target.(type) {
	case *RunQueuedPayload:
		return *value, nil
	case *EmptyPayload:
		return *value, nil
	case *StepStartedPayload:
		return *value, nil
	case *RetrievalCompletedPayload:
		return *value, nil
	case *ToolStartedPayload:
		return *value, nil
	case *ToolCompletedPayload:
		return *value, nil
	case *AssistantDeltaPayload:
		return *value, nil
	case *StepCompletedPayload:
		return *value, nil
	case *RunCompletedPayload:
		return *value, nil
	case *RunReasonPayload:
		return *value, nil
	default:
		return nil, fmt.Errorf("unsupported decoded payload %T", target)
	}
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return scanUniqueValue(decoder)
}

func scanUniqueValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanUniqueValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanUniqueValue(decoder); err != nil {
				return err
			}
		}
	}
	_, err = decoder.Token()
	return err
}
