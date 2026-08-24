package outbox

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testTime = time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)

func TestRepositoryEnqueueClaimGetAndCleanup(t *testing.T) {
	t.Parallel()
	script := &sqlScript{
		execs: []scriptExec{{affected: 1}, {affected: 2}},
		queries: []scriptQuery{
			{rows: [][]driver.Value{eventRow("evt_0001", "PENDING", 1, "relay-a", int64(4), testTime.Add(time.Minute), nil, nil)}},
			{rows: [][]driver.Value{eventRow("evt_0001", "PENDING", 1, "relay-a", int64(4), testTime.Add(time.Minute), nil, nil)}},
		},
	}
	repository := openRepository(t, script)
	payload := []byte(`{"run_id":"run_0001"}`)
	err := repository.Enqueue(context.Background(), Event{TenantID: "ten_0001", ID: "evt_0001", AggregateType: "run",
		AggregateID: "run_0001", Type: "run.queued", Payload: payload, AvailableAt: testTime, OccurredAt: testTime})
	if err != nil {
		t.Fatal(err)
	}
	payload[2] = 'X'
	events, err := repository.ClaimBatch(context.Background(), ClaimCommand{
		TenantID: "ten_0001", Owner: "relay-a", Duration: time.Minute, Limit: 10,
	})
	if err != nil || len(events) != 1 || events[0].ClaimToken != 4 || events[0].AttemptCount != 1 {
		t.Fatalf("ClaimBatch = %+v/%v", events, err)
	}
	event, err := repository.Get(context.Background(), "ten_0001", "evt_0001")
	if err != nil || event.ID != "evt_0001" || event.ClaimOwner != "relay-a" {
		t.Fatalf("Get = %+v/%v", event, err)
	}
	count, err := repository.CleanupPublished(context.Background(), CleanupCommand{TenantID: "ten_0001", Cutoff: testTime, Limit: 50})
	if err != nil || count != 2 {
		t.Fatalf("Cleanup = %d/%v", count, err)
	}

	script.mu.Lock()
	defer script.mu.Unlock()
	if !strings.Contains(script.calls[1].query, "FOR UPDATE SKIP LOCKED") || !strings.Contains(script.calls[1].query, "tenant_id=$1") {
		t.Fatalf("claim SQL lost concurrency/tenant contract: %s", script.calls[1].query)
	}
	if !strings.Contains(script.calls[3].query, "status='PUBLISHED'") || !strings.Contains(script.calls[3].query, "LIMIT $3") {
		t.Fatalf("cleanup SQL lost lifecycle/bound: %s", script.calls[3].query)
	}
	if got := script.calls[0].args[5].Value.([]byte); string(got) != `{"run_id":"run_0001"}` {
		t.Fatalf("enqueued payload was aliased: %s", got)
	}
}

func TestRepositoryAcknowledgementClassification(t *testing.T) {
	t.Parallel()
	ack := Acknowledgement{TenantID: "ten_0001", EventID: "evt_0001", Owner: "relay-a", Token: 3}
	for _, test := range []struct {
		name  string
		exec  scriptExec
		query scriptQuery
		call  func(*Repository) error
		want  error
	}{
		{"published", scriptExec{affected: 1}, scriptQuery{}, func(r *Repository) error { return r.MarkPublished(context.Background(), ack) }, nil},
		{"retry", scriptExec{affected: 1}, scriptQuery{}, func(r *Repository) error {
			return r.MarkFailed(context.Background(), Failure{Acknowledgement: ack, RetryAfter: time.Second, Cause: "broker unavailable"})
		}, nil},
		{"dead", scriptExec{affected: 1}, scriptQuery{}, func(r *Repository) error {
			return r.MarkFailed(context.Background(), Failure{Acknowledgement: ack, Cause: "poison", Dead: true})
		}, nil},
		{"not found", scriptExec{}, scriptQuery{}, func(r *Repository) error { return r.MarkPublished(context.Background(), ack) }, ErrNotFound},
		{"expired", scriptExec{}, scriptQuery{rows: [][]driver.Value{{"relay-a", int64(3), testTime, testTime}}}, func(r *Repository) error {
			return r.MarkPublished(context.Background(), ack)
		}, ErrClaimExpired},
		{"fenced owner", scriptExec{}, scriptQuery{rows: [][]driver.Value{{"relay-b", int64(4), testTime.Add(time.Minute), testTime}}}, func(r *Repository) error {
			return r.MarkPublished(context.Background(), ack)
		}, ErrClaimFenced},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := &sqlScript{execs: []scriptExec{test.exec}}
			if test.want != nil {
				script.queries = []scriptQuery{test.query}
			}
			err := test.call(openRepository(t, script))
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRepositoryValidationAndCorruptRows(t *testing.T) {
	t.Parallel()
	valid := Event{TenantID: "ten_0001", ID: "evt_0001", AggregateType: "run", AggregateID: "run_0001",
		Type: "run.queued", Payload: []byte(`{}`), AvailableAt: testTime, OccurredAt: testTime}
	for _, mutate := range []func(*Event){
		func(e *Event) { e.TenantID = "bad" }, func(e *Event) { e.ID = "bad" },
		func(e *Event) { e.Type = "bad type" }, func(e *Event) { e.Payload = []byte(`{`) },
		func(e *Event) { e.Payload = make([]byte, MaximumPayloadSize+1) }, func(e *Event) { e.Status = StatusPending },
	} {
		event := valid
		mutate(&event)
		repository := openRepository(t, &sqlScript{})
		if err := repository.Enqueue(context.Background(), event); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Enqueue validation error = %v", err)
		}
	}
	for _, command := range []ClaimCommand{
		{}, {TenantID: "ten_0001", Owner: " relay", Duration: time.Minute, Limit: 1},
		{TenantID: "ten_0001", Owner: "relay", Duration: time.Second, Limit: 1},
		{TenantID: "ten_0001", Owner: "relay", Duration: time.Minute, Limit: 101},
	} {
		if _, err := openRepository(t, &sqlScript{}).ClaimBatch(context.Background(), command); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ClaimBatch validation error = %v", err)
		}
	}
	corrupt := eventRow("evt_0001", "MYSTERY", 0, nil, int64(0), nil, nil, nil)
	if _, err := openRepository(t, &sqlScript{queries: []scriptQuery{{rows: [][]driver.Value{corrupt}}}}).
		Get(context.Background(), "ten_0001", "evt_0001"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("corrupt row error = %v", err)
	}
	if _, err := NewRepository(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil repository error = %v", err)
	}
}

func eventRow(id, status string, attempts int64, owner any, token int64, expires, published, lastError any) []driver.Value {
	return []driver.Value{"ten_0001", id, "run", "run_0001", "run.queued", []byte(`{"run_id":"run_0001"}`),
		status, testTime, testTime, attempts, owner, token, expires, published, lastError}
}

type scriptQuery struct {
	rows [][]driver.Value
	err  error
}
type scriptExec struct {
	affected int64
	err      error
}
type scriptCall struct {
	query string
	args  []driver.NamedValue
}
type sqlScript struct {
	mu      sync.Mutex
	queries []scriptQuery
	execs   []scriptExec
	calls   []scriptCall
}

var driverSequence atomic.Uint64

func openRepository(t *testing.T, script *sqlScript) *Repository {
	t.Helper()
	name := fmt.Sprintf("outbox-script-%d", driverSequence.Add(1))
	sql.Register(name, scriptDriver{script})
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

type scriptDriver struct{ script *sqlScript }

func (d scriptDriver) Open(string) (driver.Conn, error) { return &scriptConn{d.script}, nil }

type scriptConn struct{ script *sqlScript }

func (c *scriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *scriptConn) Close() error                             { return nil }
func (c *scriptConn) Begin() (driver.Tx, error)                { return nil, errors.New("transaction unsupported") }
func (c *scriptConn) CheckNamedValue(*driver.NamedValue) error { return nil }
func (c *scriptConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	c.script.calls = append(c.script.calls, scriptCall{query, cloneArgs(args)})
	if len(c.script.queries) == 0 {
		return nil, errors.New("unexpected query")
	}
	result := c.script.queries[0]
	c.script.queries = c.script.queries[1:]
	if result.err != nil {
		return nil, result.err
	}
	return &scriptRows{rows: result.rows}, nil
}
func (c *scriptConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	c.script.calls = append(c.script.calls, scriptCall{query, cloneArgs(args)})
	if len(c.script.execs) == 0 {
		return nil, errors.New("unexpected exec")
	}
	result := c.script.execs[0]
	c.script.execs = c.script.execs[1:]
	if result.err != nil {
		return nil, result.err
	}
	return driver.RowsAffected(result.affected), nil
}

func cloneArgs(args []driver.NamedValue) []driver.NamedValue {
	copyArgs := append([]driver.NamedValue(nil), args...)
	for index := range copyArgs {
		value := reflect.ValueOf(copyArgs[index].Value)
		if value.IsValid() && value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
			bytes := make([]byte, value.Len())
			reflect.Copy(reflect.ValueOf(bytes), value)
			copyArgs[index].Value = bytes
		}
	}
	return copyArgs
}

type scriptRows struct {
	rows  [][]driver.Value
	index int
}

func (r *scriptRows) Columns() []string {
	count := 0
	if len(r.rows) > 0 {
		count = len(r.rows[0])
	}
	columns := make([]string, count)
	for index := range columns {
		columns[index] = fmt.Sprintf("column_%d", index)
	}
	return columns
}
func (r *scriptRows) Close() error { return nil }
func (r *scriptRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}
