package txn

import "errors"

var (
	ErrScopeActive       = errors.New("scope 已有 active transaction")
	ErrTxnNotFound       = errors.New("transaction 不存在")
	ErrIllegalTransit    = errors.New("非法 transaction 状态转换")
	ErrInvalidScope      = errors.New("scope 必须是绝对路径")
	ErrTimeBeforeHistory = errors.New("目标时间早于最早可回溯版本")
	errHashCollision     = errors.New("version hash 冲突")
)
