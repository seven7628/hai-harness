//go:build unix

package session

import (
	"os"
	"syscall"
)

// mapFileReadOnly 把整个文件映射成只读字节切片（零拷贝）。
func mapFileReadOnly(f *os.File, size int64) ([]byte, error) {
	if size <= 0 {
		return nil, os.ErrInvalid
	}
	return syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
}

func unmapFile(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return syscall.Munmap(b)
}
