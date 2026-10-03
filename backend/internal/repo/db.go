// Package repo is the only layer that talks SQL.
//
// Reader holds read queries and works on either the pool or an open transaction.
// Tx adds the write operations and only exists inside Store.WithTx.
package repo

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Reader struct{ q querier }

type Store struct {
	Reader
	pool *pgxpool.Pool
}

type Tx struct {
	Reader
	tx pgx.Tx
}

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(_ context.Context, c *pgx.Conn) error {
		pgxdecimal.Register(c.TypeMap()) // NUMERIC <-> shopspring/decimal, no float64 anywhere
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Reader: Reader{q: pool}, pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Migrate applies embedded migrations once each, serialised across processes with an advisory lock.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7274001)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7274001)`)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, f := range files { // ReadDir returns entries sorted by filename
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, f.Name()).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + f.Name())
		if err != nil {
			return err
		}
		// Simple-protocol multi-statement exec runs as one implicit transaction: all or nothing.
		script := string(body) + fmt.Sprintf("\nINSERT INTO schema_migrations (version) VALUES ('%s');", f.Name())
		if _, err := conn.Conn().PgConn().Exec(ctx, script).ReadAll(); err != nil {
			return fmt.Errorf("migration %s: %w", f.Name(), err)
		}
	}
	return nil
}

// WithTx runs fn in a READ COMMITTED read-write transaction. Correctness relies on explicit row locks
// (see Tx.LockAccounts), not on isolation level. Deadlocks / serialization failures are retried; fn must
// therefore have no side effects outside the transaction.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = s.runTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(t pgx.Tx) error {
			return fn(&Tx{Reader: Reader{q: t}, tx: t})
		})
		if code, _ := pgErrInfo(err); code != "40P01" && code != "40001" {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	return err
}

// WithSnapshot gives fn a read-only, repeatable-read view: every query inside sees the same committed state.
func (s *Store) WithSnapshot(ctx context.Context, fn func(Reader) error) error {
	return s.runTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(t pgx.Tx) error {
		return fn(Reader{q: t})
	})
}

func (s *Store) runTx(ctx context.Context, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op once committed
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func pgErrInfo(err error) (code, constraint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}
