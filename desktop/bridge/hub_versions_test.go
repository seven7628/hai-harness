package main

// P1-06：人格版本 / 回滚 / 提示词分层预览。

import (
	"strings"
	"testing"
)

// 每次保存都留档旧版本；回滚把旧版内容落成**新版本**（当前版本不丢、可再回滚）。
func TestHubAgentVersionsAndRollback(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Alpha")
	agentID := string(p.ID)

	// v2：改名（同时验证回滚把名字也带回去）。
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": agentID, "patch": map[string]any{"name": "Beta", "role": "改过的角色"},
	})
	if !ok {
		t.Fatalf("update failed: %s", errMsg)
	}
	if v := agentFromData(t, data).Version; v != 2 {
		t.Fatalf("after update version = %d, want 2", v)
	}

	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_agent_versions", map[string]any{"agent_id": agentID})
	if !ok {
		t.Fatalf("versions failed: %s", errMsg)
	}
	if cur := int(floatNum(data, "current", 0)); cur != 2 {
		t.Fatalf("current = %d, want 2", cur)
	}
	items, _ := data["versions"].([]any)
	if len(items) != 1 {
		t.Fatalf("versions = %v, want exactly the archived v1", items)
	}
	first, _ := items[0].(map[string]any)
	if first["version"] != float64(1) || first["name"] != "Alpha" {
		t.Fatalf("archived v1 wrong: %v", first)
	}
	// 摘要不含正文（列表不用整份 profile 撑爆响应）。
	if _, has := first["instructions"]; has {
		t.Fatalf("versions list must not carry full profile: %v", first)
	}

	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_agent_rollback", map[string]any{
		"agent_id": agentID, "version": 1,
	})
	if !ok {
		t.Fatalf("rollback failed: %s", errMsg)
	}
	got := agentFromData(t, data)
	if got.Name != "Alpha" || got.Role != p.Role {
		t.Fatalf("rolled profile = %q/%q, want Alpha/%q", got.Name, got.Role, p.Role)
	}
	if got.Version != 3 {
		t.Fatalf("rollback version = %d, want 3 (新版本落地，不是把版本号改回去)", got.Version)
	}
	if v, _ := data["restored_from"]; v != float64(1) {
		t.Fatalf("restored_from = %v, want 1", v)
	}

	// 回滚自身也留档：现在能看到 v1、v2 两版（v2 是回滚前的 Beta）。
	ok, _, data = hubCmd(t, m, buf, 5, "hub_agent_versions", map[string]any{"agent_id": agentID})
	if !ok {
		t.Fatal("versions after rollback failed")
	}
	items, _ = data["versions"].([]any)
	if len(items) != 2 {
		t.Fatalf("versions after rollback = %d, want 2", len(items))
	}
	newest, _ := items[0].(map[string]any)
	if newest["version"] != float64(2) || newest["name"] != "Beta" {
		t.Fatalf("rollback must archive the pre-image (v2 Beta), got %v", newest)
	}

	// 不存在的版本：明确失败，而不是静默成功。
	if ok, errMsg, _ := hubCmd(t, m, buf, 6, "hub_agent_rollback", map[string]any{
		"agent_id": agentID, "version": 99,
	}); ok {
		t.Fatalf("rollback to missing version must fail (%s)", errMsg)
	}
	if ok, _, _ := hubCmd(t, m, buf, 7, "hub_agent_rollback", map[string]any{
		"agent_id": agentID, "version": 0,
	}); ok {
		t.Fatal("rollback without version must fail")
	}
}

// 回滚改了人格 → 已装配的会话必须重建 loop（下一个 Run 生效）。
func TestHubAgentRollbackRebuildsLoop(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Roll")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	before := bs.loop
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": agentID, "patch": map[string]any{"role": "v2 角色"},
	}); !ok {
		t.Fatalf("update failed: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_agent_rollback", map[string]any{
		"agent_id": agentID, "version": 1,
	}); !ok {
		t.Fatalf("rollback failed: %s", errMsg)
	}
	if bs.loop == before {
		t.Fatal("rollback must rebuild the session loop")
	}
	if bs.s.Loop() != bs.loop {
		t.Fatal("session still runs the old loop after rollback")
	}
}

// 分层预览：L1/L2/L3 必在，L4（记忆）即使为空也有引导文本；写了记忆后 L4 == 真正注入的文本，
// 且 L1–L3 拼起来 == 运行中 loop 的 system 分区（预览与下发同源）。
func TestHubPromptPreviewLayers(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Preview")
	agentID := string(p.ID)

	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_agent_prompt_preview", map[string]any{"agent_id": agentID})
	if !ok {
		t.Fatalf("preview failed: %s", errMsg)
	}
	layers, _ := data["layers"].([]any)
	keys := make([]string, 0, len(layers))
	var total float64
	for _, raw := range layers {
		layer, _ := raw.(map[string]any)
		keys = append(keys, str(layer, "key"))
		if str(layer, "text") == "" || str(layer, "title") == "" {
			t.Fatalf("layer %v has empty text/title", layer)
		}
		total += float64(len(str(layer, "text")))
	}
	// DM 无名册 → L1 + L2 身份；L3 环境恒在（内核一定追加 Current Workspace）；
	// L4 记忆即使为空也有引导文本，所以恒在最后一层。
	if strings.Join(keys, ",") != "protocol,identity,environment,memory" {
		t.Fatalf("DM preview layers = %v, want [protocol identity environment memory]", keys)
	}
	if got := float64(floatNum(data, "total_bytes", 0)); got != total {
		t.Fatalf("total_bytes = %v, want %v", got, total)
	}
	if note := str(data, "note"); !strings.Contains(note, "L5") {
		t.Fatalf("note must say L5 is assembled by the kernel: %q", note)
	}

	if ok, errMsg, _ := hubMemoryCmd(t, m, buf, 3, "hub_agent_memory_write", map[string]any{
		"agent_id": agentID, "path": "MEMORY.md", "content": "- [预览](preview.md) — 预览里要能看到这行\n",
	}); !ok {
		t.Fatalf("memory write failed: %s", errMsg)
	}
	ok, _, data = hubCmd(t, m, buf, 4, "hub_agent_prompt_preview", map[string]any{"agent_id": agentID})
	if !ok {
		t.Fatal("preview after memory failed")
	}
	layers, _ = data["layers"].([]any)
	last, _ := layers[len(layers)-1].(map[string]any)
	if str(last, "key") != "memory" {
		t.Fatalf("last layer = %v, want memory", last)
	}
	if !strings.Contains(str(last, "text"), "预览里要能看到这行") {
		t.Fatalf("memory layer missing content: %q", str(last, "text"))
	}
	// 与装配同源：各层按 "\n\n" 拼起来必须与运行中 loop 的**完整系统提示词**逐字节相等
	// （含当前版本的内核分隔符/环境层/记忆段）。只比字符数会漏掉分隔符与层序错误。
	bs, berr := m.hubDMSession(agentID)
	if berr != nil {
		t.Fatal(berr)
	}
	texts := make([]string, 0, len(layers))
	for _, raw := range layers {
		layer, _ := raw.(map[string]any)
		texts = append(texts, str(layer, "text"))
	}
	if got, want := strings.Join(texts, "\n\n"), bs.loop.SystemPromptText(); got != want {
		t.Fatalf("preview != composed system prompt (len %d vs %d)", len(got), len(want))
	}
	// 与装配同源：长度必须等于当前注入（两份文案漂移会在这一步暴露）。
	loaded, _, lerr := m.hubLoadAgent(agentID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	inj := m.hubMemoryInjection(loaded, m.hubRuntime(agentID).path)
	if str(last, "text") != inj {
		t.Fatalf("preview memory != injected memory (len %d vs %d)", len(str(last, "text")), len(inj))
	}
}
