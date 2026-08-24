package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type ComponentConfig struct {
	Owner           string
	PollInterval    time.Duration
	TenantScanLimit int
}

// Component continuously discovers tenants and processes their pending events.
type Component struct {
	repository *Repository
	relay      *Relay
	config     ComponentConfig
	logger     *slog.Logger
	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
}

func NewComponent(repository *Repository, relay *Relay, config ComponentConfig, logger *slog.Logger) (*Component, error) {
	if repository == nil || relay == nil || strings.TrimSpace(config.Owner) != config.Owner || config.Owner == "" ||
		config.PollInterval < time.Millisecond || config.PollInterval > time.Minute ||
		config.TenantScanLimit < 1 || config.TenantScanLimit > 1000 {
		return nil, fmt.Errorf("%w: relay component configuration", ErrInvalidArgument)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Component{repository: repository, relay: relay, config: config, logger: logger}, nil
}

func (c *Component) Name() string { return "outbox-relay" }

func (c *Component) Start(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("outbox relay start context is invalid")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("outbox relay already started")
	}
	runCtx, cancel := context.WithCancel(context.Background())
	c.cancel, c.done = cancel, make(chan struct{})
	go c.run(runCtx)
	return nil
}

func (c *Component) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("outbox relay stop context is nil")
	}
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Component) run(ctx context.Context) {
	defer close(c.done)
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		c.process(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Component) process(ctx context.Context) {
	tenants, err := c.repository.ReadyTenants(ctx, ReadyTenantsCommand{Limit: c.config.TenantScanLimit})
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Error("outbox tenant scan failed", "error", err)
		}
		return
	}
	for _, tenantID := range tenants {
		result, err := c.relay.ProcessBatch(ctx, tenantID, c.config.Owner)
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Error("outbox batch failed", "tenant_id", tenantID.String(), "error", err)
			}
			continue
		}
		if result.Claimed > 0 {
			c.logger.Debug("outbox batch processed", "tenant_id", tenantID.String(), "claimed", result.Claimed,
				"published", result.Published, "retried", result.Retried, "dead", result.Dead)
		}
	}
}
