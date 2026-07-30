package server

import (
	"pitr_fs/internal/pg"
	"pitr_fs/internal/revert"
	"pitr_fs/internal/txn"

	pb "pitr_fs/api/pitrd/v1"
)

const DefaultDaemonVersion = "dev"

type Config struct {
	DaemonVersion string
	Volume        string
	JFSMount      string
	FUSEMount     string
	Retention     string
	JFSMounted    bool
	FUSEMounted   bool
}

type Server struct {
	pb.UnimplementedPitrdServer

	db  *pg.DB
	mgr *txn.Manager
	rev *revert.Engine
	cfg Config
}

func New(db *pg.DB, mgr *txn.Manager, cfg Config) *Server {
	if cfg.DaemonVersion == "" {
		cfg.DaemonVersion = DefaultDaemonVersion
	}
	if cfg.Volume == "" {
		cfg.Volume = "default"
	}
	if cfg.Retention == "" {
		cfg.Retention = "compact"
	}
	return &Server{db: db, mgr: mgr, rev: revert.NewEngine(db), cfg: cfg}
}
