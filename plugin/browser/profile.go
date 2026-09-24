package browser

import (
	"fmt"
	"os"
	"path/filepath"
)

// 移植 @amaster.ai/pi-browser-use profile.js 的**权限检测**部分，但**不做自动移走重建**：
// macOS APFS 拒绝 rename 只读目录（实测 permission denied，与沙箱无关）——移走方案在
// macOS 上基本失效。改为统一 fail-fast + 修复提示（受管默认目录给「删除重建」动作）。
// 检测项：profile 目录可写、Default 子目录可写、关键文件（Local State / Default/Preferences）
// 可读写——任一不可用 → Chrome 会弹「无法读取偏好设置」并静默丢弃偏好变更。

// requiredProfileFiles Chrome 必须可读写的文件。
var requiredProfileFiles = []string{"Local State", filepath.Join("Default", "Preferences")}

// isReadableWritable 文件可读写探测（OpenFile RDWR，不创建）。
func isReadableWritable(p string) bool {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// isUsableDir 目录可写探测（创建 + 删除探测文件；只读目录返回 false）。
func isUsableDir(p string) bool {
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return false
	}
	probe := filepath.Join(p, ".go-code-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}

// findInaccessiblePath 返回 profile 中首个不可读写路径（目录 / Default 子目录 / 关键文件）。
// 空串 = 全部可用。
func findInaccessiblePath(userDataDir string) string {
	if !isUsableDir(userDataDir) {
		return userDataDir
	}
	defaultDir := filepath.Join(userDataDir, "Default")
	if info, err := os.Stat(defaultDir); err == nil && info.IsDir() && !isUsableDir(defaultDir) {
		return defaultDir
	}
	for _, f := range requiredProfileFiles {
		p := filepath.Join(userDataDir, f)
		if _, err := os.Stat(p); err == nil && !isReadableWritable(p) {
			return p
		}
	}
	return ""
}

// ensureProfileAccessible 校验 Chrome profile 可读写；不可用 → fail-fast 报修复提示
// （受管默认目录含「删除重建」动作——它会自动重建；自定义目录提示修所有权/权限）。
// profile 不存在 = 全新，跳过。
func ensureProfileAccessible(userDataDir string, managed bool) error {
	if userDataDir == "" {
		return nil
	}
	if _, err := os.Stat(userDataDir); err != nil {
		return nil // 全新 profile，无需校验
	}
	inacc := findInaccessiblePath(userDataDir)
	if inacc == "" {
		return nil
	}
	if managed {
		return fmt.Errorf("Chrome profile %q 不可读写（%q）。删除该受管目录后重启会重建（登录状态会重置）。", userDataDir, inacc)
	}
	return fmt.Errorf("Chrome profile %q 不可读写（%q）。请修复所有权/读写权限后重试。", userDataDir, inacc)
}
