package server

import (
	"context"
	"sync"

	"pitr_fs/internal/pg"
	"pitr_fs/internal/revert"
	"pitr_fs/internal/txn"

	pb "pitr_fs/api/pitrd/v1"
)

const DefaultDaemonVersion = "dev"

type Config struct {
	DaemonVersion           string
	Volume                  string
	JFSMount                string
	FUSEMount               string
	MountRoot               string
	JFSMounted              bool
	FUSEMounted             bool
	Volumes                 []VolumeConfig
	MountFunc               func(context.Context, string) error
	UmountFunc              func(context.Context) error
	ForceUmountFunc         func(context.Context) error
	QuiesceFunc             func(bool)
	DiscardWritesFunc       func(context.Context) (int, error)
	UpgradeDiscardRequested func() bool
}

// VolumeConfig 描述 recover/status 可管理的一个卷。每卷可以使用独立的
// PostgreSQL 元数据数据库;事务 RPC 仍由 Server 的主 db/mgr 处理。
type VolumeConfig struct {
	Name        string
	JFSMount    string
	FUSEMount   string
	JFSMounted  bool
	FUSEMounted bool
	DB          *pg.DB
}

type Server struct {
	pb.UnimplementedPitrdServer

	db      *pg.DB
	mgr     *txn.Manager
	rev     *revert.Engine
	cfg     Config
	volumes []VolumeConfig

	lifecycleMu sync.Mutex
}

func New(db *pg.DB, mgr *txn.Manager, cfg Config) *Server {
	if cfg.DaemonVersion == "" {
		cfg.DaemonVersion = DefaultDaemonVersion
	}
	if cfg.Volume == "" {
		cfg.Volume = "default"
	}
	if cfg.MountRoot == "" {
		cfg.MountRoot = "/"
	}
	volumes := cfg.Volumes
	if len(volumes) == 0 {
		volumes = []VolumeConfig{{
			Name:        cfg.Volume,
			JFSMount:    cfg.JFSMount,
			FUSEMount:   cfg.FUSEMount,
			JFSMounted:  cfg.JFSMounted,
			FUSEMounted: cfg.FUSEMounted,
			DB:          db,
		}}
	}
	return &Server{
		db: db, mgr: mgr,
		rev:     revert.NewEngine(db, revert.WithMountPath(cfg.FUSEMount)),
		cfg:     cfg,
		volumes: volumes,
	}
}
