-- ============================================================
-- pitr-fs demo: revert 函数
-- 输入:目标 version_hash
-- 语义:恢复到 "target 版本编辑完成后" 的状态。
--   v(target).created_at 是该版本编辑 *开始* 的时间戳,
--   下一个非 revert 版本 v(target+1).created_at 是该版本编辑 *结束* 的时间戳,
--   所以 cutoff = MIN(created_at) WHERE created_at > target_time AND NOT revert,
--   把 recorded_at >= cutoff 的所有 history 逆序 replay 掉即可。
-- ============================================================

CREATE OR REPLACE FUNCTION pitr_revert(target_hash char(12))
RETURNS text AS $$
DECLARE
    target_time   timestamptz;
    cutoff_time   timestamptz;
    node_rec      RECORD;
    edge_rec      RECORD;
    chunk_rec     RECORD;
    chunk_ref_rec RECORD;
    n_ops         int := 0;
BEGIN
    SELECT created_at INTO target_time
    FROM pitr_txn WHERE version_hash = target_hash;

    IF target_time IS NULL THEN
        RAISE EXCEPTION 'version % not found', target_hash;
    END IF;

    -- cutoff = 目标版本之后的第一个用户版本(排除 revert 元操作和 root)
    SELECT MIN(created_at) INTO cutoff_time
    FROM pitr_txn
    WHERE created_at > target_time
      AND state = 'committed'
      AND command NOT LIKE 'revert:%';

    IF cutoff_time IS NULL THEN
        RETURN format('nothing to revert: %s is already the latest user version', target_hash);
    END IF;

    -- 锁表防并发(demo 简化处理)
    LOCK TABLE jfs_node      IN EXCLUSIVE MODE;
    LOCK TABLE jfs_edge      IN EXCLUSIVE MODE;
    LOCK TABLE jfs_chunk     IN EXCLUSIVE MODE;
    LOCK TABLE jfs_chunk_ref IN EXCLUSIVE MODE;

    -- 关键:反向 replay 期间不再产生新 history,否则会污染
    -- 生产版本会用 SET session_replication_role = replica 完全关闭触发器,
    -- demo 里用一个临时表标记 + 触发器里判断
    ALTER TABLE jfs_node      DISABLE TRIGGER tg_pitr_node;
    ALTER TABLE jfs_edge      DISABLE TRIGGER tg_pitr_edge;
    ALTER TABLE jfs_chunk     DISABLE TRIGGER tg_pitr_chunk;
    ALTER TABLE jfs_chunk_ref DISABLE TRIGGER tg_pitr_chunk_ref;

    -- ---- jfs_edge 逆序 replay ----
    FOR edge_rec IN
        SELECT * FROM pitr_edge_history
        WHERE recorded_at >= cutoff_time
        ORDER BY id DESC
    LOOP
        n_ops := n_ops + 1;
        IF edge_rec.op = 'I' THEN
            DELETE FROM jfs_edge
            WHERE parent = edge_rec.parent AND name = edge_rec.name;
        ELSIF edge_rec.op = 'D' THEN
            INSERT INTO jfs_edge SELECT * FROM jsonb_populate_record(NULL::jfs_edge, edge_rec.snapshot);
        ELSE  -- U
            DELETE FROM jfs_edge
            WHERE parent = edge_rec.parent AND name = edge_rec.name;
            INSERT INTO jfs_edge SELECT * FROM jsonb_populate_record(NULL::jfs_edge, edge_rec.snapshot);
        END IF;
    END LOOP;

    -- ---- jfs_node 逆序 replay ----
    FOR node_rec IN
        SELECT * FROM pitr_node_history
        WHERE recorded_at >= cutoff_time
        ORDER BY id DESC
    LOOP
        n_ops := n_ops + 1;
        IF node_rec.op = 'I' THEN
            DELETE FROM jfs_node WHERE inode = node_rec.inode;
        ELSIF node_rec.op = 'D' THEN
            INSERT INTO jfs_node SELECT * FROM jsonb_populate_record(NULL::jfs_node, node_rec.snapshot);
        ELSE
            DELETE FROM jfs_node WHERE inode = node_rec.inode;
            INSERT INTO jfs_node SELECT * FROM jsonb_populate_record(NULL::jfs_node, node_rec.snapshot);
        END IF;
    END LOOP;

    -- ---- jfs_chunk 逆序 replay(内容层核心:恢复 inode 的 slice 引用) ----
    FOR chunk_rec IN
        SELECT * FROM pitr_chunk_history
        WHERE recorded_at >= cutoff_time
        ORDER BY id DESC
    LOOP
        n_ops := n_ops + 1;
        IF chunk_rec.op = 'I' THEN
            DELETE FROM jfs_chunk WHERE id = chunk_rec.chunk_id;
        ELSIF chunk_rec.op = 'D' THEN
            INSERT INTO jfs_chunk SELECT * FROM jsonb_populate_record(NULL::jfs_chunk, chunk_rec.snapshot);
        ELSE
            DELETE FROM jfs_chunk WHERE id = chunk_rec.chunk_id;
            INSERT INTO jfs_chunk SELECT * FROM jsonb_populate_record(NULL::jfs_chunk, chunk_rec.snapshot);
        END IF;
    END LOOP;

    -- ---- jfs_chunk_ref 逆序 replay(内容层核心:恢复 slice → blob 引用计数) ----
    FOR chunk_ref_rec IN
        SELECT * FROM pitr_chunk_ref_history
        WHERE recorded_at >= cutoff_time
        ORDER BY id DESC
    LOOP
        n_ops := n_ops + 1;
        IF chunk_ref_rec.op = 'I' THEN
            DELETE FROM jfs_chunk_ref WHERE chunkid = chunk_ref_rec.chunkid;
        ELSIF chunk_ref_rec.op = 'D' THEN
            INSERT INTO jfs_chunk_ref SELECT * FROM jsonb_populate_record(NULL::jfs_chunk_ref, chunk_ref_rec.snapshot);
        ELSE
            DELETE FROM jfs_chunk_ref WHERE chunkid = chunk_ref_rec.chunkid;
            INSERT INTO jfs_chunk_ref SELECT * FROM jsonb_populate_record(NULL::jfs_chunk_ref, chunk_ref_rec.snapshot);
        END IF;
    END LOOP;

    ALTER TABLE jfs_node      ENABLE TRIGGER tg_pitr_node;
    ALTER TABLE jfs_edge      ENABLE TRIGGER tg_pitr_edge;
    ALTER TABLE jfs_chunk     ENABLE TRIGGER tg_pitr_chunk;
    ALTER TABLE jfs_chunk_ref ENABLE TRIGGER tg_pitr_chunk_ref;

    -- 记录本次 revert 事件
    INSERT INTO pitr_txn (version_hash, scope_path, state, command, message)
    VALUES (
        substr(md5(clock_timestamp()::text), 1, 12),
        '/', 'committed',
        format('revert:%s', target_hash),
        format('revert to %s (%s ops)', target_hash, n_ops)
    );

    RETURN format('reverted to %s, applied %s history rows', target_hash, n_ops);
END;
$$ LANGUAGE plpgsql;
