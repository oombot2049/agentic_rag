package knowledge

import (
	"crypto/sha256"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

var knowledgeTime = time.Date(2026, 8, 23, 5, 0, 0, 0, time.UTC)

func TestKnowledgeLineageHashAndACL(t *testing.T) {
	acl, err := NewACL("ten_0001", 1, []Grant{{Kind: PrincipalUser, PrincipalID: "alice", Permission: PermissionAdmin}, {Kind: PrincipalGroup, PrincipalID: "readers", Permission: PermissionRead}})
	if err != nil {
		t.Fatal(err)
	}
	kb, err := NewKnowledgeBase("kbs_0001", "ten_0001", "Product docs", acl, knowledgeTime)
	if err != nil {
		t.Fatal(err)
	}
	document, err := NewDocument("doc_0001", kb, "manual-v1", "Manual", knowledgeTime)
	if err != nil {
		t.Fatal(err)
	}
	version, err := NewVersion("ver_0001", document, 1, sha256.Sum256([]byte("source")), knowledgeTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := version.Start(knowledgeTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := version.Ready(knowledgeTime.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	chunk, err := NewChunk("chk_0001", kb, document, version, 1, 3, "hello 世界", acl, knowledgeTime.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if chunk.TenantID() != "ten_0001" || chunk.KnowledgeBaseID() != kb.ID() || chunk.DocumentID() != document.ID() || chunk.VersionID() != version.ID() || chunk.ContentHash() != sha256.Sum256([]byte("hello 世界")) {
		t.Fatalf("lineage/hash=%+v", chunk)
	}
	if acl.TenantID() != "ten_0001" || kb.TenantID() != "ten_0001" || kb.Name() != "Product docs" || kb.Status() != KnowledgeBaseActive || document.TenantID() != "ten_0001" || document.KnowledgeBaseID() != kb.ID() || document.SourceKey() != "manual-v1" || document.Title() != "Manual" {
		t.Fatal("aggregate getters lost identity")
	}
	if version.TenantID() != "ten_0001" || version.KnowledgeBaseID() != kb.ID() || version.DocumentID() != document.ID() || version.Number() != 1 || version.SourceHash() != sha256.Sum256([]byte("source")) || version.Status() != VersionReady {
		t.Fatal("version getters lost state")
	}
	if chunk.ID() != "chk_0001" || chunk.Ordinal() != 1 || chunk.TokenCount() != 3 || chunk.Content() != "hello 世界" {
		t.Fatal("Chunk getters lost state")
	}
	if !chunk.ACL().Allows("ten_0001", PrincipalUser, "alice", PermissionRead) || chunk.ACL().Allows("ten_9999", PrincipalUser, "alice", PermissionRead) {
		t.Fatal("tenant ACL boundary failed")
	}
	copyIDs := version.ChunkIDs()
	copyIDs[0] = "chk_evil"
	if version.ChunkIDs()[0] != "chk_0001" {
		t.Fatal("Chunk IDs leaked mutable storage")
	}
	if _, err := NewChunk("chk_0002", kb, document, version, 1, 1, "duplicate ordinal", acl, knowledgeTime.Add(4*time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate ordinal=%v", err)
	}
	changed, _ := NewChunk("chk_0003", kb, document, version, 2, 1, "hello 世界!", acl, knowledgeTime.Add(4*time.Second))
	if changed.ContentHash() == chunk.ContentHash() {
		t.Fatal("content mutation retained hash")
	}
}

func TestACLCanonicalDefaultDenyAndRevision(t *testing.T) {
	input := []Grant{{Kind: PrincipalUser, PrincipalID: "bob", Permission: PermissionWrite}, {Kind: PrincipalGroup, PrincipalID: "alpha", Permission: PermissionRead}}
	acl, err := NewACL("ten_0001", 1, input)
	if err != nil {
		t.Fatal(err)
	}
	input[0].PrincipalID = "mutated"
	grants := acl.Grants()
	if grants[0].Kind != PrincipalGroup || grants[1].PrincipalID != "bob" {
		t.Fatalf("not canonical: %+v", grants)
	}
	grants[0].PrincipalID = "mutated"
	if acl.Grants()[0].PrincipalID != "alpha" {
		t.Fatal("ACL grants leaked mutable slice")
	}
	if !acl.Allows("ten_0001", PrincipalUser, "bob", PermissionRead) || !acl.Allows("ten_0001", PrincipalUser, "bob", PermissionWrite) || acl.Allows("ten_0001", PrincipalUser, "bob", PermissionAdmin) || acl.Allows("ten_0001", PrincipalUser, "unknown", PermissionRead) {
		t.Fatal("permission hierarchy/default deny failed")
	}
	if _, err := NewACL("ten_0001", 1, []Grant{{Kind: PrincipalUser, PrincipalID: "bob", Permission: PermissionRead}, {Kind: PrincipalUser, PrincipalID: "bob", Permission: PermissionAdmin}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate accepted")
	}
	kb, _ := NewKnowledgeBase("kbs_0001", "ten_0001", "name", acl, knowledgeTime)
	next, _ := NewACL("ten_0001", 2, []Grant{{Kind: PrincipalRole, PrincipalID: "admin", Permission: PermissionAdmin}})
	if err := kb.ReplaceACL(next, knowledgeTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if kb.ACL().Revision() != 2 {
		t.Fatal("revision not advanced")
	}
	if err := kb.ReplaceACL(next, knowledgeTime.Add(2*time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatal("non-monotonic revision accepted")
	}
}

func TestKnowledgeBaseLifecycleAndInvalidBoundaries(t *testing.T) {
	acl, _ := NewACL("ten_0001", 1, []Grant{{Kind: PrincipalUser, PrincipalID: "alice", Permission: PermissionRead}})
	for _, id := range []KnowledgeBaseID{"", "kb_wrong", "kbs_bad!"} {
		if _, err := NewKnowledgeBase(id, "ten_0001", "name", acl, knowledgeTime); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid ID %q accepted: %v", id, err)
		}
	}
	if _, err := NewKnowledgeBase("kbs_0001", "ten_0001", "bad\nname", acl, knowledgeTime); !errors.Is(err, ErrInvalid) {
		t.Fatal("control character in name accepted")
	}
	kb, _ := NewKnowledgeBase("kbs_0001", "ten_0001", "name", acl, knowledgeTime)
	if err := kb.FinishDelete(knowledgeTime); !errors.Is(err, ErrTransition) {
		t.Fatal("ACTIVE -> DELETED accepted")
	}
	if err := kb.BeginDelete(knowledgeTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := kb.ReplaceACL(acl, knowledgeTime.Add(2*time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatal("deleting ACL mutated")
	}
	if err := kb.FinishDelete(knowledgeTime.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := kb.BeginDelete(knowledgeTime.Add(3 * time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatal("terminal KB reopened")
	}
}

func TestDocumentAndVersionRejectInvalidInputs(t *testing.T) {
	acl, _ := NewACL("ten_0001", 1, []Grant{{Kind: PrincipalUser, PrincipalID: "alice", Permission: PermissionRead}})
	kb, _ := NewKnowledgeBase("kbs_0001", "ten_0001", "name", acl, knowledgeTime)
	for _, test := range []struct {
		id            DocumentID
		parent        *KnowledgeBase
		source, title string
		at            time.Time
	}{{"bad", kb, "source", "title", knowledgeTime}, {"doc_0001", nil, "source", "title", knowledgeTime}, {"doc_0001", kb, "bad\nkey", "title", knowledgeTime}, {"doc_0001", kb, "source", " ", knowledgeTime}, {"doc_0001", kb, "source", "title", time.Time{}}} {
		if _, err := NewDocument(test.id, test.parent, test.source, test.title, test.at); !errors.Is(err, ErrInvalid) {
			t.Errorf("document case accepted: %v", err)
		}
	}
	document, _ := NewDocument("doc_0001", kb, "source", "title", knowledgeTime)
	hash := sha256.Sum256([]byte("source"))
	for _, test := range []struct {
		id     VersionID
		parent *Document
		number uint64
		hash   [sha256.Size]byte
		at     time.Time
	}{{"bad", document, 1, hash, knowledgeTime}, {"ver_0001", nil, 1, hash, knowledgeTime}, {"ver_0001", document, 0, hash, knowledgeTime}, {"ver_0001", document, 1, [sha256.Size]byte{}, knowledgeTime}, {"ver_0001", document, 1, hash, time.Time{}}} {
		if _, err := NewVersion(test.id, test.parent, test.number, test.hash, test.at); !errors.Is(err, ErrInvalid) {
			t.Errorf("version case accepted: %v", err)
		}
	}
}

func TestChunkRejectsForgedSameTenantACL(t *testing.T) {
	kb, document, version, acl := readyFixture(t)
	forged, _ := NewACL("ten_0001", acl.Revision(), []Grant{{Kind: PrincipalRole, PrincipalID: "everyone", Permission: PermissionRead}})
	if _, err := NewChunk("chk_0001", kb, document, version, 1, 1, "secret", forged, knowledgeTime); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged ACL accepted: %v", err)
	}
	next, _ := NewACL("ten_0001", 2, acl.Grants())
	if _, err := NewChunk("chk_0001", kb, document, version, 1, 1, "secret", next, knowledgeTime); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future ACL accepted: %v", err)
	}
}

func TestVersionTransitionMatrixIsAtomic(t *testing.T) {
	type transition func(*Version) error
	transitions := map[VersionStatus]transition{VersionProcessing: func(v *Version) error { return v.Start(knowledgeTime.Add(time.Second)) }, VersionReady: func(v *Version) error { return v.Ready(knowledgeTime.Add(time.Second)) }, VersionFailed: func(v *Version) error { return v.Fail(knowledgeTime.Add(time.Second)) }, VersionSuperseded: func(v *Version) error { return v.Supersede(knowledgeTime.Add(time.Second)) }}
	legal := map[[2]VersionStatus]bool{{VersionPending, VersionProcessing}: true, {VersionProcessing, VersionReady}: true, {VersionProcessing, VersionFailed}: true, {VersionReady, VersionSuperseded}: true}
	for _, from := range []VersionStatus{VersionPending, VersionProcessing, VersionReady, VersionFailed, VersionSuperseded} {
		for to, apply := range transitions {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				version := readyVersion(t)
				version.status = from
				before := version.updatedAt
				err := apply(version)
				if legal[[2]VersionStatus{from, to}] {
					if err != nil || version.status != to {
						t.Fatalf("legal transition=%v status=%s", err, version.status)
					}
				} else if !errors.Is(err, ErrTransition) || version.status != from || version.updatedAt != before {
					t.Fatalf("illegal transition mutated: %v %s", err, version.status)
				}
			})
		}
	}
}

func TestChunkRejectsInvalidAndCrossTenantLineageProperty(t *testing.T) {
	kb, document, version, acl := readyFixture(t)
	cases := []func(*KnowledgeBase, *Document, *Version, ACL) (*Chunk, error){
		func(k *KnowledgeBase, d *Document, v *Version, a ACL) (*Chunk, error) {
			return NewChunk("bad", k, d, v, 1, 1, "x", a, knowledgeTime)
		},
		func(k *KnowledgeBase, d *Document, v *Version, a ACL) (*Chunk, error) {
			return NewChunk("chk_0001", k, d, v, 0, 1, "x", a, knowledgeTime)
		},
		func(k *KnowledgeBase, d *Document, v *Version, a ACL) (*Chunk, error) {
			return NewChunk("chk_0001", k, d, v, 1, 0, "x", a, knowledgeTime)
		},
		func(k *KnowledgeBase, d *Document, v *Version, a ACL) (*Chunk, error) {
			return NewChunk("chk_0001", k, d, v, 1, 1, " \n", a, knowledgeTime)
		},
		func(k *KnowledgeBase, d *Document, v *Version, a ACL) (*Chunk, error) {
			return NewChunk("chk_0001", k, d, v, 1, 1, strings.Repeat("x", 1<<20+1), a, knowledgeTime)
		},
	}
	for index, run := range cases {
		if _, err := run(kb, document, version, acl); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d=%v", index, err)
		}
	}
	random := rand.New(rand.NewSource(42))
	for range 1_000 {
		foreign := *document
		if random.Intn(2) == 0 {
			foreign.tenantID = "ten_9999"
		} else {
			foreign.knowledgeBaseID = "kbs_9999"
		}
		if _, err := NewChunk("chk_random", kb, &foreign, version, uint64(random.Intn(100)+1), 1, "x", acl, knowledgeTime); !errors.Is(err, ErrInvalid) {
			t.Fatal("random mismatched lineage accepted")
		}
	}
}

func readyFixture(t *testing.T) (*KnowledgeBase, *Document, *Version, ACL) {
	t.Helper()
	acl, _ := NewACL("ten_0001", 1, []Grant{{Kind: PrincipalUser, PrincipalID: "alice", Permission: PermissionRead}})
	kb, _ := NewKnowledgeBase("kbs_0001", "ten_0001", "name", acl, knowledgeTime)
	document, _ := NewDocument("doc_0001", kb, "source", "title", knowledgeTime)
	version, _ := NewVersion("ver_0001", document, 1, sha256.Sum256([]byte("source")), knowledgeTime)
	_ = version.Start(knowledgeTime)
	_ = version.Ready(knowledgeTime)
	return kb, document, version, acl
}
func readyVersion(t *testing.T) *Version {
	t.Helper()
	_, _, version, _ := readyFixture(t)
	return version
}
