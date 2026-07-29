package pg

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

var settingNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

type Rows = pgx.Rows
type Row = pgx.Row

type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (rowsAffected int64, err error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	SetLocal(ctx context.Context, key, val string) error
}

type tx struct {
	tx pgx.Tx
}

func (t *tx) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (t *tx) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	return t.tx.Query(ctx, sql, args...)
}

func (t *tx) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

func (t *tx) SetLocal(ctx context.Context, key, val string) error {
	if !settingNameRE.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidSetting, key)
	}
	// set_config 的第三个参数为 true,等价于 SET LOCAL,且 key/value 都参数化。
	if _, err := t.tx.Exec(ctx, "SELECT set_config($1, $2, true)", key, val); err != nil {
		return fmt.Errorf("SET LOCAL %s: %w", key, err)
	}
	return nil
}

func (db *DB) InTx(ctx context.Context, fn func(Tx) error) (err error) {
	raw, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("BEGIN: %w", err)
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			_ = raw.Rollback(context.WithoutCancel(ctx))
			panic(recovered)
		}
	}()

	if err := fn(&tx{tx: raw}); err != nil {
		if rollbackErr := raw.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("ROLLBACK: %w", rollbackErr))
		}
		return err
	}
	if err := raw.Commit(ctx); err != nil {
		return fmt.Errorf("COMMIT: %w", err)
	}
	return nil
}
