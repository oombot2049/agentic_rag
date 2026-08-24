package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	sessioncontrol "github.com/agenticrag/agenticrag/internal/session"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestSessionHundredWayReservationCAS(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	migrator, err := NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	if _, err := pool.ExecContext(ctx, `INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES('ten_0001','ses_race1',$1,$1)`, at); err != nil {
		t.Fatal(err)
	}

	const contenders = 100
	start := make(chan struct{})
	outcomes := make(chan error, contenders)
	var wait sync.WaitGroup
	for index := range contenders {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			runID := fmt.Sprintf("run_%04d", index)
			turnID := fmt.Sprintf("trn_%04d", index)
			messageID := fmt.Sprintf("msg_%04d", index)
			err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
				repository, err := sessioncontrol.NewRepository(tx)
				if err != nil {
					return err
				}
				reservation, err := repository.ReserveTurn(ctx, sessioncontrol.ReserveTurnCommand{
					TenantID: "ten_0001", SessionID: "ses_race1", RunID: domain.RunID(runID),
					ExpectedContextVersion: 0, ExpectedRowVersion: 1, At: at,
				})
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at)
					VALUES('ten_0001',$1,'ses_race1',$2,0,$3,'RUNNING',$4,$4)`, turnID, reservation.TurnSequence, messageID, at); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO messages(tenant_id,id,session_id,turn_id,role,visibility,content,created_at)
					VALUES('ten_0001',$1,'ses_race1',$2,'USER','CONVERSATION','question',$3)`, messageID, turnID, at); err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, `
					INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at)
					VALUES('ten_0001',$1,'ses_race1',$2,1,0,'QUEUED',$3,$3)`, runID, turnID, at)
				return err
			})
			outcomes <- err
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)
	succeeded, busy := 0, 0
	for err := range outcomes {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, sessioncontrol.ErrBusy):
			busy++
		default:
			t.Errorf("reservation outcome: %v", err)
		}
	}
	if succeeded != 1 || busy != contenders-1 {
		t.Fatalf("success/busy = %d/%d", succeeded, busy)
	}

	repository, err := sessioncontrol.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := repository.Load(ctx, "ten_0001", "ses_race1")
	if err != nil {
		t.Fatal(err)
	}
	activeRun, active := snapshot.ActiveRunID()
	if !active || snapshot.RowVersion() != 2 || snapshot.ContextVersion() != 0 || snapshot.NextTurnSequence() != 2 {
		t.Fatalf("reserved snapshot row/context/next/active = %d/%d/%d/%v", snapshot.RowVersion(), snapshot.ContextVersion(), snapshot.NextTurnSequence(), active)
	}
	var turnID string
	if err := pool.QueryRowContext(ctx, `SELECT turn_id FROM runs WHERE tenant_id='ten_0001' AND id=$1`, activeRun.String()).Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	committed, err := repository.CommitRun(ctx, sessioncontrol.CommitCommand{
		TenantID: "ten_0001", SessionID: "ses_race1", TurnID: domain.TurnID(turnID), RunID: activeRun,
		TurnSequence: 1, ExpectedRowVersion: 2, At: at.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if committed.ContextVersion() != 1 || committed.RowVersion() != 3 {
		t.Fatalf("commit versions = %d/%d", committed.ContextVersion(), committed.RowVersion())
	}
	if _, err := repository.ReserveTurn(ctx, sessioncontrol.ReserveTurnCommand{
		TenantID: "ten_0001", SessionID: "ses_race1", RunID: "run_9999",
		ExpectedContextVersion: 0, ExpectedRowVersion: 3, At: at.Add(2 * time.Second),
	}); !errors.Is(err, sessioncontrol.ErrVersionConflict) {
		t.Fatalf("stale context error = %v", err)
	}

	regenerationRun := domain.RunID("run_regen1")
	err = pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		txRepository, err := sessioncontrol.NewRepository(tx)
		if err != nil {
			return err
		}
		if _, err := txRepository.ReserveRegeneration(ctx, sessioncontrol.ReserveRegenerationCommand{
			TenantID: "ten_0001", SessionID: "ses_race1", TurnID: domain.TurnID(turnID), RunID: regenerationRun,
			ExpectedContextVersion: 1, ExpectedRowVersion: 3, At: at.Add(2 * time.Second),
		}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at)
			VALUES('ten_0001',$1,'ses_race1',$2,2,1,'QUEUED',$3,$3)`, regenerationRun.String(), turnID, at.Add(2*time.Second))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	released, err := repository.ReleaseRun(ctx, sessioncontrol.ReleaseCommand{
		TenantID: "ten_0001", SessionID: "ses_race1", RunID: regenerationRun,
		ExpectedRowVersion: 4, At: at.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.ContextVersion() != 1 || released.RowVersion() != 5 || released.NextTurnSequence() != 2 {
		t.Fatalf("release versions = context %d row %d next %d", released.ContextVersion(), released.RowVersion(), released.NextTurnSequence())
	}
	if _, err := repository.ReserveTurn(ctx, sessioncontrol.ReserveTurnCommand{
		TenantID: "ten_0001", SessionID: "ses_race1", RunID: "run_9999",
		ExpectedContextVersion: 1, ExpectedRowVersion: 4, At: at.Add(4 * time.Second),
	}); !errors.Is(err, sessioncontrol.ErrCASConflict) {
		t.Fatalf("stale row error = %v", err)
	}
	if _, err := repository.Load(ctx, "ten_9999", "ses_race1"); !errors.Is(err, sessioncontrol.ErrNotFound) {
		t.Fatalf("cross-tenant load error = %v", err)
	}
}
