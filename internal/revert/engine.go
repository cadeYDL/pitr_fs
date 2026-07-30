// Package revert 实现 PostgreSQL 元数据 undo-log 的原子回放。
package revert

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"pitr_fs/internal/pg"
	"pitr_fs/internal/txn"
)

var versionHashRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

type Engine struct {
	db *pg.DB
}

type Options struct {
	TargetHash string
	ScopePath  string
	DryRun     bool
}

func NewEngine(db *pg.DB) *Engine {
	return &Engine{db: db}
}

func (e *Engine) Revert(
	ctx context.Context,
	options Options,
) (applied int64, newVersionHash string, err error) {
	targetHash := strings.ToLower(strings.TrimSpace(options.TargetHash))
	if !versionHashRE.MatchString(targetHash) {
		return 0, "", fmt.Errorf("%w: %q", ErrInvalidHash, options.TargetHash)
	}
	var scope *string
	if options.ScopePath != "" {
		normalized, normalizeErr := txn.NormalizeScope(options.ScopePath)
		if normalizeErr != nil {
			return 0, "", normalizeErr
		}
		scope = &normalized
	}
	if e == nil || e.db == nil {
		return 0, "", errors.New("revert engine 未配置数据库")
	}

	err = e.db.InTx(ctx, func(tx pg.Tx) error {
		// 所有 revert 串行,同时让 active 检查、计数、replay 和新版本插入
		// 处在同一个快照与事务里。
		if _, lockErr := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:revert'))"); lockErr != nil {
			return lockErr
		}

		var targetID int64
		var targetState string
		if scanErr := tx.QueryRow(ctx, `
			SELECT id, state FROM pitr_txn WHERE version_hash=$1`,
			targetHash).Scan(&targetID, &targetState); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return ErrTargetMissing
			}
			return scanErr
		}
		switch targetState {
		case txn.StateRoot, txn.StateCommitted, txn.StateAuto:
		default:
			return fmt.Errorf("%w: %s", ErrTargetState, targetState)
		}

		var activeCount int64
		if scanErr := tx.QueryRow(ctx, `
			SELECT count(*)
			  FROM pitr_txn
			 WHERE state='active'
			   AND ($1::text IS NULL OR pitr_scopes_overlap(scope_path, $1))`,
			scope).Scan(&activeCount); scanErr != nil {
			return scanErr
		}
		if activeCount != 0 {
			return fmt.Errorf("%w: %d", ErrActiveScope, activeCount)
		}

		if scanErr := tx.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM pitr_node_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))) +
			  (SELECT count(*) FROM pitr_edge_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))) +
			  (SELECT count(*) FROM pitr_chunk_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))) +
			  (SELECT count(*) FROM pitr_chunk_ref_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2)))`,
			targetID, scope).Scan(&applied); scanErr != nil {
			return scanErr
		}
		if options.DryRun {
			return nil
		}

		scopePath := "/"
		if scope != nil {
			scopePath = *scope
		}
		var revertID int64
		for attempt := 0; attempt < 3; attempt++ {
			hash, hashErr := txn.NewVersionHash()
			if hashErr != nil {
				return hashErr
			}
			scanErr := tx.QueryRow(ctx, `
				INSERT INTO pitr_txn
					(version_hash,parent_id,scope_path,state,command,closed_at)
				VALUES ($1,$2,$3,'committed',$4,now())
				ON CONFLICT (version_hash) DO NOTHING
				RETURNING id`,
				hash, targetID, scopePath, "revert:"+targetHash).
				Scan(&revertID)
			if scanErr == nil {
				newVersionHash = hash
				break
			}
			if !errors.Is(scanErr, pgx.ErrNoRows) {
				return scanErr
			}
		}
		if revertID == 0 {
			return errors.New("生成唯一 revert version hash 失败:连续冲突 3 次")
		}
		if _, callErr := tx.Exec(ctx,
			"CALL pitr_revert($1,$2,$3)", targetHash, scope, revertID); callErr != nil {
			return callErr
		}
		return nil
	})
	if err != nil {
		return 0, "", fmt.Errorf("revert %s: %w", targetHash, err)
	}
	return applied, newVersionHash, nil
}
