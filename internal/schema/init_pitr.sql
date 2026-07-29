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
-- 归属 fallback 说明:若 JuiceFS 独立连接读不到 GUC,触发器会归属到 daemon
-- 事先创建的唯一开放 auto 窗口(见 pitr_current_txn)。
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
CREATE UNIQUE INDEX IF NOT EXISTS uniq_open_auto_window
    ON pitr_txn ((1))
    WHERE state = 'auto' AND closed_at IS NULL;

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

-- chunk 引用计数影子表。恢复 chunk 指针时必须同步恢复引用计数，否则旧 blob
-- 可能在回退完成前被 JuiceFS 判定为无引用。
CREATE TABLE IF NOT EXISTS pitr_chunk_ref_history (
    txn_id       bigint NOT NULL REFERENCES pitr_txn(id) ON DELETE CASCADE,
    chunkid      bigint NOT NULL,
    op           char(1) NOT NULL,
    snapshot     jsonb,
    recorded_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (txn_id, chunkid)
);
CREATE INDEX IF NOT EXISTS idx_pitr_chunk_ref_history_recorded
    ON pitr_chunk_ref_history (recorded_at);

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
-- 通用归属:
--   1. 优先读 GUC pitr.current_txn,供同连接调用与测试使用;
--   2. GUC 为空时,归到最近的、尚未关闭的 auto 版本。Phase 3 的代理层保证
--      单 daemon 内一次只开放一个 auto 窗口,JuiceFS 独立进程的写事务便可
--      通过这个窗口获得稳定 txn_id;
--   3. 两者都不存在时视为 JuiceFS 内部操作,不记录历史。

CREATE OR REPLACE FUNCTION pitr_current_txn() RETURNS bigint AS $$
DECLARE
    v_txn text := current_setting('pitr.current_txn', true);
BEGIN
    IF current_setting('pitr.suppress_capture', true) = 'on' THEN
        RETURN NULL;
    END IF;
    IF v_txn IS NULL OR v_txn = '' THEN
        SELECT id INTO v_txn
          FROM pitr_txn
         WHERE state = 'auto' AND closed_at IS NULL
         ORDER BY created_at DESC, id DESC
         LIMIT 1;
        RETURN v_txn;
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
        -- INSERT 也保留 NEW,供目录级回退判断被插入 edge 的 child inode。
        CASE WHEN TG_OP = 'INSERT' THEN to_jsonb(NEW) ELSE to_jsonb(OLD) END
    )
    ON CONFLICT (txn_id, parent, name) DO NOTHING;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_capture_chunk_ref_change() RETURNS TRIGGER AS $$
DECLARE
    v_txn bigint := pitr_current_txn();
BEGIN
    IF v_txn IS NULL THEN
        RETURN COALESCE(NEW, OLD);
    END IF;
    INSERT INTO pitr_chunk_ref_history (txn_id, chunkid, op, snapshot)
    VALUES (
        v_txn,
        COALESCE(OLD.chunkid, NEW.chunkid),
        CASE TG_OP WHEN 'INSERT' THEN 'I' WHEN 'DELETE' THEN 'D' ELSE 'U' END,
        CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE to_jsonb(OLD) END
    )
    ON CONFLICT (txn_id, chunkid) DO NOTHING;
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

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jfs_chunk_ref') THEN
        DROP TRIGGER IF EXISTS tg_pitr_chunk_ref_capture ON jfs_chunk_ref;
        CREATE TRIGGER tg_pitr_chunk_ref_capture
            AFTER INSERT OR UPDATE OR DELETE ON jfs_chunk_ref
            FOR EACH ROW EXECUTE FUNCTION pitr_capture_chunk_ref_change();
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

    -- pitr_chunk_ref_history: 每个 chunkid 只留最早一份
    DELETE FROM pitr_chunk_ref_history crh USING (
        SELECT txn_id, chunkid
        FROM (
            SELECT txn_id, chunkid, recorded_at,
                   ROW_NUMBER() OVER (PARTITION BY chunkid ORDER BY recorded_at, txn_id) AS rn
            FROM   pitr_chunk_ref_history
            WHERE  txn_id IN (SELECT id FROM pitr_txn
                              WHERE parent_id = p_commit_id AND state = 'auto')
        ) x
        WHERE rn > 1
    ) dup
    WHERE crh.txn_id = dup.txn_id AND crh.chunkid = dup.chunkid;

    -- history 重挂到 commit_id
    UPDATE pitr_node_history  SET txn_id = p_commit_id
      WHERE txn_id IN (SELECT id FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto');
    UPDATE pitr_edge_history  SET txn_id = p_commit_id
      WHERE txn_id IN (SELECT id FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto');
    UPDATE pitr_chunk_history SET txn_id = p_commit_id
      WHERE txn_id IN (SELECT id FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto');
    UPDATE pitr_chunk_ref_history SET txn_id = p_commit_id
      WHERE txn_id IN (SELECT id FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto');

    -- 删中间 auto 事务
    DELETE FROM pitr_txn WHERE parent_id = p_commit_id AND state = 'auto';

    -- 标 commit_id 为 committed
    UPDATE pitr_txn SET state = 'committed', closed_at = now()
      WHERE id = p_commit_id AND state <> 'committed';
END;
$$ LANGUAGE plpgsql;

-- 两个规范化绝对路径的作用域是否相交。使用路径段边界,避免 /a 匹配 /abc。
CREATE OR REPLACE FUNCTION pitr_scopes_overlap(a text, b text) RETURNS boolean AS $$
    SELECT a = '/' OR b = '/' OR a = b
        OR a LIKE rtrim(b, '/') || '/%'
        OR b LIKE rtrim(a, '/') || '/%';
$$ LANGUAGE sql IMMUTABLE;

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
    r_node RECORD;
    r_edge RECORD;
    r_chunk RECORD;
    r_chunk_ref RECORD;
BEGIN
    SELECT id INTO v_target_txn_id
      FROM pitr_txn WHERE version_hash = p_target_hash;
    IF v_target_txn_id IS NULL THEN
        RAISE EXCEPTION 'pitr_revert: version_hash % 不存在', p_target_hash;
    END IF;

    -- 抑制 revert 自身写入触发的打点
    PERFORM set_config('pitr.suppress_capture', 'on', true);

    -- 反向 replay 全部 history。不能只取最新 snapshot:同一 inode 经历
    -- v1→v2→v3 时,必须依次恢复 v3 前、v2 前的状态才能回到 v1。
    FOR r_node IN
        SELECT inode, op, snapshot
        FROM   pitr_node_history nh
        JOIN   pitr_txn t ON t.id = nh.txn_id
        WHERE  t.id > v_target_txn_id
          AND  (p_scope_path IS NULL OR pitr_scopes_overlap(t.scope_path, p_scope_path))
        ORDER  BY nh.recorded_at DESC, nh.txn_id DESC
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
        SELECT parent, name, op, snapshot
        FROM   pitr_edge_history eh
        JOIN   pitr_txn t ON t.id = eh.txn_id
        WHERE  t.id > v_target_txn_id
          AND  (p_scope_path IS NULL OR pitr_scopes_overlap(t.scope_path, p_scope_path))
        ORDER  BY eh.recorded_at DESC, eh.txn_id DESC
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
        SELECT inode, indx, op, snapshot
        FROM   pitr_chunk_history ch
        JOIN   pitr_txn t ON t.id = ch.txn_id
        WHERE  t.id > v_target_txn_id
          AND  (p_scope_path IS NULL OR pitr_scopes_overlap(t.scope_path, p_scope_path))
        ORDER  BY ch.recorded_at DESC, ch.txn_id DESC
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

    -- pitr_chunk_ref_history 反向 replay
    FOR r_chunk_ref IN
        SELECT chunkid, op, snapshot
        FROM   pitr_chunk_ref_history crh
        JOIN   pitr_txn t ON t.id = crh.txn_id
        WHERE  t.id > v_target_txn_id
          AND  (p_scope_path IS NULL OR pitr_scopes_overlap(t.scope_path, p_scope_path))
        ORDER  BY crh.recorded_at DESC, crh.txn_id DESC
    LOOP
        IF r_chunk_ref.op = 'I' THEN
            DELETE FROM jfs_chunk_ref WHERE chunkid = r_chunk_ref.chunkid;
        ELSIF r_chunk_ref.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_chunk_ref WHERE chunkid = r_chunk_ref.chunkid) THEN
                UPDATE jfs_chunk_ref SET (chunkid, size, refs) =
                    (SELECT jcr.chunkid, jcr.size, jcr.refs
                     FROM jsonb_populate_record(NULL::jfs_chunk_ref, r_chunk_ref.snapshot) AS jcr)
                WHERE chunkid = r_chunk_ref.chunkid;
            ELSE
                INSERT INTO jfs_chunk_ref
                SELECT * FROM jsonb_populate_record(NULL::jfs_chunk_ref, r_chunk_ref.snapshot);
            END IF;
        END IF;
    END LOOP;
END;
$$ LANGUAGE plpgsql;

-- ============================================================================
-- active transaction rollback
--
-- 只反向 replay 指定 active txn 的 auto 子版本,不影响其他 scope/txn。
-- replay 完成后删除 auto 子版本并把 active 标为 rolled_back。
-- ============================================================================

CREATE OR REPLACE PROCEDURE pitr_rollback(p_txn_id bigint) AS $$
DECLARE
    v_state text;
    r_node RECORD;
    r_edge RECORD;
    r_chunk RECORD;
    r_chunk_ref RECORD;
BEGIN
    SELECT state INTO v_state FROM pitr_txn WHERE id = p_txn_id FOR UPDATE;
    IF v_state IS NULL THEN
        RAISE EXCEPTION 'pitr_rollback: txn % 不存在', p_txn_id;
    END IF;
    IF v_state <> 'active' THEN
        RAISE EXCEPTION 'pitr_rollback: txn % state=% 不能 rollback', p_txn_id, v_state;
    END IF;

    PERFORM set_config('pitr.suppress_capture', 'on', true);

    FOR r_node IN
        SELECT nh.inode, nh.op, nh.snapshot
          FROM pitr_node_history nh
          JOIN pitr_txn t ON t.id = nh.txn_id
         WHERE t.parent_id = p_txn_id AND t.state = 'auto'
         ORDER BY nh.recorded_at DESC, nh.txn_id DESC
    LOOP
        IF r_node.op = 'I' THEN
            DELETE FROM jfs_node WHERE inode = r_node.inode;
        ELSIF r_node.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_node WHERE inode = r_node.inode) THEN
                UPDATE jfs_node
                   SET (inode, type, flags, mode, uid, gid, atime, mtime, ctime,
                        atimensec, mtimensec, ctimensec, nlink, length, rdev,
                        parent, access_acl_id, default_acl_id) =
                       (SELECT jn.inode, jn.type, jn.flags, jn.mode, jn.uid, jn.gid,
                               jn.atime, jn.mtime, jn.ctime, jn.atimensec,
                               jn.mtimensec, jn.ctimensec, jn.nlink, jn.length,
                               jn.rdev, jn.parent, jn.access_acl_id, jn.default_acl_id
                          FROM jsonb_populate_record(NULL::jfs_node, r_node.snapshot) AS jn)
                 WHERE inode = r_node.inode;
            ELSE
                INSERT INTO jfs_node
                SELECT * FROM jsonb_populate_record(NULL::jfs_node, r_node.snapshot);
            END IF;
        END IF;
    END LOOP;

    FOR r_edge IN
        SELECT eh.parent, eh.name, eh.op, eh.snapshot
          FROM pitr_edge_history eh
          JOIN pitr_txn t ON t.id = eh.txn_id
         WHERE t.parent_id = p_txn_id AND t.state = 'auto'
         ORDER BY eh.recorded_at DESC, eh.txn_id DESC
    LOOP
        IF r_edge.op = 'I' THEN
            DELETE FROM jfs_edge WHERE parent = r_edge.parent AND name = r_edge.name;
        ELSIF r_edge.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_edge
                        WHERE parent = r_edge.parent AND name = r_edge.name) THEN
                UPDATE jfs_edge SET (parent, name, inode, type) =
                    (SELECT je.parent, je.name, je.inode, je.type
                       FROM jsonb_populate_record(NULL::jfs_edge, r_edge.snapshot) AS je)
                 WHERE parent = r_edge.parent AND name = r_edge.name;
            ELSE
                INSERT INTO jfs_edge
                SELECT * FROM jsonb_populate_record(NULL::jfs_edge, r_edge.snapshot);
            END IF;
        END IF;
    END LOOP;

    FOR r_chunk IN
        SELECT ch.inode, ch.indx, ch.op, ch.snapshot
          FROM pitr_chunk_history ch
          JOIN pitr_txn t ON t.id = ch.txn_id
         WHERE t.parent_id = p_txn_id AND t.state = 'auto'
         ORDER BY ch.recorded_at DESC, ch.txn_id DESC
    LOOP
        IF r_chunk.op = 'I' THEN
            DELETE FROM jfs_chunk WHERE inode = r_chunk.inode AND indx = r_chunk.indx;
        ELSIF r_chunk.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_chunk
                        WHERE inode = r_chunk.inode AND indx = r_chunk.indx) THEN
                UPDATE jfs_chunk SET (inode, indx, slices) =
                    (SELECT jc.inode, jc.indx, jc.slices
                       FROM jsonb_populate_record(NULL::jfs_chunk, r_chunk.snapshot) AS jc)
                 WHERE inode = r_chunk.inode AND indx = r_chunk.indx;
            ELSE
                INSERT INTO jfs_chunk
                SELECT * FROM jsonb_populate_record(NULL::jfs_chunk, r_chunk.snapshot);
            END IF;
        END IF;
    END LOOP;

    FOR r_chunk_ref IN
        SELECT crh.chunkid, crh.op, crh.snapshot
          FROM pitr_chunk_ref_history crh
          JOIN pitr_txn t ON t.id = crh.txn_id
         WHERE t.parent_id = p_txn_id AND t.state = 'auto'
         ORDER BY crh.recorded_at DESC, crh.txn_id DESC
    LOOP
        IF r_chunk_ref.op = 'I' THEN
            DELETE FROM jfs_chunk_ref WHERE chunkid = r_chunk_ref.chunkid;
        ELSIF r_chunk_ref.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_chunk_ref
                        WHERE chunkid = r_chunk_ref.chunkid) THEN
                UPDATE jfs_chunk_ref SET (chunkid, size, refs) =
                    (SELECT jcr.chunkid, jcr.size, jcr.refs
                       FROM jsonb_populate_record(NULL::jfs_chunk_ref,
                                                  r_chunk_ref.snapshot) AS jcr)
                 WHERE chunkid = r_chunk_ref.chunkid;
            ELSE
                INSERT INTO jfs_chunk_ref
                SELECT * FROM jsonb_populate_record(NULL::jfs_chunk_ref,
                                                    r_chunk_ref.snapshot);
            END IF;
        END IF;
    END LOOP;

    DELETE FROM pitr_txn WHERE parent_id = p_txn_id AND state = 'auto';
    UPDATE pitr_txn
       SET state = 'rolled_back', command = 'rollback', closed_at = now()
     WHERE id = p_txn_id;
END;
$$ LANGUAGE plpgsql;

-- ============================================================================
-- failed auto compensation
--
-- FUSE action 失败时只补偿对应 auto。复用 pitr_rollback 的成熟 replay 路径:
-- 临时摘下同 parent 的其他 auto,rollback 目标 auto,然后恢复 active parent 与
-- 其他 auto 的父子关系。整个过程运行在 CALL 所在事务中,任一步失败都会原子
-- 回滚,不会暴露临时拓扑。
-- ============================================================================

CREATE OR REPLACE PROCEDURE pitr_abort_auto(p_auto_id bigint) AS $$
DECLARE
    v_parent_id      bigint;
    v_auto_state     text;
    v_parent_state   text;
    v_parent_command text;
    v_sibling_ids    bigint[];
BEGIN
    SELECT parent_id, state
      INTO v_parent_id, v_auto_state
      FROM pitr_txn
     WHERE id = p_auto_id
     FOR UPDATE;
    IF v_auto_state IS NULL THEN
        RAISE EXCEPTION 'pitr_abort_auto: auto % 不存在', p_auto_id;
    END IF;
    IF v_auto_state <> 'auto' OR v_parent_id IS NULL THEN
        RAISE EXCEPTION 'pitr_abort_auto: txn % state=% parent=% 不是 auto',
            p_auto_id, v_auto_state, v_parent_id;
    END IF;

    SELECT state, command
      INTO v_parent_state, v_parent_command
      FROM pitr_txn
     WHERE id = v_parent_id
     FOR UPDATE;
    IF v_parent_state <> 'active' THEN
        RAISE EXCEPTION 'pitr_abort_auto: parent % state=% 不是 active',
            v_parent_id, v_parent_state;
    END IF;

    SELECT array_agg(id ORDER BY id)
      INTO v_sibling_ids
      FROM pitr_txn
     WHERE parent_id = v_parent_id
       AND state = 'auto'
       AND id <> p_auto_id;

    UPDATE pitr_txn
       SET parent_id = NULL
     WHERE id = ANY(COALESCE(v_sibling_ids, ARRAY[]::bigint[]));

    CALL pitr_rollback(v_parent_id);

    UPDATE pitr_txn
       SET state = 'active',
           command = v_parent_command,
           closed_at = NULL
     WHERE id = v_parent_id;

    UPDATE pitr_txn
       SET parent_id = v_parent_id
     WHERE id = ANY(COALESCE(v_sibling_ids, ARRAY[]::bigint[]));
END;
$$ LANGUAGE plpgsql;
