//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package conversion

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity 文件身份凭据：设备号 + inode + **ctime**（inode 变更时间）。
//
// ctime 是「内容没变」的判据里最关键的一位：mtime 用户能还原（cp -p / 解压），
// ctime 不能 —— 任何写入都会更新它，换 inode 也会更新它。
func fileIdentity(fi os.FileInfo) (string, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d|%d|%d.%d", st.Dev, st.Ino, st.Ctimespec.Sec, st.Ctimespec.Nsec), true
}
