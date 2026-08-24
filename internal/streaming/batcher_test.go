package streaming

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBatcherCombinesFragmentsAndFlushesExplicitly(t *testing.T) {
	var batches []string
	batcher := testBatcher(t, Config{MaxBytes: 64, MaxDelay: time.Second}, func(_ context.Context, text string) error {
		batches = append(batches, text)
		return nil
	})
	for _, fragment := range []string{"Post", "gre", "SQL", " works"} {
		if err := batcher.Write(context.Background(), fragment); err != nil {
			t.Fatal(err)
		}
	}
	if len(batches) != 0 {
		t.Fatalf("premature batches = %q", batches)
	}
	if err := batcher.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || batches[0] != "PostgreSQL works" {
		t.Fatalf("batches = %q", batches)
	}
}

func TestBatcherSplitsAtBoundedUTF8Boundaries(t *testing.T) {
	var batches []string
	batcher := testBatcher(t, Config{MaxBytes: 64, MaxDelay: time.Second}, func(_ context.Context, text string) error {
		batches = append(batches, text)
		return nil
	})
	input := strings.Repeat("数", 50)
	if err := batcher.Write(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := batcher.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(batches, "") != input || len(batches) != 3 {
		t.Fatalf("batches = %q", batches)
	}
	for _, batch := range batches {
		if len(batch) > 64 || !utf8.ValidString(batch) {
			t.Fatalf("invalid batch bytes=%d valid=%v", len(batch), utf8.ValidString(batch))
		}
	}
}

func TestBatcherFlushesAfterDelay(t *testing.T) {
	flushed := make(chan string, 1)
	batcher := testBatcher(t, Config{MaxBytes: 64, MaxDelay: 20 * time.Millisecond}, func(_ context.Context, text string) error {
		flushed <- text
		return nil
	})
	if err := batcher.Write(context.Background(), "delayed"); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-flushed:
		if value != "delayed" {
			t.Fatalf("flushed = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("timed flush did not occur")
	}
	if err := batcher.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBatcherLatchesSinkFailureAndNeverRetriesUncertainWrite(t *testing.T) {
	want := errors.New("database unavailable")
	calls := 0
	batcher := testBatcher(t, Config{MaxBytes: 64, MaxDelay: time.Second}, func(context.Context, string) error {
		calls++
		return want
	})
	if err := batcher.Write(context.Background(), strings.Repeat("x", 64)); !errors.Is(err, want) {
		t.Fatalf("Write error = %v", err)
	}
	if err := batcher.Write(context.Background(), "later"); !errors.Is(err, want) {
		t.Fatalf("second Write error = %v", err)
	}
	if err := batcher.Close(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Close error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("sink calls = %d", calls)
	}
}

func TestBatcherSerializesConcurrentWritesWithoutLoss(t *testing.T) {
	var mu sync.Mutex
	var batches []string
	batcher := testBatcher(t, Config{MaxBytes: 64, MaxDelay: time.Second}, func(_ context.Context, text string) error {
		mu.Lock()
		defer mu.Unlock()
		batches = append(batches, text)
		return nil
	})
	const writers = 40
	var group sync.WaitGroup
	for index := 0; index < writers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := batcher.Write(context.Background(), "x"); err != nil {
				t.Errorf("Write = %v", err)
			}
		}()
	}
	group.Wait()
	if err := batcher.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	joined := strings.Join(batches, "")
	mu.Unlock()
	if joined != strings.Repeat("x", writers) {
		t.Fatalf("joined length = %d", len(joined))
	}
	if err := batcher.Write(context.Background(), "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close = %v", err)
	}
}

func TestBatcherRejectsInvalidConfigurationAndFragments(t *testing.T) {
	if _, err := NewBatcher(context.Background(), Config{MaxBytes: 63, MaxDelay: time.Second}, func(context.Context, string) error { return nil }); err == nil {
		t.Fatal("expected invalid configuration")
	}
	batcher := testBatcher(t, Config{MaxBytes: 64, MaxDelay: time.Second}, func(context.Context, string) error { return nil })
	if err := batcher.Write(context.Background(), ""); err == nil {
		t.Fatal("expected empty fragment error")
	}
	if err := batcher.Write(context.Background(), string([]byte{0xff})); err == nil {
		t.Fatal("expected invalid UTF-8 error")
	}
}

func testBatcher(t *testing.T, config Config, sink Sink) *Batcher {
	t.Helper()
	batcher, err := NewBatcher(context.Background(), config, sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batcher.Close(context.Background()) })
	return batcher
}
