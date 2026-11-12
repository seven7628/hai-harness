package main

import (
	"encoding/json"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/session"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSubLoopsCopyContextWindowAndCompressor 子 loop（Explore / spawn）必须**拷贝**主 Agent 的
// WithContextWindow/WithCompressor 配置（而非共享实例）：
//   - 上下文窗口 = 模型真实窗口（此前缺省 128k，1M 模型下压缩触发线错误）
//   - 压缩器非 nil（此前无压缩器 → 子任务上下文无限增长必撞上限）
//   - 实例相互独立（各自 NewLLMCompressor；共享实例会让子/主运行互相影响）
func TestSubLoopsCopyContextWindowAndCompressor(t *testing.T) {
	pc := &providerConfig{
		active: "opencode",
		models: map[string]map[string]int64{
			"opencode": {"deepseek-v4-flash": 1000000, "deepseek-v4-pro": 1000000},
		},
		maxTokens: map[string]map[string]int64{"opencode": {"deepseek-v4-flash": 384000}},
	}
	const model = "deepseek-v4-flash"

	main := buildExploreLoop(t.TempDir(), "s-test", pc, "opencode", model, "low", nil)
	spawn := buildSpawnLoop(nil, t.TempDir(), "s-test", pc, "opencode", model, "low", nil)

	if main.ContextWindow() != pc.windowForIn("opencode", model) {
		t.Fatalf("explore 上下文窗口 = %d, want %d（须按归属 prov 拷贝模型真实窗口）",
			main.ContextWindow(), pc.windowForIn("opencode", model))
	}
	if spawn.ContextWindow() != pc.windowForIn("opencode", model) {
		t.Fatalf("spawn 上下文窗口 = %d, want %d（须按归属 prov 拷贝模型真实窗口）",
			spawn.ContextWindow(), pc.windowForIn("opencode", model))
	}
	if main.ContextWindow() == 128000 && pc.windowForIn("opencode", model) != 128000 {
		t.Fatal("不得是缺省 128k（1M 模型须带真窗口）")
	}
	// 问题六回归：全局活跃 Provider ≠ 会话归属 prov 时，子 loop 窗口仍按归属解析
	//（此前 windowFor 按 pc.active 取表，编辑其它 Provider 会把 1M 会话打回 128k）。
	prevActive := pc.active
	pc.active = "deepseek" // deepseek 表无此模型 → 若误用 active 解析将回退 128k
	mainX := buildExploreLoop(t.TempDir(), "s-test", pc, "opencode", model, "low", nil)
	pc.active = prevActive
	if mainX.ContextWindow() != 1000000 {
		t.Fatalf("跨 Provider 归属解析失效: got %d, want 1000000", mainX.ContextWindow())
	}
	mainC, spawnC := main.Compressor(), spawn.Compressor()
	if mainC == nil || spawnC == nil {
		t.Fatalf("子 loop 压缩器不得为 nil: explore=%v spawn=%v", mainC != nil, spawnC != nil)
	}
	if mainC == spawnC {
		t.Fatal("Explore/spawn 必须各自持有独立压缩器实例（拷贝而非共享 —— 共享会互相影响）")
	}
}

// TestWorkspaceKeySymlinkNormalization 同一目录的不同路径拼写（符号链接）必须映射到同一 key：
// 否则会话/事件目录错位（重启后"历史丢失"、项目重复出现）。
func TestWorkspaceKeySymlinkNormalization(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if workspaceKey(link) != workspaceKey(real) {
		t.Fatalf("符号链接拼写必须映射到同一 key: %q != %q", workspaceKey(link), workspaceKey(real))
	}
	// 路径不存在：回退原拼写（仍稳定：同一拼写恒同一 key）
	ghost := filepath.Join(dir, "ghost")
	if workspaceKey(ghost) != workspaceKey(ghost) {
		t.Fatal("同一拼写必须稳定")
	}
	// 不同目录必须不同 key（回归：哈希唯一性不被规范化破坏）
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if workspaceKey(real) == workspaceKey(other) {
		t.Fatal("不同目录必须映射到不同 key")
	}
}

// TestRestoredMessagesCarriesImageContents 降级恢复路径（无事件日志）必须携带图片内容块：
// 纯图片消息不再被整条丢弃（此前 Text=="" 直接 continue）。走真实冷恢复链路：
// 写会话状态文件（含图文消息）→ new_session{id} 从磁盘加载 → restoredMessages。
func TestRestoredMessagesCarriesImageContents(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	id := "restore-img"

	// 直接落盘会话状态（模拟已运行过的会话；events 目录无本会话日志 → 走 restored=messages）
	msgs := []core.Message{
		core.NewUserMessage(
			core.Content{Type: core.ContentTypeText, Content: "看这张图"},
			core.Content{Type: core.ContentTypeImage, Content: "data:image/png;base64,AAAA", MimeType: "image/png"},
		),
	}
	writeSessionState(t, ws, id, msgs)

	m.dispatch(cmd(4001, "new_session", map[string]any{"workspace": ws, "id": id}))
	bs := m.get(ws, id)
	if bs == nil {
		t.Fatal("session not created")
	}
	out := restoredMessages(bs.s)
	if len(out) != 1 {
		t.Fatalf("want 1 restored message, got %d: %+v", len(out), out)
	}
	contents, _ := out[0]["contents"].([]map[string]any)
	if len(contents) != 1 || contents[0]["type"] != "image" || contents[0]["content"] != "data:image/png;base64,AAAA" || contents[0]["mime_type"] != "image/png" {
		t.Fatalf("restored message 应携带图片内容块: %+v", out[0])
	}
	// role/text 字段保持向后兼容
	if out[0]["role"] != "user" || out[0]["text"] != "看这张图" {
		t.Fatalf("role/text 兼容字段错误: %+v", out[0])
	}
}

// TestRestoredMessagesKeepsPureImage 纯图片消息（无文本）不得被丢弃。
func TestRestoredMessagesKeepsPureImage(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()
	id := "restore-pure-img"

	writeSessionState(t, ws, id, []core.Message{
		core.NewUserMessage(
			core.Content{Type: core.ContentTypeImage, Content: "data:image/jpeg;base64,BBBB", MimeType: "image/jpeg"},
		),
	})
	m.dispatch(cmd(4002, "new_session", map[string]any{"workspace": ws, "id": id}))
	bs := m.get(ws, id)
	if bs == nil {
		t.Fatal("session not created")
	}
	msgs := restoredMessages(bs.s)
	if len(msgs) != 1 {
		t.Fatalf("纯图片消息不得被丢弃: %+v", msgs)
	}
	contents, _ := msgs[0]["contents"].([]map[string]any)
	if len(contents) != 1 || contents[0]["content"] != "data:image/jpeg;base64,BBBB" {
		t.Fatalf("纯图片消息应携带图片块: %+v", msgs[0])
	}
	if strings.TrimSpace(msgs[0]["text"].(string)) != "" {
		t.Fatalf("纯图片消息 text 应为空: %+v", msgs[0])
	}
}

// writeSessionState 按 FileStore 格式落盘会话状态（session/{id}.jsonl 的 state 行）。
func writeSessionState(t *testing.T, ws, id string, msgs []core.Message) {
	t.Helper()
	dir := wsSessionsDir(ws)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := session.State{Messages: msgs, UpdatedAt: time.Now()}
	b, err := json.Marshal(map[string]any{"type": "state", "state": state})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
