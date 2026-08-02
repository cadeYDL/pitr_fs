package v1

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("scan 参数数量=%d，值数量=%d", len(dest), len(r.values))
	}
	for index := range dest {
		switch target := dest[index].(type) {
		case *int:
			*target = r.values[index].(int)
		case *string:
			*target = r.values[index].(string)
		case *bool:
			*target = r.values[index].(bool)
		default:
			return fmt.Errorf("不支持的 scan 类型 %T", target)
		}
	}
	return nil
}

type fakeDB struct {
	serverVersion int
	metaVersion   int
	missing       string
	wrongType     string
	missingKey    string
}

func (f fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "server_version_num"):
		return fakeRow{values: []any{f.serverVersion}}
	case strings.Contains(sql, "jfs_setting"):
		return fakeRow{values: []any{fmt.Sprintf(`{"MetaVersion":%d}`, f.metaVersion)}}
	case strings.Contains(sql, "pg_attribute") && !strings.Contains(sql, "pg_index"):
		key := args[0].(string) + "." + args[1].(string)
		if key == f.missing {
			return fakeRow{err: pgx.ErrNoRows}
		}
		for _, column := range requiredColumns {
			if key == column.table+"."+column.name {
				value := column.type_
				if key == f.wrongType {
					value = "text"
				}
				return fakeRow{values: []any{value}}
			}
		}
		return fakeRow{err: pgx.ErrNoRows}
	case strings.Contains(sql, "pg_index"):
		key := args[0].(string) + "." + args[1].(string)
		return fakeRow{values: []any{key != f.missingKey}}
	default:
		return fakeRow{err: fmt.Errorf("未知查询: %s", sql)}
	}
}

func compatibleFakeDB() fakeDB {
	return fakeDB{serverVersion: 160014, metaVersion: MetadataVersion}
}

func TestValidateMetadata(t *testing.T) {
	if err := ValidateMetadata(context.Background(), compatibleFakeDB()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateMetadataFailClosed(t *testing.T) {
	tests := []struct {
		name string
		db   fakeDB
		want string
	}{
		{"PostgreSQL 主版本", fakeDB{serverVersion: 170001, metaVersion: 1}, "PostgreSQL 16.x"},
		{"元数据版本", fakeDB{serverVersion: 160001, metaVersion: 2}, "MetaVersion=1"},
		{"缺少字段", func() fakeDB { db := compatibleFakeDB(); db.missing = "jfs_chunk.slices"; return db }(), "缺少字段 jfs_chunk.slices"},
		{"字段类型", func() fakeDB { db := compatibleFakeDB(); db.wrongType = "jfs_chunk.slices"; return db }(), "需要 bytea"},
		{"唯一键", func() fakeDB { db := compatibleFakeDB(); db.missingKey = "jfs_chunk.inode,indx"; return db }(), "缺少唯一键(inode,indx)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateMetadata(context.Background(), test.db)
			if !errors.Is(err, ErrIncompatible) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v，期望包含 %q", err, test.want)
			}
		})
	}
}

func TestValidateBinary(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "juicefs-good")
	bad := filepath.Join(dir, "juicefs-bad")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho 'juicefs version 1.3.0+stable."+BuildMarker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho 'juicefs version 1.3.0+official'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBinary(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBinary(context.Background(), bad); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("未拒绝无补丁二进制: %v", err)
	}
}

func TestContractConstants(t *testing.T) {
	if SliceBytes != 24 || DelayedSliceBytes != 12 {
		t.Fatalf("slice ABI 意外变化: %d/%d", SliceBytes, DelayedSliceBytes)
	}
	if got := ParseVersionOutput("juicefs version 1.3.0+stable.pitrfs.1"); got != "1.3.0+stable.pitrfs.1" {
		t.Fatalf("version=%q", got)
	}
}
