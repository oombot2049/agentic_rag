package ingestion

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/agenticrag/agenticrag/internal/artifact"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

const maximumSourceBytes int64 = 32 << 20

type SourceObject struct {
	Body      io.ReadCloser
	MediaType string
	Size      int64
	SHA256    [sha256.Size]byte
}
type Source interface {
	Open(context.Context, domain.TenantID, string) (SourceObject, error)
}
type Publication struct {
	TaskID          string
	ArtifactID      string
	KnowledgeBaseID knowledge.KnowledgeBaseID
	DocumentID      knowledge.DocumentID
	Version         *knowledge.Version
	Chunks          []*knowledge.Chunk
}
type Publisher interface {
	Publish(context.Context, Publication) error
}
type ChunkIDFactory interface {
	ChunkID(knowledge.VersionID, uint64) (knowledge.ChunkID, error)
}
type ChunkIDFactoryFunc func(knowledge.VersionID, uint64) (knowledge.ChunkID, error)

func (f ChunkIDFactoryFunc) ChunkID(version knowledge.VersionID, ordinal uint64) (knowledge.ChunkID, error) {
	return f(version, ordinal)
}

type Request struct {
	ArtifactID    string
	KnowledgeBase *knowledge.KnowledgeBase
	Document      *knowledge.Document
	VersionID     knowledge.VersionID
	VersionNumber uint64
}
type Pipeline struct {
	source     Source
	parsers    *ParserRegistry
	chunker    *FixedRuneChunker
	publisher  Publisher
	ids        ChunkIDFactory
	now        func() time.Time
	retryDelay time.Duration
}

func NewPipeline(source Source, parsers *ParserRegistry, chunker *FixedRuneChunker, publisher Publisher, ids ChunkIDFactory) (*Pipeline, error) {
	if source == nil || parsers == nil || chunker == nil || publisher == nil || ids == nil {
		return nil, fmt.Errorf("%w: pipeline dependencies", ErrInvalid)
	}
	return &Pipeline{source: source, parsers: parsers, chunker: chunker, publisher: publisher, ids: ids, now: time.Now, retryDelay: time.Second}, nil
}
func (p *Pipeline) Process(ctx context.Context, task *Task, request Request) error {
	if ctx == nil || task == nil || !validRequest(request) {
		return fmt.Errorf("%w: process command", ErrInvalid)
	}
	started := p.now().UTC()
	if err := task.Start(started); err != nil {
		return err
	}
	failure := func(code string, cause error) error { return p.fail(task, code, cause, p.now().UTC()) }
	object, err := p.source.Open(ctx, request.KnowledgeBase.TenantID(), request.ArtifactID)
	if err != nil {
		return failure("SOURCE_OPEN_FAILED", err)
	}
	if object.Body == nil || object.Size < 1 || object.Size > maximumSourceBytes || object.SHA256 == [sha256.Size]byte{} {
		if object.Body != nil {
			_ = object.Body.Close()
		}
		return failure("SOURCE_INVALID", ErrInvalid)
	}
	parser, err := p.parsers.Parser(object.MediaType)
	if err != nil {
		_ = object.Body.Close()
		return failure("MEDIA_TYPE_UNSUPPORTED", err)
	}
	text, parseErr := parser.Parse(ctx, object.Body, maximumSourceBytes)
	closeErr := object.Body.Close()
	if parseErr != nil {
		return failure("PARSE_FAILED", parseErr)
	}
	if closeErr != nil {
		return failure("SOURCE_CLOSE_FAILED", closeErr)
	}
	drafts, err := p.chunker.Chunk(ctx, text)
	if err != nil {
		return failure("CHUNK_FAILED", err)
	}
	version, err := knowledge.NewVersion(request.VersionID, request.Document, request.VersionNumber, object.SHA256, started)
	if err != nil {
		return failure("VERSION_INVALID", err)
	}
	if err = version.Start(started); err != nil {
		return failure("VERSION_INVALID", err)
	}
	if err = version.Ready(p.now().UTC()); err != nil {
		return failure("VERSION_INVALID", err)
	}
	chunks := make([]*knowledge.Chunk, 0, len(drafts))
	acl := request.KnowledgeBase.ACL()
	for _, draft := range drafts {
		id, idErr := p.ids.ChunkID(request.VersionID, draft.Ordinal)
		if idErr != nil {
			return failure("CHUNK_ID_FAILED", idErr)
		}
		chunk, chunkErr := knowledge.NewChunk(id, request.KnowledgeBase, request.Document, version, draft.Ordinal, draft.TokenCount, draft.Content, acl, p.now().UTC())
		if chunkErr != nil {
			return failure("CHUNK_INVALID", chunkErr)
		}
		chunks = append(chunks, chunk)
	}
	err = p.publisher.Publish(ctx, Publication{TaskID: task.ID(), ArtifactID: request.ArtifactID, KnowledgeBaseID: request.KnowledgeBase.ID(), DocumentID: request.Document.ID(), Version: version, Chunks: chunks})
	if err != nil {
		return failure("PUBLISH_FAILED", err)
	}
	if err = task.Complete(p.now().UTC()); err != nil {
		return err
	}
	return nil
}

func validRequest(request Request) bool {
	if request.KnowledgeBase == nil || request.Document == nil || request.VersionNumber == 0 || !validArtifactID(request.ArtifactID) {
		return false
	}
	if _, err := knowledge.ParseVersionID(request.VersionID.String()); err != nil {
		return false
	}
	return request.KnowledgeBase.Status() == knowledge.KnowledgeBaseActive && request.Document.TenantID() == request.KnowledgeBase.TenantID() && request.Document.KnowledgeBaseID() == request.KnowledgeBase.ID()
}

func validArtifactID(value string) bool {
	if len(value) < 8 || len(value) > 128 || value[:4] != "art_" {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}
func (p *Pipeline) fail(task *Task, reason string, cause error, at time.Time) error {
	if retryable(cause) && task.Attempt() < task.MaxAttempts() {
		if err := task.ScheduleRetry(reason, p.retryDelay, at); err != nil {
			return errors.Join(cause, err)
		}
	} else if err := task.Fail(reason, at); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// ArtifactSource adapts the immutable Artifact service without exposing its
// storage implementation to the ingestion core.
type ArtifactDownloader interface {
	Download(context.Context, domain.TenantID, string) (artifact.Metadata, io.ReadCloser, error)
}
type ArtifactSource struct{ downloader ArtifactDownloader }

func NewArtifactSource(downloader ArtifactDownloader) (*ArtifactSource, error) {
	if downloader == nil {
		return nil, fmt.Errorf("%w: Artifact downloader", ErrInvalid)
	}
	return &ArtifactSource{downloader: downloader}, nil
}
func (s *ArtifactSource) Open(ctx context.Context, tenantID domain.TenantID, id string) (SourceObject, error) {
	metadata, body, err := s.downloader.Download(ctx, tenantID, id)
	if err != nil {
		return SourceObject{}, err
	}
	return SourceObject{Body: body, MediaType: metadata.MediaType, Size: metadata.Size, SHA256: metadata.SHA256}, nil
}
