package idempotency

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRepositoryClaimDecisionsWithSQLExecutor(t *testing.T) {
	t.Parallel()
	scope := validScope()
	hash := mustFingerprint(t, scope.Operation)
	other := mustFingerprint(t, "CREATE_RUN")
	tests := []struct {
		name      string
		queries   []scriptedQuery
		incoming  Fingerprint
		decision  Decision
		wantError error
	}{
		{"acquired", []scriptedQuery{{rows: [][]driver.Value{recordRow(scope, hash, StatusInProgress, nil)}}}, hash, DecisionAcquired, nil},
		{"replay success", []scriptedQuery{{}, {rows: [][]driver.Value{recordRow(scope, hash, StatusSucceeded, successResponse())}}}, hash, DecisionReplaySucceeded, nil},
		{"replay final failure", []scriptedQuery{{}, {rows: [][]driver.Value{recordRow(scope, hash, StatusFailedFinal, failureResponse())}}}, hash, DecisionReplayFailedFinal, nil},
		{"key reused", []scriptedQuery{{}, {rows: [][]driver.Value{recordRow(scope, hash, StatusInProgress, nil)}}}, other, DecisionKeyReused, nil},
		{"claim query failure", []scriptedQuery{{err: errors.New("query marker")}}, hash, "", errors.New("query marker")},
		{"record disappeared", []scriptedQuery{{}, {}}, hash, "", ErrClaimLost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := openScriptedDB(t, &sqlScript{queries: test.queries})
			repository, err := NewRepository(db)
			if err != nil {
				t.Fatal(err)
			}
			result, err := repository.Claim(context.Background(), scope, test.incoming, testNow, MinimumRetention)
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) && !containsError(err, test.wantError.Error()) {
					t.Fatalf("error = %v, want %v", err, test.wantError)
				}
				return
			}
			if err != nil || result.Decision != test.decision {
				t.Fatalf("Claim = %+v/%v, want %s", result, err, test.decision)
			}
			if result.Record.Response != nil {
				result.Record.Response.Body[0] = 'x'
				if successResponse().Body[0] == 'x' {
					t.Fatal("record response aliases source body")
				}
			}
		})
	}
}

func TestRepositoryCompletionFencingAndValidation(t *testing.T) {
	t.Parallel()
	scope := validScope()
	hash := mustFingerprint(t, scope.Operation)
	response := *successResponse()
	tests := []struct {
		name   string
		script *sqlScript
		run    func(*Repository) error
		want   error
	}{
		{"success", &sqlScript{execs: []scriptedExec{{affected: 1}}}, func(r *Repository) error {
			return r.CompleteSucceeded(context.Background(), scope, hash, response, testNow)
		}, nil},
		{"lost", &sqlScript{execs: []scriptedExec{{affected: 0}}}, func(r *Repository) error {
			return r.CompleteSucceeded(context.Background(), scope, hash, response, testNow)
		}, ErrClaimLost},
		{"database", &sqlScript{execs: []scriptedExec{{err: errors.New("exec marker")}}}, func(r *Repository) error {
			return r.CompleteSucceeded(context.Background(), scope, hash, response, testNow)
		}, errors.New("exec marker")},
		{"zero hash", &sqlScript{}, func(r *Repository) error {
			return r.CompleteSucceeded(context.Background(), scope, Fingerprint{}, response, testNow)
		}, ErrInvalidInput},
		{"zero time", &sqlScript{}, func(r *Repository) error {
			return r.CompleteSucceeded(context.Background(), scope, hash, response, time.Time{})
		}, ErrInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := openScriptedDB(t, test.script)
			repository, err := NewRepository(db)
			if err != nil {
				t.Fatal(err)
			}
			err = test.run(repository)
			if test.want == nil && err != nil {
				t.Fatal(err)
			}
			if test.want != nil && !errors.Is(err, test.want) && !containsError(err, test.want.Error()) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestScanRecordRejectsCorruptPersistence(t *testing.T) {
	t.Parallel()
	scope := validScope()
	hash := mustFingerprint(t, scope.Operation)
	badTenant := recordRow(scope, hash, StatusInProgress, nil)
	badTenant[0] = "bad"
	badHash := recordRow(scope, hash, StatusInProgress, nil)
	badHash[4] = []byte("short")
	for _, row := range [][]driver.Value{badTenant, badHash} {
		db := openScriptedDB(t, &sqlScript{queries: []scriptedQuery{{rows: [][]driver.Value{row}}}})
		repository, err := NewRepository(db)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.Claim(context.Background(), scope, hash, testNow, MinimumRetention); err == nil {
			t.Fatal("corrupt row error = nil")
		}
	}
}

func successResponse() *Response {
	return &Response{StatusCode: 202, Body: json.RawMessage(`{"run_id":"run_0001"}`), Location: "/v1/runs/run_0001", ResourceType: "RUN", ResourceID: "run_0001"}
}

func failureResponse() *Response {
	return &Response{StatusCode: 409, Body: json.RawMessage(`{"error":{"code":"SESSION_BUSY"}}`)}
}

func recordRow(scope Scope, hash Fingerprint, status Status, response *Response) []driver.Value {
	var responseStatus, body, location, resourceType, resourceID driver.Value
	if response != nil {
		responseStatus = int64(response.StatusCode)
		body = append([]byte(nil), response.Body...)
		if response.Location != "" {
			location = response.Location
		}
		if response.ResourceType != "" {
			resourceType = response.ResourceType
		}
		if response.ResourceID != "" {
			resourceID = response.ResourceID
		}
	}
	return []driver.Value{
		scope.TenantID.String(), scope.AuthenticatedSubjectID, scope.Operation, scope.Key,
		hash.Bytes(), string(status), responseStatus, body, location, resourceType, resourceID,
		testNow, testNow, testNow.Add(MinimumRetention),
	}
}

type scriptedQuery struct {
	rows [][]driver.Value
	err  error
}
type scriptedExec struct {
	affected int64
	err      error
}
type sqlScript struct {
	mu      sync.Mutex
	queries []scriptedQuery
	execs   []scriptedExec
}

var scriptedDriverSequence atomic.Uint64

func openScriptedDB(t *testing.T, script *sqlScript) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("idempotency-script-%d", scriptedDriverSequence.Add(1))
	sql.Register(name, scriptedDriver{script: script})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type scriptedDriver struct{ script *sqlScript }

func (d scriptedDriver) Open(string) (driver.Conn, error) {
	return &scriptedConn{script: d.script}, nil
}

type scriptedConn struct{ script *sqlScript }

func (c *scriptedConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *scriptedConn) Close() error              { return nil }
func (c *scriptedConn) Begin() (driver.Tx, error) { return nil, errors.New("transaction unsupported") }
func (c *scriptedConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
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
	return &scriptedRows{rows: result.rows}, nil
}
func (c *scriptedConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
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

type scriptedRows struct {
	rows  [][]driver.Value
	index int
}

func (r *scriptedRows) Columns() []string {
	return []string{"tenant_id", "authenticated_subject_id", "operation", "idempotency_key", "request_hash", "status", "response_status", "response_body", "response_location", "resource_type", "resource_id", "created_at", "updated_at", "expires_at"}
}
func (r *scriptedRows) Close() error { return nil }
func (r *scriptedRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}

func containsError(err error, text string) bool {
	return err != nil && strings.Contains(err.Error(), text)
}
