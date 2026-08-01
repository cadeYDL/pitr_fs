// Package buildinfo 提供通过 Go linker 注入的构建版本信息。
package buildinfo

import "fmt"

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func Full() string {
	return fmt.Sprintf("%s (commit=%s, built=%s)", Version, Commit, BuildDate)
}
