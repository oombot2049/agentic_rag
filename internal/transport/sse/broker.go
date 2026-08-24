package sse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/events"
	platformredis "github.com/agenticrag/agenticrag/internal/platform/redis"
)

type streamClient interface {
	StreamAppend(context.Context, platformredis.Key, string, []byte, time.Duration) error
	StreamRead(context.Context, platformredis.Key, string, int64, time.Duration) ([]platformredis.StreamEntry, error)
}

// RedisBroker is a transient low-latency tail. PostgreSQL remains authoritative.
type RedisBroker struct {
	client    streamClient
	keys      platformredis.KeyBuilder
	ttl       time.Duration
	readBlock time.Duration
}

func NewRedisBroker(client streamClient, keys platformredis.KeyBuilder, ttl, readBlock time.Duration) (*RedisBroker, error) {
	if client == nil || ttl <= 0 || readBlock < time.Millisecond || readBlock > 30*time.Second {
		return nil, errors.New("SSE Redis broker configuration is invalid")
	}
	return &RedisBroker{client: client, keys: keys, ttl: ttl, readBlock: readBlock}, nil
}

func (b *RedisBroker) Publish(ctx context.Context, event events.Event) error {
	key, err := b.key(event.TenantID(), event.RunID())
	if err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal live event: %w", err)
	}
	return b.client.StreamAppend(ctx, key, event.SSEID()+"-0", data, b.ttl)
}

func (b *RedisBroker) Subscribe(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, after uint64) (<-chan events.Event, <-chan error) {
	values := make(chan events.Event)
	failures := make(chan error, 1)
	go func() {
		defer close(values)
		defer close(failures)
		key, err := b.key(tenantID, runID)
		if err != nil {
			failures <- err
			return
		}
		cursor := strconv.FormatUint(after, 10) + "-0"
		for {
			entries, err := b.client.StreamRead(ctx, key, cursor, 128, b.readBlock)
			if err != nil {
				if ctx.Err() == nil {
					failures <- err
				}
				return
			}
			for _, entry := range entries {
				event, err := events.DecodeStrict(tenantID, entry.Data)
				if err != nil || entry.ID != event.SSEID()+"-0" || event.RunID() != runID {
					failures <- fmt.Errorf("invalid live event %q", entry.ID)
					return
				}
				select {
				case values <- event:
					cursor = entry.ID
				case <-ctx.Done():
					return
				}
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return values, failures
}

func (b *RedisBroker) key(tenantID domain.TenantID, runID domain.RunID) (platformredis.Key, error) {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return platformredis.Key{}, err
	}
	if _, err := domain.ParseRunID(runID.String()); err != nil {
		return platformredis.Key{}, err
	}
	return b.keys.Build(tenantID.String(), platformredis.NamespaceEvents, "run", runID.String())
}
