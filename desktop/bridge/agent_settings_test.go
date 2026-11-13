package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/subagent"
)

func TestLoadAgentSettingsDefaultAndConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := loadAgentSettings().ReminderRounds; got != 30 {
		t.Fatalf("default reminder rounds: want 30, got %d", got)
	}

	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"agent":{"reminder_rounds":7}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadAgentSettings().ReminderRounds; got != 7 {
		t.Fatalf("configured reminder rounds: want 7, got %d", got)
	}

	if err := os.WriteFile(settingsPath, []byte(`{"agent":{"reminder_rounds":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadAgentSettings().ReminderRounds; got != 0 {
		t.Fatalf("zero should disable reminder, got %d", got)
	}
}

func TestLoadAgentSettingsRejectsNegative(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"agent":{"reminder_rounds":-1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadAgentSettings().ReminderRounds; got != 30 {
		t.Fatalf("negative reminder rounds should use default 30, got %d", got)
	}
}

// TestLoadAgentSettingsSubagentConcurrency settings.json agent 段的并发槽上限：
// 缺省 = subagent.DefaultConcurrency（1000），配置值原样生效，<=0 回退默认。
// 2026-09-20 加固：原硬编码 4，槽满会让 agent_spawn 同步阻塞 → 主 Agent 整批挂死。
func TestLoadAgentSettingsSubagentConcurrency(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ag := loadAgentSettings()
	if got := ag.maxSubagents(); got != subagent.DefaultConcurrency {
		t.Fatalf("默认主池上限: want %d, got %d", subagent.DefaultConcurrency, got)
	}
	if got := ag.maxExploreSubagents(); got != subagent.DefaultConcurrency {
		t.Fatalf("默认辅池上限: want %d, got %d", subagent.DefaultConcurrency, got)
	}
	if subagent.DefaultConcurrency < 1000 {
		t.Fatalf("默认上限不得低于 1000（4 会卡死主 Agent），got %d", subagent.DefaultConcurrency)
	}

	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"agent":{"reminder_rounds":30,"max_subagents":12,"max_explore_subagents":5}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ag = loadAgentSettings()
	if got := ag.maxSubagents(); got != 12 {
		t.Fatalf("配置主池上限: want 12, got %d", got)
	}
	if got := ag.maxExploreSubagents(); got != 5 {
		t.Fatalf("配置辅池上限: want 5, got %d", got)
	}

	if err := os.WriteFile(settingsPath, []byte(`{"agent":{"reminder_rounds":30,"max_subagents":0,"max_explore_subagents":-3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ag = loadAgentSettings()
	if got := ag.maxSubagents(); got != subagent.DefaultConcurrency {
		t.Fatalf("0 应回退默认，got %d", got)
	}
	if got := ag.maxExploreSubagents(); got != subagent.DefaultConcurrency {
		t.Fatalf("负数应回退默认，got %d", got)
	}
}
