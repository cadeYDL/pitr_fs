package txn

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type VersionSpaceEstimate struct {
	VersionHash     string
	ClosedAt        time.Time
	PinnedBytes     int64
	ReleasableBytes int64
}

// SpaceVersions 按实际裁剪顺序（最老优先）估算单独删除每个版本时可让多少
// 唯一 slice 变成 refs=0。它只在显式查询时解析紧凑 pin，不增加写路径成本。
func (m *Manager) SpaceVersions(
	ctx context.Context,
	scope string,
	limit int,
) ([]VersionSpaceEstimate, error) {
	normalized, err := NormalizeScope(scope)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := m.db.Query(ctx, `
		WITH selected AS (
		    SELECT id,version_hash,closed_at
		      FROM pitr_txn
		     WHERE state<>'root' AND closed_at IS NOT NULL
		       AND pitr_scopes_overlap(scope_path,$1)
		     ORDER BY id ASC
		     LIMIT $2
		), members AS (
		    SELECT s.id,
		           pitr_decode_u64(substring(p.slices FROM n*12+1 FOR 8)) chunkid,
		           pitr_decode_u32(substring(p.slices FROM n*12+9 FOR 4)) size
		      FROM selected s
		      JOIN pitr_slice_pin p ON p.txn_id=s.id
		      CROSS JOIN LATERAL generate_series(0,length(p.slices)/12-1) n
		), grouped AS (
		    SELECT id,chunkid,max(size)::bigint size,count(*)::bigint pin_count
		      FROM members GROUP BY id,chunkid
		), totals AS (
		    SELECT g.id,sum(g.size) pinned_bytes,
		           sum(CASE WHEN r.refs=g.pin_count THEN g.size ELSE 0 END)
		               estimated_release_bytes
		      FROM grouped g
		      JOIN jfs_chunk_ref r ON r.chunkid=g.chunkid AND r.size=g.size
		     GROUP BY g.id
		)
		SELECT s.version_hash,s.closed_at,
		       COALESCE(t.pinned_bytes,0),COALESCE(t.estimated_release_bytes,0)
		  FROM selected s LEFT JOIN totals t ON t.id=s.id
		 ORDER BY s.id ASC`, normalized, limit)
	if err != nil {
		return nil, fmt.Errorf("估算版本空间: %w", err)
	}
	defer rows.Close()
	var result []VersionSpaceEstimate
	for rows.Next() {
		var item VersionSpaceEstimate
		if err := rows.Scan(&item.VersionHash, &item.ClosedAt,
			&item.PinnedBytes, &item.ReleasableBytes); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ParseSpaceBytes 接受用户熟悉的文件容量写法。KiB/MiB/GiB/TiB 使用 1024，
// KB/MB/GB/TB 使用 1000；0、unlimited 和“无限制”表示不设空间上限。
func ParseSpaceBytes(value string) (int64, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "0", "unlimited", "无限制":
		return 0, nil
	case "":
		return 0, fmt.Errorf("空间大小不能为空")
	}
	type unitDef struct {
		suffix string
		factor float64
	}
	units := []unitDef{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
		{"kb", 1e3}, {"mb", 1e6}, {"gb", 1e9}, {"tb", 1e12},
		{"b", 1},
	}
	factor := float64(1)
	number := normalized
	for _, unit := range units {
		if strings.HasSuffix(normalized, unit.suffix) {
			factor = unit.factor
			number = strings.TrimSpace(strings.TrimSuffix(normalized, unit.suffix))
			break
		}
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0, fmt.Errorf("非法空间大小 %q，例如使用 100GiB", value)
	}
	bytes := parsed * factor
	if bytes > math.MaxInt64 || bytes < 1 {
		return 0, fmt.Errorf("空间大小超出范围: %q", value)
	}
	return int64(bytes), nil
}

func FormatSpaceBytes(bytes int64) string {
	if bytes < 0 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []struct {
		name   string
		factor int64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	}
	for _, unit := range units {
		if bytes >= unit.factor {
			return fmt.Sprintf("%.2f %s", float64(bytes)/float64(unit.factor), unit.name)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}

func FormatSpaceLimit(bytes int64) string {
	if bytes == 0 {
		return "unlimited"
	}
	return FormatSpaceBytes(bytes)
}
