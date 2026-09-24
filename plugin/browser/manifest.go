// Package browser 实现 Browser Use 插件（插件框架首个实例）：
// 受管组件（chrome-devtools-mcp，npm 安装 + 版本钉死）在 ~/.go-code/plugins/browser/，
// 启用时构造**私有** mcp.Manager（不进 settings.json mcpServers、不占 MCP tab——
// 对用户它是插件，底层用 MCP 只是实现细节）。
package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ID Browser Use 插件 id（组件目录名、私有 MCP 服务器名）。
const ID = "browser"

// Name 展示名。
const Name = "Browser Use"

// Description 插件简介（插件列表 UI 展示）。
const Description = "浏览器自动化：导航、点击、填表、截图、读取 a11y 快照（有头可见，登录/状态跨会话保留）。"

// entryPackage 受管安装的 npm 包名。
const entryPackage = "chrome-devtools-mcp"

// Manifest 受管组件元数据（组件目录内 .manifest.json）。
type Manifest struct {
	Version  string `json:"version"`
	NodePath string `json:"nodePath,omitempty"` // 安装时记录的 node（运行期 command）
	// SyncedFromVendor 组件是否由内置 vendor 同步而来（2026-09 内置化标记）。
	// true = node_modules 内容受 vendor 管理（同步会整体替换包目录）；false/缺失 =
	// legacy npm 安装（保留不动，仅迁移一次后置 true）。区分"组件缺失"与"版本落后"。
	SyncedFromVendor bool `json:"syncedFromVendor,omitempty"`
}

// componentDir 受管组件目录（<pluginsDir>/browser）。
func componentDir(pluginsDir string) string { return filepath.Join(pluginsDir, ID) }

// manifestPath 组件元数据文件路径。
func manifestPath(dir string) string { return filepath.Join(dir, ".manifest.json") }

// installedPackageDir 已安装 npm 包目录。
func installedPackageDir(dir string) string {
	return filepath.Join(dir, "node_modules", entryPackage)
}

// readManifest 读组件元数据（缺文件/坏 JSON 返回错误）。
func readManifest(dir string) (Manifest, error) {
	raw, err := os.ReadFile(manifestPath(dir))
	if err != nil {
		return Manifest{}, fmt.Errorf("读 manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("解析 manifest: %w", err)
	}
	return m, nil
}

// writeManifest 写组件元数据（原子：先写临时文件再 rename）。
func writeManifest(dir string, m Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := manifestPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, manifestPath(dir))
}

// resolveEntry 定位已装 chrome-devtools-mcp 的 bin 入口（require.resolve 语义：
// 读 package.json 的 bin 字段，返回绝对路径）。运行期指向已装入口，零网络。
func resolveEntry(dir string) (string, error) {
	pkgDir := installedPackageDir(dir)
	raw, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return "", fmt.Errorf("读 %s package.json: %w", entryPackage, err)
	}
	var pkg struct {
		Bin any `json:"bin"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return "", fmt.Errorf("解析 %s package.json: %w", entryPackage, err)
	}
	var binRel string
	switch v := pkg.Bin.(type) {
	case string:
		binRel = v
	case map[string]any:
		for k, val := range v {
			if k == "chrome-devtools-mcp" {
				binRel, _ = val.(string)
			}
		}
	}
	if binRel == "" {
		return "", fmt.Errorf("%s 未声明 chrome-devtools-mcp bin 入口", entryPackage)
	}
	return filepath.Join(pkgDir, binRel), nil
}

// dirExists 目录存在且为目录。
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// fileExists 文件存在。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
