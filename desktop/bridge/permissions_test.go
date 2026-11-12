package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadComputerApprovedApps Computer Use 已授权 App 集合解析（统一授权门禁）。
// 唯一来源：permissions.computer_approved_apps（旧 allowed_apps open 白名单已随
// 「权限」设置页移除——2026-09 决策：不再兜底，授权只认新字段）。
func TestLoadComputerApprovedApps(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离：走 homeCfgPath
	home := os.Getenv("HOME")
	cfgFile := filepath.Join(home, ".go-code", "settings.json")

	// 无文件 → 空集合（安全默认：全部拒绝）
	if got := loadComputerApprovedApps(); len(got) != 0 {
		t.Fatalf("无 settings 应为空集合: %v", got)
	}
	if err := os.MkdirAll(filepath.Dir(cfgFile), 0o755); err != nil {
		t.Fatal(err)
	}
	// 坏 JSON → 空集合
	if err := os.WriteFile(cfgFile, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadComputerApprovedApps(); len(got) != 0 {
		t.Fatalf("坏配置应为空集合: %v", got)
	}
	// 仅 allowed_apps（旧配置残留）→ 不兜底（授权已移除该来源）
	cfg := map[string]any{"permissions": map[string]any{"allowed_apps": []string{"Calendar"}}}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadComputerApprovedApps(); len(got) != 0 {
		t.Fatalf("allowed_apps 不应再兜底: %v", got)
	}
	// computer_approved_apps 数组解析
	cfg = map[string]any{"permissions": map[string]any{
		"allowed_apps":           []string{"Calendar"},
		"computer_approved_apps": []string{"提醒事项", "WPS"},
	}}
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadComputerApprovedApps()
	if len(got) != 2 || !got["提醒事项"] || !got["WPS"] || got["Calendar"] {
		t.Fatalf("computer_approved_apps 解析错: %v", got)
	}
	// 显式空数组 = 用户清空授权 → 空集合（全拒安全默认）
	cfg = map[string]any{"permissions": map[string]any{"computer_approved_apps": []string{}}}
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadComputerApprovedApps(); len(got) != 0 {
		t.Fatalf("显式空授权应为空集合: %v", got)
	}
	// 无 permissions 段 → 空集合
	if err := os.WriteFile(cfgFile, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadComputerApprovedApps(); len(got) != 0 {
		t.Fatalf("无 permissions 段应为空集合: %v", got)
	}
}

// TestComputerAppsGuide 系统提示词的已授权 App 动态清单段：
// 列出授权名（排序稳定），并引导模型用准确显示名唤起/激活（用户口语简称≠授权名）。
func TestComputerAppsGuide(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := os.Getenv("HOME")
	cfgFile := filepath.Join(home, ".go-code", "settings.json")
	if err := os.MkdirAll(filepath.Dir(cfgFile), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCfg := func(perms map[string]any) {
		b, _ := json.Marshal(map[string]any{"permissions": perms})
		if err := os.WriteFile(cfgFile, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 空授权 → 明示「未授权任何 App」，引导去设置添加
	writeCfg(map[string]any{"computer_approved_apps": []string{}})
	out := computerAppsGuide()
	if !strings.Contains(out, "none yet") || !strings.Contains(out, "Approved Apps") {
		t.Fatalf("空授权清单应引导添加: %s", out)
	}

	// 有授权 → 列表出现（排序稳定）+ 映射引导（WPS → WPS Office 示例）
	writeCfg(map[string]any{"computer_approved_apps": []string{"提醒事项", "WPS Office", "微信"}})
	out = computerAppsGuide()
	for _, want := range []string{"WPS Office", "提醒事项", "微信", "Only these apps can be opened/activated"} {
		if !strings.Contains(out, want) {
			t.Errorf("清单缺 %q；输出: %s", want, out)
		}
	}
	// WPS 简称 → WPS Office 映射引导（用户口语与授权名差异的显式示例）
	if !strings.Contains(out, `"WPS Office"`) {
		t.Errorf("清单应含 WPS→WPS Office 映射示例；输出: %s", out)
	}
	// 两次调用输出一致（map 无序 → 排序稳定，防提示词抖动）
	writeCfg(map[string]any{"computer_approved_apps": []string{"微信", "WPS Office", "提醒事项"}})
	if out2 := computerAppsGuide(); out2 != out {
		t.Errorf("清单应排序稳定: %q != %q", out, out2)
	}
}
