package rerank

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/knowledge"
)

type providerStub struct {
	scores    []float64
	err       error
	wait      bool
	documents []RerankDocument
}

func (p *providerStub) Rerank(ctx context.Context, _ string, documents []RerankDocument) ([]float64, error) {
	p.documents = append([]RerankDocument(nil), documents...)
	if p.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]float64(nil), p.scores...), p.err
}

type wordCounter struct{ err error }

func (c wordCounter) Count(_ context.Context, text string) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	return len(strings.Fields(text)), nil
}

type compressorStub struct {
	value string
	err   error
}

func (c compressorStub) Compress(context.Context, string, int) (string, error) { return c.value, c.err }

func TestRerankGoldenDedupMergeAndBudgetCompression(t *testing.T) {
	provider := &providerStub{scores: []float64{0.9, 0.8, 0.7}}
	counter := wordCounter{}
	compressor, _ := NewPrefixCompressor(counter)
	pipeline, _ := NewPipeline(provider, counter, compressor, Config{time.Second, 5, 100})
	input := []Candidate{{"ten_0001", "doc_0001", "chk_0001", 1, "Alpha one", 1}, {"ten_0001", "doc_0002", "chk_0002", 1, " alpha   ONE ", 2}, {"ten_0001", "doc_0001", "chk_0003", 2, "Beta two", 3}, {"ten_0001", "doc_0002", "chk_0004", 5, "Gamma delta epsilon", 4}}
	result, err := pipeline.Run(context.Background(), "query", input)
	if err != nil || result.RerankFallback || result.DroppedDuplicates != 1 || result.TotalTokens != 5 || len(result.Items) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(provider.documents) != 3 || len(result.Items[0].ChunkIDs) != 2 || result.Items[0].FirstOrdinal != 1 || result.Items[0].LastOrdinal != 2 || result.Items[0].Content != "Alpha one\nBeta two" {
		t.Fatalf("dedup/merge=%+v docs=%+v", result.Items, provider.documents)
	}
	if !result.Items[1].Compressed || result.Items[1].Content != "Gamma" || result.Items[1].Tokens != 1 {
		t.Fatalf("compression=%+v", result.Items[1])
	}
	if input[1].Content != " alpha   ONE " {
		t.Fatal("caller input mutated")
	}
}
func TestRerankerTimeoutAndCorruptionFallbackToInitialRank(t *testing.T) {
	for _, provider := range []*providerStub{{wait: true}, {scores: []float64{1}}, {scores: []float64{math.NaN(), 1}}} {
		counter := wordCounter{}
		compressor, _ := NewPrefixCompressor(counter)
		pipeline, _ := NewPipeline(provider, counter, compressor, Config{10 * time.Millisecond, 100, 10})
		result, err := pipeline.Run(context.Background(), "query", []Candidate{{"ten_0001", "doc_0001", "chk_0002", 2, "second", 2}, {"ten_0001", "doc_0001", "chk_0001", 1, "first", 1}})
		if err != nil || !result.RerankFallback || result.Items[0].ChunkIDs[0] != "chk_0001" {
			t.Fatalf("fallback=%+v err=%v", result, err)
		}
	}
}
func TestRerankStableTiesAndNoFalseAdjacency(t *testing.T) {
	provider := &providerStub{scores: []float64{1, 1, 1}}
	counter := wordCounter{}
	pipeline, _ := NewPipeline(provider, counter, compressorStub{value: ""}, Config{time.Second, 100, 10})
	input := []Candidate{{"ten_0001", "doc_0001", "chk_0002", 2, "two", 2}, {"ten_0001", "doc_0001", "chk_0001", 1, "one", 1}, {"ten_0001", "doc_0001", "chk_0004", 4, "four", 3}}
	result, err := pipeline.Run(context.Background(), "query", input)
	if err != nil || len(result.Items) != 2 || len(result.Items[0].ChunkIDs) != 2 || result.Items[1].FirstOrdinal != 4 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
func TestTokenBudgetPropertyAndFailureInjection(t *testing.T) {
	counter := wordCounter{}
	compressor, _ := NewPrefixCompressor(counter)
	for budget := 1; budget <= 50; budget++ {
		provider := &providerStub{scores: []float64{3, 2, 1}}
		pipeline, _ := NewPipeline(provider, counter, compressor, Config{time.Second, budget, 10})
		result, err := pipeline.Run(context.Background(), "q", []Candidate{{"ten_0001", "doc_0001", "chk_0001", 1, strings.Repeat("word ", 20), 1}, {"ten_0001", "doc_0002", "chk_0002", 1, strings.Repeat("next ", 20), 2}, {"ten_0001", "doc_0003", "chk_0003", 1, "tail", 3}})
		if err != nil || result.TotalTokens > budget {
			t.Fatalf("budget=%d result=%+v err=%v", budget, result, err)
		}
	}
	sentinel := errors.New("counter")
	pipeline, _ := NewPipeline(&providerStub{scores: []float64{1}}, wordCounter{err: sentinel}, compressorStub{}, Config{time.Second, 10, 10})
	if _, err := pipeline.Run(context.Background(), "q", []Candidate{{"ten_0001", "doc_0001", "chk_0001", 1, "content", 1}}); !errors.Is(err, sentinel) {
		t.Fatalf("counter error=%v", err)
	}
}
func TestRerankValidatesConfigurationAndInput(t *testing.T) {
	provider := &providerStub{}
	counter := wordCounter{}
	compressor := compressorStub{}
	if _, err := NewPipeline(nil, counter, compressor, Config{time.Second, 1, 1}); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil provider accepted")
	}
	pipeline, _ := NewPipeline(provider, counter, compressor, Config{time.Second, 10, 10})
	if _, err := pipeline.Run(context.Background(), " ", []Candidate{{}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("blank query accepted")
	}
	duplicate := []Candidate{{"ten_0001", "doc_0001", "chk_0001", 1, "one", 1}, {"ten_0001", "doc_0001", "chk_0001", 2, "two", 2}}
	if _, err := pipeline.Run(context.Background(), "q", duplicate); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate ID accepted")
	}
}

func BenchmarkPipeline100Candidates(b *testing.B) {
	counter := wordCounter{}
	compressor, _ := NewPrefixCompressor(counter)
	scores := make([]float64, 100)
	input := make([]Candidate, 100)
	for index := range 100 {
		scores[index] = float64(100 - index)
		input[index] = Candidate{"ten_0001", "doc_0001", knowledgeChunkID(index), uint64(index + 1), "representative retrieval content", index + 1}
	}
	pipeline, _ := NewPipeline(&providerStub{scores: scores}, counter, compressor, Config{time.Second, 1000, 100})
	b.ResetTimer()
	for range b.N {
		if _, err := pipeline.Run(context.Background(), "benchmark query", input); err != nil {
			b.Fatal(err)
		}
	}
}
func knowledgeChunkID(index int) knowledge.ChunkID {
	return knowledge.ChunkID(fmt.Sprintf("chk_%04d", index))
}
