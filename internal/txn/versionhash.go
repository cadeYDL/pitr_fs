package txn

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func newVersionHash() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("生成 version hash: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// NewVersionHash 返回 12 位十六进制版本号。需要自行持久化版本的内部组件
// （例如 revert 引擎）使用同一生成规则。
func NewVersionHash() (string, error) {
	return newVersionHash()
}
