package pg

import "errors"

var (
	ErrEmptyDSN       = errors.New("PostgreSQL DSN 不能为空")
	ErrInvalidSetting = errors.New("非法 PostgreSQL session setting")
)
