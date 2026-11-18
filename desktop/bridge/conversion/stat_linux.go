//go:build linux

package conversion

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity 文件身份凭据：设备号 + inode + **ctime**（inode 变更时间）。
// （Linux 的字段名与 BSD 不同：Ctimspec → Ctim，故分文件实现。）
func fileIdentity(fi os.FileInfo) (string, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d|%d|%d.%d", st.Dev, st.Ino, st.Ctim.Sec, st.Ctim.Nsec), true
}
