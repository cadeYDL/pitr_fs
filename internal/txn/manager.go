package txn

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pitr_fs/internal/pg"
)

const txnColumns = `id, version_hash, parent_id, scope_path, state,
	COALESCE(command, ''), COALESCE(message, ''), created_at, closed_at`

type Manager struct {
	db *pg.DB
}

func NewManager(db *pg.DB) *Manager {
	return &Manager{db: db}
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTxn(row scanner) (*Txn, error) {
	out := new(Txn)
	if err := row.Scan(
		&out.ID,
		&out.VersionHash,
		&out.ParentID,
		&out.ScopePath,
		&out.State,
		&out.Command,
		&out.Message,
		&out.CreatedAt,
		&out.ClosedAt,
	); err != nil {
		return nil, err
	}
	out.VersionHash = strings.TrimSpace(out.VersionHash)
	return out, nil
}

func NormalizeScope(scope string) (string, error) {
	if scope == "" || !strings.HasPrefix(scope, "/") {
		return "", fmt.Errorf("%w: %q", ErrInvalidScope, scope)
	}
	return path.Clean(scope), nil
}

func (m *Manager) Begin(ctx context.Context, scope, message string) (*Txn, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 3; attempt++ {
		hash, err := newVersionHash()
		if err != nil {
			return nil, err
		}
		var created *Txn
		err = m.db.InTx(ctx, func(tx pg.Tx) error {
			var parentID int64
			if err := tx.QueryRow(ctx, `
				SELECT id
				  FROM pitr_txn
				 WHERE state IN ('committed', 'root')
				 ORDER BY id DESC
				 LIMIT 1`).Scan(&parentID); err != nil {
				return fmt.Errorf("查找 parent version: %w", err)
			}
			row := tx.QueryRow(ctx, `
				INSERT INTO pitr_txn
					(version_hash, parent_id, scope_path, state, command, message)
				VALUES ($1, $2, $3, 'active', 'begin', $4)
				ON CONFLICT (version_hash) DO NOTHING
				RETURNING `+txnColumns,
				hash, parentID, normalized, message)
			created, err = scanTxn(row)
			if errors.Is(err, pgx.ErrNoRows) {
				return errHashCollision
			}
			return err
		})
		if err == nil {
			return created, nil
		}
		if errors.Is(err, errHashCollision) {
			continue
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "uniq_active_txn_per_path" {
			return nil, fmt.Errorf("%w: %s", ErrScopeActive, normalized)
		}
		return nil, fmt.Errorf("begin %s: %w", normalized, err)
	}
	return nil, errors.New("生成唯一 version hash 失败:连续冲突 3 次")
}

func (m *Manager) Commit(ctx context.Context, txnID int64, message string) (*Txn, error) {
	var committed *Txn
	err := m.db.InTx(ctx, func(tx pg.Tx) error {
		var state string
		if err := tx.QueryRow(ctx,
			"SELECT state FROM pitr_txn WHERE id=$1 FOR UPDATE", txnID).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTxnNotFound
			}
			return err
		}
		if state != StateActive {
			return fmt.Errorf("%w: %s -> committed", ErrIllegalTransit, state)
		}
		var openAutos int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pitr_txn
			 WHERE parent_id=$1 AND state='auto' AND closed_at IS NULL`,
			txnID).Scan(&openAutos); err != nil {
			return err
		}
		if openAutos != 0 {
			return fmt.Errorf("%w:仍有 %d 个开放 auto", ErrIllegalTransit, openAutos)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pitr_txn
			   SET command='commit', message=COALESCE(NULLIF($2, ''), message)
			 WHERE id=$1`,
			txnID, message); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "CALL pitr_collapse_commit($1)", txnID); err != nil {
			return err
		}
		var err error
		committed, err = scanTxn(tx.QueryRow(ctx,
			"SELECT "+txnColumns+" FROM pitr_txn WHERE id=$1", txnID))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("commit txn %d: %w", txnID, err)
	}
	return committed, nil
}

func (m *Manager) Rollback(ctx context.Context, txnID int64) (*Txn, error) {
	var rolledBack *Txn
	err := m.db.InTx(ctx, func(tx pg.Tx) error {
		var state string
		if err := tx.QueryRow(ctx,
			"SELECT state FROM pitr_txn WHERE id=$1", txnID).Scan(&state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTxnNotFound
			}
			return err
		}
		if state != StateActive {
			return fmt.Errorf("%w: %s -> rolled_back", ErrIllegalTransit, state)
		}
		if _, err := tx.Exec(ctx, "CALL pitr_rollback($1)", txnID); err != nil {
			return err
		}
		var err error
		rolledBack, err = scanTxn(tx.QueryRow(ctx,
			"SELECT "+txnColumns+" FROM pitr_txn WHERE id=$1", txnID))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("rollback txn %d: %w", txnID, err)
	}
	return rolledBack, nil
}

func (m *Manager) FindByID(ctx context.Context, txnID int64) (*Txn, error) {
	found, err := scanTxn(m.db.QueryRow(ctx,
		"SELECT "+txnColumns+" FROM pitr_txn WHERE id=$1", txnID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTxnNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find txn %d: %w", txnID, err)
	}
	return found, nil
}

func (m *Manager) FindActiveByPath(ctx context.Context, value string) (*Txn, error) {
	normalized, err := NormalizeScope(value)
	if err != nil {
		return nil, err
	}
	found, err := scanTxn(m.db.QueryRow(ctx, `
		SELECT `+txnColumns+`
		  FROM pitr_txn
		 WHERE state='active'
		   AND (scope_path='/' OR scope_path=$1
		        OR $1 LIKE rtrim(scope_path, '/') || '/%')
		 ORDER BY length(scope_path) DESC, id DESC
		 LIMIT 1`, normalized))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find active for %s: %w", normalized, err)
	}
	return found, nil
}

func (m *Manager) CreateAutoVersion(
	ctx context.Context,
	tx pg.Tx,
	parentID int64,
	command string,
) (int64, string, error) {
	var state, scope string
	if err := tx.QueryRow(ctx,
		"SELECT state, scope_path FROM pitr_txn WHERE id=$1 FOR SHARE", parentID).
		Scan(&state, &scope); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, "", ErrTxnNotFound
		}
		return 0, "", err
	}
	if state != StateActive {
		return 0, "", fmt.Errorf("%w:parent state=%s", ErrIllegalTransit, state)
	}
	for attempt := 0; attempt < 3; attempt++ {
		hash, err := newVersionHash()
		if err != nil {
			return 0, "", err
		}
		var id int64
		err = tx.QueryRow(ctx, `
			INSERT INTO pitr_txn
				(version_hash, parent_id, scope_path, state, command)
			VALUES ($1, $2, $3, 'auto', $4)
			ON CONFLICT (version_hash) DO NOTHING
			RETURNING id`, hash, parentID, scope, command).Scan(&id)
		if err == nil {
			return id, hash, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		return 0, "", err
	}
	return 0, "", errors.New("生成唯一 auto version hash 失败:连续冲突 3 次")
}

func (m *Manager) CloseAutoVersion(ctx context.Context, autoID int64) error {
	affected, err := m.db.Exec(ctx, `
		UPDATE pitr_txn SET closed_at=now()
		 WHERE id=$1 AND state='auto' AND closed_at IS NULL`, autoID)
	if err != nil {
		return fmt.Errorf("关闭 auto %d: %w", autoID, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w:auto %d 不存在或已关闭", ErrIllegalTransit, autoID)
	}
	return nil
}

func (m *Manager) List(ctx context.Context, scope string, limit int) ([]*Txn, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := m.db.Query(ctx, `
		SELECT `+txnColumns+`
		  FROM pitr_txn
		 WHERE scope_path='/' OR scope_path=$1
		    OR scope_path LIKE rtrim($1, '/') || '/%'
		    OR $1 LIKE rtrim(scope_path, '/') || '/%'
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2`, normalized, limit)
	if err != nil {
		return nil, fmt.Errorf("list txn: %w", err)
	}
	defer rows.Close()
	var out []*Txn
	for rows.Next() {
		item, err := scanTxn(rows)
		if err != nil {
			return nil, fmt.Errorf("scan txn: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list txn rows: %w", err)
	}
	return out, nil
}

func (m *Manager) CountActive(ctx context.Context) (int64, error) {
	var count int64
	if err := m.db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn WHERE state='active'").Scan(&count); err != nil {
		return 0, fmt.Errorf("count active: %w", err)
	}
	return count, nil
}
