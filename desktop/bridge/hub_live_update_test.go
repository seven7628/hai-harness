package main

// P1-10 评审 B4/B5/B6：资料页/切模型对**已装配** Hub 会话的生效语义。

import (
	"context"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
)

// dispatchResp 走非 Hub 前缀命令（switch_model 等），返回该 id 的 command_response。
func dispatchResp(t *testing.T, m *manager, buf *syncBuf, id int, typ string, payload map[string]any) (bool, string, map[string]any) {
	t.Helper()
	buf.Take()
	m.dispatch(cmd(id, typ, payload))
	for _, msg := range readLines(buf) {
		if msg["event_type"] != "command_response" {
			continue
		}
		if got, _ := msg["id"].(float64); int(got) != id {
			continue
		}
		data, _ := msg["data"].(map[string]any)
		ok, _ := msg["ok"].(bool)
		errMsg, _ := msg["error"].(string)
		return ok, errMsg, data
	}
	t.Fatalf("%s 没有 command_response", typ)
	return false, "", nil
}

// B4：资料页改 model 后，运行中的 DM 立即按新 model 重建 loop；
// 只改人格时不能把会话级选择（switch_model）改回 profile 默认。
func TestHubAgentUpdateSyncsLiveSessionModel(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "LiveModel")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got := bs.loop.Model(); got != "deepseek-v4-flash" {
		t.Fatalf("baseline loop model = %q", got)
	}

	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": string(p.ID), "patch": map[string]any{"model": "deepseek-v4-pro"},
	})
	if !ok {
		t.Fatalf("update model failed: %s", errMsg)
	}
	if data["effective"] != "next_run" {
		t.Fatalf("effective = %v, want next_run", data["effective"])
	}
	if got := bs.loop.Model(); got != "deepseek-v4-pro" {
		t.Fatalf("live loop model after update = %q, want deepseek-v4-pro", got)
	}
	if bs.model != "deepseek-v4-pro" {
		t.Fatalf("bs.model = %q", bs.model)
	}

	// 模拟用户 switch_model 选了别的模型，然后只改人格：必须保留会话级选择。
	bs.model, bs.provider = "deepseek-v4-flash", "deepseek"
	m.rebuildLoop(bs)
	if _, errMsg, _ := hubCmd(t, m, buf, 3, "hub_agent_update", map[string]any{
		"agent_id": string(p.ID), "patch": map[string]any{"role": "新职责"},
	}); errMsg != "" {
		t.Fatalf("persona-only update failed: %s", errMsg)
	}
	if got := bs.loop.Model(); got != "deepseek-v4-flash" {
		t.Fatalf("persona-only update clobbered session model: %q", got)
	}

	// 回滚也要同步：回滚到第 1 版（model=deepseek-v4-flash）。
	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_agent_rollback", map[string]any{
		"agent_id": string(p.ID), "version": 1,
	})
	if !ok {
		t.Fatalf("rollback failed: %s", errMsg)
	}
	if got := bs.loop.Model(); got != "deepseek-v4-flash" {
		t.Fatalf("live loop model after rollback = %q, want deepseek-v4-flash", got)
	}
	if data["restored_from"] != float64(1) {
		t.Fatalf("restored_from = %v", data["restored_from"])
	}
}

// B5：资料页禁用 bash 后，运行中 DM 的 engine 立即拒绝；放开后工具回来。
func TestHubAgentUpdateReappliesToolPolicyToLiveEngine(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "LiveTools")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.hubEngine.GetTool(context.Background(), "bash"); err != nil {
		t.Fatalf("baseline bash should be visible: %v", err)
	}

	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": string(p.ID), "patch": map[string]any{"tools": map[string]any{"disable": []string{"bash"}}},
	})
	if !ok {
		t.Fatalf("disable bash failed: %s", errMsg)
	}
	if data["effective"] != "next_run" {
		t.Fatalf("effective = %v, want next_run (工具开关可运行期生效)", data["effective"])
	}
	if _, err := bs.hubEngine.GetTool(context.Background(), "bash"); err == nil || !strings.Contains(err.Error(), "disabled by policy") {
		t.Fatalf("live engine still exposes bash after disable: %v", err)
	}

	// 放回：ShowTool 必须清掉粘性的 hidden，否则永远回不来。
	ok, errMsg, _ = hubCmd(t, m, buf, 3, "hub_agent_update", map[string]any{
		"agent_id": string(p.ID), "patch": map[string]any{"tools": map[string]any{"disable": []string{}}},
	})
	if !ok {
		t.Fatalf("re-enable bash failed: %s", errMsg)
	}
	if _, err := bs.hubEngine.GetTool(context.Background(), "bash"); err != nil {
		t.Fatalf("bash did not come back after clearing disable: %v", err)
	}

	// bash_roots 进了装配期沙箱：只能 next_session，不能谎称 next_run。
	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_agent_update", map[string]any{
		"agent_id": string(p.ID), "patch": map[string]any{"tools": map[string]any{"bash_roots": []string{t.TempDir()}}},
	})
	if !ok {
		t.Fatalf("bash_roots update failed: %s", errMsg)
	}
	if data["effective"] != "next_session" {
		t.Fatalf("bash_roots effective = %v, want next_session", data["effective"])
	}
}

// B6：Hub DM 的 switch_model 只改本会话，不改全局 active provider / lastModel。
func TestHubSwitchModelKeepsGlobalProvider(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "LiveSwitch")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	activeBefore := m.provCfg.providerName()
	m.mu.Lock()
	lastBefore := m.lastModel
	m.mu.Unlock()

	ok, errMsg, data := dispatchResp(t, m, buf, 9, "switch_model", map[string]any{
		"workspace": bs.workspace, "session_id": bs.id, "model": "gpt-5", "provider": "openai",
	})
	if !ok {
		t.Fatalf("switch_model failed: %s", errMsg)
	}
	if data["provider"] != "openai" {
		t.Fatalf("response provider = %v", data["provider"])
	}
	if got := bs.loop.Model(); got != "gpt-5" {
		t.Fatalf("hub loop model = %q, want gpt-5", got)
	}
	if got := m.provCfg.providerName(); got != activeBefore {
		t.Fatalf("hub switch_model changed global active provider: %q -> %q", activeBefore, got)
	}
	m.mu.Lock()
	lastAfter := m.lastModel
	m.mu.Unlock()
	if lastAfter != lastBefore {
		t.Fatalf("hub switch_model changed lastModel: %q -> %q", lastBefore, lastAfter)
	}
	// 新建项目会话仍用原全局 active（这里只断言解析结果，不真正装配项目 runtime）。
	if got := m.provCfg.providerForModel("deepseek-v4-flash"); got != "deepseek" {
		t.Fatalf("providerForModel regressed: %q", got)
	}
}

// 零值 tools 的 profile 在 update 后 engine 里所有工具应保持可见（reapply 不误伤）。
func TestHubAgentUpdateReapplyKeepsVisibleTools(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "LiveToolsKeep")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": string(p.ID), "patch": map[string]any{"role": "只改职责"},
	}); errMsg != "" {
		t.Fatalf("update failed: %s", errMsg)
	}
	for _, name := range []string{"bash", "read_file", "agent_spawn", "workspace_list", "session_send"} {
		if _, err := bs.hubEngine.GetTool(context.Background(), name); err != nil {
			t.Errorf("%q disappeared after reapply: %v", name, err)
		}
	}
	_ = agenthub.AgentID(p.ID)
}
