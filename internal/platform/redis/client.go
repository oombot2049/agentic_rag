package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	observabilitytrace "github.com/agenticrag/agenticrag/internal/observability/trace"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
)

var (
	ErrMiss       = errors.New("redis key not found")
	ErrInvalidTTL = errors.New("redis TTL must be positive and within configured maximum")
)

type Client struct {
	client        *goredis.Client
	healthTimeout time.Duration
	maxTTL        time.Duration
}

func Open(ctx context.Context, cfg config.RedisConfig) (*Client, error) {
	underlying := goredis.NewClient(&goredis.Options{
		Addr: cfg.Address, Password: cfg.Password.Reveal(), DB: cfg.DB,
		DialTimeout: cfg.DialTimeout, ReadTimeout: cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout, PoolTimeout: cfg.PoolTimeout,
		PoolSize: cfg.PoolSize, MinIdleConns: cfg.MinIdleConns,
	})
	hook, _ := observabilitytrace.NewRedisHook(otel.GetTracerProvider())
	underlying.AddHook(hook)
	connectCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	if err := underlying.Ping(connectCtx).Err(); err != nil {
		_ = underlying.Close()
		return nil, &operationError{operation: "verify connection", cause: err}
	}
	return &Client{client: underlying, healthTimeout: cfg.HealthTimeout, maxTTL: cfg.MaxTTL}, nil
}

func (c *Client) Close() error { return c.client.Close() }

func (c *Client) Health(ctx context.Context) error {
	healthCtx, cancel := context.WithTimeout(ctx, c.healthTimeout)
	defer cancel()
	if err := c.client.Ping(healthCtx).Err(); err != nil {
		return &operationError{operation: "health check", cause: err}
	}
	return nil
}

func (c *Client) Set(ctx context.Context, key Key, value []byte, ttl time.Duration) error {
	if key.value == "" {
		return errors.New("redis key is empty")
	}
	if ttl <= 0 || ttl > c.maxTTL {
		return ErrInvalidTTL
	}
	if err := c.client.Set(ctx, key.value, value, ttl).Err(); err != nil {
		return &operationError{operation: "set", cause: err}
	}
	return nil
}

func (c *Client) Get(ctx context.Context, key Key) ([]byte, error) {
	if key.value == "" {
		return nil, errors.New("redis key is empty")
	}
	value, err := c.client.Get(ctx, key.value).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, &operationError{operation: "get", cause: err}
	}
	return value, nil
}

func (c *Client) Delete(ctx context.Context, keys ...Key) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	raw := make([]string, len(keys))
	for i, key := range keys {
		if key.value == "" {
			return 0, errors.New("redis key is empty")
		}
		raw[i] = key.value
	}
	count, err := c.client.Del(ctx, raw...).Result()
	if err != nil {
		return 0, &operationError{operation: "delete", cause: err}
	}
	return count, nil
}

type operationError struct {
	operation string
	cause     error
}

func (e *operationError) Error() string { return fmt.Sprintf("redis: %s failed", e.operation) }
func (e *operationError) Unwrap() error { return e.cause }
