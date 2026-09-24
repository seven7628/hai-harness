package main

// ego lite 安装检测 + 持久化记录（2026-09）。
//
// 问题（用户报）：本机已装 ego lite，重启应用后设置→插件仍提示「下载安装 ego lite」。
// 根因：checkEgoStatus 的 available 混同了「已安装」与「正在运行」——
//   available = cli_found && app_running
// app 没在跑（或没完成 onboarding）→ cli_found/app_running 至少一个为 false →
// available=false → 前端 egoReady=false → 显示安装按钮。用户已安装的 app 被当成未安装。
//
// 方案：把「已安装」独立出来，并按要求持久化记录：
//   1. 磁盘检测：标准位置（/Applications、~/Applications）的 ego lite.app 是否含
//      ego-browser helper（与官方 install.sh 的 is_ego_lite_app 同判据）；另可从
//      ~/.local/bin/ego-browser 符号链接回溯出非标准安装位置。
//   2. 持久化记录 ~/.go-code/ego.json：检测到安装即落盘（app_path/version/时间戳）；
//      下次启动先读记录，再用磁盘校验 —— 记录命中且路径仍有效 → 视为已安装
//      （不重复扫描、也不因 app 未运行而误判未安装）。
//   3. 记录失效清理：路径已不存在（用户卸载/移动）→ 删除记录，回到「未安装」，
//      安装按钮重新出现（装好后再次持久化记录）。
//
// 与 available 的分工（前端据此分层显示）：
//   installed   已安装（磁盘/记录命中）—— 有它就不再显示「下载安装」按钮
//   cli_found   ego-browser 命令可用（onboarding 完成后才有）
//   app_running app 主进程在跑
//   available   cli_found && app_running（可真正执行浏览器任务）

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// egoBundleName app bundle 名（与官方 install.sh 的 APP_BUNDLE_NAME 一致）。
const egoBundleName = "ego lite.app"

// egoBrowserHelperName app 内置 CLI 名（与 install.sh 的 EGO_BROWSER_HELPER_NAME 一致）。
const egoBrowserHelperName = "ego-browser"

// egoRecordFile 安装记录文件名（~/.go-code/ego.json）。
const egoRecordFile = "ego.json"

// egoInstallRecord 持久化的安装记录：检测到安装时写入；下次启动读它再校验磁盘。
// 记录只是缓存（不是真相源）——磁盘校验失败即作废，避免「卸载后仍显示已安装」。
type egoInstallRecord struct {
	AppPath     string `json:"app_path"`                // app bundle 绝对路径
	Version     string `json:"version,omitempty"`       // app 构建版本（Frameworks 版本目录名，如 0.5.0.28）
	FirstSeenAt int64  `json:"first_seen_at,omitempty"` // 首次检测到安装（unix 秒）
	LastSeenAt  int64  `json:"last_seen_at,omitempty"`  // 最近一次校验命中（unix 秒）
}

// egoInstall 一次安装检测的结果。
type egoInstall struct {
	Installed  bool   // app 已安装（磁盘检测或记录校验命中）
	AppPath    string // app bundle 路径（诊断 / 激活用）
	Version    string // app 构建版本（可能为空）
	FromRecord bool   // true = 命中持久化记录（磁盘扫描未直接命中）
}

// egoMu 保护记录读写与检测缓存。
var (
	egoMu            sync.Mutex
	egoCachedInstall *egoInstall // 检测结果缓存（TTL 内复用，避免频繁 stat/进程）
	egoInstallAt     int64       // 上次检测时间戳（纳秒）
)

// egoAppRootsOverride 测试注入：非空时替代标准 app 搜索根目录（隔离真实磁盘）。
var egoAppRootsOverride []string

// egoInstallTTL 安装检测缓存有效期（前端打开插件页会查一次；避免连续查询反复扫盘）。
const egoInstallTTL = 2 * time.Second

// egoAppSearchRoots 返回 ego lite app 的搜索根目录：/Applications + ~/Applications。
func egoAppSearchRoots() []string {
	if len(egoAppRootsOverride) > 0 {
		return egoAppRootsOverride
	}
	roots := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	return roots
}

// egoAppVersion 判断给定 app bundle 是否为有效 ego lite 安装，并返回其构建版本。
// 判据与官方 install.sh 一致：bundle 内含 ego-browser helper。
// 版本优先取 Versions/Current 解析出的实际版本（bundle 内可能残留多个旧版本目录，
// 按字典序取第一个会拿到过期版本号）。
func egoAppVersion(appPath string) (version string, ok bool) {
	if fi, err := os.Stat(appPath); err != nil || !fi.IsDir() {
		return "", false
	}
	// 1. 当前版本：Versions/Current → Versions/<version>（符号链接目标名即真实版本）
	current := filepath.Join(appPath, "Contents", "Frameworks", "*.framework", "Versions", "Current")
	if matches, err := filepath.Glob(current); err == nil {
		for _, cur := range matches {
			helper := filepath.Join(cur, "Helpers", egoBrowserHelperName)
			if fi, serr := os.Stat(helper); serr == nil && !fi.IsDir() {
				if v := versionFromCurrentLink(cur, appPath); v != "" {
					return v, true
				}
				return "", true // Current 有效但解析不出版本号
			}
		}
	}
	// 2. 无 Current（或 Current 无 helper）：退到具体版本目录（跳过 Current 别名）
	matches, err := filepath.Glob(filepath.Join(
		appPath, "Contents", "Frameworks", "*.framework", "Versions", "*", "Helpers", egoBrowserHelperName,
	))
	if err == nil {
		for _, m := range matches {
			if fi, serr := os.Stat(m); serr != nil || fi.IsDir() {
				continue
			}
			if v := versionFromHelperPath(m, appPath); v != "" {
				return v, true
			}
		}
	}
	// 3. 兜底：bundle 结构变动时 Contents 下任意位置的 ego-browser
	//（对齐 install.sh 的 find_ego_browser_in_app 宽容判定）。
	found := ""
	_ = filepath.WalkDir(filepath.Join(appPath, "Contents"), func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || d.Name() != egoBrowserHelperName {
			return nil
		}
		if fi, serr := os.Stat(p); serr == nil && !fi.IsDir() {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found != "" {
		return versionFromHelperPath(found, appPath), true
	}
	return "", false
}

// versionFromCurrentLink 解析 Versions/Current 指向的版本目录名（真实版本，如 0.5.0.28）。
func versionFromCurrentLink(currentPath, appPath string) string {
	if resolved, err := filepath.EvalSymlinks(currentPath); err == nil {
		if v := versionFromHelperPath(filepath.Join(resolved, "Helpers", egoBrowserHelperName), appPath); v != "" {
			return v
		}
	}
	return ""
}

// versionFromHelperPath 从 helper 路径解析 app 版本：.../Contents/Frameworks/X.framework/Versions/<v>/Helpers/ego-browser
// → <v>（"Current" 视为未知，交由其它匹配给具体版本）。
func versionFromHelperPath(helperPath, appPath string) string {
	rel, err := filepath.Rel(appPath, helperPath)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, p := range parts {
		if p == "Versions" && i+1 < len(parts) {
			if parts[i+1] == "Current" {
				return ""
			}
			return parts[i+1]
		}
	}
	return ""
}

// findEgoAppOnDisk 在磁盘上查找已安装的 ego lite app（标准位置 + CLI 链接回溯）。
// 返回 app 路径与版本；未找到 → ok=false。
func findEgoAppOnDisk() (appPath, version string, ok bool) {
	candidates := []string{}
	for _, root := range egoAppSearchRoots() {
		candidates = append(candidates, filepath.Join(root, egoBundleName))
		// 大小写不敏感兜底（install.sh 用 find -iname；用户可能改名 egolite.app 等）
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				if strings.EqualFold(e.Name(), egoBundleName) {
					candidates = append(candidates, filepath.Join(root, e.Name()))
				}
			}
		}
	}
	// CLI 符号链接回溯：~/.local/bin/ego-browser → <任意位置>/ego lite.app/Contents/...
	for _, cli := range egoCLICandidates() {
		if resolved, err := filepath.EvalSymlinks(cli); err == nil {
			if idx := strings.Index(resolved, ".app/"); idx > 0 {
				candidates = append(candidates, resolved[:idx+len(".app")])
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		if v, valid := egoAppVersion(c); valid {
			return c, v, true
		}
	}
	return "", "", false
}

// egoCLICandidates 返回 ego-browser 命令的候选路径（PATH + ~/.local/bin 兜底）。
func egoCLICandidates() []string {
	out := []string{}
	if p, err := exec.LookPath(egoBrowserHelperName); err == nil && p != "" {
		out = append(out, p)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, ".local", "bin", egoBrowserHelperName))
		out = append(out, filepath.Join(home, ".local", "share", "ego", "active_version_dir", "Helpers", egoBrowserHelperName))
	}
	return out
}

// egoRecordPath 安装记录文件路径（<appDataDir>/ego.json）。
func egoRecordPath() string {
	return filepath.Join(appDataDir(), egoRecordFile)
}

// loadEgoRecord 读安装记录；缺失/损坏 → nil（当作无记录，不影响检测）。
func loadEgoRecord() *egoInstallRecord {
	raw, err := os.ReadFile(egoRecordPath())
	if err != nil {
		return nil
	}
	var rec egoInstallRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil
	}
	if rec.AppPath == "" {
		return nil
	}
	return &rec
}

// saveEgoRecord 原子写安装记录（tmp + rename；权限 0600，与其它配置一致）。
// 失败静默：记录是缓存增强，写不了不影响本次检测结果（下次重新扫盘）。
func saveEgoRecord(rec egoInstallRecord) {
	dir := filepath.Dir(egoRecordPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	tmp := egoRecordPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, egoRecordPath()); err != nil {
		_ = os.Remove(tmp)
	}
}

// clearEgoRecord 删除安装记录（记录失效：app 已被卸载/移走）。
func clearEgoRecord() {
	_ = os.Remove(egoRecordPath())
}

// detectEgoInstall 检测 ego lite 是否已安装（磁盘优先，持久化记录兜底），并维护记录：
//   - 磁盘命中 → 写记录（app_path/version/时间戳；版本变化也更新）
//   - 磁盘未命中但记录路径仍有效（用户挪到非标准位置）→ 视为已安装（FromRecord）
//   - 记录路径也失效（卸载/删除）→ 清掉记录 → 未安装
//
// 结果按 egoInstallTTL 缓存（前端打开插件页/轮询查询不反复扫盘）。
func detectEgoInstall() egoInstall {
	egoMu.Lock()
	defer egoMu.Unlock()
	now := time.Now().UnixNano()
	if egoCachedInstall != nil && now-egoInstallAt < int64(egoInstallTTL) {
		return *egoCachedInstall
	}
	res := egoInstall{}
	appPath, version, found := findEgoAppOnDisk()
	rec := loadEgoRecord()
	switch {
	case found:
		res = egoInstall{Installed: true, AppPath: appPath, Version: version}
		// 持久化记录：首次检测记录 first_seen；路径/版本变化也落盘（用户升级 ego lite 后版本更新）
		next := egoInstallRecord{AppPath: appPath, Version: version, LastSeenAt: time.Now().Unix()}
		if rec != nil && rec.AppPath == appPath && rec.FirstSeenAt > 0 {
			next.FirstSeenAt = rec.FirstSeenAt
		} else {
			next.FirstSeenAt = time.Now().Unix()
		}
		if rec == nil || rec.AppPath != next.AppPath || rec.Version != next.Version || rec.FirstSeenAt != next.FirstSeenAt {
			saveEgoRecord(next)
		}
	case rec != nil:
		// 记录兜底：路径仍有效（app 在非标准位置）→ 已安装；否则记录作废
		if v, valid := egoAppVersion(rec.AppPath); valid {
			ver := v
			if ver == "" {
				ver = rec.Version
			}
			res = egoInstall{Installed: true, AppPath: rec.AppPath, Version: ver, FromRecord: true}
			saveEgoRecord(egoInstallRecord{
				AppPath:     rec.AppPath,
				Version:     ver,
				FirstSeenAt: rec.FirstSeenAt,
				LastSeenAt:  time.Now().Unix(),
			})
		} else {
			clearEgoRecord() // 已卸载/移动 → 记录失效，回到未安装（安装按钮重新出现）
		}
	default:
		res = egoInstall{}
	}
	egoCachedInstall = &res
	egoInstallAt = now
	return res
}

// resetEgoInstallCache 清检测缓存（测试用；安装完成后强制重新检测也用它）。
func resetEgoInstallCache() {
	egoMu.Lock()
	egoCachedInstall = nil
	egoInstallAt = 0
	egoMu.Unlock()
}
