package main

// Project Memory 开关的持久化与装配期取值（设置面板 → bridge 命令 → 会话装配）。
//
// 要保护的行为，按用户可见性排序：
//
//	① 开关关掉后**新会话**不注入、不补戳（"我的记忆到底开没开"必须是真的）
//	② 开关改掉**不影响运行中的会话**（决策：记忆是系统提示词的一部分，中途删会让
//	    同一会话前后两轮"你记得什么"不一致）
//	③ 环境变量仍能压过面板选择（CI / 调试脚本要能强制固定）
//	④ 落盘失败必须报错，不能让面板显示"已关"而重启后又开着

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools/builtin"
)

func newTestPrefStore(t *testing.T) *memoryPrefStore {
	t.Helper()
	s := &memoryPrefStore{path: filepath.Join(t.TempDir(), "memory.json")}
	return s
}

// TestMemoryPrefsDefaultOn：用户从未设置过时默认开，且**不是**显式的 true。
func TestMemoryPrefsDefaultOn(t *testing.T) {
	s := newTestPrefStore(t)
	p := s.load()
	if p.Enabled != nil {
		t.Fatalf("未设置时 Enabled 应为 nil（三态），实际 %v", *p.Enabled)
	}
	if !p.enabled() {
		t.Fatal("未设置时应默认为开（对齐官方 autoMemoryEnabled 默认开）")
	}
}

// TestMemoryPrefsRoundTrip：显式关能落盘并读回；三态不塌陷成二态。
func TestMemoryPrefsRoundTrip(t *testing.T) {
	s := newTestPrefStore(t)
	off := false
	if err := s.set(memoryPrefs{Enabled: &off}); err != nil {
		t.Fatalf("落盘: %v", err)
	}
	p := s.load()
	if p.Enabled == nil {
		t.Fatal("读回后 Enabled 变成了 nil —— 显式关被当成没设置过")
	}
	if p.enabled() {
		t.Fatal("显式关未生效")
	}
	// 原始文件不该把 nil 写成 "enabled": null 之外的形态；显式关必须落成 false
	raw, _ := os.ReadFile(s.path)
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("落盘不是合法 JSON: %v\n%s", err, raw)
	}
	if v, ok := probe["enabled"]; !ok || v != false {
		t.Fatalf("落盘内容应含 enabled:false，实际 %s", raw)
	}
}

// TestMemoryPrefsCorruptFallsBackToDefault：文件损坏按"没设置过"处理，不阻断会话。
func TestMemoryPrefsCorruptFallsBackToDefault(t *testing.T) {
	s := newTestPrefStore(t)
	if err := os.WriteFile(s.path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	if p := s.load(); p.Enabled != nil || !p.enabled() {
		t.Fatalf("损坏文件应回落为默认开，实际 %+v", p)
	}
}

// TestMemoryPrefsEnvOverridesPanel：环境变量压过面板选择（CI 能强制关掉记忆）。
func TestMemoryPrefsEnvOverridesPanel(t *testing.T) {
	off := false
	p := memoryPrefs{Enabled: &off}
	t.Setenv(memory.EnvAutoMemoryEnabled, "0")
	// memory.Config 内部判定优先级：环境变量 > 用户偏好 > 默认
	if !p.config().Disabled() {
		t.Fatal("环境变量未压过面板的显式设置")
	}
	t.Setenv(memory.EnvAutoMemoryEnabled, "1")
	if p.config().Disabled() {
		t.Fatal("环境变量开启后仍判定为关")
	}
}

// TestPrefOffDisablesNewSessionAssembly：开关 = 关 时，注入为空且写入钩子完全惰性。
//
// 这一条是"我的记忆到底开没开"的全部语义。只断了读侧（不注入）而漏了写侧，
// 用户关了记忆却在背后继续生成文件 —— 比不实现更糟。
func TestPrefOffDisablesNewSessionAssembly(t *testing.T) {
	root := t.TempDir()
	off := false
	h := newMemoryHost(memoryPrefs{Enabled: &off}.config())

	// 注入为空
	if got := h.injection(context.Background(), t.TempDir()); got != "" {
		t.Fatalf("关闭时不该注入，得到 %q", got)
	}
	// 写入钩子惰性：即使路径就在记忆目录里，也不该建锁、不该动文件
	dirPath := filepath.Join(root, "repos", "id")
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	topic := filepath.Join(dirPath, "t.md")
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nx\n"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	ws := t.TempDir()
	ft := builtin.NewFileTools(ws).
		WithPostWrite(func(ctx context.Context, p string) (string, error) { return h.postWrite(ctx, ws, p) })
	if err := callWriteFile(ft, topic, "---\ntype: project\n---\n\ny\n"); err != nil {
		t.Fatalf("write_file 应成功: %v", err)
	}
	got, _ := os.ReadFile(topic)
	if strings.Contains(string(got), "modified:") {
		t.Fatalf("关闭开关时仍补了 modified：\n%s", got)
	}
	entries, _ := os.ReadDir(dirPath)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			t.Fatalf("关闭开关时建了锁侧车文件：%s", e.Name())
		}
	}
}

// TestPrefOnKeepsFileToolsReachable：开着时记忆目录对标准文件工具可达。
//
// 官方形态没有记忆专用工具，模型只有 read_file / write_file / edit_file。
// 记忆目录若被 deny 名单罩住，模型既读不到注入索引指向的 topic，也写不进新记忆 ——
// 面板上开关是"开"的，功能整个不成立。
func TestPrefOnKeepsFileToolsReachable(t *testing.T) {
	home := t.TempDir()
	topic := filepath.Join(home, ".go-code", "memory", "repos", "id", "t.md")
	if err := os.MkdirAll(filepath.Dir(topic), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nx\n"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	t.Setenv("HOME", home)
	ft := builtin.NewFileTools(t.TempDir())
	for _, tl := range ft.Tools() {
		if tl.Name() != "read_file" {
			continue
		}
		if _, err := tl.Call(context.Background(), "read_file", `{"path":`+jsonQuote(topic)+`}`); err != nil {
			t.Fatalf("读不到记忆目录: %v", err)
		}
		return
	}
	t.Fatal("未注册 read_file")
}

// TestMemoryCommandsRoundTrip：两个命令真的能 dispatch 出去并往返。
//
// 这一条覆盖的是"命令注册名 / payload 形状 / 响应字段"三层 —— 它们都不在
// memoryPrefStore 的单测覆盖范围内，而任何一个拼错都会让设置面板静默失效
// （前端拿到 undefined，界面退回默认值，用户看不出哪里错了）。
func TestMemoryCommandsRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := &manager{memPrefs: newMemoryPrefStore()}
	buf := &syncBuf{}
	m.out = newOut(buf)

	// 初始：默认开、未显式设置
	m.dispatch(cmd(1, "memory_settings", map[string]any{}))
	raw, _ := lastResp(t, buf)
	if raw["ok"] != true {
		t.Fatalf("memory_settings 应成功：%v", raw)
	}
	resp, _ := raw["data"].(map[string]any)
	if resp == nil {
		t.Fatalf("响应缺少 data 段：%v", raw)
	}
	if resp["enabled"] != true || resp["explicit"] != false {
		t.Fatalf("初始应是 enabled=true/explicit=false，实际 %v", resp)
	}
	if resp["applies_to"] != "next_session" {
		t.Fatalf("applies_to 必须是 next_session（UI 靠它告知生效时机），实际 %v", resp["applies_to"])
	}

	// 关掉
	m.dispatch(cmd(2, "memory_set_enabled", map[string]any{"enabled": false}))
	r2, _ := lastResp(t, buf)
	d2, _ := r2["data"].(map[string]any)
	if r2["ok"] != true || d2 == nil || d2["enabled"] != false {
		t.Fatalf("关闭应成功并回显 enabled=false：%v", r2)
	}

	// 落盘了，且读回一致
	onDisk, err := os.ReadFile(filepath.Join(home, ".go-code", "memory.json"))
	if err != nil {
		t.Fatalf("未落盘: %v", err)
	}
	if !strings.Contains(string(onDisk), `"enabled": false`) {
		t.Fatalf("落盘内容不对：%s", onDisk)
	}
	m.dispatch(cmd(3, "memory_settings", map[string]any{}))
	r3, _ := lastResp(t, buf)
	d3, _ := r3["data"].(map[string]any)
	if d3 == nil || d3["enabled"] != false || d3["explicit"] != true {
		t.Fatalf("读回应为 enabled=false/explicit=true，实际 %v", r3)
	}
}

// TestMemorySetEnabledRejectsBadPayload：字段缺失/类型错必须报错。
//
// 前端乐观更新已经改了本地状态，bridge 这边若静默接受坏 payload 并回 ok，
// UI 就不会回滚，用户会看到一个从未生效的开关。
func TestMemorySetEnabledRejectsBadPayload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := &manager{memPrefs: newMemoryPrefStore()}
	buf := &syncBuf{}
	m.out = newOut(buf)

	m.dispatch(cmd(1, "memory_set_enabled", map[string]any{}))
	if r, _ := lastResp(t, buf); r["ok"] != false {
		t.Fatalf("缺 enabled 应报错，实际 %v", r)
	}
	m.dispatch(cmd(2, "memory_set_enabled", map[string]any{"enabled": "yes"}))
	if r, _ := lastResp(t, buf); r["ok"] != false {
		t.Fatalf("enabled 非布尔应报错，实际 %v", r)
	}
}
