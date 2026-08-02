// Package v1 定义 pitr-fs 与固定 JuiceFS 运行时之间的最小兼容边界。
//
// JuiceFS 的 SQL 元数据表不是公共 API。所有不得不依赖的内部字段、编码和
// 约束都集中在这里校验；未知运行时必须 fail closed，不能尝试带病挂载。
package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	UpstreamVersion = "1.3.0"
	UpstreamCommit  = "30190ca1094d26e85f19a979ca51b0ea19af1eaa"
	PatchRevision   = "pitrfs.1"
	BuildMarker     = PatchRevision + "-30190ca"

	PostgreSQLMajor = 16
	MetadataVersion = 1

	SliceBytes        = 24
	DelayedSliceBytes = 12

	InternalOperationSetting = "pitr.internal_op"
	InternalOperationCompact = "compact"
)

var ErrIncompatible = errors.New("JuiceFS 内部 ABI 不兼容")

type columnSpec struct {
	table string
	name  string
	type_ string
}

// requiredColumns 只包含 pitr-fs 直接读取、写回或解析的内部字段。上游新增
// 无关字段不会阻塞启动；这里已有字段的类型发生变化则必须显式新增 ABI 版本。
var requiredColumns = []columnSpec{
	{"jfs_node", "inode", "bigint"},
	{"jfs_node", "type", "integer"},
	{"jfs_node", "mode", "integer"},
	{"jfs_node", "nlink", "integer"},
	{"jfs_node", "length", "bigint"},
	{"jfs_node", "parent", "bigint"},
	{"jfs_edge", "parent", "bigint"},
	{"jfs_edge", "name", "bytea"},
	{"jfs_edge", "inode", "bigint"},
	{"jfs_edge", "type", "integer"},
	{"jfs_chunk", "inode", "bigint"},
	{"jfs_chunk", "indx", "integer"},
	{"jfs_chunk", "slices", "bytea"},
	{"jfs_chunk_ref", "chunkid", "bigint"},
	{"jfs_chunk_ref", "size", "integer"},
	{"jfs_chunk_ref", "refs", "integer"},
	{"jfs_delslices", "chunkid", "bigint"},
	{"jfs_delslices", "deleted", "bigint"},
	{"jfs_delslices", "slices", "bytea"},
}

type uniqueSpec struct {
	table   string
	columns []string
}

var requiredUniqueKeys = []uniqueSpec{
	{"jfs_node", []string{"inode"}},
	{"jfs_edge", []string{"parent", "name"}},
	{"jfs_chunk", []string{"inode", "indx"}},
	{"jfs_chunk_ref", []string{"chunkid"}},
	{"jfs_delslices", []string{"chunkid"}},
}

// Contract 是可打印、可测试的运行时契约摘要。
func Contract() string {
	return fmt.Sprintf(
		"JuiceFS %s (%s, %s), PostgreSQL %d, metadata v%d, slice=%d/%dB",
		UpstreamVersion, UpstreamCommit[:12], PatchRevision,
		PostgreSQLMajor, MetadataVersion, SliceBytes, DelayedSliceBytes,
	)
}

// ValidateBinary 确认运行的是固定上游版本和带显式 Compaction 标记的 pitr
// 构建。只有版本号相同但没有补丁标记的官方二进制也必须拒绝。
func ValidateBinary(ctx context.Context, binary string) error {
	if binary == "" {
		binary = "juicefs"
	}
	out, err := exec.CommandContext(ctx, binary, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: 执行 %s version: %v (%s)",
			ErrIncompatible, binary, err, strings.TrimSpace(string(out)))
	}
	versionOutput := strings.TrimSpace(string(out))
	version := ParseVersionOutput(versionOutput)
	if !strings.HasPrefix(version, UpstreamVersion+"+") ||
		!strings.Contains(version, BuildMarker) {
		return fmt.Errorf("%w: 需要 %s，实际为 %q",
			ErrIncompatible, Contract(), versionOutput)
	}
	return nil
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type formatMetadata struct {
	MetaVersion int `json:"MetaVersion"`
}

// ValidateMetadata 校验 PostgreSQL、JuiceFS metadata version 以及 pitr-fs
// 依赖的内部字段和唯一键。校验只读，不会修改用户元数据。
func ValidateMetadata(ctx context.Context, db queryRower) error {
	if db == nil {
		return fmt.Errorf("%w: 数据库未配置", ErrIncompatible)
	}
	var serverVersion int
	if err := db.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::integer").Scan(&serverVersion); err != nil {
		return fmt.Errorf("%w: 读取 PostgreSQL 版本: %v", ErrIncompatible, err)
	}
	if major := serverVersion / 10000; major != PostgreSQLMajor {
		return fmt.Errorf("%w: 仅支持 PostgreSQL %d.x，实际 server_version_num=%d",
			ErrIncompatible, PostgreSQLMajor, serverVersion)
	}

	var rawFormat string
	if err := db.QueryRow(ctx,
		"SELECT value FROM jfs_setting WHERE name='format'").Scan(&rawFormat); err != nil {
		return fmt.Errorf("%w: 读取 jfs_setting.format: %v", ErrIncompatible, err)
	}
	var format formatMetadata
	if err := json.Unmarshal([]byte(rawFormat), &format); err != nil {
		return fmt.Errorf("%w: 解析 jfs_setting.format: %v", ErrIncompatible, err)
	}
	if format.MetaVersion != MetadataVersion {
		return fmt.Errorf("%w: 仅支持 JuiceFS MetaVersion=%d，实际为 %d",
			ErrIncompatible, MetadataVersion, format.MetaVersion)
	}

	for _, column := range requiredColumns {
		var actual string
		err := db.QueryRow(ctx, `
			SELECT format_type(a.atttypid,a.atttypmod)
			  FROM pg_attribute a
			 WHERE a.attrelid=to_regclass($1)
			   AND a.attname=$2 AND a.attnum>0 AND NOT a.attisdropped`,
			column.table, column.name).Scan(&actual)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: 缺少字段 %s.%s",
				ErrIncompatible, column.table, column.name)
		}
		if err != nil {
			return fmt.Errorf("%w: 检查字段 %s.%s: %v",
				ErrIncompatible, column.table, column.name, err)
		}
		if actual != column.type_ {
			return fmt.Errorf("%w: 字段 %s.%s 需要 %s，实际为 %s",
				ErrIncompatible, column.table, column.name, column.type_, actual)
		}
	}

	for _, key := range requiredUniqueKeys {
		var exists bool
		err := db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				  FROM pg_index i
				 WHERE i.indrelid=to_regclass($1)
				   AND i.indisunique AND i.indisvalid
				   AND (SELECT array_agg(a.attname::text ORDER BY k.ordinality)
						  FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum,ordinality)
						  JOIN pg_attribute a
						    ON a.attrelid=i.indrelid AND a.attnum=k.attnum)
				       = string_to_array($2, ',')
			)`, key.table, strings.Join(key.columns, ",")).Scan(&exists)
		if err != nil {
			return fmt.Errorf("%w: 检查 %s 唯一键(%s): %v",
				ErrIncompatible, key.table, strings.Join(key.columns, ","), err)
		}
		if !exists {
			return fmt.Errorf("%w: %s 缺少唯一键(%s)",
				ErrIncompatible, key.table, strings.Join(key.columns, ","))
		}
	}
	return nil
}

// ParseVersionOutput 供诊断和测试使用，返回 version 后的 semver 主体。
func ParseVersionOutput(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	for index := range fields {
		if fields[index] == "version" && index+1 < len(fields) {
			return fields[index+1]
		}
	}
	return ""
}
