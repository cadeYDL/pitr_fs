-- ============================================================================
-- pitr-fs: 元数据 MVCC schema + 桥接触发器 + 坍缩/revert 存储过程
--
-- 权威源: internal/schema/init_pitr.sql
-- Docker: 由 deploy/init_pitr.sql (symlink) 引用同一份
--
-- 幂等:重复执行不报错。所有 DDL 用 IF NOT EXISTS / OR REPLACE / DROP IF EXISTS。
--
-- 对应设计文档:
--   §4.1  pitr 自有表(pitr_txn + 3 张 history + pitr_blob_retention)
--   §4.2  桥接触发器(jfs_node / jfs_edge / jfs_chunk)
--   §5.3  归属机制(读 GUC pitr.current_txn)
--   §6.1  commit 坍缩存储过程
--   §9    revert 存储过程
--
-- 归属 fallback 说明:若 pitrd 走"退化方案"(GUC 不共享连接),触发器读到 NULL
-- 会走时间戳兜底路径,txn_id 归属为最近的 active 事务(见 pitr_capture_common)。
-- ============================================================================

-- ---------- 4.1 pitr 自有表 ----------

CREATE TABLE IF NOT EXISTS pitr_txn (
    id            bigserial PRIMARY KEY,
    version_hash  char(12)  UNIQUE NOT NULL,
    parent_id     bigint    REFERENCES pitr_txn(id),
    scope_path    text      NOT NULL,
    state         text      NOT NULL,                 -- active | committed | rolled_back | auto | root
    command       text,
    message       text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    closed_at     timestamptz
);

CREATE INDEX IF NOT EXISTS idx_pitr_txn_scope_state ON pitr_txn (scope_path, state);
CREATE INDEX IF NOT EXISTS idx_pitr_txn_created     ON pitr_txn (created_at DESC);

-- 一个 scope 同时只能有一个 active 事务(一期约束)
CREATE UNIQUE INDEX IF NOT EXISTS uniq_active_txn_per_path
    ON pitr_txn (scope_path)
    WHERE state = 'active';

-- inode 影子表
CREATE TABLE IF NOT EXISTS pitr_node_history (
    txn_id       bigint NOT NULL REFERENCES pitr_txn(id) ON DELETE CASCADE,
    inode        bigint NOT NULL,
    op           char(1) NOT NULL,                    -- I=insert / U=update / D=delete
    snapshot     jsonb,                                -- 旧行(U/D)或 NULL(I)
    recorded_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (txn_id, inode)
);
CREATE INDEX IF NOT EXISTS idx_pitr_node_history_recorded ON pitr_node_history (recorded_at);
CREATE INDEX IF NOT EXISTS idx_pitr_node_history_inode    ON pitr_node_history (inode);

-- edge(目录项)影子表
CREATE TABLE IF NOT EXISTS pitr_edge_history (
    txn_id       bigint NOT NULL REFERENCES pitr_txn(id) ON DELETE CASCADE,
    parent       bigint NOT NULL,
    name         bytea  NOT NULL,                     -- JuiceFS 用 bytea 存 name
    op           char(1) NOT NULL,
    snapshot     jsonb,
    recorded_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (txn_id, parent, name)
);
CREATE INDEX IF NOT EXISTS idx_pitr_edge_history_recorded ON pitr_edge_history (recorded_at);

-- chunk 影子表
CREATE TABLE IF NOT EXISTS pitr_chunk_history (
    txn_id       bigint NOT NULL REFERENCES pitr_txn(id) ON DELETE CASCADE,
    inode        bigint NOT NULL,
    indx         int    NOT NULL,                     -- chunk 编号(inode 内)
    op           char(1) NOT NULL,
    snapshot     jsonb,
    recorded_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (txn_id, inode, indx)
);
CREATE INDEX IF NOT EXISTS idx_pitr_chunk_history_recorded ON pitr_chunk_history (recorded_at);

-- blob 引用/保留
CREATE TABLE IF NOT EXISTS pitr_blob_retention (
    object_key      text PRIMARY KEY,
    first_seen      timestamptz NOT NULL DEFAULT now(),
    retained_until  timestamptz
);

-- ---------- 根版本 ----------
--
-- id=1 通常预留给 root。用 ON CONFLICT (version_hash) DO NOTHING 保证幂等。
INSERT INTO pitr_txn (version_hash, scope_path, state, command)
VALUES ('000000000000', '/', 'root', 'init')
ON CONFLICT (version_hash) DO NOTHING;

-- ---------- 4.2 桥接触发器函数 ----------
--
-- 通用归属:优先读 GUC pitr.current_txn(生产方案);为空时走时间戳 fallback,
-- 归到该 scope 下最近的 active/auto 事务(退化方案)。若两者都无,说明是
-- JuiceFS 内部 compaction 等非用户操作,直接跳过打点(避免污染历史)。

CREATE OR REPLACE FUNCTION pitr_current_txn() RETURNS bigint AS $$
DECLARE
    v_txn text := current_setting('pitr.current_txn', true);
BEGIN
    IF v_txn IS NULL OR v_txn = '' THEN
        RETURN NULL;
    END IF;
    RETURN v_txn::bigint;
EXCEPTION WHEN invalid_text_representation THEN
    RETURN NULL;
END;
$$ LANGUAGE plpgsql STABLE;

CREATE OR REPLACE FUNCTION pitr_capture_node_change() RETURNS TRIGGER AS $$
DECLARE
    v_txn bigint := pitr_current_txn();
BEGIN
    IF v_txn IS NULL THEN
        RETURN COALESCE(NEW, OLD);
    END IF;
    INSERT INTO pitr_node_history (txn_id, inode, op, snapshot)
    VALUES (
        v_txn,
        COALESCE(OLD.inode, NEW.inode),
        CASE TG_OP WHEN 'INSERT' THEN 'I' WHEN 'DELETE' THEN 'D' ELSE 'U' END,
        CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE to_jsonb(OLD) END
    )
    ON CONFLICT (txn_id, inode) DO NOTHING;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_capture_edge_change() RETURNS TRIGGER AS $$
DECLARE
    v_txn bigint := pitr_current_txn();
BEGIN
    IF v_txn IS NULL THEN
        RETURN COALESCE(NEW, OLD);
    END IF;
    INSERT INTO pitr_edge_history (txn_id, parent, name, op, snapshot)
    VALUES (
        v_txn,
        COALESCE(OLD.parent, NEW.parent),
        COALESCE(OLD.name,   NEW.name),
        CASE TG_OP WHEN 'INSERT' THEN 'I' WHEN 'DELETE' THEN 'D' ELSE 'U' END,
        CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE to_jsonb(OLD) END
    )
    ON CONFLICT (txn_id, parent, name) DO NOTHING;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_capture_chunk_change() RETURNS TRIGGER AS $$
DECLARE
    v_txn bigint := pitr_current_txn();
BEGIN
    IF v_txn IS NULL THEN
        RETURN COALESCE(NEW, OLD);
    END IF;
    INSERT INTO pitr_chunk_history (txn_id, inode, indx, op, snapshot)
    VALUES (
        v_txn,
        COALESCE(OLD.inode, NEW.inode),
        COALESCE(OLD.indx,  NEW.indx),
        CASE TG_OP WHEN 'INSERT' THEN 'I' WHEN 'DELETE' THEN 'D' ELSE 'U' END,
        CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE to_jsonb(OLD) END
    )
    ON CONFLICT (txn_id, inode, indx) DO NOTHING;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

-- ---------- 装触发器(依赖 JuiceFS 建好的 jfs_* 表) ----------
--
-- JuiceFS 在 juicefs format 后才建 jfs_*,所以这段用 DO 块检查存在性;
-- 不存在时是"首次装 pitr,JuiceFS 还没 format",跳过即可,由 entrypoint.sh
-- 在 juicefs format 之后再跑一次 init_pitr.sql 时补上。

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jfs_node') THEN
        DROP TRIGGER IF EXISTS tg_pitr_node_capture ON jfs_node;
        CREATE TRIGGER tg_pitr_node_capture
            AFTER INSERT OR UPDATE OR DELETE ON jfs_node
            FOR EACH ROW EXECUTE FUNCTION pitr_capture_node_change();
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jfs_edge') THEN
        DROP TRIGGER IF EXISTS tg_pitr_edge_capture ON jfs_edge;
        CREATE TRIGGER tg_pitr_edge_capture
            AFTER INSERT OR UPDATE OR DELETE ON jfs_edge
            FOR EACH ROW EXECUTE FUNCTION pitr_capture_edge_change();
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jfs_chunk') THEN
        DROP TRIGGER IF EXISTS tg_pitr_chunk_capture ON jfs_chunk;
        CREATE TRIGGER tg_pitr_chunk_capture
            AFTER INSERT OR UPDATE OR DELETE ON jfs_chunk
            FOR EACH ROW EXECUTE FUNCTION pitr_capture_chunk_change();
    END IF;
END $$;

-- ============================================================================
-- 6.1 commit 坍缩存储过程
--
-- 语义:把 commit_id 下所有 auto 子事务的 history 折叠到 commit_id,
-- 每个 (table, key) 只保留最早的一份 snapshot(begin 前的原值)。
-- 完成后中间的 auto 事务被删掉(state='auto' + parent_id=commit_id)。
-- ============================================================================

CREATE OR REPLACE PROCEDURE pitr_collapse_commit(p_commit_id bigint) AS $$
BEGIN
    -- pitr_node_history: 每个 inode 只留最早一份
    DELETE FROM pitr_node_history nh USING (
        SELECT txn_id, inode
        FROM (
            SELECT txn_id, inode, recorded_at,
                   ROW_NUMBER() OVER (PARTITION BY inode ORDER BY recorded_at, txn_id) AS rn
            FROM   pitr_node_history
            WHERE  txn_id IN (SELECT id FROM pitr_txn
                              WHERE parent_id = p_commit_id AND state = 'auto')
        ) x
        WHERE rn > 1
    ) dup
    WHERE nh.txn_id = dup.txn_id AND nh.inode = dup.inode;

    -- pitr_edge_history: 每 (parent, name) 只留最早一份
    DELETE FROM pitr_edge_history eh USING (
        SELECT txn_id, parent, name
        FROM (
            SELECT txn_id, parent, name, recorded_at,
                   ROW_NUMBER() OVER (PARTITION BY parent, name ORDER BY recorded_at, txn_id) AS rn
            FROM   pitr_edge_history
            WHERE  txn_id IN (SELECT id FROM pitr_txn
                              WHERE parent_id = p_commit_id AND state = 'auto')
        ) x
        WHERE rn > 1
    ) dup
    WHERE eh.txn_id = dup.txn_id AND eh.parent = dup.parent AND eh.name = dup.name;

    -- pitr_chunk_history: 每 (inode, indx) 只留最早一份
    DELETE FROM pitr_chunk_history ch USING (
        SELECT txn_id, inode, indx
        FROM (
            SELECT txn_id, inode, indx, recorded_at,
                   ROW_NUMBER() OVER (PARTITION BY inode, indx ORDER BY recorded_at, txn_id) AS rn
            FROM   pitr_chunk_history
            WHERE  txn_id IN (SELECT id FROM pitr_txn
                              WHERE parent_id = p_commit_id AND state = 'auto')
        ) x
        WHERE rn > 1
    ) dup
    WHERE ch.txn_id = dup.txn_id AND ch.inode = dup.inode AND ch.indx = dup.indx;

    -- history 重挂到 commit_id
    UPDATE pitr_node_history  SET txn_id = p_commit_id
      WHERE txn_id IN (SELECT id FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto');
    UPDATE pitr_edge_history  SET txn_id = p_commit_id
      WHERE txn_id IN (SELECT id FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto');
    UPDATE pitr_chunk_history SET txn_id = p_commit_id
      WHERE txn_id IN (SELECT id FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto');

    -- 删中间 auto 事务
    DELETE FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto';

    -- 标 commit_id 为 committed
    UPDATE pitr_txn SET state = 'committed', closed_at = now()
      WHERE id = p_commit_id AND state <> 'committed';
END;
$$ LANGUAGE plpgsql;

-- ============================================================================
-- 9. revert 存储过程
--
-- 语义:回退到 target 版本(不含) —— 把从 target 之后所有事务里 history 记录的
-- 旧行 replay 回 JuiceFS 主表。按时间倒序 replay:
--   op='U' → UPDATE jfs_* SET ... = snapshot
--   op='D' → INSERT INTO jfs_* SELECT * FROM jsonb_populate_record(...)  (恢复被删的行)
--   op='I' → DELETE FROM jfs_* WHERE 主键 = ...
--
-- 参数:
--   p_target_hash — 目标版本 version_hash(char(12))
--   p_scope_path  — 可选目录级 revert 范围(默认 NULL 表示全局)
--
-- 关键:revert 自身执行的 jfs_* 变更也会触发 pitr trigger,若不抑制会形成"revert 的
-- revert"。所以 revert 内部 SET LOCAL pitr.current_txn = '' 绕过打点。
-- 一致性:所有 replay 在同一事务内完成,原子。
-- ============================================================================

CREATE OR REPLACE PROCEDURE pitr_revert(
    p_target_hash char(12),
    p_scope_path  text DEFAULT NULL
) AS $$
DECLARE
    v_target_txn_id  bigint;
    v_target_time    timestamptz;
    r_node RECORD;
    r_edge RECORD;
    r_chunk RECORD;
BEGIN
    SELECT id, created_at INTO v_target_txn_id, v_target_time
      FROM pitr_txn WHERE version_hash = p_target_hash;
    IF v_target_txn_id IS NULL THEN
        RAISE EXCEPTION 'pitr_revert: version_hash % 不存在', p_target_hash;
    END IF;

    -- 抑制 revert 自身写入触发的打点
    PERFORM set_config('pitr.current_txn', '', true);

    -- 反向 replay pitr_node_history:按 recorded_at 倒序,每个 inode 取最新那一条
    FOR r_node IN
        SELECT DISTINCT ON (inode) inode, op, snapshot
        FROM   pitr_node_history nh
        JOIN   pitr_txn t ON t.id = nh.txn_id
        WHERE  nh.recorded_at > v_target_time
          AND  (p_scope_path IS NULL OR t.scope_path LIKE p_scope_path || '%')
        ORDER  BY inode, nh.recorded_at DESC
    LOOP
        IF r_node.op = 'I' THEN
            DELETE FROM jfs_node WHERE inode = r_node.inode;
        ELSIF r_node.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_node WHERE inode = r_node.inode) THEN
                UPDATE jfs_node
                SET (inode, type, flags, mode, uid, gid, atime, mtime, ctime, atimensec, mtimensec, ctimensec, nlink, length, rdev, parent, access_acl_id, default_acl_id) =
                    (SELECT jn.inode, jn.type, jn.flags, jn.mode, jn.uid, jn.gid, jn.atime, jn.mtime, jn.ctime, jn.atimensec, jn.mtimensec, jn.ctimensec, jn.nlink, jn.length, jn.rdev, jn.parent, jn.access_acl_id, jn.default_acl_id
                     FROM jsonb_populate_record(NULL::jfs_node, r_node.snapshot) AS jn)
                WHERE inode = r_node.inode;
            ELSE
                INSERT INTO jfs_node SELECT * FROM jsonb_populate_record(NULL::jfs_node, r_node.snapshot);
            END IF;
        END IF;
    END LOOP;

    -- pitr_edge_history 反向 replay
    FOR r_edge IN
        SELECT DISTINCT ON (parent, name) parent, name, op, snapshot
        FROM   pitr_edge_history eh
        JOIN   pitr_txn t ON t.id = eh.txn_id
        WHERE  eh.recorded_at > v_target_time
          AND  (p_scope_path IS NULL OR t.scope_path LIKE p_scope_path || '%')
        ORDER  BY parent, name, eh.recorded_at DESC
    LOOP
        IF r_edge.op = 'I' THEN
            DELETE FROM jfs_edge WHERE parent = r_edge.parent AND name = r_edge.name;
        ELSIF r_edge.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_edge WHERE parent = r_edge.parent AND name = r_edge.name) THEN
                UPDATE jfs_edge SET (parent, name, inode, type) =
                    (SELECT je.parent, je.name, je.inode, je.type
                     FROM jsonb_populate_record(NULL::jfs_edge, r_edge.snapshot) AS je)
                WHERE parent = r_edge.parent AND name = r_edge.name;
            ELSE
                INSERT INTO jfs_edge SELECT * FROM jsonb_populate_record(NULL::jfs_edge, r_edge.snapshot);
            END IF;
        END IF;
    END LOOP;

    -- pitr_chunk_history 反向 replay
    FOR r_chunk IN
        SELECT DISTINCT ON (inode, indx) inode, indx, op, snapshot
        FROM   pitr_chunk_history ch
        JOIN   pitr_txn t ON t.id = ch.txn_id
        WHERE  ch.recorded_at > v_target_time
          AND  (p_scope_path IS NULL OR t.scope_path LIKE p_scope_path || '%')
        ORDER  BY inode, indx, ch.recorded_at DESC
    LOOP
        IF r_chunk.op = 'I' THEN
            DELETE FROM jfs_chunk WHERE inode = r_chunk.inode AND indx = r_chunk.indx;
        ELSIF r_chunk.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_chunk WHERE inode = r_chunk.inode AND indx = r_chunk.indx) THEN
                UPDATE jfs_chunk SET (inode, indx, slices) =
                    (SELECT jc.inode, jc.indx, jc.slices
                     FROM jsonb_populate_record(NULL::jfs_chunk, r_chunk.snapshot) AS jc)
                WHERE inode = r_chunk.inode AND indx = r_chunk.indx;
            ELSE
                INSERT INTO jfs_chunk SELECT * FROM jsonb_populate_record(NULL::jfs_chunk, r_chunk.snapshot);
            END IF;
        END IF;
    END LOOP;
END;
$$ LANGUAGE plpgsql;
