package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	// PostgreSQL driver for the M1 control-plane source of truth (ADR-002).
	_ "github.com/jackc/pgx/v5/stdlib"
)

// OpenPostgres opens and verifies the PostgreSQL control-plane database.
// The DSN comes from trusted server configuration (MAESTRO_DATABASE_DSN or a
// resolved secret reference) and is never logged.
func OpenPostgres(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return db, nil
}
