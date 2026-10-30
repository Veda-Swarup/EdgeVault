package meta

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a connection pool to PostgreSQL.
// pgxpool is safe for concurrent use; one pool is shared across the process.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("meta: open pool: %w", err)
	}
	// Verify connectivity immediately so startup fails fast on bad config.
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("meta: ping: %w", err)
	}
	return pool, nil
}
