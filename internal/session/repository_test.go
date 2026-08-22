package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testTime = time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)

func TestReserveTurnAndConflictClassification(t *testing.T) {
	t.Parallel()
	command := validReserveCommand()
	tests := []struct {
		name         string
		queries      []sessionQuery
		wantSequence uint64
		wantError    error
	}{
		{"acquired", []sessionQuery{{rows: [][]driver.Value{sessionRow(0, 2, 2, 0, "run_0001", "")}}}, 1, nil},
		{"version before busy", []sessionQuery{{}, {rows: [][]driver.Value{sessionRow(1, 3, 2, 1, "run_9999", "trn_9999")}}}, 0, ErrVersionConflict},
		{"busy", []sessionQuery{{}, {rows: [][]driver.Value{sessionRow(0, 2, 2, 0, "run_9999", "")}}}, 0, ErrBusy},
		{"CAS", []sessionQuery{{}, {rows: [][]driver.Value{sessionRow(0, 2, 1, 0, "", "")}}}, 0, ErrCASConflict},
		{"not found", []sessionQuery{{}, {}}, 0, ErrNotFound},
		{"database", []sessionQuery{{err: errors.New("query marker")}}, 0, errors.New("query marker")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := newTestRepository(t, &sessionScript{queries: test.queries})
			result, err := repository.ReserveTurn(context.Background(), command)
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) && (err == nil || !strings.Contains(err.Error(), test.wantError.Error())) {
					t.Fatalf("error = %v, want %v", err, test.wantError)
				}
				return
			}
			if err != nil || result.TurnSequence != test.wantSequence || result.Session.RowVersion() != 2 {
				t.Fatalf("ReserveTurn = %+v/%v", result, err)
			}
		})
	}
}

func TestRegenerationCommitReleaseAndWorkerFencing(t *testing.T) {
	t.Parallel()
	regeneration := ReserveRegenerationCommand{
		TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0002",
		ExpectedContextVersion: 1, ExpectedRowVersion: 3, At: testTime,
	}
	repository := newTestRepository(t, &sessionScript{queries: []sessionQuery{{rows: [][]driver.Value{sessionRow(1, 4, 2, 1, "run_0002", "trn_0001")}}}})
	if result, err := repository.ReserveRegeneration(context.Background(), regeneration); err != nil || result.RowVersion() != 4 {
		t.Fatalf("ReserveRegeneration = %v/%v", result, err)
	}

	commit := CommitCommand{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0002", TurnSequence: 1, ExpectedRowVersion: 4, At: testTime}
	repository = newTestRepository(t, &sessionScript{queries: []sessionQuery{{rows: [][]driver.Value{sessionRow(2, 5, 2, 1, "", "trn_0001")}}}})
	if result, err := repository.CommitRun(context.Background(), commit); err != nil || result.ContextVersion() != 2 {
		t.Fatalf("CommitRun = %v/%v", result, err)
	}

	release := ReleaseCommand{TenantID: "ten_0001", SessionID: "ses_0001", RunID: "run_0002", ExpectedRowVersion: 4, At: testTime}
	repository = newTestRepository(t, &sessionScript{queries: []sessionQuery{{rows: [][]driver.Value{sessionRow(1, 5, 2, 1, "", "trn_0001")}}}})
	if result, err := repository.ReleaseRun(context.Background(), release); err != nil || result.ContextVersion() != 1 {
		t.Fatalf("ReleaseRun = %v/%v", result, err)
	}

	for _, test := range []struct {
		name    string
		loadRow []driver.Value
		want    error
	}{
		{"run fenced", sessionRow(1, 5, 2, 1, "run_9999", "trn_0001"), ErrRunFenced},
		{"row fenced", sessionRow(1, 5, 2, 1, "run_0002", "trn_0001"), ErrCASConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newTestRepository(t, &sessionScript{queries: []sessionQuery{{}, {rows: [][]driver.Value{test.loadRow}}}})
			_, err := repository.CommitRun(context.Background(), commit)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestLoadAndValidation(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t, &sessionScript{queries: []sessionQuery{{rows: [][]driver.Value{sessionRow(0, 1, 1, 0, "", "")}}}})
	loaded, err := repository.Load(context.Background(), "ten_0001", "ses_0001")
	if err != nil || loaded.ID() != "ses_0001" {
		t.Fatalf("Load = %v/%v", loaded, err)
	}
	if _, err := NewRepository(nil); err == nil {
		t.Fatal("nil executor error = nil")
	}

	invalidReserve := validReserveCommand()
	invalidReserve.ExpectedRowVersion = 0
	if _, err := repository.ReserveTurn(context.Background(), invalidReserve); err == nil {
		t.Fatal("zero row version error = nil")
	}
	if _, err := repository.Load(context.Background(), "bad", "ses_0001"); err == nil {
		t.Fatal("invalid tenant error = nil")
	}
	if _, err := repository.CommitRun(context.Background(), CommitCommand{}); err == nil {
		t.Fatal("invalid commit error = nil")
	}
	if _, err := repository.ReleaseRun(context.Background(), ReleaseCommand{}); err == nil {
		t.Fatal("invalid release error = nil")
	}
	if _, err := repository.ReserveRegeneration(context.Background(), ReserveRegenerationCommand{}); err == nil {
		t.Fatal("invalid regeneration error = nil")
	}

	tests := []struct {
		name string
		run  func() error
	}{
		{"reserve run", func() error {
			command := validReserveCommand()
			command.RunID = "bad"
			_, err := repository.ReserveTurn(context.Background(), command)
			return err
		}},
		{"reserve time", func() error {
			command := validReserveCommand()
			command.At = time.Time{}
			_, err := repository.ReserveTurn(context.Background(), command)
			return err
		}},
		{"commit sequence", func() error {
			command := CommitCommand{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001", ExpectedRowVersion: 1, At: testTime}
			_, err := repository.CommitRun(context.Background(), command)
			return err
		}},
		{"release time", func() error {
			command := ReleaseCommand{TenantID: "ten_0001", SessionID: "ses_0001", RunID: "run_0001", ExpectedRowVersion: 1}
			_, err := repository.ReleaseRun(context.Background(), command)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("validation error = nil")
			}
		})
	}
}

func TestLoadRejectsCorruptSessionReferences(t *testing.T) {
	t.Parallel()
	badActive := sessionRow(0, 1, 1, 0, "bad", "")
	badHead := sessionRow(1, 2, 2, 1, "", "bad")
	for _, row := range [][]driver.Value{badActive, badHead} {
		repository := newTestRepository(t, &sessionScript{queries: []sessionQuery{{rows: [][]driver.Value{row}}}})
		if _, err := repository.Load(context.Background(), "ten_0001", "ses_0001"); err == nil {
			t.Fatal("corrupt session error = nil")
		}
	}
}

func validReserveCommand() ReserveTurnCommand {
	return ReserveTurnCommand{TenantID: "ten_0001", SessionID: "ses_0001", RunID: "run_0001", ExpectedContextVersion: 0, ExpectedRowVersion: 1, At: testTime}
}

func sessionRow(contextVersion, rowVersion, nextSequence, committedSequence uint64, activeRun, headTurn string) []driver.Value {
	var active, head driver.Value
	if activeRun != "" {
		active = activeRun
	}
	if headTurn != "" {
		head = headTurn
	}
	return []driver.Value{"ten_0001", "ses_0001", int64(contextVersion), int64(rowVersion), int64(nextSequence), int64(committedSequence), active, head, testTime, testTime}
}

type sessionQuery struct {
	rows [][]driver.Value
	err  error
}
type sessionScript struct {
	mu      sync.Mutex
	queries []sessionQuery
}

var sessionDriverSequence atomic.Uint64

func newTestRepository(t *testing.T, script *sessionScript) *Repository {
	t.Helper()
	name := fmt.Sprintf("session-script-%d", sessionDriverSequence.Add(1))
	sql.Register(name, sessionDriver{script: script})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository, err := NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

type sessionDriver struct{ script *sessionScript }

func (d sessionDriver) Open(string) (driver.Conn, error) { return &sessionConn{script: d.script}, nil }

type sessionConn struct{ script *sessionScript }

func (c *sessionConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *sessionConn) Close() error              { return nil }
func (c *sessionConn) Begin() (driver.Tx, error) { return nil, errors.New("transaction unsupported") }
func (c *sessionConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	if len(c.script.queries) == 0 {
		return nil, errors.New("unexpected query")
	}
	result := c.script.queries[0]
	c.script.queries = c.script.queries[1:]
	if result.err != nil {
		return nil, result.err
	}
	return &sessionRows{rows: result.rows}, nil
}

type sessionRows struct {
	rows  [][]driver.Value
	index int
}

func (r *sessionRows) Columns() []string {
	return []string{"tenant_id", "id", "context_version", "row_version", "next_turn_sequence", "committed_turn_sequence", "active_run_id", "head_turn_id", "created_at", "updated_at"}
}
func (r *sessionRows) Close() error { return nil }
func (r *sessionRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}
