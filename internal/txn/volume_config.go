package txn

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// VolumeMountConfig 是由 init 写入、供 daemon 重启恢复的全局卷配置。
type VolumeMountConfig struct {
	VolumeName string
	FUSEMount  string
	Retention  string
}

func (m *Manager) LoadVolumeMountConfig(
	ctx context.Context,
	volumeName string,
) (*VolumeMountConfig, error) {
	out := new(VolumeMountConfig)
	err := m.db.QueryRow(ctx, `
		SELECT volume_name, fuse_mount, retention
		  FROM pitr_volume_config
		 WHERE volume_name=$1`, volumeName).Scan(
		&out.VolumeName, &out.FUSEMount, &out.Retention)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取卷挂载配置: %w", err)
	}
	return out, nil
}

func (m *Manager) SaveVolumeMountConfig(
	ctx context.Context,
	config VolumeMountConfig,
) error {
	_, err := m.db.Exec(ctx, `
		INSERT INTO pitr_volume_config (volume_name, fuse_mount, retention)
		VALUES ($1,$2,$3)
		ON CONFLICT (volume_name) DO UPDATE
		SET fuse_mount=EXCLUDED.fuse_mount,
		    retention=EXCLUDED.retention,
		    updated_at=now()`,
		config.VolumeName, config.FUSEMount, config.Retention)
	if err != nil {
		return fmt.Errorf("保存卷挂载配置: %w", err)
	}
	return nil
}
