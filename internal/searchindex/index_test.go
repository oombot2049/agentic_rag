package searchindex

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/agenticrag/agenticrag/internal/knowledge"
)

func TestMemoryAdapterIdempotentStageSealActivateAndDelete(t *testing.T) {
	ctx := context.Background()
	adapter := NewMemoryAdapter()
	v1 := Scope{"ten_0001", "kbs_0001", "ver_0001"}
	records := indexRecords(t, 5)
	if err := adapter.Upsert(ctx, v1, records[:3]); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Upsert(ctx, v1, records[:3]); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Upsert(ctx, v1, records[3:]); err != nil {
		t.Fatal(err)
	}
	manifest, _ := BuildManifest(records)
	if err := adapter.Seal(ctx, v1, manifest); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Seal(ctx, v1, manifest); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Activate(ctx, v1.TenantID, v1.KnowledgeBaseID, "", v1.VersionID); err != nil {
		t.Fatal(err)
	}
	active, err := adapter.ResolveActive(ctx, v1.TenantID, v1.KnowledgeBaseID)
	if err != nil || active != v1.VersionID {
		t.Fatalf("active=%s err=%v", active, err)
	}
	if _, err := adapter.DeleteVersion(ctx, v1, "", 2); !errors.Is(err, ErrActiveVersion) {
		t.Fatal("active version deleted")
	}
	v2 := Scope{"ten_0001", "kbs_0001", "ver_0002"}
	if err := adapter.Upsert(ctx, v2, records); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Seal(ctx, v2, manifest); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Activate(ctx, v2.TenantID, v2.KnowledgeBaseID, v1.VersionID, v2.VersionID); err != nil {
		t.Fatal(err)
	}
	deleted := 0
	cursor := ""
	for {
		page, err := adapter.DeleteVersion(ctx, v1, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		deleted += page.Deleted
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if deleted != 5 {
		t.Fatalf("deleted=%d", deleted)
	}
	page, err := adapter.DeleteVersion(ctx, v1, "", 2)
	if err != nil || page.Deleted != 0 {
		t.Fatal("final delete not idempotent")
	}
}
func TestConflictingUpsertAndBadManifestRejected(t *testing.T) {
	ctx := context.Background()
	adapter := NewMemoryAdapter()
	scope := Scope{"ten_0001", "kbs_0001", "ver_0001"}
	records := indexRecords(t, 1)
	_ = adapter.Upsert(ctx, scope, records)
	conflict := records[0]
	conflict.Vector[0] = 99
	if err := adapter.Upsert(ctx, scope, []Record{conflict}); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting overwrite accepted")
	}
	bad, _ := BuildManifest(indexRecords(t, 2))
	if err := adapter.Seal(ctx, scope, bad); !errors.Is(err, ErrConflict) {
		t.Fatal("bad manifest accepted")
	}
	if _, err := adapter.ResolveActive(ctx, scope.TenantID, scope.KnowledgeBaseID); !errors.Is(err, ErrNotFound) {
		t.Fatal("staged version visible")
	}
}
func TestConcurrentActivationHasOneWinnerAndNoDirtyRead(t *testing.T) {
	ctx := context.Background()
	adapter := NewMemoryAdapter()
	records := indexRecords(t, 1)
	base := Scope{"ten_0001", "kbs_0001", "ver_base"}
	_ = adapter.Upsert(ctx, base, records)
	manifest, _ := BuildManifest(records)
	_ = adapter.Seal(ctx, base, manifest)
	_ = adapter.Activate(ctx, base.TenantID, base.KnowledgeBaseID, "", base.VersionID)
	versions := []knowledge.VersionID{"ver_next1", "ver_next2"}
	for _, version := range versions {
		scope := Scope{"ten_0001", "kbs_0001", version}
		_ = adapter.Upsert(ctx, scope, records)
		_ = adapter.Seal(ctx, scope, manifest)
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, version := range versions {
		group.Add(1)
		go func(next knowledge.VersionID) {
			defer group.Done()
			results <- adapter.Activate(ctx, "ten_0001", "kbs_0001", base.VersionID, next)
		}(version)
	}
	group.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflicts)
	}
	active, _ := adapter.ResolveActive(ctx, "ten_0001", "kbs_0001")
	if active != versions[0] && active != versions[1] {
		t.Fatalf("dirty active=%s", active)
	}
}
func TestRecordValidationAndManifestOrder(t *testing.T) {
	records := indexRecords(t, 3)
	left, _ := BuildManifest(records)
	records[0], records[2] = records[2], records[0]
	right, _ := BuildManifest(records)
	if left != right {
		t.Fatal("manifest depends on input order")
	}
	bad := records[0]
	bad.Content = "changed"
	if err := ValidateRecord(Scope{"ten_0001", "kbs_0001", "ver_0001"}, bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("hash mismatch accepted")
	}
}
func indexRecords(t *testing.T, count int) []Record {
	t.Helper()
	acl, _ := knowledge.NewACL("ten_0001", 1, []knowledge.Grant{{Kind: knowledge.PrincipalRole, PrincipalID: "reader", Permission: knowledge.PermissionRead}})
	result := make([]Record, count)
	for index := range count {
		content := fmt.Sprintf("document chunk %d", index)
		id, _ := knowledge.ParseChunkID(fmt.Sprintf("chk_%04d", index))
		document, _ := knowledge.ParseDocumentID("doc_0001")
		result[index] = Record{DocumentID: document, ChunkID: id, Content: content, Model: "embed", ModelVersion: "v1", ContentHash: sha256.Sum256([]byte(content)), Vector: []float32{float32(index), 1}, ACL: acl}
	}
	return result
}
