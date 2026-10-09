package codemode

// sensitive.go：宿主侧读盘的路径策略 —— image() 是全仓唯一「由宿主进程读脚本指定路径」
// 的地方（对抗复核 F1/R2）。
//
// 为什么必须有：沙箱与文件工具都拒绝 ~/.go-code（settings.json 可能含 api_key、
// keys.json 是密钥），但 image() 走的是**宿主**的 os.ReadFile —— 不在这里判，脚本就能
// 让宿主把受保护目录里的图片直接送进模型上下文（实测可读，且错误文案还泄漏文件是否
// 存在与多大）。
//
// 语义与既有 4 份副本同步（本文件是第 5 份）：
//
//	tools/builtin/builtin.go        denyHarnessConfigPath
//	desktop/bridge/main_helpers.go  denyHarnessConfigPath
//	runtime/python.go               （注释指明同保护对象）
//	agents/fileref.go               （同上）
//	sandbox/seatbelt_common.go      defaultSensitivePaths + defaultOpenSubpaths
//
// 常量（受保护目录名、放行子树）必须与它们一致；彻底的办法是把策略提到一个共享包，
// 属后续清理（本仓既有债：4 份副本已在规格 §7.18 记录）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// harnessConfigDirName 受保护目录（home 下）—— 与 sandbox.defaultSensitivePaths 同源。
const harnessConfigDirName = ".go-code"

// harnessConfigOpenSubdirs 受管放行子树 —— 与 sandbox.defaultOpenSubpaths 同源：
// plugins（浏览器组件）与 runtime（受管 Python 运行时）只含公开内容，沙箱内也要能读写，
// 故此处同样放行。
var harnessConfigOpenSubdirs = []string{"plugins", "runtime"}

// denyHostRead 判断宿主侧读该路径是否被策略拒绝；"" = 放行，否则返回可直接给模型看的
// 拒绝原因（调用方补 "codemode image: " 前缀）。
//
// 只做**字符串层面**的判断（Clean + Rel），不碰文件系统：符号链接在这里看不出来 ——
// 攻击面不在脚本侧（脚本能读到的路径本来就受沙箱限制；这里防的是「宿主替脚本读」），
// 故不做 EvalSymlinks（那还会引入 TOCTOU 与不存在路径的额外分支）。
func denyHostRead(abs string) string {
	if abs == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	cfgDir := filepath.Join(home, harnessConfigDirName)
	rel, err := filepath.Rel(cfgDir, filepath.Clean(abs))
	if err != nil {
		return ""
	}
	// Rel 避免前缀陷阱：~/.go-codeevil/x 的 rel 是 ../.go-codeevil/x ⇒ 不在保护范围内。
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	for _, sub := range harnessConfigOpenSubdirs {
		if rel == sub || strings.HasPrefix(rel, sub+string(filepath.Separator)) {
			return ""
		}
	}
	return fmt.Sprintf("%q is inside the harness config directory (%s), which tools never read "+
		"(it can contain API keys) - copy the image somewhere else first", filepath.Clean(abs), cfgDir)
}
