package repository

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
)

func TestConversationReadPathsAndPagination(t *testing.T) {
	t.Parallel()
	script := &conversationScript{queries: []conversationQuery{
		{rows: [][]driver.Value{turnRow()}},
		{rows: [][]driver.Value{{"run_0001"}, {"run_0002"}}},
		{rows: [][]driver.Value{messageRow("msg_0001", nil, "USER", "CONVERSATION", "question")}},
		{rows: [][]driver.Value{
			messageRow("msg_0001", nil, "USER", "CONVERSATION", "one"),
			messageRow("msg_0002", "run_0001", "ASSISTANT", "CONVERSATION", "two"),
			messageRow("msg_0003", "run_0001", "ASSISTANT", "CONVERSATION", "three"),
		}},
		{rows: [][]driver.Value{sessionReadRow()}},
	}}
	repository := openConversationRepository(t, script)
	turn, err := repository.GetTurn(context.Background(), "ten_0001", "trn_0001")
	if err != nil || len(turn.RunIDs()) != 2 || turn.RunIDs()[1] != "run_0002" {
		t.Fatalf("GetTurn = %+v/%v", turn, err)
	}
	message, err := repository.GetMessage(context.Background(), "ten_0001", "msg_0001")
	if err != nil || message.Content() != "question" {
		t.Fatalf("GetMessage = %+v/%v", message, err)
	}
	page, err := repository.ListConversationMessages(context.Background(), "ten_0001", "ses_0001", nil, 2)
	if err != nil || len(page.Items) != 2 || page.NextCursor == nil || page.NextCursor.ID != "msg_0002" {
		t.Fatalf("List = %+v/%v", page, err)
	}
	session, err := repository.GetSession(context.Background(), "ten_0001", "ses_0001")
	if err != nil || session.ID() != "ses_0001" {
		t.Fatalf("GetSession = %+v/%v", session, err)
	}

	script.mu.Lock()
	defer script.mu.Unlock()
	if len(script.seen) != 5 {
		t.Fatalf("query count = %d", len(script.seen))
	}
	for _, call := range script.seen {
		if !strings.Contains(call.query, "tenant_id") || len(call.args) == 0 || call.args[0].Value != "ten_0001" {
			t.Errorf("unscoped query: %s args=%v", call.query, call.args)
		}
	}
	if !strings.Contains(script.seen[3].query, "visibility='CONVERSATION'") || !strings.Contains(script.seen[3].query, "ORDER BY created_at,id") {
		t.Fatal("history query lost visibility/keyset ordering")
	}
}

func TestConversationReadNotFoundAndQueryErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		query conversationQuery
		run   func(*Conversation) error
		want  error
	}{
		{"turn not found", conversationQuery{}, func(r *Conversation) error {
			_, err := r.GetTurn(context.Background(), "ten_0001", "trn_0001")
			return err
		}, ErrNotFound},
		{"message not found", conversationQuery{}, func(r *Conversation) error {
			_, err := r.GetMessage(context.Background(), "ten_0001", "msg_0001")
			return err
		}, ErrNotFound},
		{"message database", conversationQuery{err: errors.New("query marker")}, func(r *Conversation) error {
			_, err := r.GetMessage(context.Background(), "ten_0001", "msg_0001")
			return err
		}, errors.New("query marker")},
		{"history database", conversationQuery{err: errors.New("list marker")}, func(r *Conversation) error {
			_, err := r.ListConversationMessages(context.Background(), "ten_0001", "ses_0001", nil, 10)
			return err
		}, errors.New("list marker")},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := openConversationRepository(t, &conversationScript{queries: []conversationQuery{test.query}})
			err := test.run(repository)
			if !errors.Is(err, test.want) && (err == nil || !strings.Contains(err.Error(), test.want.Error())) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func turnRow() []driver.Value {
	return []driver.Value{"ten_0001", "trn_0001", "ses_0001", int64(1), int64(0), "msg_0001", "RUNNING", nil, repositoryTime, repositoryTime}
}

func messageRow(id string, runID any, role, visibility, content string) []driver.Value {
	return []driver.Value{"ten_0001", id, "ses_0001", "trn_0001", runID, role, "COMPLETED", visibility, content, repositoryTime}
}

func sessionReadRow() []driver.Value {
	return []driver.Value{"ten_0001", "ses_0001", int64(0), int64(1), int64(1), int64(0), nil, nil, repositoryTime, repositoryTime}
}

type conversationQuery struct {
	rows [][]driver.Value
	err  error
}
type conversationCall struct {
	query string
	args  []driver.NamedValue
}
type conversationScript struct {
	mu      sync.Mutex
	queries []conversationQuery
	seen    []conversationCall
}

var conversationDriverSequence atomic.Uint64

func openConversationRepository(t *testing.T, script *conversationScript) *Conversation {
	t.Helper()
	name := fmt.Sprintf("conversation-script-%d", conversationDriverSequence.Add(1))
	sql.Register(name, conversationDriver{script: script})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository, err := NewConversation(db)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

type conversationDriver struct{ script *conversationScript }

func (d conversationDriver) Open(string) (driver.Conn, error) {
	return &conversationConn{script: d.script}, nil
}

type conversationConn struct{ script *conversationScript }

func (c *conversationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *conversationConn) Close() error { return nil }
func (c *conversationConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transaction unsupported")
}
func (c *conversationConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	c.script.seen = append(c.script.seen, conversationCall{query: query, args: append([]driver.NamedValue(nil), args...)})
	if len(c.script.queries) == 0 {
		return nil, errors.New("unexpected query")
	}
	result := c.script.queries[0]
	c.script.queries = c.script.queries[1:]
	if result.err != nil {
		return nil, result.err
	}
	columns := 0
	if len(result.rows) > 0 {
		columns = len(result.rows[0])
	}
	return &conversationRows{rows: result.rows, columns: columns}, nil
}

type conversationRows struct {
	rows           [][]driver.Value
	columns, index int
}

func (r *conversationRows) Columns() []string {
	result := make([]string, r.columns)
	for index := range result {
		result[index] = fmt.Sprintf("column_%d", index)
	}
	return result
}
func (r *conversationRows) Close() error { return nil }
func (r *conversationRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}
