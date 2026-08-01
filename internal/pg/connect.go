package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Option func(*pgxpool.Config)

func WithMaxConns(n int32) Option {
	return func(cfg *pgxpool.Config) {
		if n > 0 {
			cfg.MaxConns = n
		}
	}
}

// WithAdvisoryLock 在独占的池连接上持有 session advisory lock。适合把
// PostgreSQL 之外的维护动作（例如 JuiceFS GC）与版本写窗口串行化。
func (db *DB) WithAdvisoryLock(
	ctx context.Context,
	key string,
	fn func() error,
) error {
	conn, err := db.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("获取维护连接: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx,
		"SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
		return fmt.Errorf("获取 advisory lock %q: %w", key, err)
	}
	defer func() {
		unlockCtx := context.WithoutCancel(ctx)
		_, _ = conn.Exec(unlockCtx,
			"SELECT pg_advisory_unlock(hashtext($1))", key)
	}()
	return fn()
}

type DB struct {
	pool *pgxpool.Pool
}

func Connect(ctx context.Context, dsn string, opts ...Option) (*DB, error) {
	if dsn == "" {
		return nil, ErrEmptyDSN
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析 PostgreSQL DSN: %w", err)
	}
	for _, opt := range opts {
		opt(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("创建 PostgreSQL 连接池: %w", err)
	}
	db := &DB{pool: pool}
	if err := db.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接 PostgreSQL: %w", err)
	}
	return db, nil
}

func (db *DB) Close() error {
	if db == nil || db.pool == nil {
		return nil
	}
	db.pool.Close()
	return nil
}

func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return nil
}

func (db *DB) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := db.pool.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (db *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return db.pool.Query(ctx, sql, args...)
}

func (db *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return db.pool.QueryRow(ctx, sql, args...)
}
