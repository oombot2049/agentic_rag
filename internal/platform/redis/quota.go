package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/agenticrag/agenticrag/internal/quota"
	goredis "github.com/redis/go-redis/v9"
)

var reserveQuotaScript = goredis.NewScript(`
local function integer(value) return string.format('%.0f', value) end
local function extend_expiry(key, ttl)
  local current = redis.call('PTTL', key)
  if current < ttl then redis.call('PEXPIRE', key, ttl) end
end
local status = redis.call('HGET', KEYS[3], 'status')
if status then
  local old_tokens = tonumber(redis.call('HGET', KEYS[3], 'tokens'))
  local old_cost = tonumber(redis.call('HGET', KEYS[3], 'cost'))
  if old_tokens ~= tonumber(ARGV[9]) or old_cost ~= tonumber(ARGV[10]) or status == 'COMPENSATED' then
    return {-1, 0}
  end
  return {1, 0}
end

local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local active_limit = tonumber(ARGV[4])
local token_limit = tonumber(ARGV[5])
local cost_limit = tonumber(ARGV[6])
local period_ms = tonumber(ARGV[7])
local lease_ms = tonumber(ARGV[8])
local tokens = tonumber(ARGV[9])
local cost = tonumber(ARGV[10])
local reservation_id = ARGV[11]
local last = tonumber(redis.call('HGET', KEYS[1], 'last') or now)
local available = tonumber(redis.call('HGET', KEYS[1], 'available') or burst)
available = math.min(burst, available + ((now - last) * rate / 1000))
if available < 1 then
  return {0, 1, math.ceil((1 - available) * 1000 / rate)}
end

local period_start = now - (now % period_ms)
local saved_period = tonumber(redis.call('HGET', KEYS[1], 'period') or period_start)
local used_tokens = tonumber(redis.call('HGET', KEYS[1], 'tokens') or 0)
local used_cost = tonumber(redis.call('HGET', KEYS[1], 'cost') or 0)
if saved_period ~= period_start then
  used_tokens = 0
  used_cost = 0
end
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
if redis.call('ZCARD', KEYS[2]) >= active_limit then return {0, 2, 1000} end
if tokens > token_limit - used_tokens then return {0, 3, 0} end
if cost > cost_limit - used_cost then return {0, 4, 0} end

if saved_period ~= period_start then
  redis.call('HSET', KEYS[1], 'tokens', 0, 'cost', 0)
end
redis.call('HSET', KEYS[1],
  'available', available - 1, 'last', integer(now), 'period', integer(period_start))
redis.call('HINCRBY', KEYS[1], 'tokens', ARGV[9])
redis.call('HINCRBY', KEYS[1], 'cost', ARGV[10])
redis.call('ZADD', KEYS[2], now + lease_ms, reservation_id)
redis.call('HSET', KEYS[3],
  'status', 'RESERVED', 'tokens', ARGV[9], 'cost', ARGV[10],
  'period', integer(period_start), 'burst', ARGV[3])
extend_expiry(KEYS[1], period_ms * 2 + lease_ms)
extend_expiry(KEYS[2], lease_ms * 2)
redis.call('PEXPIRE', KEYS[3], period_ms + lease_ms)
return {1, 0}
`)

var settleQuotaScript = goredis.NewScript(`
local function integer(value) return string.format('%.0f', value) end
local status = redis.call('HGET', KEYS[3], 'status')
if not status then return {-2} end
local actual_tokens = tonumber(ARGV[1])
local actual_cost = tonumber(ARGV[2])
if status == 'SETTLED' then
  if tonumber(redis.call('HGET', KEYS[3], 'actual_tokens')) == actual_tokens and
     tonumber(redis.call('HGET', KEYS[3], 'actual_cost')) == actual_cost then return {1} end
  return {-1}
end
if status ~= 'RESERVED' then return {-1} end
local reserved_tokens = tonumber(redis.call('HGET', KEYS[3], 'tokens'))
local reserved_cost = tonumber(redis.call('HGET', KEYS[3], 'cost'))
if actual_tokens > reserved_tokens or actual_cost > reserved_cost then return {-3} end
local receipt_period = redis.call('HGET', KEYS[3], 'period')
if receipt_period == redis.call('HGET', KEYS[1], 'period') then
  redis.call('HINCRBY', KEYS[1], 'tokens', integer(-(reserved_tokens - actual_tokens)))
  redis.call('HINCRBY', KEYS[1], 'cost', integer(-(reserved_cost - actual_cost)))
end
redis.call('ZREM', KEYS[2], ARGV[3])
redis.call('HSET', KEYS[3], 'status', 'SETTLED', 'actual_tokens', ARGV[1], 'actual_cost', ARGV[2])
return {1}
`)

var compensateQuotaScript = goredis.NewScript(`
local function integer(value) return string.format('%.0f', value) end
local status = redis.call('HGET', KEYS[3], 'status')
if not status then return {-2} end
if status == 'COMPENSATED' then return {1} end
if status ~= 'RESERVED' then return {-1} end
local reserved_tokens = tonumber(redis.call('HGET', KEYS[3], 'tokens'))
local reserved_cost = tonumber(redis.call('HGET', KEYS[3], 'cost'))
if redis.call('HGET', KEYS[3], 'period') == redis.call('HGET', KEYS[1], 'period') then
  redis.call('HINCRBY', KEYS[1], 'tokens', integer(-reserved_tokens))
  redis.call('HINCRBY', KEYS[1], 'cost', integer(-reserved_cost))
end
local available = tonumber(redis.call('HGET', KEYS[1], 'available') or 0)
local burst = tonumber(redis.call('HGET', KEYS[3], 'burst'))
redis.call('HSET', KEYS[1], 'available', math.min(burst, available + 1))
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HSET', KEYS[3], 'status', 'COMPENSATED')
return {1}
`)

// QuotaStore implements quota.Store with Redis Cluster-safe Lua scripts.
type QuotaStore struct {
	client *Client
	keys   KeyBuilder
}

func NewQuotaStore(client *Client, keys KeyBuilder) (*QuotaStore, error) {
	if client == nil || client.client == nil || keys.environment == "" {
		return nil, quota.ErrInvalid
	}
	return &QuotaStore{client: client, keys: keys}, nil
}

func (s *QuotaStore) Reserve(
	ctx context.Context,
	scope string,
	id string,
	limits quota.Limits,
	tokens int64,
	cost int64,
	at time.Time,
) (quota.StoreDecision, error) {
	if ctx == nil || ctx.Err() != nil || !quota.ValidReservationID(id) || limits.Validate() != nil ||
		!quota.ValidUsage(tokens, cost) || at.IsZero() {
		return quota.StoreDecision{}, quota.ErrInvalid
	}
	keys, err := s.scopeKeys(scope, id)
	if err != nil {
		return quota.StoreDecision{}, quota.ErrInvalid
	}
	result, err := reserveQuotaScript.Run(ctx, s.client.client, keys, []any{
		at.UTC().UnixMilli(), limits.RequestsPerSecond, limits.Burst, limits.ActiveRuns,
		limits.PeriodTokens, limits.PeriodCostNanos, limits.Period.Milliseconds(),
		limits.ReservationTTL.Milliseconds(), tokens, cost, id,
	}...).Slice()
	if err != nil {
		return quota.StoreDecision{}, &operationError{operation: "reserve quota", cause: err}
	}
	return parseReserveDecision(result)
}

func (s *QuotaStore) Settle(
	ctx context.Context,
	scope string,
	id string,
	tokens int64,
	cost int64,
	at time.Time,
) error {
	if ctx == nil || ctx.Err() != nil || !quota.ValidReservationID(id) || !quota.ValidUsage(tokens, cost) || at.IsZero() {
		return quota.ErrInvalid
	}
	keys, err := s.scopeKeys(scope, id)
	if err != nil {
		return quota.ErrInvalid
	}
	result, err := settleQuotaScript.Run(ctx, s.client.client, keys, tokens, cost, id).Slice()
	if err != nil {
		return &operationError{operation: "settle quota", cause: err}
	}
	return parseMutationResult(result)
}

func (s *QuotaStore) Compensate(ctx context.Context, scope string, id string, at time.Time) error {
	if ctx == nil || ctx.Err() != nil || !quota.ValidReservationID(id) || at.IsZero() {
		return quota.ErrInvalid
	}
	keys, err := s.scopeKeys(scope, id)
	if err != nil {
		return quota.ErrInvalid
	}
	result, err := compensateQuotaScript.Run(ctx, s.client.client, keys, id).Slice()
	if err != nil {
		return &operationError{operation: "compensate quota", cause: err}
	}
	return parseMutationResult(result)
}

func (s *QuotaStore) scopeKeys(scope string, id string) ([]string, error) {
	state, err := s.keys.Build(scope, NamespaceRate, "quota", "state")
	if err != nil {
		return nil, err
	}
	active, err := s.keys.Build(scope, NamespaceRate, "quota", "active")
	if err != nil {
		return nil, err
	}
	receipt, err := s.keys.Build(scope, NamespaceRate, "quota", "reservation", id)
	if err != nil {
		return nil, err
	}
	return []string{state.String(), active.String(), receipt.String()}, nil
}

func parseReserveDecision(result []any) (quota.StoreDecision, error) {
	code, err := resultInteger(result, 0)
	if err != nil {
		return quota.StoreDecision{}, err
	}
	switch code {
	case 1:
		return quota.StoreDecision{Allowed: true}, nil
	case -1:
		return quota.StoreDecision{}, quota.ErrConflict
	case -2:
		return quota.StoreDecision{}, quota.ErrInvalid
	case 0:
		reasonCode, reasonErr := resultInteger(result, 1)
		if reasonErr != nil {
			return quota.StoreDecision{}, reasonErr
		}
		reasons := map[int64]quota.Reason{
			1: quota.ReasonQPS, 2: quota.ReasonActiveRuns, 3: quota.ReasonTokens, 4: quota.ReasonCost,
		}
		reason, ok := reasons[reasonCode]
		if !ok {
			return quota.StoreDecision{}, errors.New("redis: invalid quota denial reason")
		}
		retryMillis, retryErr := resultInteger(result, 2)
		if retryErr != nil {
			return quota.StoreDecision{}, retryErr
		}
		return quota.StoreDecision{Reason: reason, RetryAfter: time.Duration(retryMillis) * time.Millisecond}, nil
	default:
		return quota.StoreDecision{}, errors.New("redis: invalid quota result")
	}
}

func parseMutationResult(result []any) error {
	code, err := resultInteger(result, 0)
	if err != nil {
		return err
	}
	switch code {
	case 1:
		return nil
	case -1:
		return quota.ErrConflict
	case -2:
		return quota.ErrNotFound
	case -3:
		return quota.ErrInvalid
	default:
		return errors.New("redis: invalid quota mutation result")
	}
}

func resultInteger(result []any, index int) (int64, error) {
	if index >= len(result) {
		return 0, errors.New("redis: incomplete quota result")
	}
	switch value := result[index].(type) {
	case int64:
		return value, nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("redis: malformed quota result: %w", err)
		}
		return parsed, nil
	default:
		return 0, errors.New("redis: malformed quota result")
	}
}

var _ quota.Store = (*QuotaStore)(nil)
