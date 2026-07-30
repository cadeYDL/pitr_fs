// Package revert 实现 PostgreSQL 元数据 undo-log 的原子回放。
package revert

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"pitr_fs/internal/pg"
	"pitr_fs/internal/txn"
)

var versionHashRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

type Engine struct {
	db        *pg.DB
	mountPath string
}

type Options struct {
	TargetHash string
	ScopePath  string
	DryRun     bool
}

type EngineOption func(*Engine)

func WithMountPath(value string) EngineOption {
	return func(engine *Engine) {
		if value != "" {
			engine.mountPath = path.Clean(value)
		}
	}
}

func NewEngine(db *pg.DB, options ...EngineOption) *Engine {
	engine := &Engine{db: db}
	for _, option := range options {
		option(engine)
	}
	return engine
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

		var scopeInodes []int64
		if scope != nil {
			scopeInodes, err = e.resolveScopeInodes(
				ctx, tx, targetID, *scope)
			if err != nil {
				return err
			}
		}
		if scanErr := tx.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM pitr_node_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))
			     AND ($3::bigint[] IS NULL OR h.inode=ANY($3))) +
			  (SELECT count(*) FROM pitr_edge_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))
			     AND ($3::bigint[] IS NULL OR h.parent=ANY($3)
			          OR (h.snapshot->>'inode')::bigint=ANY($3))) +
			  (SELECT count(*) FROM pitr_chunk_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))
			     AND ($3::bigint[] IS NULL OR h.inode=ANY($3))) +
			  (SELECT count(*) FROM pitr_chunk_ref_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))
			     AND ($3::bigint[] IS NULL OR NOT EXISTS (
			          SELECT 1 FROM pitr_chunk_history scoped_chunk
			           WHERE scoped_chunk.txn_id=h.txn_id
			             AND NOT (scoped_chunk.inode=ANY($3))
			     )))`,
			targetID, scope, scopeInodes).Scan(&applied); scanErr != nil {
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
			"CALL pitr_revert($1,$2,$3,$4)",
			targetHash, scope, revertID, scopeInodes); callErr != nil {
			return callErr
		}
		return nil
	})
	if err != nil {
		return 0, "", fmt.Errorf("revert %s: %w", targetHash, err)
	}
	return applied, newVersionHash, nil
}

// resolveScopeInodes 用当前 edge 与目标之后的 edge history 合成目录图。这样
// 即使 scope 或其子项当前已被 rename/delete,仍能通过历史 edge 找回闭包。
// mountPath 未配置时返回 nil,保留按 transaction scope 过滤的兼容语义。
func (e *Engine) resolveScopeInodes(
	ctx context.Context,
	tx pg.Tx,
	targetID int64,
	scope string,
) ([]int64, error) {
	if e.mountPath == "" {
		return nil, nil
	}
	relative, ok := strings.CutPrefix(path.Clean(scope), e.mountPath)
	if !ok || (relative != "" && !strings.HasPrefix(relative, "/")) {
		return []int64{}, nil
	}
	relative = strings.Trim(relative, "/")
	parts := make([]string, 0)
	if relative != "" {
		parts = strings.Split(relative, "/")
	}
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE
		history_edges AS (
		  SELECT edge.parent,convert_from(edge.name,'UTF8') AS name,edge.inode
		    FROM pitr_edge_history h
		    JOIN pitr_txn t ON t.id=h.txn_id
		    CROSS JOIN LATERAL
		      jsonb_populate_record(NULL::jfs_edge,h.snapshot) AS edge
		   WHERE t.id>$1 AND h.snapshot IS NOT NULL
		     AND pitr_scopes_overlap(t.scope_path,$2)
		),
		all_edges AS (
		  SELECT parent,convert_from(name,'UTF8') AS name,inode FROM jfs_edge
		  UNION
		  SELECT parent,name,inode FROM history_edges
		),
		walk(depth,inode) AS (
		  SELECT 0,1::bigint
		  UNION ALL
		  SELECT walk.depth+1,edge.inode
		    FROM walk JOIN all_edges edge ON edge.parent=walk.inode
		   WHERE walk.depth<cardinality($3::text[])
		     AND edge.name=($3::text[])[walk.depth+1]
		),
		roots(inode) AS (
		  SELECT inode FROM walk WHERE depth=cardinality($3::text[])
		),
		tree(inode) AS (
		  SELECT inode FROM roots
		  UNION
		  SELECT edge.inode FROM tree JOIN all_edges edge ON edge.parent=tree.inode
		)
		SELECT DISTINCT inode FROM tree ORDER BY inode`,
		targetID, scope, parts)
	if err != nil {
		return nil, fmt.Errorf("解析 revert scope inode: %w", err)
	}
	defer rows.Close()
	inodes := make([]int64, 0)
	for rows.Next() {
		var inode int64
		if err := rows.Scan(&inode); err != nil {
			return nil, fmt.Errorf("读取 revert scope inode: %w", err)
		}
		inodes = append(inodes, inode)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 revert scope inode: %w", err)
	}
	return inodes, nil
}
