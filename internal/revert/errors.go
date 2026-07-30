package revert

import "errors"

var (
	ErrInvalidHash   = errors.New("非法 version hash")
	ErrTargetMissing = errors.New("目标版本不存在")
	ErrTargetState   = errors.New("目标版本状态不可 revert")
	ErrActiveScope   = errors.New("目标范围存在 active 事务")
)
