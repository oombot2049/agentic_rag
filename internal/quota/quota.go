// Package quota enforces hierarchical deployment and tenant admission limits.
package quota

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrInvalid  = errors.New("invalid quota request")
	ErrConflict = errors.New("quota reservation conflict")
	ErrNotFound = errors.New("quota reservation not found")
)

var reservationPattern = regexp.MustCompile(`^qrs_[A-Za-z0-9_-]{4,124}$`)

// maxLuaInteger is the largest integer Redis Lua can compare exactly.
const maxLuaInteger = int64(1<<53 - 1)

type Reason string

const (
	ReasonQPS        Reason = "QPS"
	ReasonActiveRuns Reason = "ACTIVE_RUNS"
	ReasonTokens     Reason = "TOKENS"
	ReasonCost       Reason = "COST"
)

type Limits struct {
	RequestsPerSecond int64
	Burst             int64
	ActiveRuns        int64
	PeriodTokens      int64
	PeriodCostNanos   int64
	Period            time.Duration
	ReservationTTL    time.Duration
}

func (v Limits) Validate() error { return validateLimits(v) }

func ValidReservationID(value string) bool { return reservationPattern.MatchString(value) }

func ValidUsage(tokens int64, costNanos int64) bool {
	return tokens >= 0 && tokens <= maxLuaInteger && costNanos >= 0 && costNanos <= maxLuaInteger
}

type Request struct {
	TenantID           domain.TenantID
	ReservationID      string
	EstimatedTokens    int64
	EstimatedCostNanos int64
	At                 time.Time
}

type StoreDecision struct {
	Allowed    bool
	Reason     Reason
	RetryAfter time.Duration
}

type Store interface {
	Reserve(context.Context, string, string, Limits, int64, int64, time.Time) (StoreDecision, error)
	Settle(context.Context, string, string, int64, int64, time.Time) error
	Compensate(context.Context, string, string, time.Time) error
}

type PolicyResolver interface {
	Limits(context.Context, domain.TenantID) (Limits, error)
}
type PolicyResolverFunc func(context.Context, domain.TenantID) (Limits, error)

func (f PolicyResolverFunc) Limits(ctx context.Context, tenant domain.TenantID) (Limits, error) {
	return f(ctx, tenant)
}

type Reservation struct {
	TenantID                            domain.TenantID
	ID                                  string
	EstimatedTokens, EstimatedCostNanos int64
}

type Decision struct {
	Allowed     bool
	Scope       string
	Reason      Reason
	RetryAfter  time.Duration
	Reservation Reservation
}

type Service struct {
	store    Store
	policies PolicyResolver
	global   Limits
}

func NewService(store Store, policies PolicyResolver, global Limits) (*Service, error) {
	if store == nil || policies == nil || validateLimits(global) != nil {
		return nil, ErrInvalid
	}
	return &Service{store: store, policies: policies, global: global}, nil
}

func (s *Service) Admit(ctx context.Context, request Request) (Decision, error) {
	if ctx == nil || ctx.Err() != nil || validateRequest(request) != nil {
		return Decision{}, ErrInvalid
	}
	globalID := globalReservationID(request.TenantID, request.ReservationID)
	global, err := s.store.Reserve(ctx, "global", globalID, s.global, request.EstimatedTokens, request.EstimatedCostNanos, request.At.UTC())
	if err != nil {
		return Decision{}, err
	}
	if !global.Allowed {
		return denied("global", global), nil
	}
	limits, err := s.policies.Limits(ctx, request.TenantID)
	limitsErr := validateLimits(limits)
	if err != nil || limitsErr != nil {
		return Decision{}, errors.Join(err, limitsErr, s.store.Compensate(ctx, "global", globalID, request.At.UTC()))
	}
	tenantScope := request.TenantID.String()
	tenant, reserveErr := s.store.Reserve(ctx, tenantScope, request.ReservationID, limits, request.EstimatedTokens, request.EstimatedCostNanos, request.At.UTC())
	if reserveErr != nil {
		tenantCompensateErr := ignoreNotFound(s.store.Compensate(ctx, tenantScope, request.ReservationID, request.At.UTC()))
		globalCompensateErr := s.store.Compensate(ctx, "global", globalID, request.At.UTC())
		return Decision{}, errors.Join(reserveErr, tenantCompensateErr, globalCompensateErr)
	}
	if !tenant.Allowed {
		compensateErr := s.store.Compensate(ctx, "global", globalID, request.At.UTC())
		if compensateErr != nil {
			return Decision{}, compensateErr
		}
		return denied("tenant", tenant), nil
	}
	reservation := Reservation{request.TenantID, request.ReservationID, request.EstimatedTokens, request.EstimatedCostNanos}
	return Decision{Allowed: true, Reservation: reservation}, nil
}

func (s *Service) Settle(ctx context.Context, reservation Reservation, tokens, cost int64, at time.Time) error {
	if ctx == nil || ctx.Err() != nil || !validReservation(reservation) || tokens < 0 || cost < 0 || tokens > reservation.EstimatedTokens || cost > reservation.EstimatedCostNanos || at.IsZero() {
		return ErrInvalid
	}
	tenantErr := s.store.Settle(ctx, reservation.TenantID.String(), reservation.ID, tokens, cost, at.UTC())
	globalErr := s.store.Settle(ctx, "global", globalReservationID(reservation.TenantID, reservation.ID), tokens, cost, at.UTC())
	return errors.Join(tenantErr, globalErr)
}

func (s *Service) Compensate(ctx context.Context, reservation Reservation, at time.Time) error {
	if ctx == nil || ctx.Err() != nil || !validReservation(reservation) || at.IsZero() {
		return ErrInvalid
	}
	tenantErr := s.store.Compensate(ctx, reservation.TenantID.String(), reservation.ID, at.UTC())
	globalErr := s.store.Compensate(ctx, "global", globalReservationID(reservation.TenantID, reservation.ID), at.UTC())
	return errors.Join(tenantErr, globalErr)
}

func denied(scope string, value StoreDecision) Decision {
	return Decision{Scope: scope, Reason: value.Reason, RetryAfter: value.RetryAfter}
}

func ignoreNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func validateRequest(value Request) error {
	if _, err := domain.ParseTenantID(value.TenantID.String()); err != nil {
		return err
	}
	if !reservationPattern.MatchString(value.ReservationID) || value.EstimatedTokens < 0 ||
		value.EstimatedTokens > maxLuaInteger || value.EstimatedCostNanos < 0 ||
		value.EstimatedCostNanos > maxLuaInteger || value.At.IsZero() {
		return ErrInvalid
	}
	return nil
}

func validateLimits(v Limits) error {
	if v.RequestsPerSecond < 1 || v.RequestsPerSecond > 1_000_000 || v.Burst < v.RequestsPerSecond || v.Burst > 10_000_000 ||
		v.ActiveRuns < 1 || v.ActiveRuns > 1_000_000 || v.PeriodTokens < 1 || v.PeriodTokens > maxLuaInteger ||
		v.PeriodCostNanos < 1 || v.PeriodCostNanos > maxLuaInteger || v.Period < time.Minute || v.Period > 366*24*time.Hour ||
		v.ReservationTTL < time.Minute || v.ReservationTTL > 24*time.Hour {
		return fmt.Errorf("%w: limits", ErrInvalid)
	}
	return nil
}

func validReservation(v Reservation) bool {
	_, err := domain.ParseTenantID(v.TenantID.String())
	return err == nil && reservationPattern.MatchString(v.ID) && ValidUsage(v.EstimatedTokens, v.EstimatedCostNanos)
}

func globalReservationID(tenantID domain.TenantID, reservationID string) string {
	digest := sha256.Sum256([]byte(tenantID.String() + ":" + reservationID))
	return fmt.Sprintf("qrs_%x", digest[:16])
}
