package ingestion

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParserRegistryAndPlainTextFixtures(t *testing.T) {
	registry, err := NewParserRegistry(map[string]Parser{"text/plain; charset=utf-8": PlainTextParser{}})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := registry.Parser("TEXT/PLAIN; charset=UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	text, err := parser.Parse(context.Background(), strings.NewReader("  标题\r\n正文  "), 1024)
	if err != nil || text != "标题\n正文" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if _, err = registry.Parser("application/pdf"); !errors.Is(err, ErrInvalid) {
		t.Fatal("unknown parser accepted")
	}
}
func TestPlainTextParserBoundaries(t *testing.T) {
	parser := PlainTextParser{}
	for _, test := range []struct {
		name   string
		reader *strings.Reader
		limit  int64
	}{{"blank", strings.NewReader(" \n"), 10}, {"oversize", strings.NewReader("12345"), 4}, {"invalid utf8", strings.NewReader(string([]byte{0xff})), 10}} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parser.Parse(context.Background(), test.reader, test.limit); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
func TestFixedRuneChunkerUnicodeOverlapDeterministic(t *testing.T) {
	counter := TokenCounterFunc(func(_ context.Context, text string) (uint64, error) { return uint64(len([]rune(text))), nil })
	chunker, err := NewFixedRuneChunker(32, 4, counter)
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Repeat("界", 70)
	first, err := chunker.Chunk(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := chunker.Chunk(context.Background(), input)
	if len(first) != 3 || first[0].TokenCount != 32 || first[1].Content[:len("界界界界")] != first[0].Content[len(first[0].Content)-len("界界界界"):] || first[2].Ordinal != 3 {
		t.Fatalf("chunks=%+v", first)
	}
	for index := range first {
		if first[index] != second[index] {
			t.Fatal("chunking not deterministic")
		}
	}
}
func TestChunkerRejectsConfigurationAndTokenFailure(t *testing.T) {
	counter := TokenCounterFunc(func(context.Context, string) (uint64, error) { return 0, nil })
	for _, values := range [][2]int{{31, 0}, {32, 32}, {32769, 0}} {
		if _, err := NewFixedRuneChunker(values[0], values[1], counter); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid config accepted")
		}
	}
	chunker, _ := NewFixedRuneChunker(32, 0, counter)
	if _, err := chunker.Chunk(context.Background(), "content"); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero tokens accepted")
	}
}
