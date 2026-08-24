package queue

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

func TestPostgresAdapterEnqueueReplayConflictAndCapacity(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		script := &queueSQLScript{execs: []queueScriptExec{{affected: 1}, {affected: 1}}, queries: []queueScriptQuery{{}, {rows: [][]driver.Value{{int64(0), int64(0)}}}}}
		adapter := scriptedAdapter(t, script, DefaultConfig())
		if err := adapter.Enqueue(context.Background(), testMessage()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(script.calls[0].query, "pg_advisory_xact_lock") || !strings.Contains(script.calls[3].query, "INSERT INTO run_queue_messages") {
			t.Fatalf("calls=%+v", script.calls)
		}
	})
	t.Run("exact replay", func(t *testing.T) {
		script := &queueSQLScript{execs: []queueScriptExec{{affected: 1}}, queries: []queueScriptQuery{{rows: [][]driver.Value{queueMessageRow(testMessage())}}}}
		if err := scriptedAdapter(t, script, DefaultConfig()).Enqueue(context.Background(), testMessage()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("conflict", func(t *testing.T) {
		existing := testMessage()
		existing.DeliveryID = "evt_other1"
		script := &queueSQLScript{execs: []queueScriptExec{{affected: 1}}, queries: []queueScriptQuery{{rows: [][]driver.Value{queueMessageRow(existing)}}}}
		if err := scriptedAdapter(t, script, DefaultConfig()).Enqueue(context.Background(), testMessage()); !errors.Is(err, ErrConflict) {
			t.Fatalf("error=%v", err)
		}
	})
	for _, test := range []struct {
		name   string
		counts []driver.Value
		want   error
	}{
		{"tenant", []driver.Value{int64(250), int64(250)}, ErrTenantCapacity},
		{"global", []driver.Value{int64(1), int64(6000)}, ErrGlobalCapacity},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := &queueSQLScript{execs: []queueScriptExec{{affected: 1}}, queries: []queueScriptQuery{{}, {rows: [][]driver.Value{test.counts}}}}
			if err := scriptedAdapter(t, script, DefaultConfig()).Enqueue(context.Background(), testMessage()); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestPostgresAdapterClaimRenewAckNackCleanupAndFencing(t *testing.T) {
	delivery := queueDeliveryRow(testMessage(), "worker-a", 3, queueTestTime.Add(time.Minute), 4)
	t.Run("claim", func(t *testing.T) {
		script := &queueSQLScript{queries: []queueScriptQuery{{rows: [][]driver.Value{delivery}}}}
		values, err := scriptedAdapter(t, script, DefaultConfig()).Claim(context.Background(), ClaimCommand{
			TenantID: "ten_0001", Owner: "worker-a", Limit: 2, Duration: time.Minute, At: queueTestTime})
		if err != nil || len(values) != 1 || values[0].LeaseToken != 3 || values[0].DeliveryCount != 4 {
			t.Fatalf("Claim=%+v error=%v", values, err)
		}
	})
	t.Run("renew", func(t *testing.T) {
		script := &queueSQLScript{queries: []queueScriptQuery{{rows: [][]driver.Value{delivery}}}}
		value, err := scriptedAdapter(t, script, DefaultConfig()).Renew(context.Background(), RenewCommand{
			LeaseCommand: LeaseCommand{TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-a", Token: 3, At: queueTestTime}, Duration: time.Minute})
		if err != nil || value.LeaseToken != 3 {
			t.Fatalf("Renew=%+v error=%v", value, err)
		}
	})
	t.Run("ack", func(t *testing.T) {
		script := &queueSQLScript{execs: []queueScriptExec{{affected: 1}}}
		if err := scriptedAdapter(t, script, DefaultConfig()).Ack(context.Background(), testLease()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("nack", func(t *testing.T) {
		script := &queueSQLScript{execs: []queueScriptExec{{affected: 1}}}
		if err := scriptedAdapter(t, script, DefaultConfig()).Nack(context.Background(), NackCommand{
			LeaseCommand: testLease(), RetryAfter: time.Second, Reason: "provider unavailable"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		script := &queueSQLScript{execs: []queueScriptExec{{affected: 2}}}
		count, err := scriptedAdapter(t, script, DefaultConfig()).Cleanup(context.Background(), CleanupCommand{
			TenantID: "ten_0001", Cutoff: queueTestTime.Add(time.Hour), Limit: 10})
		if err != nil || count != 2 {
			t.Fatalf("Cleanup=%d error=%v", count, err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		script := &queueSQLScript{execs: []queueScriptExec{{affected: 0}}, queries: []queueScriptQuery{{rows: [][]driver.Value{{"LEASED", "worker-a", int64(3), queueTestTime}}}}}
		if err := scriptedAdapter(t, script, DefaultConfig()).Ack(context.Background(), testLease()); !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("fenced", func(t *testing.T) {
		script := &queueSQLScript{execs: []queueScriptExec{{affected: 0}}, queries: []queueScriptQuery{{rows: [][]driver.Value{{"LEASED", "worker-b", int64(4), queueTestTime.Add(time.Minute)}}}}}
		if err := scriptedAdapter(t, script, DefaultConfig()).Ack(context.Background(), testLease()); !errors.Is(err, ErrFenced) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestPostgresAdapterListsReadyTenants(t *testing.T) {
	script := &queueSQLScript{queries: []queueScriptQuery{{rows: [][]driver.Value{{"ten_0001"}, {"ten_0002"}}}}}
	values, err := scriptedAdapter(t, script, DefaultConfig()).ReadyTenants(context.Background(), ReadyTenantsCommand{Limit: 10, At: queueTestTime})
	if err != nil || len(values) != 2 || values[0] != "ten_0001" || values[1] != "ten_0002" {
		t.Fatalf("ReadyTenants=%v error=%v", values, err)
	}
	if !strings.Contains(script.calls[0].query, "GROUP BY message.tenant_id") {
		t.Fatalf("query=%s", script.calls[0].query)
	}
}

func TestPostgresAdapterRejectsInvalidCommandsBeforeSQL(t *testing.T) {
	adapter := scriptedAdapter(t, &queueSQLScript{}, DefaultConfig())
	if _, err := adapter.ReadyTenants(context.Background(), ReadyTenantsCommand{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("ReadyTenants error=%v", err)
	}
	if _, err := adapter.Claim(context.Background(), ClaimCommand{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Claim error=%v", err)
	}
	if _, err := adapter.Renew(context.Background(), RenewCommand{LeaseCommand: testLease(), Duration: time.Second}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Renew error=%v", err)
	}
	badNack := NackCommand{LeaseCommand: testLease(), Reason: "bad\nreason"}
	if err := adapter.Nack(context.Background(), badNack); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Nack error=%v", err)
	}
	if _, err := adapter.Cleanup(context.Background(), CleanupCommand{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("Cleanup error=%v", err)
	}
}

func testLease() LeaseCommand {
	return LeaseCommand{TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-a", Token: 3, At: queueTestTime.Add(time.Second)}
}

func queueMessageRow(message Message) []driver.Value {
	return []driver.Value{message.TenantID.String(), message.RunID.String(), message.SessionID.String(), message.TurnID.String(),
		message.DeliveryID, []byte(message.Payload), nullableDriver(message.Trace.TraceParent), nullableDriver(message.Trace.TraceState),
		message.AvailableAt, message.CreatedAt}
}

func queueDeliveryRow(message Message, owner string, token uint64, expires time.Time, count uint64) []driver.Value {
	return append(queueMessageRow(message), owner, int64(token), expires, int64(count))
}

func nullableDriver(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type queueScriptQuery struct {
	rows [][]driver.Value
	err  error
}
type queueScriptExec struct {
	affected int64
	err      error
}
type queueScriptCall struct {
	query string
	args  []driver.NamedValue
}
type queueSQLScript struct {
	mu      sync.Mutex
	queries []queueScriptQuery
	execs   []queueScriptExec
	calls   []queueScriptCall
}

var queueDriverSequence atomic.Uint64

func scriptedAdapter(t *testing.T, script *queueSQLScript, configuration Config) *PostgresAdapter {
	t.Helper()
	name := fmt.Sprintf("queue-script-%d", queueDriverSequence.Add(1))
	sql.Register(name, queueScriptDriver{script})
	database, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	adapter, err := NewPostgresAdapter(func(ctx context.Context, _ *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return work(ctx, database)
	}, configuration)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

type queueScriptDriver struct{ script *queueSQLScript }

func (d queueScriptDriver) Open(string) (driver.Conn, error) {
	return &queueScriptConnection{script: d.script}, nil
}

type queueScriptConnection struct{ script *queueSQLScript }

func (connection *queueScriptConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (connection *queueScriptConnection) Close() error { return nil }
func (connection *queueScriptConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("begin unsupported")
}
func (connection *queueScriptConnection) CheckNamedValue(*driver.NamedValue) error {
	return nil
}
func (connection *queueScriptConnection) QueryContext(_ context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
	connection.script.mu.Lock()
	defer connection.script.mu.Unlock()
	connection.script.calls = append(connection.script.calls, queueScriptCall{query: query, args: cloneQueueArgs(arguments)})
	if len(connection.script.queries) == 0 {
		return nil, errors.New("unexpected query")
	}
	result := connection.script.queries[0]
	connection.script.queries = connection.script.queries[1:]
	if result.err != nil {
		return nil, result.err
	}
	return &queueScriptRows{rows: result.rows}, nil
}
func (connection *queueScriptConnection) ExecContext(_ context.Context, query string, arguments []driver.NamedValue) (driver.Result, error) {
	connection.script.mu.Lock()
	defer connection.script.mu.Unlock()
	connection.script.calls = append(connection.script.calls, queueScriptCall{query: query, args: cloneQueueArgs(arguments)})
	if len(connection.script.execs) == 0 {
		return nil, errors.New("unexpected exec")
	}
	result := connection.script.execs[0]
	connection.script.execs = connection.script.execs[1:]
	if result.err != nil {
		return nil, result.err
	}
	return driver.RowsAffected(result.affected), nil
}

type queueScriptRows struct {
	rows  [][]driver.Value
	index int
}

func (rows *queueScriptRows) Columns() []string {
	width := 0
	if len(rows.rows) > 0 {
		width = len(rows.rows[0])
	}
	columns := make([]string, width)
	for index := range columns {
		columns[index] = fmt.Sprintf("column_%d", index)
	}
	return columns
}
func (rows *queueScriptRows) Close() error { return nil }
func (rows *queueScriptRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.rows) {
		return io.EOF
	}
	copy(destination, rows.rows[rows.index])
	rows.index++
	return nil
}

func cloneQueueArgs(arguments []driver.NamedValue) []driver.NamedValue {
	result := append([]driver.NamedValue(nil), arguments...)
	for index := range result {
		if value, ok := result[index].Value.([]byte); ok {
			result[index].Value = append([]byte(nil), value...)
		}
	}
	return result
}
