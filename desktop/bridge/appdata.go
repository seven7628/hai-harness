package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// 应用级数据目录约定：会话 / 事件 / 指标 / 定时任务 / gocache 都是 APP 层面（跨 workspace），
// 统一落在 ~/.go-code/ 下；工作区目录（{ws}/.go-code/）仅保留 skills 管理与 git worktree。
// 因此这里把 sessions/events 从「{ws}/.go-code/…」迁到「~/.go-code/sessions|events/<wsKey>/…」，
// 每个 workspace 一个子目录（按稳定的 workspaceKey），保留按工作区隔离/枚举的既有语义。

// appDataDir 全局应用数据目录（~/.go-code，跨 workspace）。home 解析失败回退当前目录。
func appDataDir() string {
	if appDataDirOverride != "" {
		return appDataDirOverride
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".go-code")
}

// appDataDirOverride 测试注入：非空时 appDataDir() 返回该值（把 APP 级存储隔离到临时目录，
// 避免测试写真实 ~/.go-code 且与读取配置的 HOME 解耦）。仅测试使用，生产恒为空。
var appDataDirOverride string

// workspaceKey 把 workspace 绝对路径稳定映射为一个安全的子目录名：
// 可读扁平化（路径分段与非法字符转下划线）+ 短哈希后缀（唯一、防超长截断碰撞）。
// 同一路径恒映射到同一 key；不同路径绝不冲突（哈希保证）。
// 2026-08-22：先按物理路径规范化（EvalSymlinks）—— macOS 上 /Users 是 /private/Users 的
// 符号链接，同一目录的不同拼写此前映射到不同 key → 会话/事件目录错位（重启后"历史丢失"/
// 项目重复出现）。解析失败（路径不存在等）回退原拼写：同一拼写仍恒稳定，不产生新错。
func workspaceKey(wsPath string) string {
	abs, err := filepath.Abs(wsPath)
	if err != nil {
		abs = filepath.Clean(wsPath)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	// 可读扁平化：保留字母/数字/./-，其余（含 / 与盘符冒号）压成下划线，连续下划线合并
	var b strings.Builder
	underscore := true // 前导不落 _
	for _, r := range abs {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' {
			b.WriteRune(r)
			underscore = false
		} else if !underscore {
			b.WriteByte('_')
			underscore = true
		}
	}
	key := strings.Trim(b.String(), "_")
	if key == "" {
		key = "ws"
	}
	// 短哈希保证唯一（同名目录截断后仍不冲突）
	sum := sha256.Sum256([]byte(abs))
	hash := hex.EncodeToString(sum[:4])
	if len(key) > 80 {
		key = key[:80]
	}
	return key + "-" + hash
}

// wsSessionsDir 某 workspace 的会话存储目录（APP 级，~/.go-code/sessions/<wsKey>）。
func wsSessionsDir(wsPath string) string {
	return filepath.Join(appDataDir(), "sessions", workspaceKey(wsPath))
}

// wsEventsDir 某 workspace 的事件日志目录（APP 级，~/.go-code/events/<wsKey>）。
func wsEventsDir(wsPath string) string {
	return filepath.Join(appDataDir(), "events", workspaceKey(wsPath))
}
