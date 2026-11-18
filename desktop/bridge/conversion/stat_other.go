//go:build !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !linux

package conversion

import "os"

// fileIdentity：这些平台上拿不到 ctime ⇒ 没有身份凭据。
//
// 后果：StatKey 只由 (path, size, mtime) 组成，比 unix 弱（unix 还带 ctime+inode）。
// 保守起见见 keymemo.go 顶部关于「mtime 可被还原」的说明 —— 桌面端只发 macOS，
// 这条分支只是为了让包在别的平台上仍能编译。
func fileIdentity(fi os.FileInfo) (string, bool) { return "", false }
