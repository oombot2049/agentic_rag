package turn

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type finalStoreStub struct {
	result FinalResult
	err    error
	calls  int
}

func (s *finalStoreStub) CommitFinal(_ context.Context, _ FinalRequest) (FinalResult, error) {
	s.calls++
	return s.result, s.err
}

func validFinalRequest() FinalRequest {
	return FinalRequest{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", MessageID: "msg_0002",
		Content: "grounded answer", ExpectedSessionRowVersion: 2, Authority: FinalAuthority{Owner: "worker-a", Token: 3, ExecutionVersion: 4},
		At: time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC), EventRetention: 24 * time.Hour}
}

func TestFinalCommitService(t *testing.T) {
	req := validFinalRequest()
	store := &finalStoreStub{result: FinalResult{MessageID: req.MessageID, ContextVersion: 1, SessionRowVersion: 3, EventSequence: 8}}
	service, err := NewFinalCommitService(store)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.Commit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.EventSequence != 8 || store.calls != 1 {
		t.Fatalf("unexpected result: %+v calls=%d", got, store.calls)
	}

	sentinel := errors.New("database down")
	store.err = sentinel
	if _, err = service.Commit(context.Background(), req); !errors.Is(err, sentinel) {
		t.Fatalf("store error not preserved: %v", err)
	}
}

func TestFinalCommitServiceRejectsInvalidInputBeforeStore(t *testing.T) {
	base := validFinalRequest()
	tests := []func(*FinalRequest){
		func(r *FinalRequest) { r.TenantID = "bad" }, func(r *FinalRequest) { r.SessionID = "bad" }, func(r *FinalRequest) { r.TurnID = "bad" },
		func(r *FinalRequest) { r.RunID = "bad" }, func(r *FinalRequest) { r.MessageID = "bad" }, func(r *FinalRequest) { r.Content = " \n" },
		func(r *FinalRequest) { r.Content = strings.Repeat("x", maximumFinalContent+1) }, func(r *FinalRequest) { r.ExpectedSessionRowVersion = 0 },
		func(r *FinalRequest) { r.Authority.Owner = "" }, func(r *FinalRequest) { r.Authority.Token = 0 }, func(r *FinalRequest) { r.Authority.ExecutionVersion = 0 },
		func(r *FinalRequest) { r.At = time.Time{} }, func(r *FinalRequest) { r.EventRetention = time.Minute },
	}
	for index, mutate := range tests {
		request := base
		mutate(&request)
		store := &finalStoreStub{}
		service, _ := NewFinalCommitService(store)
		if _, err := service.Commit(context.Background(), request); err == nil || store.calls != 0 {
			t.Errorf("case %d: err=%v calls=%d", index, err, store.calls)
		}
	}
	if _, err := NewFinalCommitService(nil); err == nil {
		t.Error("nil store accepted")
	}
}

func TestFinalCommitServiceRejectsInvalidStoreResult(t *testing.T) {
	request := validFinalRequest()
	store := &finalStoreStub{result: FinalResult{MessageID: domain.MessageID("msg_other")}}
	service, _ := NewFinalCommitService(store)
	if _, err := service.Commit(context.Background(), request); err == nil {
		t.Fatal("invalid result accepted")
	}
}
