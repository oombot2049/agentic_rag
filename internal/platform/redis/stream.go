package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

type StreamEntry struct {
	ID   string
	Data []byte
}

func (c *Client) StreamAppend(ctx context.Context, key Key, id string, data []byte, ttl time.Duration) error {
	if c == nil || key.value == "" || !validStreamID(id) || len(data) == 0 || len(data) > 128<<10 {
		return errors.New("redis stream append input is invalid")
	}
	if ttl <= 0 || ttl > c.maxTTL {
		return ErrInvalidTTL
	}
	if _, err := appendStreamScript.Run(ctx, c.client, []string{key.value}, id, data, ttl.Milliseconds()).Result(); err != nil {
		return &operationError{operation: "append stream event", cause: err}
	}
	return nil
}

// StreamAppendNext atomically appends transient data with the next numeric
// stream ID. It is intended for best-effort output streams whose IDs are scoped
// independently from durable PostgreSQL event sequences.
func (c *Client) StreamAppendNext(ctx context.Context, key Key, data []byte, ttl time.Duration) (string, error) {
	if c == nil || key.value == "" || len(data) == 0 || len(data) > 128<<10 {
		return "", errors.New("redis stream append-next input is invalid")
	}
	if ttl <= 0 || ttl > c.maxTTL {
		return "", ErrInvalidTTL
	}
	value, err := appendNextStreamScript.Run(ctx, c.client, []string{key.value}, data, ttl.Milliseconds()).Text()
	if err != nil {
		return "", &operationError{operation: "append transient stream event", cause: err}
	}
	if !validStreamID(value) || value == "0-0" {
		return "", errors.New("redis returned an invalid transient stream ID")
	}
	return value, nil
}

var appendNextStreamScript = goredis.NewScript(`
local latest = redis.call('XREVRANGE', KEYS[1], '+', '-', 'COUNT', 1)
local clock = redis.call('TIME')
local sequence = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
if #latest > 0 then
  local dash = string.find(latest[1][1], '-')
  local previous = tonumber(string.sub(latest[1][1], 1, dash - 1))
  if sequence <= previous then
    sequence = previous + 1
  end
end
local id = tostring(sequence) .. '-0'
redis.call('XADD', KEYS[1], id, 'data', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return id`)

// appendStreamScript makes Relay retry safe. The same sequence and bytes are a
// successful replay; reusing a sequence for different bytes fails closed.
var appendStreamScript = goredis.NewScript(`
local existing = redis.call('XRANGE', KEYS[1], ARGV[1], ARGV[1])
if #existing > 0 then
  local fields = existing[1][2]
  for index = 1, #fields, 2 do
    if fields[index] == 'data' then
      if fields[index + 1] ~= ARGV[2] then
        return redis.error_reply('stream sequence payload conflict')
      end
      redis.call('PEXPIRE', KEYS[1], ARGV[3])
      return 0
    end
  end
  return redis.error_reply('stream sequence has no data field')
end
redis.call('XADD', KEYS[1], ARGV[1], 'data', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1`)

func (c *Client) StreamRead(ctx context.Context, key Key, afterID string, count int64, block time.Duration) ([]StreamEntry, error) {
	if c == nil || key.value == "" || !validStreamID(afterID) || count < 1 || count > 1000 || block < time.Millisecond || block > 30*time.Second {
		return nil, errors.New("redis stream read input is invalid")
	}
	streams, err := c.client.XRead(ctx, &goredis.XReadArgs{Streams: []string{key.value, afterID}, Count: count, Block: block}).Result()
	if errors.Is(err, goredis.Nil) {
		return []StreamEntry{}, nil
	}
	if err != nil {
		return nil, &operationError{operation: "read stream events", cause: err}
	}
	result := make([]StreamEntry, 0, count)
	for _, stream := range streams {
		for _, message := range stream.Messages {
			value, ok := message.Values["data"]
			if !ok {
				return nil, fmt.Errorf("redis stream event %s has no data", message.ID)
			}
			var data []byte
			switch typed := value.(type) {
			case string:
				data = []byte(typed)
			case []byte:
				data = append([]byte(nil), typed...)
			default:
				return nil, fmt.Errorf("redis stream event %s has invalid data", message.ID)
			}
			result = append(result, StreamEntry{ID: message.ID, Data: data})
		}
	}
	return result, nil
}

func validStreamID(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "0" || strings.HasPrefix(parts[0], "+") || strings.HasPrefix(parts[0], "0") && parts[0] != "0" {
		return false
	}
	sequence, err := strconv.ParseUint(parts[0], 10, 63)
	return err == nil && (sequence > 0 || value == "0-0")
}
