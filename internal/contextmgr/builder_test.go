package contextmgr

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"github.com/agenticrag/agenticrag/internal/domain"
)

func TestBuilderCanonicalSnapshotOrderingDigestAndImmutability(t *testing.T) {
	store := &contextStoreStub{value: validConversation()}
	builder, _ := New(store)
	request := validBuildRequest()
	snapshot, err := builder.Build(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	entries := snapshot.Entries()
	wantSources := []Source{SourceSystem, SourceHistory, SourceHistory, SourceHistory, SourceHistory, SourceMemory, SourceCurrentUser}
	gotSources := make([]Source, len(entries))
	for index := range entries {
		gotSources[index] = entries[index].Source
	}
	if !reflect.DeepEqual(gotSources, wantSources) || entries[1].Role != domain.MessageRoleUser || entries[2].Role != domain.MessageRoleAssistant || entries[6].Content != "current question" {
		t.Fatalf("entries = %+v", entries)
	}
	digest := digestHex(snapshot)
	const wantDigest = "c95943be3ad1abea848493e4693afaee8dd7f5e96e4a1a05e65000a7434533a5"
	if digest != wantDigest {
		t.Fatalf("digest=%s", digest)
	}
	request.SystemInstructions[0] = "mutated"
	request.Memories[0].Content = "mutated"
	entries[0].Content = "mutated"
	if snapshot.Entries()[0].Content != "follow policy" || snapshot.Entries()[5].Content != "known preference" || digestHex(snapshot) != wantDigest {
		t.Fatal("snapshot was mutable through caller or accessor slices")
	}
}

func TestBuilderFencesVersionAndRejectsIncompleteHistory(t *testing.T) {
	for _, test := range []struct {
		name  string
		value Conversation
		want  error
	}{
		{"version", func() Conversation { value := validConversation(); value.ContextVersion = 3; return value }(), ErrFenced},
		{"owner", func() Conversation { value := validConversation(); value.ActiveRunID = "run_other"; return value }(), ErrFenced},
		{"missing assistant", func() Conversation { value := validConversation(); value.History = value.History[:3]; return value }(), ErrCorruptSource},
		{"candidate role", func() Conversation {
			value := validConversation()
			value.History[1].Role = domain.MessageRoleTool
			return value
		}(), ErrCorruptSource},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder, _ := New(&contextStoreStub{value: test.value})
			if _, err := builder.Build(context.Background(), validBuildRequest()); !errors.Is(err, test.want) {
				t.Fatalf("Build error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestBuilderValidatesInputsAndPropagatesStoreErrors(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("New error=%v", err)
	}
	builder, _ := New(&contextStoreStub{err: ErrNotFound})
	if _, err := builder.Build(context.Background(), validBuildRequest()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("store error=%v", err)
	}
	value := validBuildRequest()
	value.SystemInstructions = []string{" "}
	builder, _ = New(&contextStoreStub{value: validConversation()})
	if _, err := builder.Build(context.Background(), value); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("system error=%v", err)
	}
	value = validBuildRequest()
	value.Memories = append(value.Memories, value.Memories[0])
	if _, err := builder.Build(context.Background(), value); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("memory error=%v", err)
	}
}

type contextStoreStub struct {
	value Conversation
	err   error
}

func (s *contextStoreStub) Load(context.Context, Query) (Conversation, error) { return s.value, s.err }

func validBuildRequest() BuildRequest {
	return BuildRequest{Query: Query{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0003", RunID: "run_0003", BaseContextVersion: 2},
		SystemInstructions: []string{"follow policy"}, Memories: []Memory{{ID: "mem-001", Content: "known preference"}}}
}

func validConversation() Conversation {
	return Conversation{ContextVersion: 2, ActiveRunID: "run_0003",
		Current: SourceMessage{MessageID: "msg_0005", TurnID: "trn_0003", TurnSequence: 3, Role: domain.MessageRoleUser, Content: "current question"},
		History: []SourceMessage{
			{MessageID: "msg_0001", TurnID: "trn_0001", TurnSequence: 1, Role: domain.MessageRoleUser, Content: "question one"},
			{MessageID: "msg_0002", TurnID: "trn_0001", RunID: "run_0001", TurnSequence: 1, Role: domain.MessageRoleAssistant, Content: "answer one"},
			{MessageID: "msg_0003", TurnID: "trn_0002", TurnSequence: 2, Role: domain.MessageRoleUser, Content: "question two"},
			{MessageID: "msg_0004", TurnID: "trn_0002", RunID: "run_0002", TurnSequence: 2, Role: domain.MessageRoleAssistant, Content: "answer two"},
		}}
}

func digestHex(snapshot Snapshot) string {
	value := snapshot.Digest()
	return hex.EncodeToString(value[:])
}
