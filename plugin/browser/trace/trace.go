// Package trace 提供 Browser Use 全链路耗时日志（诊断「全工具慢」用）。
// 状态：**已停用**（Browser Use 已稳定，不再写 trace 文件，避免占用磁盘）。
// 保留 API 签名（Log/Phase/File）兼容既有调用点；函数体为空实现（no-op），
// 不写 ~/.go-code/browser_trace.log、不写 stderr。如需重新启用诊断，恢复 write 逻辑即可。
package trace

import (
	"os"
	"path/filepath"
	"time"
)

// HomeDir 返回 trace 文件目录（与 bridge startup.log 同目录 ~/.go-code）。
func HomeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".go-code")
	}
	return os.TempDir()
}

// File 返回 trace 日志文件路径（已停用，仅保留 API）。
func File() string { return filepath.Join(HomeDir(), "browser_trace.log") }

// Log 记一条全量链路日志。**已停用：no-op**（不写文件、不写 stderr）。
func Log(_ string, _ ...any) {}

// Phase 记一条带耗时的阶段日志。**已停用：no-op**。
func Phase(_ string, _ time.Time) {}
