// Package schema — 装载并对外暴露 init_pitr.sql,给 Go 测试与部署共用。
//
// 单一权威源头是 internal/schema/init_pitr.sql;
// deploy/init_pitr.sql 是 symlink → 这里,让 Dockerfile 和 go:embed 吃同一份。
package schema

import _ "embed"

//go:embed init_pitr.sql
var InitSQL string
