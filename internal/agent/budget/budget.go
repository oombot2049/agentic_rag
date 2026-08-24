// Package budget provides immutable, checkpointable Agent execution budgets.
package budget

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/agent/action"
)

const SchemaVersion = 1

var (
	ErrInvalidArgument        = errors.New("agent budget invalid argument")
	ErrCorruptSnapshot        = errors.New("agent budget corrupt snapshot")
	ErrOutstandingReservation = errors.New("agent budget outstanding model reservation")
	ErrSettlementMismatch     = errors.New("agent budget model settlement mismatch")
)

type CompletionStatus string

const (
	StatusContinue           CompletionStatus = "CONTINUE"
	StatusCompletedWithLimit CompletionStatus = "COMPLETED_WITH_LIMIT"
)

type LimitReason string

const (
	ReasonMaxSteps     LimitReason = "MAX_STEPS"
	ReasonMaxDuration  LimitReason = "MAX_DURATION"
	ReasonMaxTokens    LimitReason = "MAX_TOKENS"
	ReasonMaxToolCalls LimitReason = "MAX_TOOL_CALLS"
	ReasonMaxCost      LimitReason = "MAX_COST"
	ReasonActionLoop   LimitReason = "ACTION_LOOP"
)

type Decision struct {
	Status CompletionStatus
	Reason LimitReason
}

func (d Decision) Allowed() bool { return d.Status == StatusContinue }

type Limits struct {
	MaxSteps                 int
	MaxDuration              time.Duration
	MaxTokens                int64
	MaxToolCalls             int
	MaxCostNanos             int64
	MaxConsecutiveSameAction int
	CycleWindow              int
	MaxCycleRepeats          int
}

func DefaultLimits() Limits {
	return Limits{MaxSteps: 12, MaxDuration: 2 * time.Minute, MaxTokens: 100_000,
		MaxToolCalls: 8, MaxCostNanos: 1_000_000_000, MaxConsecutiveSameAction: 3,
		CycleWindow: 24, MaxCycleRepeats: 3}
}

type Clock interface{ Now() time.Time }

type Controller struct {
	limits Limits
	clock  Clock
}

func New(limits Limits, clock Clock) (*Controller, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Controller{limits: limits, clock: clock}, nil
}

func validateLimits(value Limits) error {
	if value.MaxSteps < 1 || value.MaxSteps > 20 || value.MaxDuration <= 0 || value.MaxDuration > 24*time.Hour ||
		value.MaxTokens < 1 || value.MaxTokens > math.MaxInt64/2 || value.MaxToolCalls < 1 || value.MaxToolCalls > value.MaxSteps ||
		value.MaxCostNanos < 1 || value.MaxCostNanos > math.MaxInt64/2 ||
		value.MaxConsecutiveSameAction < 1 || value.MaxConsecutiveSameAction > 20 ||
		value.MaxCycleRepeats < 2 || value.MaxCycleRepeats > 10 || value.CycleWindow < value.MaxCycleRepeats*2 || value.CycleWindow > 256 {
		return fmt.Errorf("%w: limits", ErrInvalidArgument)
	}
	return nil
}

type reservation struct {
	operationKey    string
	inputTokens     int64
	maxOutputTokens int64
	maxCostNanos    int64
}

type Ledger struct {
	startedAt    time.Time
	steps        int
	tokens       int64
	toolCalls    int
	costNanos    int64
	fingerprints []string
	outstanding  *reservation
}

type ReservationSnapshot struct {
	OperationKey    string `json:"operation_key"`
	InputTokens     int64  `json:"input_tokens"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	MaxCostNanos    int64  `json:"max_cost_nanos"`
}

type Snapshot struct {
	SchemaVersion int                  `json:"schema_version"`
	StartedAt     time.Time            `json:"started_at"`
	Steps         int                  `json:"steps"`
	Tokens        int64                `json:"tokens"`
	ToolCalls     int                  `json:"tool_calls"`
	CostNanos     int64                `json:"cost_nanos"`
	Fingerprints  []string             `json:"fingerprints"`
	Outstanding   *ReservationSnapshot `json:"outstanding,omitempty"`
}

func (c *Controller) Start() Ledger {
	return Ledger{startedAt: c.clock.Now().UTC()}
}

func (c *Controller) Snapshot(value Ledger) Snapshot {
	result := Snapshot{SchemaVersion: SchemaVersion, StartedAt: value.startedAt, Steps: value.steps,
		Tokens: value.tokens, ToolCalls: value.toolCalls, CostNanos: value.costNanos,
		Fingerprints: append([]string(nil), value.fingerprints...)}
	if value.outstanding != nil {
		result.Outstanding = &ReservationSnapshot{OperationKey: value.outstanding.operationKey,
			InputTokens: value.outstanding.inputTokens, MaxOutputTokens: value.outstanding.maxOutputTokens,
			MaxCostNanos: value.outstanding.maxCostNanos}
	}
	return result
}

func (c *Controller) Restore(snapshot Snapshot) (Ledger, error) {
	if c == nil || snapshot.SchemaVersion != SchemaVersion || snapshot.StartedAt.IsZero() ||
		snapshot.Steps < 0 || snapshot.Steps > c.limits.MaxSteps || snapshot.Tokens < 0 || snapshot.Tokens > c.limits.MaxTokens ||
		snapshot.ToolCalls < 0 || snapshot.ToolCalls > c.limits.MaxToolCalls || snapshot.ToolCalls > snapshot.Steps ||
		snapshot.CostNanos < 0 || snapshot.CostNanos > c.limits.MaxCostNanos ||
		len(snapshot.Fingerprints) > c.limits.CycleWindow || snapshot.Steps < len(snapshot.Fingerprints) {
		return Ledger{}, ErrCorruptSnapshot
	}
	for _, fingerprint := range snapshot.Fingerprints {
		if len(fingerprint) != sha256.Size*2 {
			return Ledger{}, ErrCorruptSnapshot
		}
		if _, err := hex.DecodeString(fingerprint); err != nil {
			return Ledger{}, ErrCorruptSnapshot
		}
	}
	result := Ledger{startedAt: snapshot.StartedAt.UTC(), steps: snapshot.Steps, tokens: snapshot.Tokens,
		toolCalls: snapshot.ToolCalls, costNanos: snapshot.CostNanos,
		fingerprints: append([]string(nil), snapshot.Fingerprints...)}
	if snapshot.Outstanding != nil {
		value := snapshot.Outstanding
		if !validOperationKey(value.OperationKey) || value.InputTokens < 0 || value.MaxOutputTokens < 1 || value.MaxCostNanos < 0 {
			return Ledger{}, ErrCorruptSnapshot
		}
		reservedTokens, ok := checkedAdd(value.InputTokens, value.MaxOutputTokens)
		if !ok || exceeds(result.tokens, reservedTokens, c.limits.MaxTokens) || exceeds(result.costNanos, value.MaxCostNanos, c.limits.MaxCostNanos) {
			return Ledger{}, ErrCorruptSnapshot
		}
		result.outstanding = &reservation{operationKey: value.OperationKey, inputTokens: value.InputTokens,
			maxOutputTokens: value.MaxOutputTokens, maxCostNanos: value.MaxCostNanos}
	}
	return result, nil
}

type ModelReservation struct {
	OperationKey    string
	InputTokens     int64
	MaxOutputTokens int64
	MaxCostNanos    int64
}

func (c *Controller) ReserveModel(current Ledger, request ModelReservation) (Ledger, Decision, error) {
	if c == nil || !validOperationKey(request.OperationKey) || request.InputTokens < 0 || request.MaxOutputTokens < 1 || request.MaxCostNanos < 0 {
		return Ledger{}, Decision{}, fmt.Errorf("%w: model reservation", ErrInvalidArgument)
	}
	if decision := c.timeDecision(current); !decision.Allowed() {
		return current, decision, nil
	}
	if current.outstanding != nil {
		if sameReservation(*current.outstanding, request) {
			return cloneLedger(current), continueDecision(), nil
		}
		return Ledger{}, Decision{}, ErrOutstandingReservation
	}
	reservedTokens, ok := checkedAdd(request.InputTokens, request.MaxOutputTokens)
	if !ok || exceeds(current.tokens, reservedTokens, c.limits.MaxTokens) {
		return current, limitDecision(ReasonMaxTokens), nil
	}
	if exceeds(current.costNanos, request.MaxCostNanos, c.limits.MaxCostNanos) {
		return current, limitDecision(ReasonMaxCost), nil
	}
	next := cloneLedger(current)
	next.outstanding = &reservation{operationKey: request.OperationKey, inputTokens: request.InputTokens,
		maxOutputTokens: request.MaxOutputTokens, maxCostNanos: request.MaxCostNanos}
	return next, continueDecision(), nil
}

type ModelSettlement struct {
	OperationKey string
	InputTokens  int64
	OutputTokens int64
	CostNanos    int64
}

func (c *Controller) CommitModel(current Ledger, settlement ModelSettlement) (Ledger, error) {
	if c == nil || current.outstanding == nil || settlement.OperationKey != current.outstanding.operationKey ||
		settlement.InputTokens < 0 || settlement.OutputTokens < 0 || settlement.CostNanos < 0 ||
		settlement.InputTokens > current.outstanding.inputTokens || settlement.OutputTokens > current.outstanding.maxOutputTokens ||
		settlement.CostNanos > current.outstanding.maxCostNanos {
		return Ledger{}, ErrSettlementMismatch
	}
	actualTokens, ok := checkedAdd(settlement.InputTokens, settlement.OutputTokens)
	if !ok || exceeds(current.tokens, actualTokens, c.limits.MaxTokens) || exceeds(current.costNanos, settlement.CostNanos, c.limits.MaxCostNanos) {
		return Ledger{}, ErrSettlementMismatch
	}
	next := cloneLedger(current)
	next.tokens += actualTokens
	next.costNanos += settlement.CostNanos
	next.outstanding = nil
	return next, nil
}

func (c *Controller) ReleaseModel(current Ledger, operationKey string) (Ledger, error) {
	if c == nil || current.outstanding == nil || operationKey != current.outstanding.operationKey {
		return Ledger{}, ErrSettlementMismatch
	}
	next := cloneLedger(current)
	next.outstanding = nil
	return next, nil
}

func (c *Controller) AdmitAction(current Ledger, value action.Action) (Ledger, Decision, error) {
	if c == nil {
		return Ledger{}, Decision{}, ErrInvalidArgument
	}
	if current.outstanding != nil {
		return Ledger{}, Decision{}, ErrOutstandingReservation
	}
	if decision := c.timeDecision(current); !decision.Allowed() {
		return current, decision, nil
	}
	if current.steps >= c.limits.MaxSteps {
		return current, limitDecision(ReasonMaxSteps), nil
	}
	if value.Type() == action.TypeTool && current.toolCalls >= c.limits.MaxToolCalls {
		return current, limitDecision(ReasonMaxToolCalls), nil
	}
	fingerprint, err := actionFingerprint(value)
	if err != nil {
		return Ledger{}, Decision{}, fmt.Errorf("%w: Action fingerprint", ErrInvalidArgument)
	}
	if c.loopDetected(current.fingerprints, fingerprint) {
		return current, limitDecision(ReasonActionLoop), nil
	}
	next := cloneLedger(current)
	next.steps++
	if value.Type() == action.TypeTool {
		next.toolCalls++
	}
	next.fingerprints = append(next.fingerprints, fingerprint)
	if len(next.fingerprints) > c.limits.CycleWindow {
		next.fingerprints = append([]string(nil), next.fingerprints[len(next.fingerprints)-c.limits.CycleWindow:]...)
	}
	return next, continueDecision(), nil
}

func (c *Controller) Check(current Ledger) Decision {
	if decision := c.timeDecision(current); !decision.Allowed() {
		return decision
	}
	if current.steps >= c.limits.MaxSteps {
		return limitDecision(ReasonMaxSteps)
	}
	if current.tokens >= c.limits.MaxTokens {
		return limitDecision(ReasonMaxTokens)
	}
	if current.toolCalls >= c.limits.MaxToolCalls {
		return limitDecision(ReasonMaxToolCalls)
	}
	if current.costNanos >= c.limits.MaxCostNanos {
		return limitDecision(ReasonMaxCost)
	}
	return continueDecision()
}

func (c *Controller) Deadline(current Ledger) time.Time {
	return current.startedAt.Add(c.limits.MaxDuration)
}

func (c *Controller) timeDecision(current Ledger) Decision {
	if !c.clock.Now().UTC().Before(c.Deadline(current)) {
		return limitDecision(ReasonMaxDuration)
	}
	return continueDecision()
}

func (c *Controller) loopDetected(history []string, next string) bool {
	consecutive := 1
	for index := len(history) - 1; index >= 0 && history[index] == next; index-- {
		consecutive++
	}
	if consecutive > c.limits.MaxConsecutiveSameAction {
		return true
	}
	candidate := append(append([]string(nil), history...), next)
	for patternLength := 2; patternLength*c.limits.MaxCycleRepeats <= len(candidate) && patternLength*c.limits.MaxCycleRepeats <= c.limits.CycleWindow; patternLength++ {
		start := len(candidate) - patternLength*c.limits.MaxCycleRepeats
		matches := true
		for repeat := 1; repeat < c.limits.MaxCycleRepeats && matches; repeat++ {
			for offset := 0; offset < patternLength; offset++ {
				if candidate[start+offset] != candidate[start+repeat*patternLength+offset] {
					matches = false
					break
				}
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func actionFingerprint(value action.Action) (string, error) {
	encoded, err := value.Encode()
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return "", err
	}
	delete(document, "reason_code")
	canonical, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func cloneLedger(value Ledger) Ledger {
	value.fingerprints = append([]string(nil), value.fingerprints...)
	if value.outstanding != nil {
		copyReservation := *value.outstanding
		value.outstanding = &copyReservation
	}
	return value
}

func sameReservation(current reservation, request ModelReservation) bool {
	return current.operationKey == request.OperationKey && current.inputTokens == request.InputTokens &&
		current.maxOutputTokens == request.MaxOutputTokens && current.maxCostNanos == request.MaxCostNanos
}

func validOperationKey(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func checkedAdd(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, false
	}
	return left + right, true
}
func exceeds(current, addition, maximum int64) bool {
	value, ok := checkedAdd(current, addition)
	return !ok || value > maximum
}
func continueDecision() Decision { return Decision{Status: StatusContinue} }
func limitDecision(reason LimitReason) Decision {
	return Decision{Status: StatusCompletedWithLimit, Reason: reason}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
