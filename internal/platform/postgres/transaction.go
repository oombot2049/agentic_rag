package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// DBTX is the database capability shared by pools and transactions.
type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (p *Pool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return p.db.ExecContext(ctx, query, args...)
}
func (p *Pool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return p.db.QueryContext(ctx, query, args...)
}
func (p *Pool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return p.db.QueryRowContext(ctx, query, args...)
}

// WithinTx executes database-only work in one transaction.
func (p *Pool) WithinTx(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) (err error) {
	if work == nil {
		return errors.New("transaction work is nil")
	}
	tx, err := p.db.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback()
			panic(recovered)
		}
	}()
	if err := work(ctx, tx); err != nil {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		return errors.Join(err, rollbackErr)
	}
	return tx.Commit()
}
