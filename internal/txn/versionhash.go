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
