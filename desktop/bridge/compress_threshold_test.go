package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/seven7628/hai-harness/agents"
)

// writeAgentThresholdSettings 把 settings.json（agent 段）原样写进**当前（已隔离的）HOME**
// 的 ~/.go-code/settings.json —— loadAgentSettings 的读取源。
// 自包含：不依赖同包其它测试文件的写配置辅助。
func writeAgentThresholdSettings(t *testing.T, raw string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAgentSettingsCompressThreshold agent.compress_threshold 的解析口径：
// 缺省 = codeAgentCompressThreshold；配置值原样生效；0/负数/>1/类型错/坏 JSON
// 一律回退默认（0 = 永不压缩是误配，绝不能当合法配置吃进去）。
func TestAgentSettingsCompressThreshold(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got := loadAgentSettings().compressThreshold(); got != codeAgentCompressThreshold {
		t.Fatalf("缺省压缩阈值: want %v, got %v", codeAgentCompressThreshold, got)
	}

	for _, tc := range []struct {
		raw  string
		want float64
	}{
		{`{"agent":{"compress_threshold":0.5}}`, 0.5},
		{`{"agent":{"compress_threshold":0.55}}`, 0.55},
		{`{"agent":{"compress_threshold":0.95}}`, 0.95},
	} {
		writeAgentThresholdSettings(t, tc.raw)
		if got := loadAgentSettings().compressThreshold(); got != tc.want {
			t.Errorf("%s: compressThreshold = %v, want %v", tc.raw, got, tc.want)
		}
	}

	// 与其它 agent 字段共存（reminder_rounds 同段，不能互相顶掉）
	writeAgentThresholdSettings(t, `{"agent":{"compress_threshold":0.6,"reminder_rounds":7}}`)
	if got := loadAgentSettings().compressThreshold(); got != 0.6 {
		t.Errorf("与 reminder_rounds 共存时 compressThreshold = %v, want 0.6", got)
	}
	if got := loadAgentSettings().ReminderRounds; got != 7 {
		t.Errorf("同段 reminder_rounds 被顶掉: got %d, want 7", got)
	}

	// 负路径：全部回退默认（不留 0，也不放行区间外的值）
	for _, raw := range []string{
		`{"agent":{"compress_threshold":0}}`,     // 0 = 永不压缩
		`{"agent":{"compress_threshold":-0.5}}`,  // 负数
		`{"agent":{"compress_threshold":3}}`,     // >1
		`{"agent":{"compress_threshold":"80%"}}`, // 类型错（字符串）
		`{"agent":{"compress_threshold":null}}`,  // 显式 null
		`{"agent":{"compress_threshold":true}}`,  // 布尔
		`{"agent":{}`,                            // 坏 JSON
		`{"agent":{"reminder_rounds":30}}`,       // 缺字段
	} {
		writeAgentThresholdSettings(t, raw)
		if got := loadAgentSettings().compressThreshold(); got != codeAgentCompressThreshold {
			t.Errorf("%s: compressThreshold = %v, want 默认 %v（误配不得生效）", raw, got, codeAgentCompressThreshold)
		}
	}
}

// TestCodeAgentTuningCompressThresholdFollowsSettings 三宿主装配点都读 settings 的
// agent.compress_threshold（未配置 → 默认），并经 options() 落到 agents.Config。
// 这是设置面板那个滑杆的装配层落点：同一次测试内改两次文件，值跟着走 ⇒ 证明确实读
// settings，不是硬编码。
func TestCodeAgentTuningCompressThresholdFollowsSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hosts := []codeAgentHost{hostMainLoop, hostSpawnLoop, hostExploreLoop}

	writeAgentThresholdSettings(t, fmt.Sprintf(`{"agent":{"reminder_rounds":30,"compress_threshold":%s}}`,
		strconv.FormatFloat(0.55, 'f', -1, 64)))
	for _, host := range hosts {
		tuning := codeAgentTuningFor(host)
		if tuning.compressThreshold != 0.55 {
			t.Errorf("%s: compressThreshold = %v, want 0.55（没跟着设置走 ⇒ 未接线或读错源）", host, tuning.compressThreshold)
		}
		var cfg agents.Config
		for _, o := range tuning.options() {
			o(&cfg)
		}
		if cfg.CompressThreshold != 0.55 {
			t.Errorf("%s: Config.CompressThreshold = %v, want 0.55（options() 未应用该值）", host, cfg.CompressThreshold)
		}
	}

	// 改设置 → 再读一次值跟着走（证明是每次装配时重读，不是启动期快照）
	writeAgentThresholdSettings(t, `{"agent":{"compress_threshold":0.6}}`)
	for _, host := range hosts {
		if got := codeAgentTuningFor(host).compressThreshold; got != 0.6 {
			t.Errorf("%s: 改设置后 compressThreshold = %v, want 0.6", host, got)
		}
	}

	// 未配置 → 默认（缺字段与坏值都不该把压缩关掉或推到 1 以上）
	for _, raw := range []string{
		`{"agent":{"reminder_rounds":30}}`,
		`{"agent":{"compress_threshold":0}}`,
		`{"agent":{"compress_threshold":-0.5}}`,
		`{"agent":{"compress_threshold":3}}`,
	} {
		writeAgentThresholdSettings(t, raw)
		for _, host := range hosts {
			if got := codeAgentTuningFor(host).compressThreshold; got != codeAgentCompressThreshold {
				t.Errorf("%s: %s → compressThreshold = %v, want 默认 %v", host, raw, got, codeAgentCompressThreshold)
			}
		}
	}
}

// TestFloatNumForCompressThreshold floatNum（set_compress_threshold 的取值器）：
// 只认 float64，其余一律回落 fallback。
//
// 为什么值得钉（2026-10）：这个函数的 fallback 是调用方传的 -1（命令分支用 !(t>0)
// 挡非法值）。若哪天有人给它加 json.Number 分支并忽略 Float64() 的 error，
// json.Number("abc") 会静默变成 0 —— 而 0 恰好是"永不压缩"的误配值，且能穿过
// !(t>0) 之外的路径。这条断言保证任何非 float64 输入都走 fallback，不加宽。
func TestFloatNumForCompressThreshold(t *testing.T) {
	f := -1.0
	cases := []struct {
		name    string
		payload map[string]any
		want    float64
	}{
		{"合法值", map[string]any{"compress_threshold": 0.7}, 0.7},
		{"nil map", nil, f},
		{"缺字段", map[string]any{}, f},
		{"null", map[string]any{"compress_threshold": nil}, f},
		{"字符串", map[string]any{"compress_threshold": "0.7"}, f},
		{"布尔", map[string]any{"compress_threshold": true}, f},
		{"0 也照传（由调用方判非法）", map[string]any{"compress_threshold": 0.0}, 0.0},
		{"负数照传", map[string]any{"compress_threshold": -0.5}, -0.5},
		{">1 照传", map[string]any{"compress_threshold": 3.0}, 3.0},
		{"int 不加宽", map[string]any{"compress_threshold": 1}, f},
	}
	for _, tc := range cases {
		if got := floatNum(tc.payload, "compress_threshold", f); got != tc.want {
			t.Errorf("%s: floatNum = %v, want %v", tc.name, got, tc.want)
		}
	}
}
