package knowledge

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type KnowledgeBaseStatus string

const (
	KnowledgeBaseActive   KnowledgeBaseStatus = "ACTIVE"
	KnowledgeBaseDeleting KnowledgeBaseStatus = "DELETING"
	KnowledgeBaseDeleted  KnowledgeBaseStatus = "DELETED"
)

type KnowledgeBase struct {
	id                   KnowledgeBaseID
	tenantID             domain.TenantID
	name                 string
	status               KnowledgeBaseStatus
	acl                  ACL
	createdAt, updatedAt time.Time
}

func NewKnowledgeBase(id KnowledgeBaseID, tenantID domain.TenantID, name string, acl ACL, at time.Time) (*KnowledgeBase, error) {
	if _, err := ParseKnowledgeBaseID(id.String()); err != nil {
		return nil, err
	}
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return nil, err
	}
	if !validName(name) || acl.tenantID != tenantID {
		return nil, fmt.Errorf("%w: knowledge base name or ACL", ErrInvalid)
	}
	at, err := validTime(at)
	if err != nil {
		return nil, err
	}
	return &KnowledgeBase{id: id, tenantID: tenantID, name: name, status: KnowledgeBaseActive, acl: acl, createdAt: at, updatedAt: at}, nil
}
func (k *KnowledgeBase) ReplaceACL(next ACL, at time.Time) error {
	if k.status != KnowledgeBaseActive {
		return ErrTransition
	}
	if next.tenantID != k.tenantID || next.revision != k.acl.revision+1 {
		return fmt.Errorf("%w: ACL tenant or revision", ErrInvalid)
	}
	at, err := mutationTime(k.updatedAt, at)
	if err != nil {
		return err
	}
	k.acl = next
	k.updatedAt = at
	return nil
}
func (k *KnowledgeBase) BeginDelete(at time.Time) error {
	return k.transition(KnowledgeBaseDeleting, at)
}
func (k *KnowledgeBase) FinishDelete(at time.Time) error {
	return k.transition(KnowledgeBaseDeleted, at)
}
func (k *KnowledgeBase) transition(next KnowledgeBaseStatus, at time.Time) error {
	legal := k.status == KnowledgeBaseActive && next == KnowledgeBaseDeleting || k.status == KnowledgeBaseDeleting && next == KnowledgeBaseDeleted
	if !legal {
		return ErrTransition
	}
	at, err := mutationTime(k.updatedAt, at)
	if err != nil {
		return err
	}
	k.status = next
	k.updatedAt = at
	return nil
}
func (k *KnowledgeBase) ID() KnowledgeBaseID         { return k.id }
func (k *KnowledgeBase) TenantID() domain.TenantID   { return k.tenantID }
func (k *KnowledgeBase) Name() string                { return k.name }
func (k *KnowledgeBase) Status() KnowledgeBaseStatus { return k.status }
func (k *KnowledgeBase) ACL() ACL                    { return cloneACL(k.acl) }

type Document struct {
	id               DocumentID
	tenantID         domain.TenantID
	knowledgeBaseID  KnowledgeBaseID
	sourceKey, title string
	createdAt        time.Time
}

func NewDocument(id DocumentID, knowledgeBase *KnowledgeBase, sourceKey, title string, at time.Time) (*Document, error) {
	if _, err := ParseDocumentID(id.String()); err != nil {
		return nil, err
	}
	if knowledgeBase == nil || knowledgeBase.status != KnowledgeBaseActive || !validOpaqueKey(sourceKey) || !validName(title) {
		return nil, fmt.Errorf("%w: document parent or metadata", ErrInvalid)
	}
	at, err := validTime(at)
	if err != nil {
		return nil, err
	}
	return &Document{id: id, tenantID: knowledgeBase.tenantID, knowledgeBaseID: knowledgeBase.id, sourceKey: sourceKey, title: title, createdAt: at}, nil
}
func (d *Document) ID() DocumentID                   { return d.id }
func (d *Document) TenantID() domain.TenantID        { return d.tenantID }
func (d *Document) KnowledgeBaseID() KnowledgeBaseID { return d.knowledgeBaseID }
func (d *Document) SourceKey() string                { return d.sourceKey }
func (d *Document) Title() string                    { return d.title }

type VersionStatus string

const (
	VersionPending    VersionStatus = "PENDING"
	VersionProcessing VersionStatus = "PROCESSING"
	VersionReady      VersionStatus = "READY"
	VersionFailed     VersionStatus = "FAILED"
	VersionSuperseded VersionStatus = "SUPERSEDED"
)

type Version struct {
	id                   VersionID
	tenantID             domain.TenantID
	knowledgeBaseID      KnowledgeBaseID
	documentID           DocumentID
	number               uint64
	sourceHash           [sha256.Size]byte
	status               VersionStatus
	chunkIDs             []ChunkID
	chunkOrdinals        map[uint64]ChunkID
	createdAt, updatedAt time.Time
}

func NewVersion(id VersionID, document *Document, number uint64, sourceHash [sha256.Size]byte, at time.Time) (*Version, error) {
	if _, err := ParseVersionID(id.String()); err != nil {
		return nil, err
	}
	if document == nil || number == 0 || zeroHash(sourceHash) {
		return nil, fmt.Errorf("%w: version parent, number or source hash", ErrInvalid)
	}
	at, err := validTime(at)
	if err != nil {
		return nil, err
	}
	return &Version{id: id, tenantID: document.tenantID, knowledgeBaseID: document.knowledgeBaseID, documentID: document.id, number: number, sourceHash: sourceHash, status: VersionPending, chunkOrdinals: make(map[uint64]ChunkID), createdAt: at, updatedAt: at}, nil
}
func (v *Version) Start(at time.Time) error     { return v.transition(VersionProcessing, at) }
func (v *Version) Ready(at time.Time) error     { return v.transition(VersionReady, at) }
func (v *Version) Fail(at time.Time) error      { return v.transition(VersionFailed, at) }
func (v *Version) Supersede(at time.Time) error { return v.transition(VersionSuperseded, at) }
func (v *Version) transition(next VersionStatus, at time.Time) error {
	legal := v.status == VersionPending && next == VersionProcessing || v.status == VersionProcessing && (next == VersionReady || next == VersionFailed) || v.status == VersionReady && next == VersionSuperseded
	if !legal {
		return ErrTransition
	}
	at, err := mutationTime(v.updatedAt, at)
	if err != nil {
		return err
	}
	v.status = next
	v.updatedAt = at
	return nil
}
func (v *Version) ID() VersionID                    { return v.id }
func (v *Version) TenantID() domain.TenantID        { return v.tenantID }
func (v *Version) KnowledgeBaseID() KnowledgeBaseID { return v.knowledgeBaseID }
func (v *Version) DocumentID() DocumentID           { return v.documentID }
func (v *Version) Number() uint64                   { return v.number }
func (v *Version) SourceHash() [sha256.Size]byte    { return v.sourceHash }
func (v *Version) Status() VersionStatus            { return v.status }
func (v *Version) ChunkIDs() []ChunkID              { return append([]ChunkID(nil), v.chunkIDs...) }

type Chunk struct {
	id                  ChunkID
	tenantID            domain.TenantID
	knowledgeBaseID     KnowledgeBaseID
	documentID          DocumentID
	versionID           VersionID
	ordinal, tokenCount uint64
	content             string
	contentHash         [sha256.Size]byte
	acl                 ACL
	createdAt           time.Time
}

func NewChunk(id ChunkID, knowledgeBase *KnowledgeBase, document *Document, version *Version, ordinal, tokenCount uint64, content string, acl ACL, at time.Time) (*Chunk, error) {
	if _, err := ParseChunkID(id.String()); err != nil {
		return nil, err
	}
	if knowledgeBase == nil || document == nil || version == nil {
		return nil, fmt.Errorf("%w: Chunk parent", ErrInvalid)
	}
	if knowledgeBase.status != KnowledgeBaseActive || version.status != VersionReady || document.tenantID != knowledgeBase.tenantID || version.tenantID != knowledgeBase.tenantID || document.knowledgeBaseID != knowledgeBase.id || version.knowledgeBaseID != knowledgeBase.id || version.documentID != document.id || !sameACL(acl, knowledgeBase.acl) {
		return nil, fmt.Errorf("%w: Chunk lineage or ACL", ErrInvalid)
	}
	if ordinal == 0 || tokenCount == 0 || len(content) > 1<<20 || strings.TrimSpace(content) == "" || !utf8.ValidString(content) {
		return nil, fmt.Errorf("%w: Chunk content, ordinal or token count", ErrInvalid)
	}
	at, err := validTime(at)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(content))
	if _, exists := version.chunkOrdinals[ordinal]; exists {
		return nil, fmt.Errorf("%w: duplicate Chunk ordinal", ErrInvalid)
	}
	for _, existing := range version.chunkIDs {
		if existing == id {
			return nil, fmt.Errorf("%w: duplicate Chunk ID", ErrInvalid)
		}
	}
	result := &Chunk{id: id, tenantID: knowledgeBase.tenantID, knowledgeBaseID: knowledgeBase.id, documentID: document.id, versionID: version.id, ordinal: ordinal, tokenCount: tokenCount, content: content, contentHash: hash, acl: cloneACL(acl), createdAt: at}
	version.chunkIDs = append(version.chunkIDs, id)
	version.chunkOrdinals[ordinal] = id
	return result, nil
}
func (c *Chunk) ID() ChunkID                      { return c.id }
func (c *Chunk) TenantID() domain.TenantID        { return c.tenantID }
func (c *Chunk) KnowledgeBaseID() KnowledgeBaseID { return c.knowledgeBaseID }
func (c *Chunk) DocumentID() DocumentID           { return c.documentID }
func (c *Chunk) VersionID() VersionID             { return c.versionID }
func (c *Chunk) Ordinal() uint64                  { return c.ordinal }
func (c *Chunk) TokenCount() uint64               { return c.tokenCount }
func (c *Chunk) Content() string                  { return c.content }
func (c *Chunk) ContentHash() [sha256.Size]byte   { return c.contentHash }
func (c *Chunk) ACL() ACL                         { return cloneACL(c.acl) }

func cloneACL(value ACL) ACL { value.grants = append([]Grant(nil), value.grants...); return value }
func sameACL(left, right ACL) bool {
	return left.tenantID == right.tenantID && left.revision == right.revision && slices.Equal(left.grants, right.grants)
}
func validName(value string) bool {
	return len(value) >= 1 && len(value) <= 256 && strings.TrimSpace(value) == value && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}
func validOpaqueKey(value string) bool {
	return len(value) >= 1 && len(value) <= 512 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}
func zeroHash(value [sha256.Size]byte) bool { return value == [sha256.Size]byte{} }
func validTime(value time.Time) (time.Time, error) {
	if value.IsZero() {
		return time.Time{}, fmt.Errorf("%w: zero time", ErrInvalid)
	}
	return value.UTC(), nil
}
func mutationTime(previous, value time.Time) (time.Time, error) {
	value, err := validTime(value)
	if err != nil {
		return time.Time{}, err
	}
	if value.Before(previous) {
		return time.Time{}, fmt.Errorf("%w: time moved backwards", ErrInvalid)
	}
	return value, nil
}
