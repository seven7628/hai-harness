package builtin

import (
	"os"

	"github.com/seven7628/hai-harness/internal/atomicfile"
)

// 文件写并发保护的**薄壳**：实现已提取到 internal/atomicfile（PR-05a0）。
// 只保留**仍被调用**的四个名字（lockPath/replaceLocked/appendTo 及 edit_file 用的组合），
// 新代码直接用 atomicfile.Replace / Append，不要再往这里加逻辑。
//
// 语义（与提取前逐字一致）：
//   - 覆盖类写入（write_file 覆盖 / edit_file）走 temp+rename 原子替换，写前 flock 串行化；
//   - 追加类写入（write_file append）走 appendTo：flock + O_APPEND 单次写。
// syscall.Flock 为 POSIX 建议锁（darwin/linux），Windows 需另行实现。

func lockPath(p string) (*os.File, error) { return atomicfile.LockPath(p) }

func replaceLocked(lock *os.File, p, content string) error {
	return atomicfile.ReplaceLocked(lock, p, content)
}

func appendTo(p, content string) error { return atomicfile.Append(p, content) }
