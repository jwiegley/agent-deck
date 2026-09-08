package statedb

import (
	"context"
	"database/sql"
	"time"
)

const immediateTransactionTimeout = 10 * time.Second

// immediateTransaction reserves SQLite's single writer slot before any CAS
// read. That prevents a concurrent commit from invalidating a deferred read
// snapshot and turning the following write into SQLITE_BUSY_SNAPSHOT.
type immediateTransaction struct {
	ctx      context.Context
	conn     *sql.Conn
	finished bool
}

func (tx *immediateTransaction) Exec(query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(tx.ctx, query, args...)
}

func (tx *immediateTransaction) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(tx.ctx, query, args...)
}

func (tx *immediateTransaction) QueryRow(query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(tx.ctx, query, args...)
}

func (tx *immediateTransaction) Prepare(query string) (*sql.Stmt, error) {
	return tx.conn.PrepareContext(tx.ctx, query)
}

func (tx *immediateTransaction) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(tx.ctx, query, args...)
}

func (tx *immediateTransaction) Commit() error {
	if tx.finished {
		return sql.ErrTxDone
	}
	_, err := tx.conn.ExecContext(tx.ctx, "COMMIT")
	if err == nil {
		tx.finished = true
	}
	return err
}

func (tx *immediateTransaction) rollback() {
	if tx.finished {
		return
	}
	// Match database/sql.Tx rollback cleanup: once the bounded operation ends,
	// always clear the connection before returning it to the pool.
	_, _ = tx.conn.ExecContext(context.Background(), "ROLLBACK")
	tx.finished = true
}

// withImmediateTransaction is for short, database-only operations. Callers
// must not perform process, network, or other external I/O while it holds the
// SQLite writer reservation.
func (s *StateDB) withImmediateTransaction(op func(*immediateTransaction) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), immediateTransactionTimeout)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	tx := &immediateTransaction{ctx: ctx, conn: conn}
	defer tx.rollback()
	if s.testAfterImmediateBegin != nil {
		s.testAfterImmediateBegin()
	}
	return op(tx)
}
