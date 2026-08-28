package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // driver presence guard
)

// PostgresStore is the M1 domain-store aggregate over the PostgreSQL
// control-plane schema: identity, runner registry, outbox/inbox and write
// idempotency (the frozen M1 store contracts from the I1 contract PR).
//
// The M0-era aggregate interfaces (projects/features/tasks/... on
// Repositories) remain SQLite-served until the cutover stream ports them;
// this aggregate deliberately does not partially implement Repositories.
//
// All methods are safe for concurrent use; WithinTx binds every statement
// of its callback to one transaction.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore wraps an open, verified PostgreSQL connection pool.
func NewPostgresStore(db *sql.DB) (*PostgresStore, error) {
	if db == nil {
		return nil, fmt.Errorf("postgres store: nil database handle")
	}
	return &PostgresStore{db: db}, nil
}

// DB exposes the pool for migration tooling and health probes.
func (s *PostgresStore) DB() *sql.DB { return s.db }

// Identities returns the user/membership store bound to the pool.
func (s *PostgresStore) Identities() IdentityStore { return pgIdentityStore{q: s.db} }

// RunnerRegistry returns the device registry store bound to the pool.
func (s *PostgresStore) RunnerRegistry() RunnerRegistryStore { return pgRunnerRegistryStore{q: s.db} }

// Outbox returns the transactional outbox store bound to the pool.
func (s *PostgresStore) Outbox() OutboxStore { return pgOutboxStore{q: s.db} }

// Inbox returns the durable inbox store bound to the pool.
func (s *PostgresStore) Inbox() InboxStore { return pgInboxStore{q: s.db} }

// APIIdempotency returns the write-path idempotency store bound to the pool.
func (s *PostgresStore) APIIdempotency() APIIdempotencyStore { return pgIdempotencyStore{q: s.db} }

// BeginTx opens a transaction and returns the domain-store set bound to it.
// The caller owns Commit/Rollback; statements issued through the returned
// PostgresTxStores never touch the base pool (SVC-GATE-003).
func (s *PostgresStore) BeginTx(ctx context.Context) (*PostgresTxStores, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres store: begin tx: %w", err)
	}
	return &PostgresTxStores{tx: tx}, nil
}

// PostgresTxStores is the transaction-scoped M1 domain-store set. Its zero
// SQL surface beyond the shared interfaces keeps the UnitOfWork callback
// from re-entering the pool.
type PostgresTxStores struct {
	tx *sql.Tx
}

// Commit finalizes the transaction.
func (t *PostgresTxStores) Commit() error { return t.tx.Commit() }

// Rollback aborts the transaction; safe to call after Commit.
func (t *PostgresTxStores) Rollback() error { return t.tx.Rollback() }

// Identities returns the user/membership store bound to this transaction.
func (t *PostgresTxStores) Identities() IdentityStore { return pgIdentityStore{q: t.tx} }

// RunnerRegistry returns the device registry store bound to this transaction.
func (t *PostgresTxStores) RunnerRegistry() RunnerRegistryStore {
	return pgRunnerRegistryStore{q: t.tx}
}

// Outbox returns the transactional outbox store bound to this transaction.
func (t *PostgresTxStores) Outbox() OutboxStore { return pgOutboxStore{q: t.tx} }

// Inbox returns the durable inbox store bound to this transaction.
func (t *PostgresTxStores) Inbox() InboxStore { return pgInboxStore{q: t.tx} }

// APIIdempotency returns the idempotency store bound to this transaction.
func (t *PostgresTxStores) APIIdempotency() APIIdempotencyStore { return pgIdempotencyStore{q: t.tx} }

// Compile-time assertions that every PG implementation satisfies its frozen
// contract, including transaction binding through the shared pgExecer.
var (
	_ IdentityStore       = pgIdentityStore{}
	_ RunnerRegistryStore = pgRunnerRegistryStore{}
	_ OutboxStore         = pgOutboxStore{}
	_ InboxStore          = pgInboxStore{}
	_ APIIdempotencyStore = pgIdempotencyStore{}
	_ pgExecer            = (*sql.DB)(nil)
	_ pgExecer            = (*sql.Tx)(nil)
)
