// Package app composes dependencies and owns process component lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrAlreadyStarted = errors.New("application already started")
	ErrStopped        = errors.New("application is stopped")
)

// Component is an inactive service dependency with explicit lifecycle.
type Component interface {
	Name() string
	Start(context.Context) error
	Stop(context.Context) error
}

type appState uint8

const (
	stateReady appState = iota
	stateRunning
	stateStopped
)

// App owns components in dependency declaration order.
type App struct {
	mu              sync.Mutex
	state           appState
	components      []Component
	started         []Component
	shutdownTimeout time.Duration
}

// Start starts each component once in declaration order. On failure it rolls
// back successfully started components in reverse order.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch a.state {
	case stateRunning:
		return ErrAlreadyStarted
	case stateStopped:
		return ErrStopped
	}

	for _, component := range a.components {
		if err := component.Start(ctx); err != nil {
			startErr := fmt.Errorf("start component %q: %w", component.Name(), err)
			rollbackCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
			rollbackErr := stopReverse(rollbackCtx, a.started)
			cancel()
			a.started = nil
			a.state = stateStopped
			return errors.Join(startErr, rollbackErr)
		}
		a.started = append(a.started, component)
	}
	a.state = stateRunning
	return nil
}

// Shutdown stops started components in reverse order. It is idempotent and
// attempts every component even when one fails.
func (a *App) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	if a.state == stateStopped {
		a.mu.Unlock()
		return nil
	}
	a.state = stateStopped
	started := append([]Component(nil), a.started...)
	a.started = nil
	a.mu.Unlock()
	return stopReverse(ctx, started)
}

// Run starts the application, waits for cancellation, then performs bounded
// graceful shutdown with a fresh context.
func (a *App) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run context is nil")
	}
	if err := a.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()
	return a.Shutdown(shutdownCtx)
}

func stopReverse(ctx context.Context, components []Component) error {
	var failures []error
	for i := len(components) - 1; i >= 0; i-- {
		component := components[i]
		if err := component.Stop(ctx); err != nil {
			failures = append(failures, fmt.Errorf("stop component %q: %w", component.Name(), err))
		}
	}
	return errors.Join(failures...)
}
