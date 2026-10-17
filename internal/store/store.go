// Package store keeps sessions in Postgres.
//
// Every query is parameterized; no SQL is ever built from client input.
// Batch writes pass arrays and unnest them server-side, so one statement
// carries any number of rows without approaching the protocol's parameter
// limit.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound reports that a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is a pool of Postgres connections.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to databaseURL, waiting for the database to accept
// connections until ctx is done, which covers a database container that is
// still starting.
func Open(ctx context.Context, databaseURL string, log *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx redacts the password from its parse errors.
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["application_name"]; !ok {
		cfg.ConnConfig.RuntimeParams["application_name"] = "biga-server"
	}
	if !strings.Contains(databaseURL, "pool_max_conns") {
		cfg.MaxConns = 16
	}
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", err)
	}
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return &Store{pool: pool}, nil
		}
		log.Warn("waiting for Postgres", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("connect to Postgres: %w", err)
		case <-time.After(min(time.Duration(attempt)*time.Second, 5*time.Second)):
		}
	}
}

// Close releases every connection.
func (s *Store) Close() { s.pool.Close() }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Q runs queries outside a transaction.
func (s *Store) Q() *Queries { return &Queries{db: s.pool} }

// Tx runs fn in a transaction, committing when it returns nil and rolling
// back otherwise.
func (s *Store) Tx(ctx context.Context, fn func(*Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // no-op after commit
	if err := fn(&Queries{db: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// cleanupLockKey identifies the retention sweep's advisory lock.
const cleanupLockKey = 0x62696761 // "biga"

// WithCleanupLock runs fn only if no other server instance is running the
// retention sweep, reporting whether it ran. The lock is a session-level
// advisory lock held on one connection for the duration of fn.
func (s *Store) WithCleanupLock(ctx context.Context, fn func() error) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, cleanupLockKey).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, cleanupLockKey)
	}()
	return true, fn()
}

// dbtx is what both the pool and a transaction provide.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Queries runs statements on the pool or inside one transaction.
type Queries struct {
	db dbtx
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
