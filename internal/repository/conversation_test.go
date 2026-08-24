package repository

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var repositoryTime = time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)

func TestInsertAggregatesUseTenantFirstAndDomainValues(t *testing.T) {
	t.Parallel()
	database := &captureDB{}
	repository, err := NewConversation(database)
	if err != nil {
		t.Fatal(err)
	}
	session, err := domain.NewSession("ses_0001", "ten_0001", repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	assertFirstArguments(t, database.args, "ten_0001", "ses_0001")

	run, err := domain.NewRun("run_0001", "ten_0001", "ses_0001", "trn_0001", 1, 0, repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := domain.NewTurn("trn_0001", "ten_0001", "ses_0001", 1, 0, "msg_0001", run.ID(), repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertTurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	assertFirstArguments(t, database.args, "ten_0001", "trn_0001")

	message, err := domain.NewUserMessage("msg_0001", "ten_0001", "ses_0001", "trn_0001", "question", repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	assertFirstArguments(t, database.args, "ten_0001", "msg_0001")
	if database.args[4] != nil || database.args[5] != "USER" || database.args[7] != "CONVERSATION" {
		t.Fatalf("message args = %#v", database.args)
	}
}

func TestInsertRunPersistsNormalizedExecutionOptions(t *testing.T) {
	t.Parallel()
	database := &captureDB{}
	repository, err := NewExecution(database)
	if err != nil {
		t.Fatal(err)
	}
	run, err := domain.NewRun("run_0001", "ten_0001", "ses_0001", "trn_0001", 1, 0, repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertRunWithOptions(context.Background(), run, 7); err != nil {
		t.Fatal(err)
	}
	if len(database.args) != 11 || database.args[10] != 7 || !stringsContain(database.query, "max_steps") {
		t.Fatalf("InsertRun arguments=%#v query=%s", database.args, database.query)
	}
	if err := repository.InsertRunWithOptions(context.Background(), run, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid max steps = %v", err)
	}
}

func TestInsertValidationAndDatabaseErrors(t *testing.T) {
	t.Parallel()
	if _, err := NewConversation(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil database error = %v", err)
	}
	repository, err := NewConversation(&captureDB{execErr: errors.New("exec marker")})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertSession(context.Background(), nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil session error = %v", err)
	}
	if err := repository.InsertTurn(context.Background(), nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil turn error = %v", err)
	}
	if err := repository.InsertMessage(context.Background(), nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil message error = %v", err)
	}
	session, err := domain.NewSession("ses_0001", "ten_0001", repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertSession(context.Background(), session); err == nil || !stringsContain(err.Error(), "exec marker") {
		t.Fatalf("database error = %v", err)
	}
}

func TestMessageHydration(t *testing.T) {
	t.Parallel()
	values := []any{"ten_0001", "msg_0002", "ses_0001", "trn_0001", "run_0001", "ASSISTANT", "COMPLETED", "CANDIDATE", "answer", repositoryTime}
	message, err := scanMessage(valueScanner{values: values})
	if err != nil {
		t.Fatal(err)
	}
	runID, ok := message.RunID()
	if !ok || runID != "run_0001" || message.Content() != "answer" {
		t.Fatalf("message = %+v", message)
	}
	brokenStatus := append([]any(nil), values...)
	brokenStatus[6] = "STREAMING"
	if _, err := scanMessage(valueScanner{values: brokenStatus}); err == nil {
		t.Fatal("non-completed status error = nil")
	}
	brokenTenant := append([]any(nil), values...)
	brokenTenant[0] = "bad"
	if _, err := scanMessage(valueScanner{values: brokenTenant}); err == nil {
		t.Fatal("bad tenant error = nil")
	}
}

func TestTurnSnapshotParsing(t *testing.T) {
	t.Parallel()
	snapshot := domain.TurnSnapshot{Sequence: 1, RunIDs: []domain.RunID{"run_0001"}, CreatedAt: repositoryTime, UpdatedAt: repositoryTime}
	if err := parseTurnSnapshot(&snapshot, "ten_0001", "trn_0001", "ses_0001", "msg_0001", "RUNNING", sql.NullString{}); err != nil {
		t.Fatal(err)
	}
	turn, err := domain.RestoreTurn(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if turn.ID() != "trn_0001" {
		t.Fatalf("turn ID = %s", turn.ID())
	}
	if err := parseTurnSnapshot(&domain.TurnSnapshot{}, "bad", "trn_0001", "ses_0001", "msg_0001", "RUNNING", sql.NullString{}); err == nil {
		t.Fatal("bad tenant error = nil")
	}
}

func TestHistoryArgumentValidation(t *testing.T) {
	t.Parallel()
	repository, err := NewConversation(&captureDB{})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, 101} {
		if _, err := repository.ListConversationMessages(context.Background(), "ten_0001", "ses_0001", nil, limit); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("limit %d error = %v", limit, err)
		}
	}
	if _, err := repository.ListConversationMessages(context.Background(), "ten_0001", "ses_0001", &MessageCursor{ID: "msg_0001"}, 10); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero cursor time error = %v", err)
	}
	if _, err := repository.ListConversationMessages(context.Background(), "bad", "ses_0001", nil, 10); err == nil {
		t.Fatal("invalid tenant error = nil")
	}
	if _, err := repository.GetTurn(context.Background(), "ten_0001", "bad"); err == nil {
		t.Fatal("invalid turn error = nil")
	}
	if _, err := repository.GetMessage(context.Background(), "ten_0001", "bad"); err == nil {
		t.Fatal("invalid message error = nil")
	}
}

type captureDB struct {
	query   string
	args    []any
	execErr error
}

func (d *captureDB) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	d.query, d.args = query, append([]any(nil), args...)
	return captureResult(1), d.execErr
}
func (d *captureDB) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("query unsupported")
}
func (d *captureDB) QueryRowContext(context.Context, string, ...any) *sql.Row { return &sql.Row{} }

type captureResult int64

func (r captureResult) LastInsertId() (int64, error) { return 0, errors.New("unsupported") }
func (r captureResult) RowsAffected() (int64, error) { return int64(r), nil }

type valueScanner struct{ values []any }

func (s valueScanner) Scan(destinations ...any) error {
	if len(destinations) != len(s.values) {
		return errors.New("destination count mismatch")
	}
	for index, value := range s.values {
		destination := reflect.ValueOf(destinations[index])
		if destination.Kind() != reflect.Pointer {
			return errors.New("destination is not pointer")
		}
		if nullable, ok := destinations[index].(*sql.NullString); ok {
			if value == nil {
				*nullable = sql.NullString{}
			} else {
				*nullable = sql.NullString{String: value.(string), Valid: true}
			}
			continue
		}
		destination.Elem().Set(reflect.ValueOf(value).Convert(destination.Elem().Type()))
	}
	return nil
}

func assertFirstArguments(t *testing.T, args []any, first, second string) {
	t.Helper()
	if len(args) < 2 || args[0] != first || args[1] != second {
		t.Fatalf("first arguments = %#v", args)
	}
}

func stringsContain(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
