package turn

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
)

type queryMemoryStore struct {
	turn     *domain.Turn
	run      *domain.Run
	messages []*domain.Message
	err      error
}

func (s *queryMemoryStore) GetTurn(context.Context, domain.TenantID, domain.TurnID) (*domain.Turn, error) {
	return s.turn, s.err
}

func (s *queryMemoryStore) GetRun(context.Context, domain.TenantID, domain.RunID) (*domain.Run, error) {
	return s.run, s.err
}

func (s *queryMemoryStore) ListMessages(_ context.Context, _ domain.TenantID, _ domain.SessionID, after *MessageCursor, limit int) (MessageResultPage, error) {
	if s.err != nil {
		return MessageResultPage{}, s.err
	}
	start := 0
	if after != nil {
		for index, message := range s.messages {
			if message.CreatedAt().After(after.CreatedAt) || message.CreatedAt().Equal(after.CreatedAt) && message.ID().String() > after.ID.String() {
				start = index
				break
			}
			start = len(s.messages)
		}
	}
	end := min(start+limit, len(s.messages))
	page := MessageResultPage{Items: s.messages[start:end]}
	if end < len(s.messages) {
		last := s.messages[end-1]
		page.NextCursor = &MessageCursor{CreatedAt: last.CreatedAt(), ID: last.ID()}
	}
	return page, nil
}

func TestCursorRoundTripBindingAndTamperResistance(t *testing.T) {
	t.Parallel()
	key := []byte("0123456789abcdef0123456789abcdef")
	codec, err := NewCursorCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 0xff
	cursor := MessageCursor{CreatedAt: time.Date(2026, 8, 23, 1, 2, 3, 456, time.FixedZone("offset", 8*60*60)), ID: "msg_0001"}
	token, err := codec.Encode("ten_0001", "ses_0001", cursor)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := codec.Decode("ten_0001", "ses_0001", token)
	if err != nil || decoded.ID != cursor.ID || !decoded.CreatedAt.Equal(cursor.CreatedAt) || decoded.CreatedAt.Location() != time.UTC {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	for name, test := range map[string][3]string{
		"tenant":  {"ten_0002", "ses_0001", token},
		"session": {"ten_0001", "ses_0002", token},
		"tamper":  {"ten_0001", "ses_0001", token[:len(token)-1] + replacement(token[len(token)-1])},
		"padding": {"ten_0001", "ses_0001", token + "="},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := codec.Decode(domain.TenantID(test[0]), domain.SessionID(test[1]), test[2]); err == nil {
				t.Fatal("Decode error = nil")
			}
		})
	}
	if _, err := NewCursorCodec([]byte("short")); err == nil {
		t.Fatal("short key error = nil")
	}
}

func TestQueryServiceProjectsDurableStateAndPages(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	logicalTurn, err := domain.RestoreTurn(domain.TurnSnapshot{ID: "trn_0001", TenantID: "ten_0001", SessionID: "ses_0001",
		Sequence: 1, BaseContextVersion: 0, UserMessageID: "msg_0001", Status: domain.TurnStatusCompleted,
		RunIDs: []domain.RunID{"run_0001", "run_0002"}, SelectedRunID: "run_0002", CreatedAt: at, UpdatedAt: at.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	run, err := domain.RestoreRun(domain.RunSnapshot{ID: "run_0002", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001",
		RunNo: 2, BaseContextVersion: 0, Status: domain.RunStatusSelected, OutputMessageID: "msg_0002", CreatedAt: at, UpdatedAt: at.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	user, _ := domain.NewUserMessage("msg_0001", "ten_0001", "ses_0001", "trn_0001", "question", at)
	assistant, _ := domain.NewAssistantCandidate("msg_0002", "ten_0001", "ses_0001", "trn_0001", "run_0002", "answer", at)
	_ = assistant.PromoteToConversation()
	store := &queryMemoryStore{turn: logicalTurn, run: run, messages: []*domain.Message{user, assistant}}
	codec, _ := NewCursorCodec([]byte("0123456789abcdef0123456789abcdef"))
	service, err := NewQueryService(store, codec)
	if err != nil {
		t.Fatal(err)
	}
	turnResult, err := service.GetTurn(context.Background(), "ten_0001", "trn_0001")
	if err != nil || turnResult.SelectedRunID == nil || *turnResult.SelectedRunID != "run_0002" || turnResult.CompletedAt == nil || strings.Join(turnResult.RunIDs, ",") != "run_0001,run_0002" {
		t.Fatalf("Turn=%+v err=%v", turnResult, err)
	}
	runResult, err := service.GetRun(context.Background(), "ten_0001", "run_0002")
	if err != nil || !runResult.Terminal || runResult.FinalMessageID == nil || *runResult.FinalMessageID != "msg_0002" || runResult.CompletedAt == nil || runResult.StartedAt != nil {
		t.Fatalf("Run=%+v err=%v", runResult, err)
	}
	first, err := service.ListMessages(context.Background(), "ten_0001", "ses_0001", "", 1)
	if err != nil || len(first.Items) != 1 || first.NextCursor == nil || first.Items[0].Role != "USER" {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	second, err := service.ListMessages(context.Background(), "ten_0001", "ses_0001", *first.NextCursor, 1)
	if err != nil || len(second.Items) != 1 || second.Items[0].Role != "ASSISTANT" || second.Items[0].RunID == nil || second.NextCursor != nil {
		t.Fatalf("second page=%+v err=%v", second, err)
	}
}

func TestQueryServiceValidationNotFoundAndHiddenMessage(t *testing.T) {
	t.Parallel()
	codec, _ := NewCursorCodec([]byte("0123456789abcdef0123456789abcdef"))
	store := &queryMemoryStore{err: ErrQueryNotFound}
	service, _ := NewQueryService(store, codec)
	if _, err := service.GetTurn(context.Background(), "ten_0001", "trn_0001"); !apperror.IsCode(err, apperror.CodeTurnNotFound) {
		t.Fatalf("missing Turn=%v", err)
	}
	if _, err := service.GetRun(context.Background(), "ten_0001", "run_0001"); !apperror.IsCode(err, apperror.CodeRunNotFound) {
		t.Fatalf("missing Run=%v", err)
	}
	if _, err := service.ListMessages(context.Background(), "ten_0001", "ses_0001", "", 50); !apperror.IsCode(err, apperror.CodeSessionNotFound) {
		t.Fatalf("missing Session=%v", err)
	}
	if _, err := service.GetTurn(context.Background(), "ten_0001", "bad"); !apperror.IsCode(err, apperror.CodeInvalidArgument) {
		t.Fatalf("bad Turn ID=%v", err)
	}
	if _, err := service.ListMessages(context.Background(), "ten_0001", "ses_0001", "tampered", 50); !apperror.IsCode(err, apperror.CodeInvalidArgument) {
		t.Fatalf("bad cursor=%v", err)
	}
	if _, err := service.ListMessages(context.Background(), "ten_0001", "ses_0001", "", 101); !apperror.IsCode(err, apperror.CodeInvalidArgument) {
		t.Fatalf("bad page size=%v", err)
	}
	candidate, _ := domain.NewAssistantCandidate("msg_0002", "ten_0001", "ses_0001", "trn_0001", "run_0001", "hidden", time.Now())
	store.err, store.messages = nil, []*domain.Message{candidate}
	if _, err := service.ListMessages(context.Background(), "ten_0001", "ses_0001", "", 50); !apperror.IsCode(err, apperror.CodeInternal) {
		t.Fatalf("hidden Message=%v", err)
	}
	if _, err := NewQueryService(nil, codec); err == nil {
		t.Fatal("nil store error = nil")
	}
}

func TestRunProjectionHidesUnselectedCandidateMessage(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC()
	run, err := domain.RestoreRun(domain.RunSnapshot{ID: "run_0001", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001",
		RunNo: 1, Status: domain.RunStatusSuperseded, OutputMessageID: "msg_0001", CreatedAt: at, UpdatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	view := runView(run)
	if !view.Terminal || view.FinalMessageID != nil {
		t.Fatalf("superseded projection = %+v", view)
	}
}

func replacement(value byte) string {
	if value == 'A' {
		return "B"
	}
	return "A"
}
