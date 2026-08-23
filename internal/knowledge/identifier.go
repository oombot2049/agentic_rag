// Package knowledge defines persistence-independent knowledge and ACL models.
package knowledge

import (
	"errors"
	"fmt"
)

var (
	ErrInvalid    = errors.New("invalid knowledge model")
	ErrTransition = errors.New("invalid knowledge state transition")
)

type KnowledgeBaseID string
type DocumentID string
type VersionID string
type ChunkID string

func (id KnowledgeBaseID) String() string { return string(id) }
func (id DocumentID) String() string      { return string(id) }
func (id VersionID) String() string       { return string(id) }
func (id ChunkID) String() string         { return string(id) }

func ParseKnowledgeBaseID(value string) (KnowledgeBaseID, error) {
	return parseID("knowledge_base_id", "kbs_", value, func(v string) KnowledgeBaseID { return KnowledgeBaseID(v) })
}
func ParseDocumentID(value string) (DocumentID, error) {
	return parseID("document_id", "doc_", value, func(v string) DocumentID { return DocumentID(v) })
}
func ParseVersionID(value string) (VersionID, error) {
	return parseID("version_id", "ver_", value, func(v string) VersionID { return VersionID(v) })
}
func ParseChunkID(value string) (ChunkID, error) {
	return parseID("chunk_id", "chk_", value, func(v string) ChunkID { return ChunkID(v) })
}

func parseID[T ~string](field, prefix, value string, convert func(string) T) (T, error) {
	var zero T
	if len(value) < 8 || len(value) > 128 || len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return zero, fmt.Errorf("%w: %s has invalid prefix or length", ErrInvalid, field)
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return zero, fmt.Errorf("%w: %s contains invalid characters", ErrInvalid, field)
		}
	}
	return convert(value), nil
}
