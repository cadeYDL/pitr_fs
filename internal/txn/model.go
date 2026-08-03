package txn

import "time"

const (
	StateRoot       = "root"
	StateActive     = "active"
	StateAuto       = "auto"
	StateCommitted  = "committed"
	StateRolledBack = "rolled_back"
)

type Txn struct {
	ID             int64
	WorkspaceID    int64
	VersionHash    string
	ScopePath      string
	ParentID       *int64
	State          string
	Command        string
	Message        string
	PosixOp        string
	ProcessCommand string
	ActorUID       int64
	ActorGID       int64
	ActorPID       int64
	ActorName      string
	ChangeSummary  string
	CreatedAt      time.Time
	ClosedAt       *time.Time
}

type VersionMetadata struct {
	PosixOp        string
	ProcessCommand string
	ActorUID       int64
	ActorGID       int64
	ActorPID       int64
	ActorName      string
}
