package main

// GO-11 回归：cron 设置的三态语义（未设置 ≠ 显式 false）。
//
// 旧实现把 settings.json 的 cron 段 Unmarshal 进**零值**结构体，于是
// 「缺 enabled 键」与「enabled:false」不可区分 —— 升级/手写一份只带 auto_clean 的配置
// 就等于静默关闭定时任务（CronCreate 们从模型工具表里消失，用户没有任何提示）。

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCronSettings 在隔离的 HOME 下写 settings.json（content 为整文件内容）。
func writeCronSettings(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows：os.UserHomeDir 读 USERPROFILE
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// TestCronSettingsMissingKeyKeepsDefaults 是本条的核心回归：cron 段存在但缺键时
// 必须落回「默认全开」，不能被零值当成显式关闭。
func TestCronSettingsMissingKeyKeepsDefaults(t *testing.T) {
	cases := []struct {
		name          string
		settings      string
		wantEnabled   bool
		wantAutoClean bool
	}{
		{
			name:          "只写 auto_clean（升级场景）",
			settings:      `{"cron":{"auto_clean":true}}`,
			wantEnabled:   true,
			wantAutoClean: true,
		},
		{
			name:          "只写 enabled",
			settings:      `{"cron":{"enabled":true}}`,
			wantEnabled:   true,
			wantAutoClean: true,
		},
		{
			name:          "空 cron 段",
			settings:      `{"cron":{}}`,
			wantEnabled:   true,
			wantAutoClean: true,
		},
		{
			name:          "显式 false 必须生效",
			settings:      `{"cron":{"enabled":false,"auto_clean":false}}`,
			wantEnabled:   false,
			wantAutoClean: false,
		},
		{
			name:          "一半显式一半缺省",
			settings:      `{"cron":{"enabled":false}}`,
			wantEnabled:   false,
			wantAutoClean: true,
		},
		{
			name:          "JSON null 视同未设置",
			settings:      `{"cron":{"enabled":null,"auto_clean":null}}`,
			wantEnabled:   true,
			wantAutoClean: true,
		},
		{
			name:          "坏 JSON 回默认全开",
			settings:      `{"cron":{"enabled":`,
			wantEnabled:   true,
			wantAutoClean: true,
		},
		{
			name:          "没有 cron 段",
			settings:      `{"provider":{}}`,
			wantEnabled:   true,
			wantAutoClean: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeCronSettings(t, tc.settings)
			cs := loadCronSettings()
			if got := cs.enabledOn(); got != tc.wantEnabled {
				t.Fatalf("enabledOn() = %v, want %v (settings=%s)", got, tc.wantEnabled, tc.settings)
			}
			if got := cs.autoCleanOn(); got != tc.wantAutoClean {
				t.Fatalf("autoCleanOn() = %v, want %v (settings=%s)", got, tc.wantAutoClean, tc.settings)
			}
		})
	}
}

// TestCronSettingsMissingFile 没有 settings.json（全新用户）→ 默认全开。
func TestCronSettingsMissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cs := loadCronSettings()
	if !cs.enabledOn() || !cs.autoCleanOn() {
		t.Fatalf("缺文件时必须默认全开，得到 enabled=%v auto_clean=%v", cs.enabledOn(), cs.autoCleanOn())
	}
}

// TestCronSettingsZeroValueTreatedAsDefault 零值 cronSettings{} 的读侧兜底：
// 任何手写结构体（测试注入 / 未来新调用点）都不会被判成「用户关掉了」。
func TestCronSettingsZeroValueTreatedAsDefault(t *testing.T) {
	var cs cronSettings
	if !cs.enabledOn() || !cs.autoCleanOn() {
		t.Fatalf("零值必须回落默认全开，得到 enabled=%v auto_clean=%v", cs.enabledOn(), cs.autoCleanOn())
	}
	off := false
	explicit := cronSettings{Enabled: &off}
	if explicit.enabledOn() {
		t.Fatal("显式 false 必须生效（不能被默认值盖掉）")
	}
}

// TestCronListDataExposesEffectiveBool cron_list 响应里的两个开关必须是**生效值**
// （bool，不是指针），前端按 `!== false` 判定，缺字段/指针序列化都会让它误判。
func TestCronListDataExposesEffectiveBool(t *testing.T) {
	writeCronSettings(t, `{"cron":{"auto_clean":false}}`)
	m := &manager{}
	d := m.cronListData()
	if got, ok := d["enabled"].(bool); !ok || !got {
		t.Fatalf("cron_list.enabled 应为 bool true（未设置=默认开），得到 %#v", d["enabled"])
	}
	if got, ok := d["auto_clean"].(bool); !ok || got {
		t.Fatalf("cron_list.auto_clean 应为 bool false（显式关闭），得到 %#v", d["auto_clean"])
	}
}
