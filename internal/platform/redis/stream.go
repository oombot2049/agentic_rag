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
	pipeline := c.client.TxPipeline()
	pipeline.XAdd(ctx, &goredis.XAddArgs{Stream: key.value, ID: id, Values: map[string]any{"data": data}})
	pipeline.Expire(ctx, key.value, ttl)
	if _, err := pipeline.Exec(ctx); err != nil {
		return &operationError{operation: "append stream event", cause: err}
	}
	return nil
}

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
