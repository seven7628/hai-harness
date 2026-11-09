//go:build !unix

package session

import (
	"errors"
	"os"
)

// 非 unix 平台不做映射：恢复路径自动回退到拼装路径（功能一致，只是慢）。
func mapFileReadOnly(*os.File, int64) ([]byte, error) {
	return nil, errors.New("mmap unsupported")
}

func unmapFile([]byte) error { return nil }
