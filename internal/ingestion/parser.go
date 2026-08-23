package ingestion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"
)

type Parser interface {
	Parse(context.Context, io.Reader, int64) (string, error)
}
type ParserFunc func(context.Context, io.Reader, int64) (string, error)

func (f ParserFunc) Parse(ctx context.Context, reader io.Reader, limit int64) (string, error) {
	return f(ctx, reader, limit)
}

type ParserRegistry struct{ byMediaType map[string]Parser }

func NewParserRegistry(values map[string]Parser) (*ParserRegistry, error) {
	if len(values) == 0 || len(values) > 64 {
		return nil, fmt.Errorf("%w: parser registry size", ErrInvalid)
	}
	owned := make(map[string]Parser, len(values))
	for raw, parser := range values {
		mediaType, err := canonicalMediaType(raw)
		if err != nil || parser == nil {
			return nil, fmt.Errorf("%w: parser registration", ErrInvalid)
		}
		if _, exists := owned[mediaType]; exists {
			return nil, fmt.Errorf("%w: duplicate parser media type", ErrInvalid)
		}
		owned[mediaType] = parser
	}
	return &ParserRegistry{byMediaType: owned}, nil
}
func (r *ParserRegistry) Parser(mediaType string) (Parser, error) {
	canonical, err := canonicalMediaType(mediaType)
	if err != nil {
		return nil, err
	}
	parser, ok := r.byMediaType[canonical]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported media type", ErrInvalid)
	}
	return parser, nil
}
func canonicalMediaType(value string) (string, error) {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil || parsed == "" {
		return "", fmt.Errorf("%w: media type", ErrInvalid)
	}
	return strings.ToLower(parsed), nil
}

type PlainTextParser struct{}

func (PlainTextParser) Parse(ctx context.Context, reader io.Reader, limit int64) (string, error) {
	if ctx == nil || reader == nil || limit < 1 {
		return "", fmt.Errorf("%w: parser arguments", ErrInvalid)
	}
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: reader}, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("%w: parsed text exceeds limit", ErrInvalid)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%w: parsed text is not UTF-8", ErrInvalid)
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if text == "" {
		return "", fmt.Errorf("%w: parsed text is blank", ErrInvalid)
	}
	return text, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}

type TokenCounter interface {
	Count(context.Context, string) (uint64, error)
}
type TokenCounterFunc func(context.Context, string) (uint64, error)

func (f TokenCounterFunc) Count(ctx context.Context, text string) (uint64, error) {
	return f(ctx, text)
}

type ChunkDraft struct {
	Ordinal, TokenCount uint64
	Content             string
}
type FixedRuneChunker struct {
	maxRunes, overlap int
	tokens            TokenCounter
}

func NewFixedRuneChunker(maxRunes, overlap int, tokens TokenCounter) (*FixedRuneChunker, error) {
	if maxRunes < 32 || maxRunes > 32768 || overlap < 0 || overlap >= maxRunes || tokens == nil {
		return nil, fmt.Errorf("%w: chunker configuration", ErrInvalid)
	}
	return &FixedRuneChunker{maxRunes: maxRunes, overlap: overlap, tokens: tokens}, nil
}
func (c *FixedRuneChunker) Chunk(ctx context.Context, text string) ([]ChunkDraft, error) {
	if ctx == nil || strings.TrimSpace(text) == "" || !utf8.ValidString(text) {
		return nil, fmt.Errorf("%w: chunk text", ErrInvalid)
	}
	runes := []rune(text)
	step := c.maxRunes - c.overlap
	result := make([]ChunkDraft, 0, (len(runes)+step-1)/step)
	for start := 0; start < len(runes); start += step {
		end := min(start+c.maxRunes, len(runes))
		content := string(runes[start:end])
		count, err := c.tokens.Count(ctx, content)
		if err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, fmt.Errorf("%w: zero token Chunk", ErrInvalid)
		}
		result = append(result, ChunkDraft{Ordinal: uint64(len(result) + 1), TokenCount: count, Content: content})
		if end == len(runes) {
			break
		}
	}
	if len(result) == 0 {
		return nil, errors.New("chunker produced no Chunks")
	}
	return result, nil
}
