package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	conversationrepo "github.com/agenticrag/agenticrag/internal/repository"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestConversationRepositoryTenantIsolationTransactionsAndPagination(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	migrator, err := NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	insertInitialAggregate(t, ctx, pool, "ten_0001")
	insertInitialAggregate(t, ctx, pool, "ten_0002")
	if _, err := pool.ExecContext(ctx, `UPDATE messages SET content='tenant B question' WHERE tenant_id='ten_0002' AND id='msg_0001'`); err != nil {
		t.Fatal(err)
	}

	repository, err := conversationrepo.NewConversation(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		tenant  domain.TenantID
		content string
	}{{"ten_0001", "question"}, {"ten_0002", "tenant B question"}} {
		message, err := repository.GetMessage(ctx, test.tenant, "msg_0001")
		if err != nil || message.Content() != test.content {
			t.Fatalf("tenant %s message = %+v/%v", test.tenant, message, err)
		}
		turn, err := repository.GetTurn(ctx, test.tenant, "trn_0001")
		if err != nil || len(turn.RunIDs()) != 1 || turn.RunIDs()[0] != "run_0001" {
			t.Fatalf("tenant %s turn = %+v/%v", test.tenant, turn, err)
		}
		if _, err := repository.GetSession(ctx, test.tenant, "ses_0001"); err != nil {
			t.Fatalf("tenant %s session: %v", test.tenant, err)
		}
	}
	if _, err := repository.GetMessage(ctx, "ten_9999", "msg_0001"); !errors.Is(err, conversationrepo.ErrNotFound) {
		t.Fatalf("cross-tenant message error = %v", err)
	}

	var baseTime time.Time
	if err := pool.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE tenant_id='ten_0001' AND id='msg_0001'`).Scan(&baseTime); err != nil {
		t.Fatal(err)
	}
	candidate, err := domain.NewAssistantCandidate("msg_0002", "ten_0001", "ses_0001", "trn_0001", "run_0001", "hidden candidate", baseTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := domain.NewAssistantCandidate("msg_0003", "ten_0001", "ses_0001", "trn_0001", "run_0001", "visible answer", baseTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := conversation.PromoteToConversation(); err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertMessage(ctx, candidate); err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertMessage(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	first, err := repository.ListConversationMessages(ctx, "ten_0001", "ses_0001", nil, 1)
	if err != nil || len(first.Items) != 1 || first.Items[0].ID() != "msg_0001" || first.NextCursor == nil {
		t.Fatalf("first page = %+v/%v", first, err)
	}
	second, err := repository.ListConversationMessages(ctx, "ten_0001", "ses_0001", first.NextCursor, 10)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID() != "msg_0003" || second.NextCursor != nil {
		t.Fatalf("second page = %+v/%v", second, err)
	}

	rollbackMarker := errors.New("rollback conversation marker")
	err = pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		return insertConversationWithRepository(ctx, tx, "ten_0003", rollbackMarker)
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatalf("rollback error = %v", err)
	}
	if _, err := repository.GetSession(ctx, "ten_0003", "ses_tx001"); !errors.Is(err, conversationrepo.ErrNotFound) {
		t.Fatalf("rolled-back session error = %v", err)
	}

	if err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		return insertConversationWithRepository(ctx, tx, "ten_0004", nil)
	}); err != nil {
		t.Fatalf("committed transaction: %v", err)
	}
	committedTurn, err := repository.GetTurn(ctx, "ten_0004", "trn_tx001")
	if err != nil || len(committedTurn.RunIDs()) != 1 {
		t.Fatalf("committed turn = %+v/%v", committedTurn, err)
	}
}

func insertConversationWithRepository(ctx context.Context, tx DBTX, tenantID domain.TenantID, returnError error) error {
	repository, err := conversationrepo.NewConversation(tx)
	if err != nil {
		return err
	}
	at := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	session, err := domain.NewSession("ses_tx001", tenantID, at)
	if err != nil {
		return err
	}
	run, err := domain.NewRun("run_tx001", tenantID, session.ID(), "trn_tx001", 1, 0, at)
	if err != nil {
		return err
	}
	turn, err := domain.NewTurn("trn_tx001", tenantID, session.ID(), 1, 0, "msg_tx001", run.ID(), at)
	if err != nil {
		return err
	}
	message, err := domain.NewUserMessage("msg_tx001", tenantID, session.ID(), turn.ID(), "transaction question", at)
	if err != nil {
		return err
	}
	if err := repository.InsertSession(ctx, session); err != nil {
		return err
	}
	if err := repository.InsertTurn(ctx, turn); err != nil {
		return err
	}
	if err := repository.InsertMessage(ctx, message); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at)
		VALUES($1,'run_tx001','ses_tx001','trn_tx001',1,0,'QUEUED',$2,$2)`, tenantID.String(), at); err != nil {
		return err
	}
	return returnError
}
