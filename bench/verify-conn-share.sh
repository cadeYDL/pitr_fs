#!/usr/bin/env bash
# Task 0.3 POC:验证 pitrd 让 JuiceFS 共享同一 PG 连接、触发器读 GUC 的方案能否成立。
#
# 本脚本只验证 **PG 层机制**(GUC 语义 + MVCC 可见性),不启 JuiceFS。
# "JuiceFS Go SDK 能否暴露 sql.DB 供复用" 是独立调研,见 docs/P0-report.md。
#
# 4 个断言场景:
#   A. 同一连接、同一事务里 SET LOCAL 后 UPDATE  → 触发器读到 GUC        [核心正路]
#   B. 跨连接:X 里 SET LOCAL 后 commit,Y 里 UPDATE → 触发器读到 NULL     [头号风险]
#   C. 同连接但用 SET SESSION 而非 SET LOCAL,GUC 泄漏到下一个事务         [连接池陷阱]
#   D. 加了 FK 后, Y 在 X 未 commit 时引用 X 的 pitr_txn.id → FK 违反      [MVCC 可见性]
#
# 全部 PASS  = 生产版本方案在 PG 层是自洽的,剩下只看 JuiceFS SDK 能否复用连接
# B 里 GUC 是 NULL,而 A 是 42 = 定量证实"必须共享连接"的强需求

set -euo pipefail

CONTAINER="${CONTAINER:-pitr-verify03-pg}"
PORT="${PORT:-55444}"
DB=verify03
FAIL=0

PSQL="docker exec -i $CONTAINER psql -U postgres -d $DB -qtAX"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

assert_eq() {
    local name="$1" expected="$2" actual="$3"
    if [[ "$expected" == "$actual" ]]; then
        printf "  [PASS] %s (got %q)\n" "$name" "$actual"
    else
        printf "  [FAIL] %s (expected %q, got %q)\n" "$name" "$expected" "$actual"
        FAIL=1
    fi
}

# ============================================================
# 0. 起隔离 PG
# ============================================================
echo "==> 0/5 起隔离 PG"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$CONTAINER" \
    -e POSTGRES_PASSWORD=x -e POSTGRES_DB=postgres \
    -p 127.0.0.1:$PORT:5432 \
    postgres:16 >/dev/null

until docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1; do sleep 0.5; done
docker exec "$CONTAINER" psql -U postgres -c "CREATE DATABASE $DB;" >/dev/null

# ============================================================
# 1. 装极简 schema + 生产版触发器(读 GUC)
# ============================================================
echo "==> 1/5 装 mock schema + 生产版触发器(读 pitr.current_txn)"
docker exec -i "$CONTAINER" psql -U postgres -d "$DB" >/dev/null <<'SQL'
CREATE TABLE jfs_node (inode bigint PRIMARY KEY, length bigint);

CREATE TABLE pitr_txn (
    id           bigserial PRIMARY KEY,
    version_hash text UNIQUE NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO pitr_txn (version_hash) VALUES ('root');   -- id=1

CREATE TABLE pitr_node_history (
    id          bigserial PRIMARY KEY,
    inode       bigint NOT NULL,
    op          char(1) NOT NULL,
    txn_id      bigint,                                -- 归属;NULL 表示未打点
    snapshot    jsonb,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE OR REPLACE FUNCTION pitr_capture_node() RETURNS TRIGGER AS $$
DECLARE
    v_txn bigint := NULLIF(current_setting('pitr.current_txn', true), '')::bigint;
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO pitr_node_history (inode, op, txn_id) VALUES (NEW.inode, 'I', v_txn);
        RETURN NEW;
    ELSIF TG_OP = 'UPDATE' THEN
        INSERT INTO pitr_node_history (inode, op, txn_id, snapshot)
        VALUES (OLD.inode, 'U', v_txn, to_jsonb(OLD));
        RETURN NEW;
    ELSE
        INSERT INTO pitr_node_history (inode, op, txn_id, snapshot)
        VALUES (OLD.inode, 'D', v_txn, to_jsonb(OLD));
        RETURN OLD;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tg_pitr_node
    AFTER INSERT OR UPDATE OR DELETE ON jfs_node
    FOR EACH ROW EXECUTE FUNCTION pitr_capture_node();

INSERT INTO jfs_node VALUES (1, 100), (2, 200), (3, 300), (4, 400);
TRUNCATE pitr_node_history RESTART IDENTITY;
SQL

# ============================================================
# 场景 A: 同一 psql 会话、同一事务里 SET LOCAL + UPDATE
# ============================================================
echo
echo "==> 2/5 场景 A: 同连接 + SET LOCAL + UPDATE"
docker exec -i "$CONTAINER" psql -U postgres -d "$DB" >/dev/null <<'SQL'
BEGIN;
SET LOCAL pitr.current_txn = '42';
UPDATE jfs_node SET length = 999 WHERE inode = 1;
COMMIT;
SQL
got=$($PSQL -c "SELECT txn_id FROM pitr_node_history WHERE inode = 1 ORDER BY id DESC LIMIT 1;")
assert_eq "A) 同连接触发器读到 GUC" "42" "$got"

# ============================================================
# 场景 B: 跨连接 (每个 docker exec 是独立 backend/session)
# ============================================================
echo
echo "==> 3/5 场景 B: 跨连接 SET LOCAL 不可见"
# 连接 X: 设 GUC 后 commit,session 结束 GUC 丢失
docker exec -i "$CONTAINER" psql -U postgres -d "$DB" >/dev/null <<'SQL'
BEGIN;
SET LOCAL pitr.current_txn = '77';
COMMIT;
SQL
# 连接 Y: 全新 session,GUC 是空
docker exec -i "$CONTAINER" psql -U postgres -d "$DB" >/dev/null <<'SQL'
UPDATE jfs_node SET length = 888 WHERE inode = 2;
SQL
got=$($PSQL -c "SELECT COALESCE(txn_id::text, 'NULL') FROM pitr_node_history WHERE inode = 2 ORDER BY id DESC LIMIT 1;")
assert_eq "B) 跨连接 GUC 丢失" "NULL" "$got"

# ============================================================
# 场景 C: SET SESSION 泄漏 (连接池反复用的常见坑)
# ============================================================
echo
echo "==> 4/5 场景 C: SET SESSION 泄漏到下一个事务"
docker exec -i "$CONTAINER" psql -U postgres -d "$DB" >/dev/null <<'SQL'
BEGIN;
SET SESSION pitr.current_txn = '99';   -- 注意:不是 LOCAL
UPDATE jfs_node SET length = 111 WHERE inode = 3;
COMMIT;
-- 模拟连接池把这条连接分配给下一个请求, 而请求方忘了 RESET
BEGIN;
UPDATE jfs_node SET length = 222 WHERE inode = 4;
COMMIT;
SQL
got=$($PSQL -c "SELECT txn_id FROM pitr_node_history WHERE inode = 4 ORDER BY id DESC LIMIT 1;")
assert_eq "C) SET SESSION 泄漏 → inode=4 被错误归属为 99" "99" "$got"

# ============================================================
# 场景 D: MVCC 可见性 —— 跨连接 FK 引用未 commit 的 pitr_txn
#
# 布置:给 pitr_node_history.txn_id 加 FK 到 pitr_txn(id)
# 连接 X (后台): 起事务、INSERT pitr_txn 拿到新 id、pg_sleep 4 秒后 ROLLBACK
# 连接 Y (前台): 拿到 X 打算生成的 id (=2), SET LOCAL 后 UPDATE → 触发器写 history
#              → FK 检查看不见未提交的 pitr_txn 行 → 报错
# 预期结果: 连接 Y 报 FK 违反,rc != 0
# ============================================================
echo
echo "==> 5/5 场景 D: MVCC —— 跨连接 FK 引用未 commit 的 pitr_txn"
# A/C 已经在 history 里留了 txn_id=42/99 但 pitr_txn 里没有对应 id,
# 直接 ADD FK 会撞现有孤儿行 → 先 TRUNCATE 让 FK 能装上
$PSQL -c "TRUNCATE pitr_node_history RESTART IDENTITY;" >/dev/null
$PSQL -c "ALTER TABLE pitr_node_history
          ADD CONSTRAINT fk_txn FOREIGN KEY (txn_id) REFERENCES pitr_txn(id);" >/dev/null

# 后台连接 X: 消费 id=2, 保持事务打开 4 秒
(
    docker exec -i "$CONTAINER" psql -U postgres -d "$DB" >/dev/null 2>&1 <<'SQL'
BEGIN;
INSERT INTO pitr_txn (version_hash) VALUES ('unfinished-2');
SELECT pg_sleep(4);
ROLLBACK;
SQL
) &
bg_pid=$!
sleep 1   # 给 X 时间进入 sleep 状态

# 前台连接 Y: 引用 X 尚未 commit 的 id=2
set +e
err=$(docker exec -i "$CONTAINER" psql -U postgres -d "$DB" -v ON_ERROR_STOP=1 2>&1 <<'SQL'
BEGIN;
SET LOCAL pitr.current_txn = '2';
UPDATE jfs_node SET length = 333 WHERE inode = 1;
COMMIT;
SQL
)
rc=$?
set -e
wait "$bg_pid" 2>/dev/null || true

if [[ $rc -ne 0 ]] && echo "$err" | grep -qE "foreign key|violates|is not present"; then
    reason=$(echo "$err" | grep -oE 'violates.*|foreign key.*|is not present.*' | head -1 | tr -d '\r')
    printf "  [PASS] D) 跨连接 FK 引用未 commit pitr_txn 被拒 (%s)\n" "$reason"
else
    printf "  [FAIL] D) 预期 FK 违反,实际 rc=%d\n" "$rc"
    echo "$err" | sed 's/^/         /'
    FAIL=1
fi

# ============================================================
# 汇总
# ============================================================
echo
if [[ $FAIL -eq 0 ]]; then
    cat <<'EOF'
=================== 4/4 场景断言全部通过 ===================
结论:
  A. 生产版方案(SET LOCAL + trigger 读 GUC)在同一连接内是自洽的
  B. 一旦跨连接,GUC 立即丢失,归属被打穿 → 必须共享连接
  C. 用 SET SESSION 而非 SET LOCAL 会污染连接池后续请求 → 归属方案必须 LOCAL
  D. 就算能"传 id 过去",FK 会撞 MVCC → 归属和主写必须在同一事务

即:0.3 的头号风险是 **JuiceFS SDK 是否暴露复用外部 sql.DB 的 API**;
    暴露 → 走生产方案;不暴露 → 走时间戳退化方案(见 docs/P0-report.md)。
EOF
    exit 0
else
    echo "=================== 有断言失败,请回看输出 ==================="
    exit 1
fi
