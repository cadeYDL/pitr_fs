package schema

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ============================================================================
// Test 环境:一个 docker PG 全 test package 复用,每个测试用独立 database 隔离。
//
// 依赖:
//   - 环境里有 docker 可用
//   - 若设置 PITR_TEST_PG_DSN, 跳过 docker,直接用外部 PG(适合 CI/Sandbox 已有 PG)
// ============================================================================

var (
	adminDSN    string
	containerID string
	dbCounter   int
)

func TestMain(m *testing.M) {
	if dsn := os.Getenv("PITR_TEST_PG_DSN"); dsn != "" {
		adminDSN = dsn
		os.Exit(m.Run())
	}

	if _, err := exec.LookPath("docker"); err != nil {
		message := "未找到 docker,也未设置 PITR_TEST_PG_DSN"
		if os.Getenv("PITR_REQUIRE_INTEGRATION") == "1" {
			fmt.Fprintln(os.Stderr, "SQL 集成测试环境缺失:", message)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "跳过 SQL 集成测试:", message)
		os.Exit(0)
	}

	code, err := runWithDocker(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docker PG 环境启动失败:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runWithDocker(m *testing.M) (int, error) {
	name := fmt.Sprintf("pitr-schema-test-%d", time.Now().UnixNano())
	// 测试仅支持 Linux，直接通过 Docker bridge 地址连接隔离 PostgreSQL。
	out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=x", "-e", "POSTGRES_DB=postgres",
		"postgres:16.14-bookworm").CombinedOutput()
	if err != nil {
		return 1, fmt.Errorf("docker run: %w: %s", err, out)
	}
	containerID = strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", containerID).Run()

	dsn, err := waitReadyDSN(name)
	if err != nil {
		return 1, err
	}
	adminDSN = dsn
	return m.Run(), nil
}

// waitReadyDSN — 尝试两种可达 host, 返回第一个 ping 通的 DSN
func waitReadyDSN(name string) (string, error) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		hosts := []string{containerIP(name)}
		for _, h := range hosts {
			if h == "" {
				continue
			}
			dsn := fmt.Sprintf("postgres://postgres:x@%s:5432/postgres?sslmode=disable", h)
			if ping(dsn) == nil {
				return dsn, nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", errors.New("PG 60 秒内未通过 Docker bridge 地址就绪")
}

func containerIP(name string) string {
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func ping(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	return c.Ping(ctx)
}

// freshDB — 建独立 database 并返回 (dsn, cleanup)。跑 InitSQL 之前调用。
func freshDB(t *testing.T) (string, func()) {
	t.Helper()
	dbCounter++
	name := fmt.Sprintf("pitr_test_%d_%d", time.Now().UnixNano(), dbCounter)

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatalf("create db: %v", err)
	}
	admin.Close(ctx)

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	parsed.Path = "/" + name
	dsn := parsed.String()
	cleanup := func() {
		a, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			return
		}
		defer a.Close(ctx)
		_, _ = a.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	}
	return dsn, cleanup
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", oneLine(sql), err)
	}
}

func oneLine(s string) string {
	f := strings.Fields(s)
	if len(f) > 12 {
		return strings.Join(f[:12], " ") + " ..."
	}
	return strings.Join(f, " ")
}

func mustScalarInt(t *testing.T, conn *pgx.Conn, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("scalar %q: %v", oneLine(sql), err)
	}
	return n
}

// execUnderTxn — 在同一 pgx 事务里执行 SET LOCAL pitr.current_txn + 一段 SQL。
// SET LOCAL 只在事务内生效, 触发器读 GUC 才拿得到 txn_id。
func execUnderTxn(t *testing.T, conn *pgx.Conn, txnID int64, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL pitr.current_txn = "+strconv.FormatInt(txnID, 10)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("set local: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("exec %q: %v", oneLine(sql), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func applySchema(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.Exec(ctx, InitSQL); err != nil {
		t.Fatalf("apply InitSQL: %v", err)
	}
	return c
}

// ============================================================================
// TestSQL_Idempotent — init_pitr.sql 连续跑两次都不报错(幂等)
// ============================================================================
func TestSQL_Idempotent(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())
	// 再跑一次
	if _, err := conn.Exec(context.Background(), InitSQL); err != nil {
		t.Fatalf("第二次执行 InitSQL 应幂等,却报错: %v", err)
	}
	// 根版本只应有一条
	n := mustScalarInt(t, conn, "SELECT count(*) FROM pitr_txn WHERE state='root'")
	if n != 1 {
		t.Errorf("跑两次后 root 版本应仍只有 1 条,实际 %d", n)
	}
}

// ============================================================================
// TestSQL_TablesExist — 所有 pitr_* 表 + 索引 + 存储过程都建了
// ============================================================================
func TestSQL_TablesExist(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	for _, tbl := range []string{
		"pitr_workspace", "pitr_workspace_mount",
		"pitr_txn", "pitr_node_history", "pitr_edge_history",
		"pitr_chunk_history", "pitr_chunk_ref_history", "pitr_blob_retention",
		"pitr_slice_pin", "pitr_slice_ref", "pitr_gc_queue",
		"pitr_prune_queue",
		"pitr_internal_state", "pitr_slice_index_state", "pitr_space_state",
		"pitr_schema_state",
		"pitr_config", "pitr_volume_config",
	} {
		n := mustScalarInt(t, conn,
			"SELECT count(*) FROM information_schema.tables WHERE table_name = $1", tbl)
		if n != 1 {
			t.Errorf("表 %s 未建", tbl)
		}
	}
	for _, column := range []string{
		"schema_revision", "min_logic_revision", "digest", "logic_version",
	} {
		got := mustScalarInt(t, conn, `
			SELECT count(*) FROM information_schema.columns
			 WHERE table_name='pitr_schema_state' AND column_name=$1`, column)
		if got != 1 {
			t.Errorf("pitr_schema_state 缺少列 %s", column)
		}
	}

	// uniq_active_txn_per_path 部分索引
	n := mustScalarInt(t, conn,
		"SELECT count(*) FROM pg_indexes WHERE indexname = 'uniq_active_txn_per_path'")
	if n != 1 {
		t.Error("uniq_active_txn_per_path 部分索引未建")
	}
	n = mustScalarInt(t, conn,
		"SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_pitr_txn_closed'")
	if n != 1 {
		t.Error("idx_pitr_txn_closed 部分索引未建")
	}
	for _, column := range []string{
		"posix_op", "process_command", "actor_uid", "actor_gid",
		"actor_pid", "actor_name", "change_summary",
	} {
		n = mustScalarInt(t, conn, `
			SELECT count(*)
			  FROM information_schema.columns
			 WHERE table_name='pitr_txn' AND column_name=$1`, column)
		if n != 1 {
			t.Errorf("pitr_txn.%s 未建", column)
		}
	}

	// 存储过程
	for _, proc := range []string{"pitr_collapse_commit", "pitr_revert",
		"pitr_revert_from_temp", "pitr_rollback",
		"pitr_capture_node_change", "pitr_capture_edge_change",
		"pitr_capture_chunk_change", "pitr_capture_chunk_ref_change",
		"pitr_scopes_overlap", "pitr_reconcile_slice_refs",
		"pitr_rebuild_slice_index",
		"pitr_rebuild_space_state", "pitr_track_chunk_ref_space"} {
		n := mustScalarInt(t, conn,
			"SELECT count(*) FROM pg_proc WHERE proname = $1", proc)
		if n < 1 {
			t.Errorf("过程/函数 %s 未建", proc)
		}
	}
}

func TestSQL_MigratesLegacyStateIntoDefaultWorkspace(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, `
		INSERT INTO pitr_volume_config(volume_name,fuse_mount)
		VALUES ('legacy-volume','/pitr/legacy')
		ON CONFLICT (volume_name) DO UPDATE SET fuse_mount=EXCLUDED.fuse_mount`)
	mustExec(t, conn, `
		INSERT INTO pitr_txn(version_hash,scope_path,state,command,closed_at)
		VALUES ('legacy000001','/legacy','auto','write',now())`)
	mustExec(t, conn, InitSQL)

	var workspaceName, volumeName string
	if err := conn.QueryRow(context.Background(), `
		SELECT w.name,w.volume_name
		  FROM pitr_txn t JOIN pitr_workspace w ON w.id=t.workspace_id
		 WHERE t.version_hash='legacy000001'`).Scan(
		&workspaceName, &volumeName); err != nil {
		t.Fatal(err)
	}
	if workspaceName != "default" || volumeName != "legacy-volume" {
		t.Fatalf("workspace=%q volume=%q", workspaceName, volumeName)
	}
	got := mustScalarInt(t, conn, `
		SELECT count(*) FROM pitr_workspace_mount
		 WHERE workspace_id=1 AND fuse_mount='/pitr/legacy'`)
	if got != 1 {
		t.Fatalf("legacy mount rows=%d", got)
	}
}

func TestSQL_MigratesLegacyActiveTransaction(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, `
		INSERT INTO pitr_txn(version_hash,parent_id,scope_path,state,command)
		VALUES ('legacyactive',1,'/workspace/project','active','begin')`)
	mustExec(t, conn, InitSQL)

	var state, command string
	var closed bool
	if err := conn.QueryRow(context.Background(), `
		SELECT state,command,closed_at IS NOT NULL
		  FROM pitr_txn WHERE version_hash='legacyactive'`).
		Scan(&state, &command, &closed); err != nil {
		t.Fatal(err)
	}
	if state != "auto" || command != "migration:legacy-active" || !closed {
		t.Fatalf("state=%q command=%q closed=%v", state, command, closed)
	}
}

// ============================================================================
// TestTrigger_CapturesUpdate — mock 一张 jfs_node,UPDATE 后 history 里有 op='U' 记录
//
// 注意:init_pitr.sql 装触发器时用 DO 块检查 jfs_node 存在。所以要先建
// jfs_node,再 re-apply schema 触发 trigger 挂载。
// ============================================================================
func TestTrigger_CapturesUpdate(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	// 触发器现在还没挂上, re-apply init 才会挂
	if _, err := conn.Exec(context.Background(), InitSQL); err != nil {
		t.Fatalf("re-apply after mock jfs_node: %v", err)
	}

	// 建一个 auto 版本,给 GUC 用
	txnID := mustScalarInt(t, conn, `
        INSERT INTO pitr_txn (version_hash, scope_path, state, command, closed_at)
        VALUES ('t01_upd_xxxx', '/', 'auto', 'write:/a', now()) RETURNING id`)

	mustExec(t, conn, "INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (100, 33188, 1, 0)")
	// SET LOCAL 只在事务内生效, 触发器读 GUC 才拿得到 txn_id
	execUnderTxn(t, conn, txnID, "UPDATE jfs_node SET length = 42 WHERE inode = 100")

	got := mustScalarInt(t, conn, `SELECT count(*) FROM pitr_node_history
        WHERE txn_id = $1 AND inode = 100 AND op = 'U'`, txnID)
	if got != 1 {
		t.Errorf("期望 1 条 op='U' history,实际 %d", got)
	}
}

// ============================================================================
// TestTrigger_SessionVarFilter — 未设 pitr.current_txn 时不记录 history
// ============================================================================
func TestTrigger_SessionVarFilter(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	if _, err := conn.Exec(context.Background(), InitSQL); err != nil {
		t.Fatalf("re-apply: %v", err)
	}

	// 不设 GUC(默认空), UPDATE 不应产生 history
	mustExec(t, conn, "INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (200, 33188, 1, 0)")
	mustExec(t, conn, "UPDATE jfs_node SET length = 1 WHERE inode = 200")

	got := mustScalarInt(t, conn, "SELECT count(*) FROM pitr_node_history WHERE inode = 200")
	if got != 0 {
		t.Errorf("未设 GUC 时不应打点,实际有 %d 条", got)
	}
}

// ============================================================================
// TestTrigger_OpenAutoFallback — JuiceFS 使用独立连接时没有 GUC,触发器应把写入
// 归到唯一开放的 auto 窗口;窗口关闭后不再记录。
// ============================================================================
func TestTrigger_OpenAutoFallback(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn, `
		INSERT INTO jfs_node (inode, mode, nlink, length)
		VALUES (210, 33188, 1, 0), (211, 33188, 1, 0)`)

	autoID := mustScalarInt(t, conn, `
		INSERT INTO pitr_txn (version_hash, parent_id, scope_path, state, command)
		VALUES ('fallback0001', 1, '/a', 'auto', 'write:/a/f') RETURNING id`)

	// 不设置 pitr.current_txn,模拟 JuiceFS 的独立 PG 连接。
	mustExec(t, conn, "UPDATE jfs_node SET length = 10 WHERE inode = 210")
	got := mustScalarInt(t, conn,
		"SELECT count(*) FROM pitr_node_history WHERE txn_id=$1 AND inode=210", autoID)
	if got != 1 {
		t.Fatalf("开放 auto 窗口应捕获 1 条 history,实际 %d", got)
	}

	mustExec(t, conn, "UPDATE pitr_txn SET closed_at=now() WHERE id=$1", autoID)
	mustExec(t, conn, "UPDATE jfs_node SET length = 10 WHERE inode = 211")
	got = mustScalarInt(t, conn,
		"SELECT count(*) FROM pitr_node_history WHERE txn_id=$1 AND inode=211", autoID)
	if got != 0 {
		t.Fatalf("auto 窗口关闭后不应继续捕获,实际 %d", got)
	}
}

// JuiceFS Compaction 可能与用户写窗口重叠。固定 JuiceFS 补丁会在它自己的
// PostgreSQL 事务设置 pitr.internal_op=compact；这类物理重写不能进入用户
// 版本，否则 revert 会恢复更碎片化的 slice 布局。
func TestTrigger_CompactOperationBypassesOpenAuto(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn, `
		INSERT INTO jfs_chunk(inode,indx,slices)
		VALUES (215,0,decode('000000000000000000000385000000040000000000000004','hex'))`)
	autoID := mustScalarInt(t, conn, `
		INSERT INTO pitr_txn(version_hash,parent_id,scope_path,state,command)
		VALUES ('compact00001',1,'/a','auto','write:/a/f') RETURNING id`)

	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(),
		"SELECT set_config('pitr.internal_op','compact',true)"); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		UPDATE jfs_chunk
		   SET slices=decode('000000000000000000000386000000040000000000000004','hex')
		 WHERE inode=215 AND indx=0`); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := mustScalarInt(t, conn, `
		SELECT count(*) FROM pitr_chunk_history
		 WHERE txn_id=$1 AND inode=215 AND indx=0`, autoID)
	if got != 0 {
		t.Fatalf("Compaction 不应进入开放 auto 版本，实际捕获 %d 条", got)
	}
}

// ============================================================================
// TestTrigger_CapturesChunkRef — chunk 指针回退所需的引用计数也必须进入 history。
// ============================================================================
func TestTrigger_CapturesChunkRef(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn, "INSERT INTO jfs_chunk_ref (chunkid, size, refs) VALUES (900, 4, 1)")
	autoID := mustScalarInt(t, conn, `
		INSERT INTO pitr_txn (version_hash, parent_id, scope_path, state, command, closed_at)
		VALUES ('chunkref0001', 1, '/a', 'auto', 'write:/a/f', now()) RETURNING id`)

	execUnderTxn(t, conn, autoID, "UPDATE jfs_chunk_ref SET refs=0 WHERE chunkid=900")
	got := mustScalarInt(t, conn, `
		SELECT count(*) FROM pitr_chunk_ref_history
		WHERE txn_id=$1 AND chunkid=900 AND op='U'
		  AND (snapshot->>'refs')::int=1`, autoID)
	if got != 1 {
		t.Fatalf("chunk_ref UPDATE 应捕获旧引用计数,实际 %d", got)
	}
}

func TestTrigger_NodeDeleteCapturesChunkBeforeAsyncCleanup(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn, `
		INSERT INTO jfs_node(inode,mode,nlink,length)
		VALUES (220,33188,1,4)`)
	mustExec(t, conn, `
		INSERT INTO jfs_chunk(inode,indx,slices)
		VALUES (220,0,decode('000000000000000000000384000000040000000000000004','hex'))`)
	mustExec(t, conn,
		"INSERT INTO jfs_chunk_ref(chunkid,size,refs) VALUES (900,4,1)")
	_ = mustScalarInt(t, conn, `
		INSERT INTO pitr_txn(version_hash,scope_path,state,command,closed_at)
		VALUES ('unlinkbase01','/','committed','baseline',now()) RETURNING id`)
	unlinkID := mustScalarInt(t, conn, `
		INSERT INTO pitr_txn(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('unlinkafter1',1,'/','auto','unlink:/a',now()) RETURNING id`)

	// node 先删、chunk 由 JuiceFS 后台稍后删除，复现真实 unlink 竞态。
	execUnderTxn(t, conn, unlinkID, "DELETE FROM jfs_node WHERE inode=220")
	got := mustScalarInt(t, conn, `
		SELECT count(*) FROM pitr_chunk_history
		 WHERE txn_id=$1 AND inode=220 AND indx=0 AND op='D'
		   AND snapshot IS NOT NULL`, unlinkID)
	if got != 1 {
		t.Fatalf("node DELETE 应主动捕获 chunk 快照,实际 %d", got)
	}
	refs := mustScalarInt(t, conn,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=900")
	if refs != 2 {
		t.Fatalf("历史 chunk 应增加一个物理 pin,refs=%d", refs)
	}
	// JuiceFS 的异步 unlink 会在 auto 窗口关闭后才减少当前引用，因此这次
	// UPDATE 不会进入 chunk_ref history。旧实现只恢复 chunk 行，随后释放
	// pin 会把当前文件仍在使用的 slice 错降到 0。
	mustExec(t, conn, "UPDATE jfs_chunk_ref SET refs=refs-1 WHERE chunkid=900")
	mustExec(t, conn, "DELETE FROM jfs_chunk WHERE inode=220")
	mustExec(t, conn, "CALL pitr_revert($1,$2)", "unlinkbase01", "/")
	if got = mustScalarInt(t, conn,
		"SELECT count(*) FROM jfs_chunk WHERE inode=220 AND indx=0"); got != 1 {
		t.Fatalf("异步清理后 revert 应恢复 chunk,实际 %d", got)
	}
	refs = mustScalarInt(t, conn,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=900")
	if refs != 2 {
		t.Fatalf("恢复后应包含当前引用和历史 pin,refs=%d", refs)
	}
	// 淘汰 unlink 版本后只释放历史 pin，当前恢复文件仍保留一个引用。
	mustExec(t, conn, "UPDATE pitr_txn SET parent_id=1 WHERE parent_id=$1", unlinkID)
	mustExec(t, conn, "DELETE FROM pitr_txn WHERE id=$1", unlinkID)
	refs = mustScalarInt(t, conn,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=900")
	if refs != 1 {
		t.Fatalf("淘汰历史后应保留当前文件引用,refs=%d", refs)
	}
}

func TestSliceIndexRebuildRepairsMissingAndUndercountedRefs(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn, `
		INSERT INTO jfs_node(inode,mode,nlink,length)
		VALUES (221,33188,1,4)`)
	mustExec(t, conn, `
		INSERT INTO jfs_chunk(inode,indx,slices)
		VALUES (221,0,decode('000000000000000000000385000000040000000000000004','hex'))`)
	mustExec(t, conn,
		"INSERT INTO jfs_chunk_ref(chunkid,size,refs) VALUES (901,4,1)")
	autoID := mustScalarInt(t, conn, `
		INSERT INTO pitr_txn(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('rebuildref01',1,'/','auto','write:/a',now()) RETURNING id`)
	// 捕获当前 chunk 形成一份可重建的历史 pin，再模拟旧版本把 refs 行
	// 完全丢失。
	execUnderTxn(t, conn, autoID,
		"UPDATE jfs_chunk SET slices=slices WHERE inode=221 AND indx=0")
	mustExec(t, conn, "DELETE FROM jfs_chunk_ref WHERE chunkid=901")

	mustExec(t, conn, "SELECT pitr_rebuild_slice_index()")
	refs := mustScalarInt(t, conn,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=901")
	if refs != 2 {
		t.Fatalf("重建后应恢复当前引用+历史 pin,refs=%d", refs)
	}
	pins := mustScalarInt(t, conn,
		"SELECT pins FROM pitr_slice_ref WHERE chunkid=901")
	if pins != 1 {
		t.Fatalf("重建后的历史 pin=%d", pins)
	}
}

func TestSliceRefReconcileRejectsSizeConflictAtomically(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn, `
		INSERT INTO jfs_chunk(inode,indx,slices)
		VALUES (222,0,decode('000000000000000000000387000000040000000000000004','hex'))`)
	mustExec(t, conn,
		"INSERT INTO jfs_chunk_ref(chunkid,size,refs) VALUES (903,8,7)")
	if _, err := conn.Exec(context.Background(),
		"SELECT pitr_reconcile_slice_refs(NULL)"); err == nil {
		t.Fatal("同一 slice 的 size 冲突应拒绝校准")
	}
	refs := mustScalarInt(t, conn,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=903 AND size=8")
	if refs != 7 {
		t.Fatalf("失败校准不应留下中间态,refs=%d", refs)
	}
}

func TestVersionReleaseRepairsLegacyMissingRef(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn,
		"INSERT INTO jfs_chunk_ref(chunkid,size,refs) VALUES (902,4,1)")
	autoID := mustScalarInt(t, conn, `
		INSERT INTO pitr_txn(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('release_ref1',1,'/','auto','unlink:/old',now()) RETURNING id`)
	mustExec(t, conn,
		"SELECT pitr_pin_chunk_slices($1,decode('000000000000000000000386000000040000000000000004','hex'))", autoID)
	// 历史独占 slice 的对象可能已经被旧 bug 清掉；clear/prune 至少必须能
	// 原子移除损坏版本，不能永远卡在“引用行丢失”。
	mustExec(t, conn, "DELETE FROM jfs_chunk_ref WHERE chunkid=902")
	mustExec(t, conn, "DELETE FROM pitr_txn WHERE id=$1", autoID)
	if got := mustScalarInt(t, conn,
		"SELECT count(*) FROM pitr_slice_ref WHERE chunkid=902"); got != 0 {
		t.Fatalf("损坏历史释放后仍残留 slice_ref=%d", got)
	}
}

func TestPinAndReleaseAggregatesDuplicateSlices(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn,
		"INSERT INTO jfs_chunk_ref(chunkid,size,refs) VALUES (904,4,1)")
	autoID := mustScalarInt(t, conn, `
		INSERT INTO pitr_txn(version_hash,parent_id,scope_path,state,command,closed_at)
		VALUES ('pin_batch_01',1,'/','auto','write:/batch',now()) RETURNING id`)
	// 同一 slice 可在一个 chunk 的覆盖链中出现多次；批量 pin/release 必须
	// 按出现次数聚合，而不能把 DISTINCT chunkid 误当成一次引用。
	const one = "000000000000000000000388000000040000000000000004"
	mustExec(t, conn,
		"SELECT pitr_pin_chunk_slices($1,decode($2,'hex'))", autoID, one+one)
	if got := mustScalarInt(t, conn,
		"SELECT pins FROM pitr_slice_ref WHERE chunkid=904"); got != 2 {
		t.Fatalf("重复 slice 应聚合为两个历史 pin,pins=%d", got)
	}
	if got := mustScalarInt(t, conn,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=904"); got != 3 {
		t.Fatalf("JuiceFS refs 应包含当前引用和两个历史 pin,refs=%d", got)
	}
	if got := mustScalarInt(t, conn,
		"SELECT length(slices) FROM pitr_slice_pin WHERE txn_id=$1", autoID); got != 24 {
		t.Fatalf("紧凑 pin bundle 应保留两项,bytes=%d", got)
	}

	mustExec(t, conn, "DELETE FROM pitr_txn WHERE id=$1", autoID)
	if got := mustScalarInt(t, conn,
		"SELECT count(*) FROM pitr_slice_ref WHERE chunkid=904"); got != 0 {
		t.Fatalf("版本释放后不应残留历史 pin,row=%d", got)
	}
	if got := mustScalarInt(t, conn,
		"SELECT refs FROM jfs_chunk_ref WHERE chunkid=904"); got != 1 {
		t.Fatalf("版本释放后应保留当前引用,refs=%d", got)
	}
}

// ============================================================================
// TestCollapse_KeepsEarliestSnapshot — 模拟 auto v1/v2/v3,坍缩后每个 inode 只留
// 最早那次改动的 snapshot,commit 版本自身持有全部 history
// ============================================================================
func TestCollapse_KeepsEarliestSnapshot(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	if _, err := conn.Exec(context.Background(), InitSQL); err != nil {
		t.Fatalf("re-apply: %v", err)
	}

	// 建一个 committed 事务作为 parent
	commitID := mustScalarInt(t, conn, `
        INSERT INTO pitr_txn (version_hash, scope_path, state, command)
        VALUES ('cmt_01_xxxxx', '/a', 'committed', 'commit') RETURNING id`)

	// 建 3 个 auto 子事务 v1/v2/v3
	var autoIDs [3]int64
	for i, tag := range []string{"a01_v1_xxxxx", "a01_v2_xxxxx", "a01_v3_xxxxx"} {
		autoIDs[i] = mustScalarInt(t, conn, `
            INSERT INTO pitr_txn (version_hash, parent_id, scope_path, state, command, closed_at)
            VALUES ($1, $2, '/a', 'auto', 'w', now()) RETURNING id`, tag, commitID)
	}

	// 建一行 jfs_node, 让 auto v1/v2/v3 各改一次同一 inode
	mustExec(t, conn, "INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (300, 33188, 1, 0)")
	for i, tid := range autoIDs {
		execUnderTxn(t, conn, tid,
			"UPDATE jfs_node SET length = $1 WHERE inode = 300", int64(10*(i+1)))
	}
	// 3 条 auto history 应齐
	before := mustScalarInt(t, conn, "SELECT count(*) FROM pitr_node_history WHERE inode = 300")
	if before != 3 {
		t.Fatalf("坍缩前应有 3 条 history,实际 %d", before)
	}

	// 坍缩
	mustExec(t, conn, "CALL pitr_collapse_commit($1)", commitID)

	// 只应剩 1 条,归属到 commit_id
	after := mustScalarInt(t, conn, "SELECT count(*) FROM pitr_node_history WHERE inode = 300")
	if after != 1 {
		t.Errorf("坍缩后应剩 1 条,实际 %d", after)
	}
	owner := mustScalarInt(t, conn, "SELECT txn_id FROM pitr_node_history WHERE inode = 300")
	if owner != commitID {
		t.Errorf("剩下的 history 应归属 commit_id=%d,实际 %d", commitID, owner)
	}

	// 剩下这条应该是 v1 之前的 snapshot(length=0)
	var length int64
	if err := conn.QueryRow(context.Background(),
		`SELECT (snapshot->>'length')::bigint FROM pitr_node_history WHERE inode = 300`).Scan(&length); err != nil {
		t.Fatalf("读 snapshot: %v", err)
	}
	if length != 0 {
		t.Errorf("坍缩后应保留最早的 snapshot(length=0),实际 length=%d", length)
	}

	// auto 事务已被删
	autoLeft := mustScalarInt(t, conn,
		"SELECT count(*) FROM pitr_txn WHERE parent_id = $1 AND state = 'auto'", commitID)
	if autoLeft != 0 {
		t.Errorf("坍缩后 auto 子事务应清空,实际剩 %d", autoLeft)
	}
}

// ============================================================================
// TestRevert_RestoresRow — 插入一行 → UPDATE → revert 到之前 → 行恢复原值
// ============================================================================
func TestRevert_RestoresRow(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	if _, err := conn.Exec(context.Background(), InitSQL); err != nil {
		t.Fatalf("re-apply: %v", err)
	}

	// v_before 版本 tag: 记录时点 (created_at) 作为 revert 目标
	// 先插入一行,让 v_before 之前的世界是"没这行"
	mustExec(t, conn, "INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (400, 33188, 1, 100)")
	_ = mustScalarInt(t, conn, `
        INSERT INTO pitr_txn (version_hash, scope_path, state, command)
        VALUES ('rv_before_xx', '/a', 'committed', 'w') RETURNING id`)

	// 再开一个 auto 事务,把 length 从 100 改成 999
	autoID := mustScalarInt(t, conn, `
        INSERT INTO pitr_txn (version_hash, scope_path, state, command)
        VALUES ('rv_after_xxx', '/a', 'auto', 'w') RETURNING id`)
	execUnderTxn(t, conn, autoID, "UPDATE jfs_node SET length = 999 WHERE inode = 400")

	// 现在 length 应是 999,history 里有一条 op='U' snapshot length=100
	cur := mustScalarInt(t, conn, "SELECT length FROM jfs_node WHERE inode = 400")
	if cur != 999 {
		t.Fatalf("revert 前 length 应是 999,实际 %d", cur)
	}

	// revert 到 rv_before
	mustExec(t, conn, "CALL pitr_revert($1)", "rv_before_xx")

	// length 应回到 100
	got := mustScalarInt(t, conn, "SELECT length FROM jfs_node WHERE inode = 400")
	if got != 100 {
		t.Errorf("revert 后 length 应回到 100,实际 %d", got)
	}
}

// ============================================================================
// TestRevert_ReplaysAllVersions — 同一 inode 多次修改时必须反向应用全部快照。
// ============================================================================
func TestRevert_ReplaysAllVersions(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn, "INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (410, 33188, 1, 100)")
	_ = mustScalarInt(t, conn, `
		INSERT INTO pitr_txn (version_hash, scope_path, state, command)
		VALUES ('rv_multi_v1x', '/a', 'committed', 'commit') RETURNING id`)

	for _, tc := range []struct {
		hash   string
		length int64
	}{
		{hash: "rv_multi_v2x", length: 200},
		{hash: "rv_multi_v3x", length: 300},
	} {
		autoID := mustScalarInt(t, conn, `
			INSERT INTO pitr_txn (version_hash, parent_id, scope_path, state, command, closed_at)
			VALUES ($1, 1, '/a', 'auto', 'write:/a/f', now()) RETURNING id`, tc.hash)
		execUnderTxn(t, conn, autoID,
			"UPDATE jfs_node SET length=$1 WHERE inode=410", tc.length)
	}

	mustExec(t, conn, "CALL pitr_revert($1)", "rv_multi_v1x")
	got := mustScalarInt(t, conn, "SELECT length FROM jfs_node WHERE inode=410")
	if got != 100 {
		t.Fatalf("多版本 revert 应恢复到 length=100,实际 %d", got)
	}
}

// ============================================================================
// TestRevert_ScopeUsesPathBoundary — /a 不能错误匹配 /abc。
// ============================================================================
func TestRevert_ScopeUsesPathBoundary(t *testing.T) {
	dsn, cleanup := freshDB(t)
	defer cleanup()
	conn := applySchema(t, dsn)
	defer conn.Close(context.Background())

	mustExec(t, conn, mockJFSNodeSQL)
	mustExec(t, conn, InitSQL)
	mustExec(t, conn, "INSERT INTO jfs_node (inode, mode, nlink, length) VALUES (420, 33188, 1, 1)")
	_ = mustScalarInt(t, conn, `
		INSERT INTO pitr_txn (version_hash, scope_path, state, command)
		VALUES ('scopebefore1', '/', 'committed', 'commit') RETURNING id`)
	autoID := mustScalarInt(t, conn, `
		INSERT INTO pitr_txn (version_hash, parent_id, scope_path, state, command, closed_at)
		VALUES ('scopeafter01', 1, '/abc', 'auto', 'write:/abc/f', now()) RETURNING id`)
	execUnderTxn(t, conn, autoID, "UPDATE jfs_node SET length=2 WHERE inode=420")

	mustExec(t, conn, "CALL pitr_revert($1, $2)", "scopebefore1", "/a")
	got := mustScalarInt(t, conn, "SELECT length FROM jfs_node WHERE inode=420")
	if got != 2 {
		t.Fatalf("revert /a 不应影响 /abc,实际 length=%d", got)
	}
}

// ============================================================================
// mock jfs_* 表(裁剪版, 仅包含被 pitr 依赖的字段)
// 真实 JuiceFS 表结构:https://github.com/juicedata/juicefs/blob/main/pkg/meta/sql.go
// ============================================================================
const mockJFSNodeSQL = `
CREATE TABLE IF NOT EXISTS jfs_node (
    inode           bigint  PRIMARY KEY,
    type            smallint,
    flags           smallint,
    mode            int,
    uid             int,
    gid             int,
    atime           bigint,
    mtime           bigint,
    ctime           bigint,
    atimensec       int,
    mtimensec       int,
    ctimensec       int,
    nlink           int,
    length          bigint,
    rdev            int,
    parent          bigint,
    access_acl_id   int,
    default_acl_id  int
);
CREATE TABLE IF NOT EXISTS jfs_edge (
    parent  bigint,
    name    bytea,
    inode   bigint,
    type    smallint,
    PRIMARY KEY (parent, name)
);
CREATE TABLE IF NOT EXISTS jfs_chunk (
    inode   bigint,
    indx    int,
    slices  bytea,
    PRIMARY KEY (inode, indx)
);
CREATE TABLE IF NOT EXISTS jfs_chunk_ref (
    chunkid  bigint PRIMARY KEY,
    size     int,
    refs     int
);
CREATE TABLE IF NOT EXISTS jfs_delslices (
    chunkid bigint PRIMARY KEY,
    deleted bigint,
    slices bytea
);
`
