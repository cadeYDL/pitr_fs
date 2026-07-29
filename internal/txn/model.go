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
	ID          int64
	VersionHash string
	ScopePath   string
	ParentID    *int64
	State       string
	Command     string
	Message     string
	CreatedAt   time.Time
	ClosedAt    *time.Time
}
