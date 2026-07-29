package txn

import "errors"

var (
	ErrScopeActive    = errors.New("scope 已有 active transaction")
	ErrTxnNotFound    = errors.New("transaction 不存在")
	ErrIllegalTransit = errors.New("非法 transaction 状态转换")
	ErrInvalidScope   = errors.New("scope 必须是绝对路径")
	errHashCollision  = errors.New("version hash 冲突")
)
