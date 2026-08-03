// Package revert 实现 PostgreSQL 元数据 undo-log 的原子回放。
package revert

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"pitr_fs/internal/pg"
	"pitr_fs/internal/txn"
)

var versionHashRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

func ValidVersionHash(value string) bool {
	return versionHashRE.MatchString(strings.ToLower(strings.TrimSpace(value)))
}

type Engine struct {
	db        *pg.DB
	mountMu   sync.RWMutex
	mountPath string
}

// SetMountPath 在 init 选定用户挂载点后更新目录级 revert 的路径基准。
func (e *Engine) SetMountPath(value string) {
	if e == nil {
		return
	}
	e.mountMu.Lock()
	defer e.mountMu.Unlock()
	e.mountPath = ""
	if value != "" {
		e.mountPath = path.Clean(value)
	}
}

func (e *Engine) getMountPath() string {
	e.mountMu.RLock()
	defer e.mountMu.RUnlock()
	return e.mountPath
}

const (
	openWriteWait = 2 * time.Second
	openWritePoll = 10 * time.Millisecond
)

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
	if !ValidVersionHash(targetHash) {
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
	if waitErr := e.waitForOpenWrites(ctx, scope); waitErr != nil {
		return 0, "", waitErr
	}

	err = e.db.InTx(ctx, func(tx pg.Tx) error {
		// 所有 revert 串行,同时让 active 检查、计数、replay 和新版本插入
		// 处在同一个快照与事务里。
		if _, lockErr := tx.Exec(ctx,
			"SELECT pg_advisory_xact_lock(hashtext('pitr-fs:versions'))"); lockErr != nil {
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
			 WHERE (state='active' OR (state='auto' AND closed_at IS NULL))
			   AND ($1::text IS NULL OR pitr_scopes_overlap(scope_path, $1))`,
			scope).Scan(&activeCount); scanErr != nil {
			return scanErr
		}
		if activeCount != 0 {
			return fmt.Errorf("%w: %d", ErrActiveScope, activeCount)
		}

		if _, createErr := tx.Exec(ctx, `
			CREATE TEMP TABLE pitr_revert_scope_inode(
				inode bigint PRIMARY KEY
			) ON COMMIT DROP`); createErr != nil {
			return fmt.Errorf("创建 revert scope 临时表: %w", createErr)
		}
		scopeFiltered := false
		if scope != nil {
			scopeFiltered, err = e.prepareScopeInodes(
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
			     AND (NOT $3::boolean OR EXISTS (
			          SELECT 1 FROM pg_temp.pitr_revert_scope_inode s
			           WHERE s.inode=h.inode))) +
			  (SELECT count(*) FROM pitr_edge_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))
			     AND (NOT $3::boolean OR EXISTS (
			          SELECT 1 FROM pg_temp.pitr_revert_scope_inode s
			           WHERE s.inode=h.parent
			              OR s.inode=(h.snapshot->>'inode')::bigint))) +
			  (SELECT count(*) FROM pitr_chunk_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))
			     AND (NOT $3::boolean OR EXISTS (
			          SELECT 1 FROM pg_temp.pitr_revert_scope_inode s
			           WHERE s.inode=h.inode))) +
			  (SELECT count(*) FROM pitr_chunk_ref_history h
			    JOIN pitr_txn t ON t.id=h.txn_id
			   WHERE t.id>$1
			     AND ($2::text IS NULL OR pitr_scopes_overlap(t.scope_path,$2))
			     AND (NOT $3::boolean OR NOT EXISTS (
			          SELECT 1 FROM pitr_chunk_history scoped_chunk
			           WHERE scoped_chunk.txn_id=h.txn_id
			             AND NOT EXISTS (
			                 SELECT 1 FROM pg_temp.pitr_revert_scope_inode s
			                  WHERE s.inode=scoped_chunk.inode)
			     )))`,
			targetID, scope, scopeFiltered).Scan(&applied); scanErr != nil {
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
					(version_hash,parent_id,scope_path,state,command,posix_op,
					 process_command,actor_name,change_summary,closed_at)
				VALUES ($1,$2,$3,'committed',$4,$5,$6,'pitrd',$7,now())
				ON CONFLICT (version_hash) DO NOTHING
				RETURNING id`,
				hash, targetID, scopePath, "revert:"+targetHash,
				fmt.Sprintf("revert(%q)", targetHash),
				"pitr rever...",
				fmt.Sprintf("replay %d history rows", applied)).
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
			"CALL pitr_revert_from_temp($1,$2,$3,$4)",
			targetHash, scope, revertID, scopeFiltered); callErr != nil {
			return callErr
		}
		if _, pruneErr := txn.PruneClosedVersions(
			ctx, tx, scopePath); pruneErr != nil {
			return pruneErr
		}
		return nil
	})
	if err != nil {
		return 0, "", fmt.Errorf("revert %s: %w", targetHash, err)
	}
	return applied, newVersionHash, nil
}

// waitForOpenWrites 隐藏 FUSE close 后异步 Release 的短暂窗口。手工 active
// 事务仍由事务内检查立即拒绝；这里只等待自动写窗口，避免用户刚写完文件就
// revert 时看到内部实现细节。
func (e *Engine) waitForOpenWrites(
	ctx context.Context,
	scope *string,
) error {
	deadline := time.Now().Add(openWriteWait)
	for {
		var open int64
		if err := e.db.QueryRow(ctx, `
			SELECT count(*)
			  FROM pitr_txn
			 WHERE state='auto' AND closed_at IS NULL
			   AND ($1::text IS NULL OR pitr_scopes_overlap(scope_path,$1))`,
			scope).Scan(&open); err != nil {
			return fmt.Errorf("等待开放写窗口: %w", err)
		}
		if open == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %d", ErrActiveScope, open)
		}
		timer := time.NewTimer(openWritePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// prepareScopeInodes 用当前 edge 与目标之后的 edge history 合成目录图。这样
// 即使 scope 或其子项当前已被 rename/delete,仍能通过历史 edge 找回闭包。
// 闭包保留在当前 PostgreSQL 事务的临时表中，计数与 replay 都在服务端 join，
// 不再把大型 inode 数组物化到 Go 后又传回数据库。
// mountPath 未配置时返回 false,保留按 transaction scope 过滤的兼容语义。
func (e *Engine) prepareScopeInodes(
	ctx context.Context,
	tx pg.Tx,
	targetID int64,
	scope string,
) (bool, error) {
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE IF NOT EXISTS pitr_revert_scope_inode(
			inode bigint PRIMARY KEY
		) ON COMMIT DROP;
		TRUNCATE pg_temp.pitr_revert_scope_inode`); err != nil {
		return false, fmt.Errorf("准备 revert scope 临时表: %w", err)
	}
	mountPath := e.getMountPath()
	if mountPath == "" {
		return false, nil
	}
	relative, ok := strings.CutPrefix(path.Clean(scope), mountPath)
	if !ok || (relative != "" && !strings.HasPrefix(relative, "/")) {
		return true, nil
	}
	relative = strings.Trim(relative, "/")
	parts := make([]string, 0)
	if relative != "" {
		parts = strings.Split(relative, "/")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO pg_temp.pitr_revert_scope_inode(inode)
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
		SELECT DISTINCT inode FROM tree`,
		targetID, scope, parts)
	if err != nil {
		return false, fmt.Errorf("解析 revert scope inode: %w", err)
	}
	return true, nil
}
