// Package workspace 管理工作空间及其 POSIX 挂载入口。
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pitr_fs/internal/pg"
)

const DefaultName = "default"

var (
	ErrNotFound      = errors.New("workspace 不存在")
	ErrInvalidName   = errors.New("workspace 名称无效")
	ErrMountConflict = errors.New("workspace 挂载点冲突")
	validName        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

type Workspace struct {
	ID          int64
	Name        string
	VolumeName  string
	BackendPath string
	CreatedAt   time.Time
}

type Mount struct {
	WorkspaceID int64
	Path        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type ResolvedMount struct {
	Workspace Workspace
	MountPath string
	Scope     string
}

// Catalog 把 workspace 创建、版本基线和挂载路由隐藏在一个持久化接口后。
type Catalog struct {
	db *pg.DB
}

func NewCatalog(db *pg.DB) *Catalog {
	return &Catalog{db: db}
}

type scanner interface {
	Scan(...any) error
}

func scanWorkspace(row scanner) (Workspace, error) {
	var out Workspace
	err := row.Scan(&out.ID, &out.Name, &out.VolumeName, &out.BackendPath,
		&out.CreatedAt)
	return out, err
}

func (c *Catalog) GetByName(ctx context.Context, name string) (Workspace, error) {
	out, err := scanWorkspace(c.db.QueryRow(ctx, `
		SELECT id,name,volume_name,backend_path,created_at
		  FROM pitr_workspace WHERE name=$1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("读取 workspace %s: %w", name, err)
	}
	return out, nil
}

func (c *Catalog) GetByID(ctx context.Context, id int64) (Workspace, error) {
	out, err := scanWorkspace(c.db.QueryRow(ctx, `
		SELECT id,name,volume_name,backend_path,created_at
		  FROM pitr_workspace WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("读取 workspace %d: %w", id, err)
	}
	return out, nil
}

func (c *Catalog) Ensure(
	ctx context.Context,
	name string,
	volumeName string,
) (Workspace, error) {
	name = strings.TrimSpace(name)
	volumeName = strings.TrimSpace(volumeName)
	if !validName.MatchString(name) {
		return Workspace{}, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if volumeName == "" {
		return Workspace{}, errors.New("JuiceFS 卷名不能为空")
	}
	backendPath := path.Join("/.pitr/workspaces", name)
	if name == DefaultName {
		backendPath = "/"
	}

	var result Workspace
	err := c.db.InTx(ctx, func(tx pg.Tx) error {
		var err error
		result, err = scanWorkspace(tx.QueryRow(ctx, `
			SELECT id,name,volume_name,backend_path,created_at
			  FROM pitr_workspace WHERE name=$1 FOR UPDATE`, name))
		if err == nil {
			if result.VolumeName != volumeName {
				if name != DefaultName || result.VolumeName != DefaultName {
					return fmt.Errorf("workspace %s 已绑定卷 %s", name,
						result.VolumeName)
				}
				if err := tx.QueryRow(ctx, `
					UPDATE pitr_workspace SET volume_name=$2 WHERE id=$1
					RETURNING id,name,volume_name,backend_path,created_at`,
					result.ID, volumeName).Scan(
					&result.ID, &result.Name, &result.VolumeName,
					&result.BackendPath, &result.CreatedAt); err != nil {
					return err
				}
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO pitr_workspace(name,volume_name,backend_path)
			VALUES ($1,$2,$3)
			RETURNING id,name,volume_name,backend_path,created_at`,
			name, volumeName, backendPath).Scan(
			&result.ID, &result.Name, &result.VolumeName,
			&result.BackendPath, &result.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pitr_config(workspace_id,scope_path,history_limit)
			VALUES ($1,'/',100)`, result.ID); err != nil {
			return err
		}
		for attempt := 0; attempt < 3; attempt++ {
			hash, err := revisionHash()
			if err != nil {
				return err
			}
			var id int64
			err = tx.QueryRow(ctx, `
				INSERT INTO pitr_txn(
					workspace_id,version_hash,scope_path,state,command)
				VALUES ($1,$2,'/','root','init')
				ON CONFLICT (version_hash) DO NOTHING RETURNING id`,
				result.ID, hash).Scan(&id)
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		return errors.New("生成 workspace 根版本失败")
	})
	if err != nil {
		return Workspace{}, fmt.Errorf("确保 workspace %s: %w", name, err)
	}
	return result, nil
}

func revisionHash() (string, error) {
	var value [6]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (c *Catalog) AddMount(ctx context.Context, workspaceID int64, value string) error {
	value = path.Clean(value)
	if !path.IsAbs(value) || value == "/" {
		return fmt.Errorf("挂载点必须是非根绝对路径: %q", value)
	}
	return c.db.InTx(ctx, func(tx pg.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pitr_workspace WHERE id=$1)`,
			workspaceID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: %d", ErrNotFound, workspaceID)
		}
		var conflict string
		err := tx.QueryRow(ctx, `
			SELECT fuse_mount FROM pitr_workspace_mount
			 WHERE fuse_mount<>$1
			   AND (fuse_mount LIKE rtrim($1,'/') || '/%'
			        OR $1 LIKE rtrim(fuse_mount,'/') || '/%')
			 LIMIT 1`, value).Scan(&conflict)
		if err == nil {
			return fmt.Errorf("%w: %s 与 %s 重叠", ErrMountConflict,
				value, conflict)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO pitr_workspace_mount(workspace_id,fuse_mount)
			VALUES ($1,$2)
			ON CONFLICT (workspace_id,fuse_mount) DO UPDATE
			   SET updated_at=now()`, workspaceID, value)
		return err
	})
}

func (c *Catalog) RemoveMount(ctx context.Context, value string) error {
	value = path.Clean(value)
	result, err := c.db.Exec(ctx,
		"DELETE FROM pitr_workspace_mount WHERE fuse_mount=$1", value)
	if err != nil {
		return fmt.Errorf("删除 workspace 挂载 %s: %w", value, err)
	}
	if result == 0 {
		return fmt.Errorf("%w: mount %s", ErrNotFound, value)
	}
	return nil
}

func (c *Catalog) Mounts(ctx context.Context, workspaceID int64) ([]Mount, error) {
	rows, err := c.db.Query(ctx, `
		SELECT workspace_id,fuse_mount,created_at,updated_at
		  FROM pitr_workspace_mount WHERE workspace_id=$1
		 ORDER BY fuse_mount`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mount
	for rows.Next() {
		var item Mount
		if err := rows.Scan(&item.WorkspaceID, &item.Path,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (c *Catalog) List(ctx context.Context) ([]Workspace, error) {
	rows, err := c.db.Query(ctx, `
		SELECT id,name,volume_name,backend_path,created_at
		  FROM pitr_workspace ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		item, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (c *Catalog) ResolveMount(ctx context.Context, value string) (ResolvedMount, error) {
	value = path.Clean(value)
	var out ResolvedMount
	err := c.db.QueryRow(ctx, `
		SELECT w.id,w.name,w.volume_name,w.backend_path,w.created_at,m.fuse_mount
		  FROM pitr_workspace_mount m
		  JOIN pitr_workspace w ON w.id=m.workspace_id
		 WHERE $1=m.fuse_mount OR $1 LIKE rtrim(m.fuse_mount,'/') || '/%'
		 ORDER BY length(m.fuse_mount) DESC LIMIT 1`, value).Scan(
		&out.Workspace.ID, &out.Workspace.Name, &out.Workspace.VolumeName,
		&out.Workspace.BackendPath, &out.Workspace.CreatedAt, &out.MountPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResolvedMount{}, fmt.Errorf("%w: path %s", ErrNotFound, value)
	}
	if err != nil {
		return ResolvedMount{}, err
	}
	out.Scope = strings.TrimPrefix(value, out.MountPath)
	if out.Scope == "" {
		out.Scope = "/"
	}
	out.Scope = path.Clean("/" + strings.TrimPrefix(out.Scope, "/"))
	return out, nil
}
