-- ============================================================
-- pitr-fs demo: 元数据 MVCC schema + 桥接触发器
-- 目标:验证 "在 JuiceFS 的 jfs_node/jfs_edge 上挂触发器 →
--        变更被捕获到 shadow 表 → 反向 replay 可以恢复"
-- ============================================================

-- 版本 / 事务表(demo 版,字段做了精简)
CREATE TABLE IF NOT EXISTS pitr_txn (
    id            bigserial PRIMARY KEY,
    version_hash  char(12)   UNIQUE NOT NULL,
    scope_path    text       NOT NULL,
    state         text       NOT NULL,            -- committed | rolled_back | root
    command       text,
    message       text,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS idx_pitr_txn_created ON pitr_txn (created_at);

-- inode 变更影子表
CREATE TABLE IF NOT EXISTS pitr_node_history (
    id           bigserial   PRIMARY KEY,
    inode        bigint      NOT NULL,
    op           char(1)     NOT NULL,           -- I=insert / U=update / D=delete
    snapshot     jsonb,                          -- 变更前的完整旧行(U/D 时)
    recorded_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS idx_pitr_node_history_time ON pitr_node_history (recorded_at);
CREATE INDEX IF NOT EXISTS idx_pitr_node_history_inode ON pitr_node_history (inode);

-- 目录项变更影子表
CREATE TABLE IF NOT EXISTS pitr_edge_history (
    id           bigserial   PRIMARY KEY,
    parent       bigint      NOT NULL,
    name         bytea       NOT NULL,           -- JuiceFS 用 bytea 存 name
    op           char(1)     NOT NULL,
    snapshot     jsonb,
    recorded_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS idx_pitr_edge_history_time ON pitr_edge_history (recorded_at);

-- chunk 变更影子表(jfs_chunk: 一个 inode 的第 N 个 64MB chunk 由哪些 slice 组成)
CREATE TABLE IF NOT EXISTS pitr_chunk_history (
    id           bigserial   PRIMARY KEY,
    chunk_id     bigint      NOT NULL,           -- jfs_chunk.id (主键)
    op           char(1)     NOT NULL,
    snapshot     jsonb,
    recorded_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS idx_pitr_chunk_history_time ON pitr_chunk_history (recorded_at);

-- chunk_ref 变更影子表(jfs_chunk_ref: slice → blob 的引用计数)
CREATE TABLE IF NOT EXISTS pitr_chunk_ref_history (
    id           bigserial   PRIMARY KEY,
    chunkid      bigint      NOT NULL,           -- jfs_chunk_ref.chunkid (主键)
    op           char(1)     NOT NULL,
    snapshot     jsonb,
    recorded_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS idx_pitr_chunk_ref_history_time ON pitr_chunk_ref_history (recorded_at);

-- 根版本
INSERT INTO pitr_txn (version_hash, scope_path, state, command)
VALUES ('000000000000', '/', 'root', 'init')
ON CONFLICT (version_hash) DO NOTHING;

-- ============================================================
-- 触发器:每次 jfs_node / jfs_edge 变更都无条件推进影子表
-- demo 版不依赖 session 变量归属(避免连接共享问题),
-- 通过 recorded_at 时间戳与 pitr_txn.created_at 关联版本。
-- 生产版本会用 pitr.current_txn GUC 精确归属。
-- ============================================================

CREATE OR REPLACE FUNCTION pitr_capture_node_change() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO pitr_node_history (inode, op, snapshot)
        VALUES (NEW.inode, 'I', NULL);
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        INSERT INTO pitr_node_history (inode, op, snapshot)
        VALUES (OLD.inode, 'U', to_jsonb(OLD));
        RETURN NEW;
    ELSE  -- DELETE
        INSERT INTO pitr_node_history (inode, op, snapshot)
        VALUES (OLD.inode, 'D', to_jsonb(OLD));
        RETURN OLD;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_capture_edge_change() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO pitr_edge_history (parent, name, op, snapshot)
        VALUES (NEW.parent, NEW.name, 'I', NULL);
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        INSERT INTO pitr_edge_history (parent, name, op, snapshot)
        VALUES (OLD.parent, OLD.name, 'U', to_jsonb(OLD));
        RETURN NEW;
    ELSE
        INSERT INTO pitr_edge_history (parent, name, op, snapshot)
        VALUES (OLD.parent, OLD.name, 'D', to_jsonb(OLD));
        RETURN OLD;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_capture_chunk_change() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO pitr_chunk_history (chunk_id, op, snapshot)
        VALUES (NEW.id, 'I', NULL);
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        INSERT INTO pitr_chunk_history (chunk_id, op, snapshot)
        VALUES (OLD.id, 'U', to_jsonb(OLD));
        RETURN NEW;
    ELSE
        INSERT INTO pitr_chunk_history (chunk_id, op, snapshot)
        VALUES (OLD.id, 'D', to_jsonb(OLD));
        RETURN OLD;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_capture_chunk_ref_change() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO pitr_chunk_ref_history (chunkid, op, snapshot)
        VALUES (NEW.chunkid, 'I', NULL);
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        INSERT INTO pitr_chunk_ref_history (chunkid, op, snapshot)
        VALUES (OLD.chunkid, 'U', to_jsonb(OLD));
        RETURN NEW;
    ELSE
        INSERT INTO pitr_chunk_ref_history (chunkid, op, snapshot)
        VALUES (OLD.chunkid, 'D', to_jsonb(OLD));
        RETURN OLD;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- 装到 JuiceFS 的表上(JuiceFS 必须先 format 建好这两张表)
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jfs_node') THEN
        DROP TRIGGER IF EXISTS tg_pitr_node ON jfs_node;
        CREATE TRIGGER tg_pitr_node
            AFTER INSERT OR UPDATE OR DELETE ON jfs_node
            FOR EACH ROW EXECUTE FUNCTION pitr_capture_node_change();
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jfs_edge') THEN
        DROP TRIGGER IF EXISTS tg_pitr_edge ON jfs_edge;
        CREATE TRIGGER tg_pitr_edge
            AFTER INSERT OR UPDATE OR DELETE ON jfs_edge
            FOR EACH ROW EXECUTE FUNCTION pitr_capture_edge_change();
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jfs_chunk') THEN
        DROP TRIGGER IF EXISTS tg_pitr_chunk ON jfs_chunk;
        CREATE TRIGGER tg_pitr_chunk
            AFTER INSERT OR UPDATE OR DELETE ON jfs_chunk
            FOR EACH ROW EXECUTE FUNCTION pitr_capture_chunk_change();
    END IF;

    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'jfs_chunk_ref') THEN
        DROP TRIGGER IF EXISTS tg_pitr_chunk_ref ON jfs_chunk_ref;
        CREATE TRIGGER tg_pitr_chunk_ref
            AFTER INSERT OR UPDATE OR DELETE ON jfs_chunk_ref
            FOR EACH ROW EXECUTE FUNCTION pitr_capture_chunk_ref_change();
    END IF;
END $$;
