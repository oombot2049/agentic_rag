// Package streaming owns bounded aggregation of high-frequency model output.
package streaming

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	DefaultMaxBytes = 512
	MaximumMaxBytes = 8192
	MinimumMaxBytes = 64
	DefaultMaxDelay = 75 * time.Millisecond
)

var ErrClosed = errors.New("delta batcher is closed")

// Sink publishes one already-bounded assistant.delta payload. Implementations
// must return only after the payload has either committed or failed.
type Sink func(context.Context, string) error

type Config struct {
	MaxBytes int
	MaxDelay time.Duration
}

func DefaultConfig() Config {
	return Config{MaxBytes: DefaultMaxBytes, MaxDelay: DefaultMaxDelay}
}

// Batcher combines small model fragments before they enter the durable Run
// event and Outbox path. It is safe for concurrent use and preserves the order
// in which calls acquire the batcher. A sink failure is terminal: silently
// dropping a durable delta or retrying an uncertain write could corrupt output.
type Batcher struct {
	mu              sync.Mutex
	lifetime        context.Context
	sink            Sink
	config          Config
	buffer          strings.Builder
	timer           *time.Timer
	timerGeneration uint64
	terminalErr     error
	closed          bool
}

func NewBatcher(lifetime context.Context, config Config, sink Sink) (*Batcher, error) {
	if lifetime == nil || sink == nil || config.MaxBytes < MinimumMaxBytes || config.MaxBytes > MaximumMaxBytes ||
		config.MaxDelay < 10*time.Millisecond || config.MaxDelay > time.Second {
		return nil, errors.New("delta batcher configuration is invalid")
	}
	return &Batcher{lifetime: lifetime, sink: sink, config: config}, nil
}

// Write adds a valid UTF-8 fragment. Oversized fragments are split only at
// rune boundaries, and full batches are synchronously persisted.
func (b *Batcher) Write(ctx context.Context, fragment string) error {
	if ctx == nil {
		return errors.New("delta write context is nil")
	}
	if fragment == "" || !utf8.ValidString(fragment) {
		return errors.New("delta fragment must be non-empty UTF-8")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.available(); err != nil {
		return err
	}
	for len(fragment) > 0 {
		remaining := b.config.MaxBytes - b.buffer.Len()
		prefix := validPrefix(fragment, remaining)
		if prefix == 0 {
			if err := b.flushLocked(ctx); err != nil {
				return err
			}
			continue
		}
		wasEmpty := b.buffer.Len() == 0
		_, _ = b.buffer.WriteString(fragment[:prefix])
		fragment = fragment[prefix:]
		if wasEmpty {
			b.armTimerLocked()
		}
		if b.buffer.Len() == b.config.MaxBytes {
			if err := b.flushLocked(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// Flush persists the current partial batch. Worker code must call it before a
// Step completion event and before Final Commit.
func (b *Batcher) Flush(ctx context.Context) error {
	if ctx == nil {
		return errors.New("delta flush context is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.available(); err != nil {
		return err
	}
	return b.flushLocked(ctx)
}

// Close performs a final flush and permanently rejects later writes.
func (b *Batcher) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("delta close context is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		if b.terminalErr != nil {
			return b.terminalErr
		}
		return nil
	}
	err := b.terminalErr
	if err == nil {
		err = b.flushLocked(ctx)
	}
	b.closed = true
	b.disarmTimerLocked()
	return err
}

func (b *Batcher) available() error {
	if b.terminalErr != nil {
		return b.terminalErr
	}
	if b.closed {
		return ErrClosed
	}
	return nil
}

func (b *Batcher) flushLocked(ctx context.Context) error {
	if b.buffer.Len() == 0 {
		b.disarmTimerLocked()
		return nil
	}
	b.disarmTimerLocked()
	text := b.buffer.String()
	if err := b.sink(ctx, text); err != nil {
		b.buffer.Reset()
		b.terminalErr = fmt.Errorf("persist assistant delta: %w", err)
		return b.terminalErr
	}
	b.buffer.Reset()
	return nil
}

func (b *Batcher) armTimerLocked() {
	b.timerGeneration++
	generation := b.timerGeneration
	b.timer = time.AfterFunc(b.config.MaxDelay, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed || b.terminalErr != nil || generation != b.timerGeneration {
			return
		}
		_ = b.flushLocked(b.lifetime)
	})
}

func (b *Batcher) disarmTimerLocked() {
	b.timerGeneration++
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

func validPrefix(value string, maximum int) int {
	if len(value) <= maximum {
		return len(value)
	}
	end := maximum
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return end
}
