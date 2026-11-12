package main

// ego lite 安装检测 + 持久化记录测试（2026-09）：
//   - 磁盘检测：标准位置的 app bundle（含 ego-browser helper）→ 已安装 + 版本
//   - 持久化记录：检测到安装即落盘；下次读记录命中（app 在非标准位置也能识别）
//   - 记录失效清理：app 被卸载/移走 → 记录删除，回到未安装（安装按钮重新出现）
//   - 目录非 app / 无 helper → 不算安装
//   - egoStatusToMap 字段完整（installed 与 available 分层）
//
// 隔离：HOME + appDataDirOverride（记录落临时目录）+ egoAppRootsOverride（假 app 根）。
// 不依赖真实 /Applications、不启动真实进程。

import (
	"os"
	"path/filepath"
	"testing"
)

// mkFakeEgoApp 造一个最小 ego lite.app bundle：Contents/Frameworks/<fw>.framework/Versions/<ver>/Helpers/ego-browser
// 结构与真实 bundle 一致（install.sh 的定位方式）。version 为空则只造 Current。
func mkFakeEgoApp(t *testing.T, root, version string) string {
	t.Helper()
	app := filepath.Join(root, egoBundleName)
	helpers := filepath.Join(app, "Contents", "Frameworks", "ego Framework.framework", "Versions", version, "Helpers")
	if version == "" {
		helpers = filepath.Join(app, "Contents", "Frameworks", "ego Framework.framework", "Versions", "Current", "Helpers")
	}
	if err := os.MkdirAll(helpers, 0o755); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(helpers, egoBrowserHelperName)
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

// setupEgoTest 隔离测试环境：HOME/appDataDir/搜索根 + 清空检测缓存。
// PATH 也隔离：否则 exec.LookPath("ego-browser") 会命中真实安装，
// 经 CLI 符号链接回溯把 /Applications/ego lite.app 带进测试（污染判定）。
func setupEgoTest(t *testing.T, roots ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // 无 ego-browser → CLI 回溯不介入
	appDataDirOverride = t.TempDir()
	t.Cleanup(func() { appDataDirOverride = "" })
	egoAppRootsOverride = roots
	t.Cleanup(func() {
		egoAppRootsOverride = nil
		resetEgoInstallCache()
	})
	resetEgoInstallCache()
}

// TestDetectEgoInstallFromDisk 标准位置 app → 已安装 + 版本，并落盘记录。
func TestDetectEgoInstallFromDisk(t *testing.T) {
	root := t.TempDir()
	setupEgoTest(t, root)
	mkFakeEgoApp(t, root, "1.2.3")

	got := detectEgoInstall()
	if !got.Installed {
		t.Fatalf("应检测到已安装: %+v", got)
	}
	if got.Version != "1.2.3" {
		t.Fatalf("版本应为 1.2.3，got %q", got.Version)
	}
	if got.FromRecord {
		t.Fatal("磁盘直接命中，不应标记 FromRecord")
	}
	// 持久化记录已写盘
	rec := loadEgoRecord()
	if rec == nil {
		t.Fatal("应已持久化安装记录")
	}
	if rec.AppPath != got.AppPath {
		t.Fatalf("记录 app_path=%q，want %q", rec.AppPath, got.AppPath)
	}
	if rec.Version != "1.2.3" {
		t.Fatalf("记录 version=%q，want 1.2.3", rec.Version)
	}
	if rec.FirstSeenAt == 0 {
		t.Fatal("记录应有 first_seen_at")
	}
}

// TestEgoInstallRecordSurvivesRestart 模拟重启：磁盘扫描未命中（非标准位置），
// 但记录中的 app 路径仍有效 → 仍判已安装（用户已装的 app 不被当作未安装）。
func TestEgoInstallRecordSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	setupEgoTest(t, root) // 搜索根 = root（标准位置为空）
	mkFakeEgoApp(t, root, "9.9.9")
	if got := detectEgoInstall(); !got.Installed {
		t.Fatalf("首次应检测到安装: %+v", got)
	}
	// 把 app 挪到搜索根之外（模拟非标准安装位置）+ 清缓存 = 重启后的磁盘状态
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(elsewhere, egoBundleName)
	if err := os.Rename(filepath.Join(root, egoBundleName), moved); err != nil {
		t.Fatal(err)
	}
	// 记录指向旧路径（已失效）→ 应清理记录并判未安装
	resetEgoInstallCache()
	got := detectEgoInstall()
	if got.Installed {
		t.Fatalf("app 已移走，应由记录校验失败判未安装: %+v", got)
	}
	if loadEgoRecord() != nil {
		t.Fatal("失效记录应被清理")
	}
	// 手动把记录指向新位置（模拟记录本来就指向非标准位置）→ 应命中记录判已安装
	saveEgoRecord(egoInstallRecord{AppPath: moved})
	resetEgoInstallCache()
	got = detectEgoInstall()
	if !got.Installed || !got.FromRecord {
		t.Fatalf("记录指向有效 app 时应判已安装（FromRecord）: %+v", got)
	}
}

// TestEgoInstallRecordClearedOnUninstall 卸载（app 删除）→ 记录清理 + 未安装。
func TestEgoInstallRecordClearedOnUninstall(t *testing.T) {
	root := t.TempDir()
	setupEgoTest(t, root)
	app := mkFakeEgoApp(t, root, "3.0.0")
	if got := detectEgoInstall(); !got.Installed {
		t.Fatalf("首次应检测到安装: %+v", got)
	}
	if loadEgoRecord() == nil {
		t.Fatal("应已落盘记录")
	}
	// 卸载：删掉 app bundle
	if err := os.RemoveAll(app); err != nil {
		t.Fatal(err)
	}
	resetEgoInstallCache()
	got := detectEgoInstall()
	if got.Installed {
		t.Fatalf("卸载后不应判已安装: %+v", got)
	}
	if loadEgoRecord() != nil {
		t.Fatal("卸载后记录应被清理（安装按钮需重新出现）")
	}
}

// TestEgoAppVersionRejectsNonApp 非 app 目录 / 缺 helper → 不算安装。
func TestEgoAppVersionRejectsNonApp(t *testing.T) {
	root := t.TempDir()
	setupEgoTest(t, root)
	// 只有同名目录，没有 helper
	empty := filepath.Join(root, egoBundleName)
	if err := os.MkdirAll(filepath.Join(empty, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := egoAppVersion(empty); ok {
		t.Fatal("无 ego-browser helper 的目录不应算安装")
	}
	if got := detectEgoInstall(); got.Installed {
		t.Fatalf("无有效 app 时应判未安装: %+v", got)
	}
	// 不存在的路径
	if _, ok := egoAppVersion(filepath.Join(root, "nope.app")); ok {
		t.Fatal("不存在的路径不应算安装")
	}
}

// TestEgoAppVersionCurrentOnly helper 仅存在于 Versions/Current → 已安装且版本未知。
func TestEgoAppVersionCurrentOnly(t *testing.T) {
	root := t.TempDir()
	setupEgoTest(t, root)
	mkFakeEgoApp(t, root, "") // 仅 Current
	got := detectEgoInstall()
	if !got.Installed {
		t.Fatalf("仅 Current 结构应判已安装: %+v", got)
	}
	if got.Version != "" {
		t.Fatalf("Current 别名不应作为版本号，got %q", got.Version)
	}
}

// TestEgoAppVersionPicksCurrentNotStaleDir bundle 内残留多个版本目录（真实升级后常见）
// → 必须取 Current 指向的当前版本，不能按字典序拿过期版本号。
func TestEgoAppVersionPicksCurrentNotStaleDir(t *testing.T) {
	root := t.TempDir()
	setupEgoTest(t, root)
	app := mkFakeEgoApp(t, root, "0.4.7.4") // 旧版本目录（字典序在前）
	// 新版本目录 + Current 符号链接指向它（复刻真实 bundle 结构）
	versionsDir := filepath.Join(app, "Contents", "Frameworks", "ego Framework.framework", "Versions")
	newer := filepath.Join(versionsDir, "0.5.0.28", "Helpers")
	if err := os.MkdirAll(newer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newer, egoBrowserHelperName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(versionsDir, "Current")
	if err := os.Symlink(filepath.Join(versionsDir, "0.5.0.28"), current); err != nil {
		t.Fatal(err)
	}
	got := detectEgoInstall()
	if !got.Installed {
		t.Fatalf("应判已安装: %+v", got)
	}
	if got.Version != "0.5.0.28" {
		t.Fatalf("应取 Current 指向的当前版本 0.5.0.28（而非残留旧目录 0.4.7.4），got %q", got.Version)
	}
	if rec := loadEgoRecord(); rec == nil || rec.Version != "0.5.0.28" {
		t.Fatalf("持久化记录也应存当前版本 0.5.0.28，got %+v", rec)
	}
}

// TestEgoInstallCacheReuse 检测结果按 TTL 缓存（同一次查询不重复扫盘）。
func TestEgoInstallCacheReuse(t *testing.T) {
	root := t.TempDir()
	setupEgoTest(t, root)
	mkFakeEgoApp(t, root, "1.0.0")
	first := detectEgoInstall()
	if !first.Installed {
		t.Fatalf("应检测到安装: %+v", first)
	}
	// 缓存期内删除 app：仍返回缓存结果（未到 TTL）
	if err := os.RemoveAll(filepath.Join(root, egoBundleName)); err != nil {
		t.Fatal(err)
	}
	if cached := detectEgoInstall(); !cached.Installed {
		t.Fatal("TTL 内应复用缓存结果")
	}
	// 清缓存后重新检测 → 反映磁盘现状
	resetEgoInstallCache()
	if after := detectEgoInstall(); after.Installed {
		t.Fatalf("清缓存后应反映磁盘现状（已删除 → 未安装）: %+v", after)
	}
}

// TestEgoStatusMapHasInstalledField 状态 map 字段完整：installed 与 available 分层。
func TestEgoStatusMapHasInstalledField(t *testing.T) {
	st := egoStatus{Installed: true, Available: false, CliFound: false, AppRunning: false, AppPath: "/Applications/ego lite.app"}
	m := egoStatusToMap(st)
	if v, ok := m["installed"].(bool); !ok || !v {
		t.Fatalf("installed 应为 true: %v", m["installed"])
	}
	if v, ok := m["available"].(bool); !ok || v {
		t.Fatalf("available 应为 false（未运行）: %v", m["available"])
	}
	if m["app_path"] != "/Applications/ego lite.app" {
		t.Fatalf("app_path 应回传: %v", m["app_path"])
	}
	for _, k := range []string{"installed", "available", "app_running", "cli_found", "app_path", "version", "error"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("状态缺字段 %s: %v", k, m)
		}
	}
}
