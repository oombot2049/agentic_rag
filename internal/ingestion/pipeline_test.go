package ingestion

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/artifact"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

type sourceStub struct {
	object SourceObject
	err    error
	opens  int
}

func (s *sourceStub) Open(context.Context, domain.TenantID, string) (SourceObject, error) {
	s.opens++
	return s.object, s.err
}

type closeReader struct {
	io.Reader
	closed bool
	err    error
}
type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func (r *closeReader) Close() error { r.closed = true; return r.err }

type publisherStub struct {
	err    error
	calls  int
	active knowledge.VersionID
	chunks map[knowledge.ChunkID][sha256.Size]byte
}

func (p *publisherStub) Publish(_ context.Context, value Publication) error {
	p.calls++
	if p.err != nil {
		return p.err
	}
	next := make(map[knowledge.ChunkID][sha256.Size]byte, len(value.Chunks))
	for _, chunk := range value.Chunks {
		if existing, ok := next[chunk.ID()]; ok && existing != chunk.ContentHash() {
			return errors.New("conflict")
		}
		next[chunk.ID()] = chunk.ContentHash()
	}
	if p.active == value.Version.ID() {
		for id, hash := range next {
			if p.chunks[id] != hash {
				return errors.New("replay conflict")
			}
		}
		return nil
	}
	p.active = value.Version.ID()
	p.chunks = next
	return nil
}
func TestPipelineFixtureAtomicPublishAndIdempotentRedelivery(t *testing.T) {
	pipeline, source, publisher, request := fixturePipeline(t, "first paragraph\n\n第二段")
	task, _ := NewTask("ing_0001", 3, ingestionTime)
	if err := pipeline.Process(context.Background(), task, request); err != nil {
		t.Fatal(err)
	}
	if task.Status() != TaskCompleted || publisher.calls != 1 || publisher.active != "ver_0001" || len(publisher.chunks) != 1 || !source.object.Body.(*closeReader).closed {
		t.Fatalf("task=%+v publisher=%+v", task, publisher)
	}
	// Queue redelivery may reconstruct Task state before observing completion;
	// stable IDs make Publisher replay a no-op instead of duplicating Chunks.
	source.object.Body = &closeReader{Reader: strings.NewReader("first paragraph\n\n第二段")}
	replayed, _ := NewTask("ing_0001", 3, ingestionTime)
	if err := pipeline.Process(context.Background(), replayed, request); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 2 || len(publisher.chunks) != 1 {
		t.Fatal("redelivery duplicated publication")
	}
}
func TestPipelinePublisherFailureDoesNotSwitchLiveVersionAndRetries(t *testing.T) {
	pipeline, _, publisher, request := fixturePipeline(t, "content")
	publisher.active = "ver_live"
	publisher.chunks = map[knowledge.ChunkID][sha256.Size]byte{"chk_live": sha256.Sum256([]byte("live"))}
	publisher.err = Transient(errors.New("database unavailable"))
	task, _ := NewTask("ing_0001", 2, ingestionTime)
	err := pipeline.Process(context.Background(), task, request)
	if err == nil || task.Status() != TaskRetryWait || publisher.active != "ver_live" || len(publisher.chunks) != 1 {
		t.Fatalf("failure leaked: task=%+v active=%s", task, publisher.active)
	}
	pipeline.now = func() time.Time { return ingestionTime.Add(2 * time.Second) }
	if err = task.Ready(ingestionTime.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	sourceBody := &closeReader{Reader: strings.NewReader("content")}
	pipeline.source.(*sourceStub).object.Body = sourceBody
	err = pipeline.Process(context.Background(), task, request)
	if err == nil || task.Status() != TaskFailed || publisher.active != "ver_live" {
		t.Fatalf("exhausted=%v task=%+v", err, task)
	}
}
func TestPipelinePermanentFailuresDoNotRetry(t *testing.T) {
	pipeline, source, _, request := fixturePipeline(t, "content")
	task, _ := NewTask("ing_0001", 3, ingestionTime)
	source.object.MediaType = "application/pdf"
	if err := pipeline.Process(context.Background(), task, request); !errors.Is(err, ErrInvalid) || task.Status() != TaskFailed || task.ReasonCode() != "MEDIA_TYPE_UNSUPPORTED" {
		t.Fatalf("error=%v task=%+v", err, task)
	}
	pipeline, source, _, request = fixturePipeline(t, "content")
	task, _ = NewTask("ing_0002", 3, ingestionTime)
	source.object.Body = &closeReader{Reader: strings.NewReader("content"), err: errors.New("close")}
	if err := pipeline.Process(context.Background(), task, request); err == nil || task.Status() != TaskFailed || task.ReasonCode() != "SOURCE_CLOSE_FAILED" {
		t.Fatalf("close error=%v task=%+v", err, task)
	}
}

func TestPipelineEveryTransientStageSchedulesRetry(t *testing.T) {
	sentinel := errors.New("injected")
	tests := []struct {
		name, reason string
		mutate       func(*Pipeline, *sourceStub, *publisherStub)
	}{
		{"source", "SOURCE_OPEN_FAILED", func(_ *Pipeline, s *sourceStub, _ *publisherStub) { s.err = Transient(sentinel) }},
		{"parse read", "PARSE_FAILED", func(_ *Pipeline, s *sourceStub, _ *publisherStub) {
			s.object.Body = &closeReader{Reader: errorReader{err: Transient(sentinel)}}
		}},
		{"tokens", "CHUNK_FAILED", func(p *Pipeline, _ *sourceStub, _ *publisherStub) {
			p.chunker.tokens = TokenCounterFunc(func(context.Context, string) (uint64, error) { return 0, Transient(sentinel) })
		}},
		{"chunk ID", "CHUNK_ID_FAILED", func(p *Pipeline, _ *sourceStub, _ *publisherStub) {
			p.ids = ChunkIDFactoryFunc(func(knowledge.VersionID, uint64) (knowledge.ChunkID, error) { return "", Transient(sentinel) })
		}},
		{"publish", "PUBLISH_FAILED", func(_ *Pipeline, _ *sourceStub, p *publisherStub) { p.err = Transient(sentinel) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pipeline, source, publisher, request := fixturePipeline(t, "content")
			test.mutate(pipeline, source, publisher)
			task, _ := NewTask("ing_0001", 3, ingestionTime)
			err := pipeline.Process(context.Background(), task, request)
			if !errors.Is(err, sentinel) || task.Status() != TaskRetryWait || task.ReasonCode() != test.reason {
				t.Fatalf("error=%v task=%+v", err, task)
			}
		})
	}
}

func TestPipelineRejectsInvalidCommandBeforeAttempt(t *testing.T) {
	pipeline, _, _, request := fixturePipeline(t, "content")
	for _, mutate := range []func(*Request){func(r *Request) { r.ArtifactID = "bad" }, func(r *Request) { r.VersionID = "bad" }, func(r *Request) { r.VersionNumber = 0 }, func(r *Request) { r.Document = nil }} {
		candidate := request
		mutate(&candidate)
		task, _ := NewTask("ing_0001", 2, ingestionTime)
		if err := pipeline.Process(context.Background(), task, candidate); !errors.Is(err, ErrInvalid) || task.Attempt() != 0 || task.Status() != TaskQueued {
			t.Fatalf("invalid command consumed attempt: %v %+v", err, task)
		}
	}
}
func fixturePipeline(t *testing.T, text string) (*Pipeline, *sourceStub, *publisherStub, Request) {
	t.Helper()
	acl, _ := knowledge.NewACL("ten_0001", 1, []knowledge.Grant{{Kind: knowledge.PrincipalUser, PrincipalID: "alice", Permission: knowledge.PermissionRead}})
	kb, _ := knowledge.NewKnowledgeBase("kbs_0001", "ten_0001", "KB", acl, ingestionTime)
	document, _ := knowledge.NewDocument("doc_0001", kb, "fixture", "Fixture", ingestionTime)
	body := &closeReader{Reader: strings.NewReader(text)}
	source := &sourceStub{object: SourceObject{Body: body, MediaType: "text/plain", Size: int64(len(text)), SHA256: sha256.Sum256([]byte(text))}}
	registry, _ := NewParserRegistry(map[string]Parser{"text/plain": PlainTextParser{}})
	chunker, _ := NewFixedRuneChunker(128, 16, TokenCounterFunc(func(_ context.Context, value string) (uint64, error) { return uint64(len([]rune(value))), nil }))
	publisher := &publisherStub{}
	ids := ChunkIDFactoryFunc(func(version knowledge.VersionID, ordinal uint64) (knowledge.ChunkID, error) {
		return knowledge.ParseChunkID(fmt.Sprintf("chk_%s_%04d", strings.TrimPrefix(version.String(), "ver_"), ordinal))
	})
	pipeline, _ := NewPipeline(source, registry, chunker, publisher, ids)
	clock := ingestionTime
	pipeline.now = func() time.Time { clock = clock.Add(time.Millisecond); return clock }
	return pipeline, source, publisher, Request{ArtifactID: "art_0001", KnowledgeBase: kb, Document: document, VersionID: "ver_0001", VersionNumber: 1}
}

type downloaderStub struct {
	metadata artifact.Metadata
	body     io.ReadCloser
	err      error
}

func (d downloaderStub) Download(context.Context, domain.TenantID, string) (artifact.Metadata, io.ReadCloser, error) {
	return d.metadata, d.body, d.err
}
func TestArtifactSourceAdapter(t *testing.T) {
	hash := sha256.Sum256([]byte("x"))
	body := io.NopCloser(strings.NewReader("x"))
	source, err := NewArtifactSource(downloaderStub{metadata: artifact.Metadata{MediaType: "text/plain", Size: 1, SHA256: hash}, body: body})
	if err != nil {
		t.Fatal(err)
	}
	object, err := source.Open(context.Background(), "ten_0001", "art_0001")
	if err != nil || object.Size != 1 || object.SHA256 != hash {
		t.Fatalf("object=%+v err=%v", object, err)
	}
	if _, err = NewArtifactSource(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil downloader accepted")
	}
}
