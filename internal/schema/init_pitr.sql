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
    posix_op      text,
    process_command text,
    actor_uid     bigint,
    actor_gid     bigint,
    actor_pid     bigint,
    actor_name    text,
    change_summary text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    closed_at     timestamptz
);

ALTER TABLE pitr_txn ADD COLUMN IF NOT EXISTS posix_op text;
ALTER TABLE pitr_txn ADD COLUMN IF NOT EXISTS process_command text;
ALTER TABLE pitr_txn ADD COLUMN IF NOT EXISTS actor_uid bigint;
ALTER TABLE pitr_txn ADD COLUMN IF NOT EXISTS actor_gid bigint;
ALTER TABLE pitr_txn ADD COLUMN IF NOT EXISTS actor_pid bigint;
ALTER TABLE pitr_txn ADD COLUMN IF NOT EXISTS actor_name text;
ALTER TABLE pitr_txn ADD COLUMN IF NOT EXISTS change_summary text;

CREATE INDEX IF NOT EXISTS idx_pitr_txn_scope_state ON pitr_txn (scope_path, state);
CREATE INDEX IF NOT EXISTS idx_pitr_txn_created     ON pitr_txn (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_pitr_txn_closed
    ON pitr_txn (closed_at DESC, id DESC)
    WHERE closed_at IS NOT NULL;
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

-- 历史 slice 的物理 pin。pitr_slice_pin 使用 JuiceFS 自身 delslices 的紧凑
-- 编码(id:uint64 + size:uint32,每项 12 B),避免为每个 block 建一行。
-- pitr_slice_ref 是按 slice 聚合的 pin 计数,用于在回放 jfs_chunk_ref 时把
-- JuiceFS 的逻辑引用与 PITR 历史引用分开计算。
CREATE TABLE IF NOT EXISTS pitr_slice_pin (
    txn_id       bigint PRIMARY KEY REFERENCES pitr_txn(id) ON DELETE CASCADE,
    delayed_id   bigint UNIQUE NOT NULL,
    slices       bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- squash 释放并重建同一个 end 版本时，旧 delslices bundle 需要迁移到
-- 独立 ID，才能立即用规范的 8e18+txn_id 重新建立历史 pin。单独序列保证
-- 同一个 end 被多次扩大范围 squash 时也不会碰撞。
CREATE SEQUENCE IF NOT EXISTS pitr_gc_bundle_id_seq AS bigint
    START WITH 7000000000000000000
    MINVALUE 7000000000000000000
    MAXVALUE 7999999999999999999
    NO CYCLE;

CREATE TABLE IF NOT EXISTS pitr_slice_ref (
    chunkid      bigint PRIMARY KEY,
    size         integer NOT NULL CHECK (size > 0),
    pins         bigint NOT NULL CHECK (pins > 0)
);

-- 多次版本淘汰合并成一个 GC 请求,由 daemon 低频批量处理。队列落库可保证
-- daemon 崩溃/重启后不会漏掉对象回收。
CREATE TABLE IF NOT EXISTS pitr_gc_queue (
    singleton    boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    requested_at timestamptz NOT NULL DEFAULT now(),
    attempts     bigint NOT NULL DEFAULT 0,
    estimated_bytes bigint NOT NULL DEFAULT 0 CHECK (estimated_bytes >= 0),
    last_error   text
);
ALTER TABLE pitr_gc_queue
    ADD COLUMN IF NOT EXISTS estimated_bytes bigint NOT NULL DEFAULT 0;

-- 前台版本关闭只执行有上限的裁剪，其余工作由 daemon 分批继续。单例队列
-- 只表示“仍需按当前配置重算”，避免为每个候选版本复制一份任务状态。
CREATE TABLE IF NOT EXISTS pitr_prune_queue (
    singleton    boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    requested_at timestamptz NOT NULL DEFAULT now(),
    attempts     bigint NOT NULL DEFAULT 0,
    last_error   text
);

-- 数据库内的 schema 账本是升级判断的权威来源。init_pitr.sql 只负责确保
-- 表结构存在；部署器在同一个 psql --single-transaction 中写入本次逻辑
-- 版本、摘要和兼容边界，避免数据库恢复后被宿主机旧摘要误判为已校准。
CREATE TABLE IF NOT EXISTS pitr_schema_state (
    singleton           boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    schema_revision     bigint NOT NULL CHECK (schema_revision > 0),
    min_logic_revision  bigint NOT NULL CHECK (min_logic_revision > 0),
    digest              text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    logic_version       text NOT NULL,
    applied_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (min_logic_revision <= schema_revision)
);

CREATE TABLE IF NOT EXISTS pitr_internal_state (
    key          text PRIMARY KEY,
    value        text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- slice 索引升级水位。时间用于判断“索引后发生过版本清理”，txn id 用作
-- 无歧义的版本水位（避免相同时间戳精度导致漏建）。
CREATE TABLE IF NOT EXISTS pitr_slice_index_state (
    singleton               boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    indexed_at              timestamptz NOT NULL,
    indexed_through_txn_id  bigint NOT NULL DEFAULT 0,
    last_version_cleanup_at timestamptz
);

-- 用户视角的近似空间计数。retained 是当前文件和可恢复版本仍需保留的
-- 唯一 slice 字节，reclaimable 是 refs 已归零、等待原生 GC 的 slice 字节。
CREATE TABLE IF NOT EXISTS pitr_space_state (
    singleton        boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    retained_bytes   bigint NOT NULL DEFAULT 0 CHECK (retained_bytes >= 0),
    reclaimable_bytes bigint NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
    accounted_at     timestamptz NOT NULL DEFAULT now()
);

-- 持久化分层配置。当前控制面只允许写全局 '/'，查询按最长路径前缀解析，
-- 为后续“子目录继承父目录、就近配置优先”预留数据模型。
CREATE TABLE IF NOT EXISTS pitr_config (
    scope_path      text PRIMARY KEY,
    history_limit   bigint NOT NULL CHECK (history_limit = -1 OR history_limit > 0),
    max_space_bytes bigint NOT NULL DEFAULT 0 CHECK (max_space_bytes >= 0),
    space_reserve_percent integer NOT NULL DEFAULT 20
        CHECK (space_reserve_percent BETWEEN 1 AND 99),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE pitr_config
    ADD COLUMN IF NOT EXISTS max_space_bytes bigint NOT NULL DEFAULT 0;
ALTER TABLE pitr_config
    ADD COLUMN IF NOT EXISTS space_reserve_percent integer NOT NULL DEFAULT 20;
ALTER TABLE pitr_config
    ALTER COLUMN history_limit TYPE bigint;
-- 旧版本只允许正整数；-1 现在表示不按版本数量裁剪。空间水位策略仍生效。
ALTER TABLE pitr_config
    DROP CONSTRAINT IF EXISTS pitr_config_history_limit_check;
ALTER TABLE pitr_config
    ADD CONSTRAINT pitr_config_history_limit_check
    CHECK (history_limit = -1 OR history_limit > 0);

-- 卷挂载配置。当前服务只管理一个全局卷；单独建表可让 daemon 在重启后
-- 恢复由 `pitr init <path>` 选定的用户挂载点。
CREATE TABLE IF NOT EXISTS pitr_volume_config (
    volume_name    text PRIMARY KEY,
    fuse_mount     text NOT NULL,
    -- 仅用于兼容旧 schema；运行时不再读取或更新。
    retention      text NOT NULL DEFAULT 'compact',
    updated_at     timestamptz NOT NULL DEFAULT now()
);

INSERT INTO pitr_config (scope_path, history_limit)
VALUES ('/', 100)
ON CONFLICT (scope_path) DO NOTHING;

-- ---------- 根版本 ----------
--
-- id=1 通常预留给 root。用 ON CONFLICT (version_hash) DO NOTHING 保证幂等。
INSERT INTO pitr_txn (version_hash, scope_path, state, command)
VALUES ('000000000000', '/', 'root', 'init')
ON CONFLICT (version_hash) DO NOTHING;

-- 自动版本模式升级迁移：旧版本可能遗留 active 手工事务。保留其版本节点
-- 和子 auto 历史，但将它关闭为普通自动版本，避免永久阻塞 revert/clear。
UPDATE pitr_txn
   SET state='auto',
       command='migration:legacy-active',
       closed_at=COALESCE(closed_at, now())
 WHERE state='active';

-- ---------- 4.2 桥接触发器函数 ----------
--
-- 通用归属:
--   1. 优先读 GUC pitr.current_txn,供同连接调用与测试使用;
--   2. GUC 为空时,归到最近的、尚未关闭的 auto 版本。Phase 3 的代理层保证
--      单 daemon 内一次只开放一个 auto 窗口,JuiceFS 独立进程的写事务便可
--      通过这个窗口获得稳定 txn_id;
--   3. 固定 JuiceFS 补丁在 Compaction 事务设置 pitr.internal_op=compact，
--      即使此时存在开放 auto 窗口也不捕获，避免版本回退撤销物理压缩；
--   4. 以上归属都不存在时视为其他 JuiceFS 内部操作,不记录历史。

CREATE OR REPLACE FUNCTION pitr_current_txn() RETURNS bigint AS $$
DECLARE
    v_txn text := current_setting('pitr.current_txn', true);
BEGIN
    IF current_setting('pitr.suppress_capture', true) = 'on' THEN
        RETURN NULL;
    END IF;
    IF current_setting('pitr.internal_op', true) = 'compact' THEN
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

-- JuiceFS slice 编码使用大端整数。chunk.slices 每项 24 B:
-- pos(4)+id(8)+size(4)+off(4)+len(4);delslices 每项仅 id(8)+size(4)。
CREATE OR REPLACE FUNCTION pitr_decode_u64(p_value bytea) RETURNS bigint AS $$
    SELECT (
        get_byte(p_value,0)::numeric * 72057594037927936 +
        get_byte(p_value,1)::numeric * 281474976710656 +
        get_byte(p_value,2)::numeric * 1099511627776 +
        get_byte(p_value,3)::numeric * 4294967296 +
        get_byte(p_value,4)::numeric * 16777216 +
        get_byte(p_value,5)::numeric * 65536 +
        get_byte(p_value,6)::numeric * 256 +
        get_byte(p_value,7)::numeric
    )::bigint
$$ LANGUAGE sql IMMUTABLE STRICT;

CREATE OR REPLACE FUNCTION pitr_decode_u32(p_value bytea) RETURNS integer AS $$
    SELECT (
        get_byte(p_value,0)::bigint * 16777216 +
        get_byte(p_value,1)::bigint * 65536 +
        get_byte(p_value,2)::bigint * 256 +
        get_byte(p_value,3)::bigint
    )::integer
$$ LANGUAGE sql IMMUTABLE STRICT;

CREATE OR REPLACE FUNCTION pitr_pin_delayed_id(p_txn_id bigint)
RETURNS bigint AS $$
BEGIN
    IF p_txn_id < 0 OR p_txn_id > 1000000000000000000 THEN
        RAISE EXCEPTION 'txn id % 超出 slice pin 保留区', p_txn_id;
    END IF;
    RETURN 8000000000000000000 + p_txn_id;
END;
$$ LANGUAGE plpgsql IMMUTABLE STRICT;

-- 为一个首次捕获的旧 chunk 状态建立历史引用。只有 history INSERT 成功时
-- 才调用,所以同一版本内对同一 chunk 的重复更新不会重复 pin。
CREATE OR REPLACE FUNCTION pitr_pin_chunk_slices(
    p_txn_id bigint,
    p_slices bytea
) RETURNS void AS $$
DECLARE
    v_delayed      bytea;
    v_delayed_id   bigint := pitr_pin_delayed_id(p_txn_id);
    v_existing     bytea;
    v_old_suppress text := current_setting('pitr.suppress_capture', true);
BEGIN
    IF p_slices IS NULL OR length(p_slices) = 0 THEN
        RETURN;
    END IF;
    IF length(p_slices) % 24 <> 0 THEN
        RAISE EXCEPTION 'txn % 的 chunk slice 编码长度非法:%',
            p_txn_id, length(p_slices);
    END IF;

    PERFORM set_config('pitr.suppress_capture', 'on', true);
    -- 一次解码整个 bundle，并在 SQL 内按 slice 聚合。这样一个 chunk 的
    -- 覆盖链再长，也只对每个唯一 slice 更新一行引用计数。
    IF EXISTS (
        WITH decoded AS (
            SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
                   pitr_decode_u32(substring(piece FROM 9 FOR 4)) AS size
              FROM (
                  SELECT substring(p_slices FROM g.i*24+5 FOR 12) AS piece
                    FROM generate_series(0,length(p_slices)/24-1) AS g(i)
              ) pieces
        )
        SELECT 1 FROM decoded WHERE chunkid<>0 AND size<=0
    ) THEN
        RAISE EXCEPTION 'txn % 的 slice size 非法', p_txn_id;
    END IF;
    IF EXISTS (
        WITH decoded AS (
            SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
                   pitr_decode_u32(substring(piece FROM 9 FOR 4)) AS size
              FROM (
                  SELECT substring(p_slices FROM g.i*24+5 FOR 12) AS piece
                    FROM generate_series(0,length(p_slices)/24-1) AS g(i)
              ) pieces
        ), known_sizes AS (
            SELECT chunkid,size FROM decoded WHERE chunkid<>0
            UNION ALL
            SELECT r.chunkid,r.size FROM pitr_slice_ref r
             WHERE r.chunkid IN (SELECT chunkid FROM decoded WHERE chunkid<>0)
            UNION ALL
            SELECT r.chunkid,r.size FROM jfs_chunk_ref r
             WHERE r.chunkid IN (SELECT chunkid FROM decoded WHERE chunkid<>0)
        )
        SELECT 1 FROM known_sizes GROUP BY chunkid
         HAVING count(DISTINCT size)>1
    ) THEN
        RAISE EXCEPTION 'txn % 的 slice size 发生冲突', p_txn_id;
    END IF;

    WITH decoded AS (
        SELECT g.i AS ordinal,
               substring(p_slices FROM g.i*24+5 FOR 12) AS piece
          FROM generate_series(0,length(p_slices)/24-1) AS g(i)
    )
    SELECT string_agg(piece,''::bytea ORDER BY ordinal) INTO v_delayed
      FROM decoded
     WHERE pitr_decode_u64(substring(piece FROM 1 FOR 8))<>0;

    WITH decoded AS (
        SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
               pitr_decode_u32(substring(piece FROM 9 FOR 4)) AS size
          FROM (
              SELECT substring(p_slices FROM g.i*24+5 FOR 12) AS piece
                FROM generate_series(0,length(p_slices)/24-1) AS g(i)
          ) pieces
    ), aggregated AS (
        SELECT chunkid,min(size)::integer AS size,count(*)::bigint AS refs
          FROM decoded WHERE chunkid<>0 GROUP BY chunkid
    )
    INSERT INTO pitr_slice_ref(chunkid,size,pins)
    SELECT chunkid,size,refs FROM aggregated
    ON CONFLICT (chunkid) DO UPDATE
       SET pins=pitr_slice_ref.pins+EXCLUDED.pins;

    -- 历史 pin 的来源是 jfs_chunk 快照。正常情况下 JuiceFS 引用行仍然
    -- 存在；旧版本曾在异步 unlink/revert 时把引用行降到 0 并提前清掉，
    -- 此处必须能够从权威快照自愈，而不是让后续 schema 升级永久失败。
    WITH decoded AS (
        SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
               pitr_decode_u32(substring(piece FROM 9 FOR 4)) AS size
          FROM (
              SELECT substring(p_slices FROM g.i*24+5 FOR 12) AS piece
                FROM generate_series(0,length(p_slices)/24-1) AS g(i)
          ) pieces
    ), aggregated AS (
        SELECT chunkid,min(size)::integer AS size,count(*)::integer AS refs
          FROM decoded WHERE chunkid<>0 GROUP BY chunkid
    )
    INSERT INTO jfs_chunk_ref(chunkid,size,refs)
    SELECT chunkid,size,refs FROM aggregated
    ON CONFLICT (chunkid) DO UPDATE
       SET refs=jfs_chunk_ref.refs+EXCLUDED.refs;
    PERFORM set_config('pitr.suppress_capture', COALESCE(v_old_suppress,''), true);

    IF v_delayed IS NULL OR length(v_delayed) = 0 THEN
        RETURN;
    END IF;
    SELECT slices INTO v_existing
      FROM pitr_slice_pin WHERE txn_id=p_txn_id FOR UPDATE;
    IF FOUND THEN
        UPDATE pitr_slice_pin SET slices=slices || v_delayed
         WHERE txn_id=p_txn_id;
        UPDATE jfs_delslices SET slices=slices || v_delayed
         WHERE chunkid=v_delayed_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'txn % 的 JuiceFS pin 行丢失', p_txn_id;
        END IF;
    ELSE
        IF EXISTS (SELECT 1 FROM jfs_delslices WHERE chunkid=v_delayed_id) THEN
            RAISE EXCEPTION 'slice pin 保留 id % 已被占用', v_delayed_id;
        END IF;
        INSERT INTO pitr_slice_pin(txn_id,delayed_id,slices)
        VALUES (p_txn_id,v_delayed_id,v_delayed);
        -- year 9999。该行只由 PITR 版本释放触发器删除,不会被 JuiceFS
        -- trash 超时清理;官方 gc 会把其中 slice 视为可达。
        INSERT INTO jfs_delslices(chunkid,deleted,slices)
        VALUES (v_delayed_id,253402300799,v_delayed);
    END IF;
EXCEPTION WHEN OTHERS THEN
    PERFORM set_config('pitr.suppress_capture', COALESCE(v_old_suppress,''), true);
    RAISE;
END;
$$ LANGUAGE plpgsql;

-- pitr_chunk_ref_history 存 JuiceFS 的逻辑 refs,不把历史 pin 数写入快照。
-- replay 时再加上“此刻仍保留”的 pin,从而不因时间旅行覆盖物理引用。
CREATE OR REPLACE FUNCTION pitr_logical_chunk_ref_snapshot(
    p_row jsonb,
    p_chunkid bigint
) RETURNS jsonb AS $$
DECLARE
    v_pins bigint := 0;
    v_refs bigint;
BEGIN
    SELECT pins INTO v_pins FROM pitr_slice_ref WHERE chunkid=p_chunkid;
    v_pins := COALESCE(v_pins,0);
    v_refs := (p_row->>'refs')::bigint - v_pins;
    IF v_refs < 0 THEN
        RAISE EXCEPTION 'slice % 逻辑 refs 为负数:% - %',
            p_chunkid, p_row->>'refs', v_pins;
    END IF;
    RETURN jsonb_set(p_row,'{refs}',to_jsonb(v_refs),false);
END;
$$ LANGUAGE plpgsql STABLE STRICT;

-- 以当前 jfs_chunk 中的 slice 出现次数和现存历史 pin 为权威，校准
-- jfs_chunk_ref。JuiceFS 的异步 unlink 可能在 auto 窗口关闭后才减少引用，
-- 这次变化没有版本归属；revert 直接恢复 chunk 行时也不会触发 JuiceFS 自身
-- 的引用维护，因此不能只依赖 chunk_ref history。
--
-- p_chunkids=NULL 表示全量校准（启动/升级重建）；传入数组时只校准受一次
-- revert 影响的 slice。返回值为兼容预留，当前固定为 0。
CREATE OR REPLACE FUNCTION pitr_reconcile_slice_refs(
    p_chunkids bigint[] DEFAULT NULL
) RETURNS bigint AS $$
DECLARE
    v_drift_count bigint := 0;
    v_old_suppress text := current_setting('pitr.suppress_capture', true);
BEGIN
    PERFORM set_config('pitr.suppress_capture', 'on', true);

    -- 同一 slice id 的 size 必须在当前 chunk、历史 pin 和 JuiceFS 引用行
    -- 三方保持稳定；否则无法判断对象真实边界，宁可中止而不覆盖元数据。
    IF EXISTS (
        WITH decoded AS (
            SELECT pitr_decode_u64(substring(c.slices FROM g.i*24+5 FOR 8)) AS chunkid,
                   pitr_decode_u32(substring(c.slices FROM g.i*24+13 FOR 4)) AS size
              FROM jfs_chunk c
              CROSS JOIN LATERAL generate_series(0,length(c.slices)/24-1) AS g(i)
        ), known_sizes AS (
            SELECT chunkid,size FROM decoded WHERE chunkid<>0
            UNION ALL
            SELECT chunkid,size FROM pitr_slice_ref
            UNION ALL
            SELECT chunkid,size FROM jfs_chunk_ref
        )
        SELECT 1 FROM known_sizes
         WHERE chunkid<>0
           AND (p_chunkids IS NULL OR chunkid=ANY(p_chunkids))
         GROUP BY chunkid HAVING count(DISTINCT size)>1
    ) THEN
        RAISE EXCEPTION '当前 chunk 中存在 size 冲突的 slice，无法校准引用';
    END IF;

    IF p_chunkids IS NULL THEN
        UPDATE jfs_chunk_ref SET refs=0;
    ELSE
        UPDATE jfs_chunk_ref SET refs=0 WHERE chunkid=ANY(p_chunkids);
    END IF;

    WITH decoded AS (
        SELECT pitr_decode_u64(substring(c.slices FROM g.i*24+5 FOR 8)) AS chunkid,
               pitr_decode_u32(substring(c.slices FROM g.i*24+13 FOR 4)) AS size
          FROM jfs_chunk c
          CROSS JOIN LATERAL generate_series(0,length(c.slices)/24-1) AS g(i)
    ), current_usage AS (
        SELECT chunkid,min(size)::integer AS size,count(*)::bigint AS refs
          FROM decoded
         WHERE chunkid<>0
           AND (p_chunkids IS NULL OR chunkid=ANY(p_chunkids))
         GROUP BY chunkid
    ), desired AS (
        SELECT COALESCE(c.chunkid,p.chunkid) AS chunkid,
               COALESCE(c.size,p.size) AS size,
               COALESCE(c.refs,0)+COALESCE(p.pins,0) AS refs
          FROM current_usage c
          FULL JOIN (
              SELECT chunkid,size,pins FROM pitr_slice_ref
               WHERE p_chunkids IS NULL OR chunkid=ANY(p_chunkids)
          ) p USING(chunkid)
    )
    INSERT INTO jfs_chunk_ref(chunkid,size,refs)
    SELECT chunkid,size,refs::integer FROM desired WHERE refs>0
    ON CONFLICT (chunkid) DO UPDATE
       SET refs=EXCLUDED.refs
     WHERE jfs_chunk_ref.size=EXCLUDED.size;

    PERFORM set_config('pitr.suppress_capture', COALESCE(v_old_suppress,''), true);
    RETURN v_drift_count;
EXCEPTION WHEN OTHERS THEN
    PERFORM set_config('pitr.suppress_capture', COALESCE(v_old_suppress,''), true);
    RAISE;
END;
$$ LANGUAGE plpgsql;

-- 从“当前 chunk + 现存 history”完整重建 slice 索引。旧版本可能因异步
-- unlink/revert 遗留引用漂移，不能通过 refs-pins 推导当前引用；先清除旧 pin，
-- 从当前 chunk 精确重算，再重建历史 pin。全部动作处于同一事务。
CREATE OR REPLACE FUNCTION pitr_rebuild_slice_index() RETURNS void AS $$
DECLARE
    v_pin               RECORD;
    v_history           RECORD;
    v_last_txn_id       bigint := 0;
    v_cleanup_at        timestamptz;
    v_legacy_drift      bigint := 0;
    v_old_suppress      text := current_setting('pitr.suppress_capture', true);
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('pitr-fs:versions'));
    PERFORM set_config('pitr.suppress_capture', 'on', true);

    SELECT count(*) INTO v_legacy_drift
      FROM pitr_slice_ref r LEFT JOIN jfs_chunk_ref j USING(chunkid)
     WHERE j.chunkid IS NULL OR j.size<>r.size OR j.refs<r.pins;

    FOR v_pin IN SELECT delayed_id FROM pitr_slice_pin FOR UPDATE LOOP
        DELETE FROM jfs_delslices WHERE chunkid=v_pin.delayed_id;
    END LOOP;
    DELETE FROM pitr_slice_pin;
    DELETE FROM pitr_slice_ref;

    -- 清掉历史 pin 后，先把 JuiceFS 引用恢复为“当前 chunk 出现次数”。这样
    -- 历史独占、引用行已丢失的 slice 也能由后续 pin 重建安全插回。
    PERFORM pitr_reconcile_slice_refs(NULL);

    FOR v_history IN
        SELECT h.txn_id,
               (jsonb_populate_record(NULL::jfs_chunk,h.snapshot)).slices AS slices
          FROM pitr_chunk_history h
         WHERE h.snapshot IS NOT NULL
         ORDER BY h.txn_id,h.inode,h.indx
    LOOP
        PERFORM pitr_pin_chunk_slices(v_history.txn_id,v_history.slices);
    END LOOP;
    IF v_legacy_drift>0 THEN
        RAISE WARNING '检测到 % 个历史 slice 的旧引用计数漂移，已按当前文件和版本快照重建；修复前已被回收的数据块无法恢复，请先在副本验证较早版本回退',
            v_legacy_drift;
    END IF;
    SELECT COALESCE(max(id),0) INTO v_last_txn_id FROM pitr_txn;
    SELECT last_version_cleanup_at INTO v_cleanup_at
      FROM pitr_slice_index_state WHERE singleton;
    INSERT INTO pitr_slice_index_state(
        singleton,indexed_at,indexed_through_txn_id,last_version_cleanup_at
    ) VALUES (true,clock_timestamp(),v_last_txn_id,v_cleanup_at)
    ON CONFLICT (singleton) DO UPDATE
       SET indexed_at=EXCLUDED.indexed_at,
           indexed_through_txn_id=EXCLUDED.indexed_through_txn_id,
           last_version_cleanup_at=EXCLUDED.last_version_cleanup_at;
    PERFORM set_config('pitr.suppress_capture', COALESCE(v_old_suppress,''), true);
EXCEPTION WHEN OTHERS THEN
    PERFORM set_config('pitr.suppress_capture', COALESCE(v_old_suppress,''), true);
    RAISE;
END;
$$ LANGUAGE plpgsql;

-- jfs_chunk_ref 是空间估算的权威增量源。refs>0 表示当前状态或某个历史
-- 版本仍然需要该 slice；refs<=0 表示可由 JuiceFS GC 回收。
CREATE OR REPLACE FUNCTION pitr_track_chunk_ref_space() RETURNS TRIGGER AS $$
DECLARE
    v_inserted integer;
    v_retained_delta bigint := 0;
    v_reclaimable_delta bigint := 0;
BEGIN
    INSERT INTO pitr_space_state(
        singleton,retained_bytes,reclaimable_bytes,accounted_at
    )
    SELECT true,
           COALESCE(sum(size) FILTER (WHERE refs>0),0)::bigint,
           COALESCE(sum(size) FILTER (WHERE refs<=0),0)::bigint,
           clock_timestamp()
      FROM jfs_chunk_ref
    ON CONFLICT (singleton) DO NOTHING;
    GET DIAGNOSTICS v_inserted = ROW_COUNT;
    -- AFTER trigger 第一次初始化时，聚合已经包含本次变更，无需再加 delta。
    IF v_inserted = 1 THEN
        RETURN COALESCE(NEW,OLD);
    END IF;

    IF TG_OP <> 'INSERT' THEN
        IF OLD.refs > 0 THEN
            v_retained_delta := v_retained_delta-OLD.size;
        ELSE
            v_reclaimable_delta := v_reclaimable_delta-OLD.size;
        END IF;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        IF NEW.refs > 0 THEN
            v_retained_delta := v_retained_delta+NEW.size;
        ELSE
            v_reclaimable_delta := v_reclaimable_delta+NEW.size;
        END IF;
    END IF;
    UPDATE pitr_space_state
       SET retained_bytes=retained_bytes+v_retained_delta,
           reclaimable_bytes=reclaimable_bytes+v_reclaimable_delta,
           accounted_at=clock_timestamp()
     WHERE singleton;
    RETURN COALESCE(NEW,OLD);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_rebuild_space_state() RETURNS void AS $$
BEGIN
    INSERT INTO pitr_space_state(
        singleton,retained_bytes,reclaimable_bytes,accounted_at
    )
    SELECT true,
           COALESCE(sum(size) FILTER (WHERE refs>0),0)::bigint,
           COALESCE(sum(size) FILTER (WHERE refs<=0),0)::bigint,
           clock_timestamp()
      FROM jfs_chunk_ref
    ON CONFLICT (singleton) DO UPDATE
       SET retained_bytes=EXCLUDED.retained_bytes,
           reclaimable_bytes=EXCLUDED.reclaimable_bytes,
           accounted_at=EXCLUDED.accounted_at;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_release_version_pins() RETURNS TRIGGER AS $$
DECLARE
    v_pin          RECORD;
    v_reconcile_ids bigint[];
    v_release_bytes bigint := 0;
    v_old_suppress text := current_setting('pitr.suppress_capture', true);
BEGIN
    INSERT INTO pitr_slice_index_state(
        singleton,indexed_at,indexed_through_txn_id,last_version_cleanup_at
    ) VALUES (true,'epoch',0,clock_timestamp())
    ON CONFLICT (singleton) DO UPDATE
       SET last_version_cleanup_at=EXCLUDED.last_version_cleanup_at;
    SELECT delayed_id,slices INTO v_pin
      FROM pitr_slice_pin WHERE txn_id=OLD.id FOR UPDATE;
    IF NOT FOUND THEN
        RETURN OLD;
    END IF;
    IF length(v_pin.slices) % 12 <> 0 THEN
        RAISE EXCEPTION 'txn % 的 slice pin 编码损坏', OLD.id;
    END IF;
    PERFORM set_config('pitr.suppress_capture', 'on', true);
    -- 先验证整个 bundle，任何损坏都在修改引用前原子失败。
    IF EXISTS (
        WITH decoded AS (
            SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
                   pitr_decode_u32(substring(piece FROM 9 FOR 4)) AS size
              FROM (
                  SELECT substring(v_pin.slices FROM g.i*12+1 FOR 12) AS piece
                    FROM generate_series(0,length(v_pin.slices)/12-1) AS g(i)
              ) pieces
        ), releasing AS (
            SELECT chunkid,min(size)::integer AS size,count(*)::bigint AS refs
              FROM decoded GROUP BY chunkid
        )
        SELECT 1 FROM releasing x
          LEFT JOIN pitr_slice_ref r USING(chunkid)
         WHERE r.chunkid IS NULL OR r.size<>x.size OR r.pins<x.refs
    ) THEN
        RAISE EXCEPTION '释放 txn % 时 slice pin 丢失或计数不足', OLD.id;
    END IF;

    -- 旧版本可能存在 refs<pins 或 JuiceFS 引用行丢失。记录异常集合，先
    -- 释放 PITR pin，再只对这些 slice 从当前 chunk+剩余 pin 精确重算。
    WITH decoded AS (
        SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
               pitr_decode_u32(substring(piece FROM 9 FOR 4)) AS size
          FROM (
              SELECT substring(v_pin.slices FROM g.i*12+1 FOR 12) AS piece
                FROM generate_series(0,length(v_pin.slices)/12-1) AS g(i)
          ) pieces
    ), releasing AS (
        SELECT chunkid,min(size)::integer AS size,count(*)::bigint AS refs
          FROM decoded GROUP BY chunkid
    )
    SELECT array_agg(x.chunkid ORDER BY x.chunkid) INTO v_reconcile_ids
      FROM releasing x
      JOIN pitr_slice_ref p USING(chunkid)
      LEFT JOIN jfs_chunk_ref j USING(chunkid)
     WHERE j.chunkid IS NULL OR j.size<>x.size OR j.refs<p.pins;

    WITH decoded AS (
        SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
               count(*)::bigint AS refs
          FROM (
              SELECT substring(v_pin.slices FROM g.i*12+1 FOR 12) AS piece
                FROM generate_series(0,length(v_pin.slices)/12-1) AS g(i)
          ) pieces
         GROUP BY pitr_decode_u64(substring(piece FROM 1 FOR 8))
    )
    DELETE FROM pitr_slice_ref r USING decoded x
     WHERE r.chunkid=x.chunkid AND r.pins=x.refs;

    WITH decoded AS (
        SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
               count(*)::bigint AS refs
          FROM (
              SELECT substring(v_pin.slices FROM g.i*12+1 FOR 12) AS piece
                FROM generate_series(0,length(v_pin.slices)/12-1) AS g(i)
          ) pieces
         GROUP BY pitr_decode_u64(substring(piece FROM 1 FOR 8))
    )
    UPDATE pitr_slice_ref r SET pins=r.pins-x.refs
      FROM decoded x
     WHERE r.chunkid=x.chunkid AND r.pins>x.refs;

    WITH decoded AS (
        SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
               count(*)::integer AS refs
          FROM (
              SELECT substring(v_pin.slices FROM g.i*12+1 FOR 12) AS piece
                FROM generate_series(0,length(v_pin.slices)/12-1) AS g(i)
          ) pieces
         GROUP BY pitr_decode_u64(substring(piece FROM 1 FOR 8))
    )
    UPDATE jfs_chunk_ref j SET refs=j.refs-x.refs
      FROM decoded x
     WHERE j.chunkid=x.chunkid
       AND (v_reconcile_ids IS NULL OR NOT j.chunkid=ANY(v_reconcile_ids));

    IF v_reconcile_ids IS NOT NULL THEN
        PERFORM pitr_reconcile_slice_refs(v_reconcile_ids);
    END IF;

    WITH decoded AS (
        SELECT pitr_decode_u64(substring(piece FROM 1 FOR 8)) AS chunkid,
               pitr_decode_u32(substring(piece FROM 9 FOR 4)) AS size
          FROM (
              SELECT substring(v_pin.slices FROM g.i*12+1 FOR 12) AS piece
                FROM generate_series(0,length(v_pin.slices)/12-1) AS g(i)
          ) pieces
    ), released AS (
        SELECT chunkid,min(size)::integer AS size FROM decoded GROUP BY chunkid
    )
    SELECT COALESCE(sum(x.size),0)::bigint INTO v_release_bytes
      FROM released x LEFT JOIN jfs_chunk_ref j USING(chunkid)
     WHERE COALESCE(j.refs,0)=0;
    -- pin 释放后不能直接丢掉 delslices 索引，否则 refs=0 的 slice 可能不再
    -- 出现在 JuiceFS delete 扫描中。把远期 pin 改成立即到期的待删记录；
    -- 仍被当前数据或其他版本引用的 slice 会由 JuiceFS 引用检查跳过。
    UPDATE jfs_delslices
       SET deleted=extract(epoch FROM clock_timestamp())::bigint
     WHERE chunkid=v_pin.delayed_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION '释放 txn % 时 JuiceFS pin 行丢失', OLD.id;
    END IF;
    PERFORM set_config('pitr.suppress_capture', COALESCE(v_old_suppress,''), true);
    INSERT INTO pitr_gc_queue(singleton,requested_at,estimated_bytes)
    VALUES (true,now(),v_release_bytes)
    ON CONFLICT (singleton) DO UPDATE
       SET requested_at=EXCLUDED.requested_at,
           estimated_bytes=pitr_gc_queue.estimated_bytes+EXCLUDED.estimated_bytes;
    RETURN OLD;
EXCEPTION WHEN OTHERS THEN
    PERFORM set_config('pitr.suppress_capture', COALESCE(v_old_suppress,''), true);
    RAISE;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS tg_pitr_release_version_pins ON pitr_txn;
CREATE TRIGGER tg_pitr_release_version_pins
    BEFORE DELETE ON pitr_txn
    FOR EACH ROW EXECUTE FUNCTION pitr_release_version_pins();

CREATE OR REPLACE FUNCTION pitr_capture_node_change() RETURNS TRIGGER AS $$
DECLARE
    v_txn bigint := pitr_current_txn();
    v_chunk RECORD;
    v_inserted integer;
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
    -- JuiceFS 对 unlink 的 chunk 清理可能同步执行，也可能交给后台异步执行。
    -- 不能依赖 jfs_chunk DELETE trigger：异步清理发生在 auto 窗口关闭后，
    -- 已经没有可归属的版本。node DELETE 时主动快照仍存在的全部 chunk，
    -- 同步路径已记录的行由唯一键去重；这样恢复文件长度时不会得到稀疏空洞。
    IF TG_OP = 'DELETE' THEN
        FOR v_chunk IN
            SELECT c.inode,c.indx,c.slices,to_jsonb(c) AS snapshot
              FROM jfs_chunk c
             WHERE c.inode=OLD.inode
             ORDER BY c.indx
        LOOP
            INSERT INTO pitr_chunk_history(txn_id,inode,indx,op,snapshot)
            VALUES (v_txn,v_chunk.inode,v_chunk.indx,'D',v_chunk.snapshot)
            ON CONFLICT (txn_id,inode,indx) DO NOTHING;
            GET DIAGNOSTICS v_inserted = ROW_COUNT;
            IF v_inserted = 1 AND v_chunk.slices IS NOT NULL THEN
                PERFORM pitr_pin_chunk_slices(v_txn,v_chunk.slices);
            END IF;
        END LOOP;
    END IF;
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
        CASE WHEN TG_OP = 'INSERT' THEN NULL
             ELSE pitr_logical_chunk_ref_snapshot(to_jsonb(OLD),OLD.chunkid)
        END
    )
    ON CONFLICT (txn_id, chunkid) DO NOTHING;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION pitr_capture_chunk_change() RETURNS TRIGGER AS $$
DECLARE
    v_txn bigint := pitr_current_txn();
    v_inserted integer;
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
    GET DIAGNOSTICS v_inserted = ROW_COUNT;
    IF v_inserted = 1 AND TG_OP <> 'INSERT' AND OLD.slices IS NOT NULL THEN
        PERFORM pitr_pin_chunk_slices(v_txn,OLD.slices);
    END IF;
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
        DROP TRIGGER IF EXISTS tg_pitr_chunk_ref_space ON jfs_chunk_ref;
        CREATE TRIGGER tg_pitr_chunk_ref_space
            AFTER INSERT OR UPDATE OR DELETE ON jfs_chunk_ref
            FOR EACH ROW EXECUTE FUNCTION pitr_track_chunk_ref_space();
    END IF;
END $$;

-- 升级/重启校准：首次建立索引、出现更晚版本，或索引后发生过版本清理时
-- 全量重建。清理会移除历史行，不能只追加；txn id 水位避免时间戳碰撞漏建。
DO $$
DECLARE
    v_state           RECORD;
    v_last_txn_id     bigint := 0;
    v_last_version_at timestamptz;
    v_rebuild         boolean := false;
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='jfs_chunk')
       AND EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='jfs_chunk_ref')
       AND EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='jfs_delslices') THEN
        SELECT * INTO v_state FROM pitr_slice_index_state WHERE singleton;
        SELECT COALESCE(max(id),0),
               max(GREATEST(created_at,COALESCE(closed_at,created_at)))
          INTO v_last_txn_id,v_last_version_at
          FROM pitr_txn;
        IF NOT FOUND OR v_state.indexed_at IS NULL THEN
            v_rebuild := true;
        ELSE
            v_rebuild := v_state.indexed_through_txn_id < v_last_txn_id
                OR (v_last_version_at IS NOT NULL
                    AND v_state.indexed_at < v_last_version_at)
                OR (v_state.last_version_cleanup_at IS NOT NULL
                    AND v_state.indexed_at < v_state.last_version_cleanup_at);
        END IF;
        IF v_rebuild THEN
            PERFORM pitr_rebuild_slice_index();
        END IF;
    END IF;
END $$;

-- 初始化/升级后做一次全量校准；正常运行期间由 jfs_chunk_ref trigger 增量维护，
-- 不扫描对象存储，也不在每次 write 上聚合整表。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='jfs_chunk_ref') THEN
        PERFORM pitr_rebuild_space_state();
    END IF;
END $$;

-- ============================================================================
-- 6.1 commit 坍缩存储过程
--
-- 语义:把 commit_id 下所有 auto 子事务的 history 折叠到 commit_id,
-- 每个 (table, key) 只保留最早的一份 snapshot(begin 前的原值)。
-- 完成后中间的 auto 事务被删掉(state='auto' + parent_id=commit_id)。
-- ============================================================================

-- commit 坍缩只改变版本归属,不能释放物理 pin。把 auto 的紧凑 slice 缓冲
-- 合并到 commit 行,全局 pitr_slice_ref/jfs_chunk_ref 计数保持不变。
CREATE OR REPLACE FUNCTION pitr_move_slice_pins(
    p_from_ids bigint[],
    p_to_id bigint
) RETURNS void AS $$
DECLARE
    v_source      RECORD;
    v_target_slices bytea;
    v_target_id   bigint := pitr_pin_delayed_id(p_to_id);
BEGIN
    SELECT slices INTO v_target_slices
      FROM pitr_slice_pin WHERE txn_id=p_to_id FOR UPDATE;
    IF NOT FOUND AND EXISTS (
        SELECT 1 FROM jfs_delslices WHERE chunkid=v_target_id
    ) THEN
        RAISE EXCEPTION 'commit slice pin 保留 id % 已被占用', v_target_id;
    END IF;

    FOR v_source IN
        SELECT txn_id,delayed_id,slices
          FROM pitr_slice_pin
         WHERE txn_id=ANY(p_from_ids)
         ORDER BY txn_id
         FOR UPDATE
    LOOP
        IF v_target_slices IS NULL THEN
            INSERT INTO pitr_slice_pin(txn_id,delayed_id,slices)
            VALUES (p_to_id,v_target_id,v_source.slices);
            INSERT INTO jfs_delslices(chunkid,deleted,slices)
            VALUES (v_target_id,253402300799,v_source.slices);
            v_target_slices := v_source.slices;
        ELSE
            UPDATE pitr_slice_pin SET slices=slices || v_source.slices
             WHERE txn_id=p_to_id;
            UPDATE jfs_delslices SET slices=slices || v_source.slices
             WHERE chunkid=v_target_id;
            v_target_slices := v_target_slices || v_source.slices;
        END IF;
        DELETE FROM jfs_delslices WHERE chunkid=v_source.delayed_id;
        DELETE FROM pitr_slice_pin WHERE txn_id=v_source.txn_id;
    END LOOP;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE PROCEDURE pitr_collapse_commit(p_commit_id bigint) AS $$
DECLARE
    v_auto_ids bigint[];
BEGIN
    SELECT COALESCE(array_agg(id),ARRAY[]::bigint[]) INTO v_auto_ids
      FROM pitr_txn WHERE parent_id=p_commit_id AND state='auto';
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

    PERFORM pitr_move_slice_pins(v_auto_ids,p_commit_id);

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

-- Phase 1/3 的旧签名只有两个参数。PostgreSQL 的 OR REPLACE 不会在签名变化
-- 时删除旧 overload,升级前先显式移除,避免两参数 CALL 解析歧义。
DROP PROCEDURE IF EXISTS pitr_revert(character, text);
DROP PROCEDURE IF EXISTS pitr_revert(character, text, bigint);

CREATE OR REPLACE PROCEDURE pitr_revert(
    p_target_hash char(12),
    p_scope_path  text DEFAULT NULL,
    p_capture_txn_id bigint DEFAULT NULL,
    p_scope_inodes bigint[] DEFAULT NULL
) AS $$
DECLARE
    v_target_txn_id  bigint;
    r_node RECORD;
    r_edge RECORD;
    r_chunk RECORD;
    r_chunk_ref RECORD;
    v_pin_count bigint;
    v_pin_size integer;
    v_current_slices bytea;
    v_snapshot_slices bytea;
    v_loop_chunkids bigint[];
    v_affected_chunkids bigint[] := ARRAY[]::bigint[];
BEGIN
    SELECT id INTO v_target_txn_id
      FROM pitr_txn WHERE version_hash = p_target_hash;
    IF v_target_txn_id IS NULL THEN
        RAISE EXCEPTION 'pitr_revert: version_hash % 不存在', p_target_hash;
    END IF;

    -- Engine 会预先插入一条 revert txn 并传入 id。replay 对主表造成的变化
    -- 由 trigger 捕获到这条 txn,使本次 revert 自身也可被后续 revert 撤销。
    -- 直接调用旧的两参数形式时仍保持历史行为:抑制捕获。
    IF p_capture_txn_id IS NULL THEN
        PERFORM set_config('pitr.suppress_capture', 'on', true);
    ELSE
        PERFORM set_config('pitr.current_txn', p_capture_txn_id::text, true);
    END IF;

    -- 与 JuiceFS 元数据写互斥。并发写会在事务结束前阻塞,不会观察半完成状态。
    LOCK TABLE jfs_node, jfs_edge, jfs_chunk, jfs_chunk_ref
        IN EXCLUSIVE MODE;

    -- 反向 replay 全部 history。不能只取最新 snapshot:同一 inode 经历
    -- v1→v2→v3 时,必须依次恢复 v3 前、v2 前的状态才能回到 v1。
    FOR r_node IN
        SELECT inode, op, snapshot
        FROM   pitr_node_history nh
        JOIN   pitr_txn t ON t.id = nh.txn_id
        WHERE  t.id > v_target_txn_id
          AND  (p_scope_path IS NULL OR pitr_scopes_overlap(t.scope_path, p_scope_path))
          AND  (p_scope_inodes IS NULL OR nh.inode = ANY(p_scope_inodes))
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
          AND  (p_scope_inodes IS NULL
                OR eh.parent = ANY(p_scope_inodes)
                OR (eh.snapshot->>'inode')::bigint = ANY(p_scope_inodes))
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
          AND  (p_scope_inodes IS NULL OR ch.inode = ANY(p_scope_inodes))
        ORDER  BY ch.recorded_at DESC, ch.txn_id DESC
    LOOP
        SELECT slices INTO v_current_slices FROM jfs_chunk
         WHERE inode=r_chunk.inode AND indx=r_chunk.indx;
        IF r_chunk.snapshot IS NULL THEN
            v_snapshot_slices := NULL;
        ELSE
            v_snapshot_slices :=
                (jsonb_populate_record(NULL::jfs_chunk,r_chunk.snapshot)).slices;
        END IF;
        WITH encoded AS (
            SELECT v_current_slices AS slices
            UNION ALL
            SELECT v_snapshot_slices
        ), decoded AS (
            SELECT pitr_decode_u64(
                       substring(e.slices FROM g.i*24+5 FOR 8)) AS chunkid
              FROM encoded e
              CROSS JOIN LATERAL generate_series(0,length(e.slices)/24-1) AS g(i)
        )
        SELECT COALESCE(array_agg(DISTINCT chunkid)
                        FILTER (WHERE chunkid<>0),ARRAY[]::bigint[])
          INTO v_loop_chunkids FROM decoded;
        v_affected_chunkids := v_affected_chunkids || v_loop_chunkids;

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
          -- chunk_ref 没有 inode。只有当该 txn 的全部 chunk history 都在
          -- scope closure 内时才安全 replay;跨 scope 的 broad txn 宁可保留
          -- 额外引用(垃圾稍晚回收),也不能覆盖 scope 外文件的当前 refcount。
          AND  (p_scope_inodes IS NULL OR NOT EXISTS (
                   SELECT 1 FROM pitr_chunk_history scoped_chunk
                    WHERE scoped_chunk.txn_id = crh.txn_id
                      AND NOT (scoped_chunk.inode = ANY(p_scope_inodes))
               ))
        ORDER  BY crh.recorded_at DESC, crh.txn_id DESC
    LOOP
        SELECT pins,size INTO v_pin_count,v_pin_size
          FROM pitr_slice_ref WHERE chunkid=r_chunk_ref.chunkid;
        v_pin_count := COALESCE(v_pin_count,0);
        IF r_chunk_ref.op = 'I' THEN
            IF v_pin_count = 0 THEN
                DELETE FROM jfs_chunk_ref WHERE chunkid = r_chunk_ref.chunkid;
            ELSE
                INSERT INTO jfs_chunk_ref(chunkid,size,refs)
                VALUES (r_chunk_ref.chunkid,v_pin_size,v_pin_count)
                ON CONFLICT (chunkid) DO UPDATE
                   SET size=EXCLUDED.size,refs=EXCLUDED.refs;
            END IF;
        ELSIF r_chunk_ref.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_chunk_ref WHERE chunkid = r_chunk_ref.chunkid) THEN
                UPDATE jfs_chunk_ref
                   SET size=(r_chunk_ref.snapshot->>'size')::integer,
                       refs=(r_chunk_ref.snapshot->>'refs')::integer+v_pin_count
                WHERE chunkid = r_chunk_ref.chunkid;
            ELSE
                INSERT INTO jfs_chunk_ref(chunkid,size,refs)
                VALUES (r_chunk_ref.chunkid,
                        (r_chunk_ref.snapshot->>'size')::integer,
                        (r_chunk_ref.snapshot->>'refs')::integer+v_pin_count);
            END IF;
        END IF;
    END LOOP;

    -- chunk_ref history 可能缺少 auto 关闭后才发生的异步 unlink 减引用。
    -- 先保留原有 replay 兼容非 chunk 来源的引用变化，再对本次触及的 slice
    -- 以当前 chunk + 历史 pin 做精确收敛。
    SELECT COALESCE(array_agg(DISTINCT chunkid),ARRAY[]::bigint[])
      INTO v_affected_chunkids
      FROM unnest(v_affected_chunkids) AS affected(chunkid);
    PERFORM pitr_reconcile_slice_refs(v_affected_chunkids);
END;
$$ LANGUAGE plpgsql;

-- Engine 把目录闭包保存在连接级临时表中。用动态 SQL 读取 pg_temp，避免
-- procedure 创建时依赖尚不存在的临时 relation；聚合数组只在 PostgreSQL
-- 进程内部传给兼容的 pitr_revert 主过程，不经过 Go 客户端往返。
CREATE OR REPLACE PROCEDURE pitr_revert_from_temp(
    p_target_hash char(12),
    p_scope_path text,
    p_capture_txn_id bigint,
    p_filter_scope boolean
) AS $$
DECLARE
    v_scope_inodes bigint[];
BEGIN
    IF p_filter_scope THEN
        EXECUTE 'SELECT COALESCE(array_agg(inode),ARRAY[]::bigint[])
                   FROM pg_temp.pitr_revert_scope_inode'
           INTO v_scope_inodes;
    ELSE
        v_scope_inodes := NULL;
    END IF;
    CALL pitr_revert(
        p_target_hash,p_scope_path,p_capture_txn_id,v_scope_inodes);
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
    v_pin_count bigint;
    v_pin_size integer;
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
        SELECT pins,size INTO v_pin_count,v_pin_size
          FROM pitr_slice_ref WHERE chunkid=r_chunk_ref.chunkid;
        v_pin_count := COALESCE(v_pin_count,0);
        IF r_chunk_ref.op = 'I' THEN
            IF v_pin_count = 0 THEN
                DELETE FROM jfs_chunk_ref WHERE chunkid = r_chunk_ref.chunkid;
            ELSE
                INSERT INTO jfs_chunk_ref(chunkid,size,refs)
                VALUES (r_chunk_ref.chunkid,v_pin_size,v_pin_count)
                ON CONFLICT (chunkid) DO UPDATE
                   SET size=EXCLUDED.size,refs=EXCLUDED.refs;
            END IF;
        ELSIF r_chunk_ref.snapshot IS NOT NULL THEN
            IF EXISTS (SELECT 1 FROM jfs_chunk_ref
                        WHERE chunkid = r_chunk_ref.chunkid) THEN
                UPDATE jfs_chunk_ref
                   SET size=(r_chunk_ref.snapshot->>'size')::integer,
                       refs=(r_chunk_ref.snapshot->>'refs')::integer+v_pin_count
                 WHERE chunkid = r_chunk_ref.chunkid;
            ELSE
                INSERT INTO jfs_chunk_ref(chunkid,size,refs)
                VALUES (r_chunk_ref.chunkid,
                        (r_chunk_ref.snapshot->>'size')::integer,
                        (r_chunk_ref.snapshot->>'refs')::integer+v_pin_count);
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
    v_parent_hash    char(12);
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

    SELECT state, command, version_hash
      INTO v_parent_state, v_parent_command, v_parent_hash
      FROM pitr_txn
     WHERE id = v_parent_id
     FOR UPDATE;

    -- 自动快照模式下 auto 直接挂在最近的已关闭版本上。唯一开放窗口保证它
    -- 是当前 HEAD；失败时回到 parent 并删除该 auto 即可。
    IF v_parent_state <> 'active' THEN
        CALL pitr_revert(v_parent_hash, NULL, NULL, NULL);
        DELETE FROM pitr_txn WHERE id = p_auto_id;
        RETURN;
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
