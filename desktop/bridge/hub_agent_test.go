package main

// P1-03：智能体 CRUD 命令（create/get/list/update/delete + 校验 + 版本 + 事件）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
)

// hubCmd 发一条 hub_* 命令并返回它的 command_response.data。
// 响应与事件都写进同一个 syncBuf，所以按 id 精确匹配（buf 是累积的）。
func hubCmd(t *testing.T, m *manager, buf *syncBuf, id int, typ string, payload map[string]any) (bool, string, map[string]any) {
	t.Helper()
	before := buf.String()
	if !m.dispatchHub(command{Id: id, Type: Command(typ), Payload: payload}) {
		t.Fatalf("%s 未被 dispatchHub 处理", typ)
	}
	after := buf.String()
	for _, line := range strings.Split(strings.TrimPrefix(after, before), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if msg["event_type"] != "command_response" || int(msg["id"].(float64)) != id {
			continue
		}
		data, _ := msg["data"].(map[string]any)
		ok, _ := msg["ok"].(bool)
		errMsg, _ := msg["error"].(string)
		return ok, errMsg, data
	}
	t.Fatalf("%s 没有 command_response（输出:\n%s）", typ, after)
	return false, "", nil
}

// hubEvents 取出某个 event_type 的事件行。
func hubEvents(t *testing.T, buf *syncBuf, eventType string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if msg["event_type"] == eventType {
			out = append(out, msg)
		}
	}
	return out
}

func agentFromData(t *testing.T, data map[string]any) agenthub.AgentProfile {
	t.Helper()
	raw, err := json.Marshal(data["agent"])
	if err != nil {
		t.Fatalf("agent 字段不可序列化: %v", err)
	}
	var p agenthub.AgentProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("agent 字段不是 profile: %v", err)
	}
	return p
}

// 创建 → 默认值/ID/版本/事件 → 列表 → 单查。
func TestHubAgentCreateListGet(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Researcher", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "竞品研究员"},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	p := agentFromData(t, data)
	if !strings.HasPrefix(string(p.ID), "ag_") {
		t.Fatalf("id = %q, want ag_*", p.ID)
	}
	if p.Version != 1 {
		t.Fatalf("version = %d, want 1", p.Version)
	}
	if p.CompressThreshold != agenthub.DefaultCompressThreshold {
		t.Fatalf("compress_threshold = %v, want default %v", p.CompressThreshold, agenthub.DefaultCompressThreshold)
	}
	if p.Policy.DailyBudgetUSD != agenthub.DefaultAgentDailyBudgetUSD {
		t.Fatalf("daily_budget_usd = %v, want default %v", p.Policy.DailyBudgetUSD, agenthub.DefaultAgentDailyBudgetUSD)
	}
	if p.Accent == "" || p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatalf("defaults not applied: %+v", p)
	}
	// 状态点事件：create 之后至少有一条 idle。
	var sawIdle bool
	for _, ev := range hubEvents(t, buf, "hub_agent_status") {
		if ev["agent_id"] == string(p.ID) && ev["status"] == "idle" {
			sawIdle = true
		}
	}
	if !sawIdle {
		t.Fatalf("missing hub_agent_status{idle} for %s", p.ID)
	}

	ok, errMsg, data = hubCmd(t, m, buf, 2, "hub_agent_list", map[string]any{})
	if !ok {
		t.Fatalf("list failed: %s", errMsg)
	}
	agents, _ := data["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("list len = %d, want 1 (%v)", len(agents), data["agents"])
	}

	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_agent_get", map[string]any{"agent_id": string(p.ID)})
	if !ok {
		t.Fatalf("get failed: %s", errMsg)
	}
	if got := agentFromData(t, data); got.Name != "Researcher" || got.Role != "竞品研究员" {
		t.Fatalf("get roundtrip mismatch: %+v", got)
	}
}

// provider 校验：空 = 没填；未配置/已删 = provider_not_found，且不落盘。
func TestHubAgentCreateValidatesProvider(t *testing.T) {
	m, buf := testManager(t)

	ok, errMsg, _ := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "NoProvider", "model": "x"},
	})
	if ok || !strings.Contains(errMsg, "provider") {
		t.Fatalf("empty provider must be rejected, ok=%v err=%q", ok, errMsg)
	}

	ok, errMsg, _ = hubCmd(t, m, buf, 2, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "BadProvider", "provider": "nope", "model": "x"},
	})
	if ok || !strings.Contains(errMsg, "未配置") {
		t.Fatalf("unknown provider must be rejected, ok=%v err=%q", ok, errMsg)
	}

	// 被拒的两次都不能留下 profile。
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_agent_list", map[string]any{"include_deleted": true})
	if !ok {
		t.Fatalf("list failed: %s", errMsg)
	}
	if agents, _ := data["agents"].([]any); len(agents) != 0 {
		t.Fatalf("rejected creates leaked into store: %v", agents)
	}
}

// 名字唯一（不区分大小写）；软删的智能体不占名额。
func TestHubAgentNameUniqueAndCount(t *testing.T) {
	m, buf := testManager(t)
	mk := func(id int, name string) (bool, string) {
		ok, errMsg, _ := hubCmd(t, m, buf, id, "hub_agent_create", map[string]any{
			"profile": map[string]any{"name": name, "provider": "deepseek", "model": "deepseek-v4-flash"},
		})
		return ok, errMsg
	}
	if ok, errMsg := mk(1, "Writer"); !ok {
		t.Fatalf("first create failed: %s", errMsg)
	}
	ok, errMsg := mk(2, "writer")
	if ok || !strings.Contains(errMsg, "already used") {
		t.Fatalf("duplicate name must be rejected (case-insensitive), ok=%v err=%q", ok, errMsg)
	}
	// 软删之后可以同名重建。
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_agent_list", map[string]any{})
	if !ok {
		t.Fatalf("list failed: %s", errMsg)
	}
	agents, _ := data["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("list len = %d, want 1", len(agents))
	}
	first := agentFromData(t, map[string]any{"agent": agents[0]})
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_agent_delete", map[string]any{"agent_id": string(first.ID)}); !ok {
		t.Fatalf("soft delete failed: %s", errMsg)
	}
	if ok, errMsg := mk(5, "Writer"); !ok {
		t.Fatalf("recreate after soft delete failed: %s", errMsg)
	}
}

// 保存即版本：update 先归档旧 profile，再 version++，字段未给的保持原值。
func TestHubAgentUpdateVersions(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "Editor", "provider": "deepseek", "model": "deepseek-v4-flash",
			"role": "编辑", "persona": "严谨",
		},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	created := agentFromData(t, data)

	ok, errMsg, data = hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": string(created.ID),
		"patch":    map[string]any{"role": "主编"},
	})
	if !ok {
		t.Fatalf("update failed: %s", errMsg)
	}
	updated := agentFromData(t, data)
	if updated.Version != created.Version+1 {
		t.Fatalf("version = %d, want %d", updated.Version, created.Version+1)
	}
	if updated.Role != "主编" || updated.Name != "Editor" || updated.Persona != "严谨" {
		t.Fatalf("patch merge wrong: %+v", updated)
	}
	if updated.ID != created.ID {
		t.Fatalf("id changed: %q -> %q", created.ID, updated.ID)
	}

	// 历史版本里必须有旧 role（回滚的事实源）。
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListProfileVersions(t.Context(), created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}
	if versions[0].Profile.Role != "编辑" || versions[0].Version != created.Version {
		t.Fatalf("archived version wrong: %+v", versions[0])
	}
}

// 删除：默认软删（列表隐藏、记录保留、幂等），purge 才物理删目录。
func TestHubAgentDeleteSoftAndPurge(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Doomed", "provider": "deepseek", "model": "deepseek-v4-flash"},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	p := agentFromData(t, data)

	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_delete", map[string]any{"agent_id": string(p.ID)}); !ok {
		t.Fatalf("soft delete failed: %s", errMsg)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_agent_list", map[string]any{})
	if !ok {
		t.Fatalf("list failed: %s", errMsg)
	}
	if agents, _ := data["agents"].([]any); len(agents) != 0 {
		t.Fatalf("soft-deleted agent still listed: %v", agents)
	}
	ok, _, data = hubCmd(t, m, buf, 4, "hub_agent_list", map[string]any{"include_deleted": true})
	if !ok {
		t.Fatal("list include_deleted failed")
	}
	if agents, _ := data["agents"].([]any); len(agents) != 1 {
		t.Fatalf("include_deleted len = %d, want 1", len(agents))
	}

	// 幂等：再删一次不报错、不重复加版本。
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_agent_delete", map[string]any{"agent_id": string(p.ID)}); !ok {
		t.Fatalf("second soft delete must be idempotent: %s", errMsg)
	}

	// purge：目录消失，get 报错。
	if ok, errMsg, _ := hubCmd(t, m, buf, 6, "hub_agent_delete", map[string]any{"agent_id": string(p.ID), "purge": true}); !ok {
		t.Fatalf("purge failed: %s", errMsg)
	}
	dir := filepath.Join(appDataDir(), "hub", "agents", string(p.ID))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("purged agent dir still exists (%v): %s", err, dir)
	}
	if ok, _, _ := hubCmd(t, m, buf, 7, "hub_agent_get", map[string]any{"agent_id": string(p.ID)}); ok {
		t.Fatal("get after purge should fail")
	}
}

// dispatch 必须真的把 hub_* 交出去（只在新函数里自测「自己调自己」不算接线）。
func TestHubCommandsRoutedFromDispatch(t *testing.T) {
	m, buf := testManager(t)
	m.dispatch(command{Id: 1, Type: CmdHubAgentList, Payload: map[string]any{}})
	var found bool
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if msg["event_type"] == "command_response" && int(msg["id"].(float64)) == 1 {
			found = true
			if ok, _ := msg["ok"].(bool); !ok {
				t.Fatalf("hub_agent_list via dispatch failed: %v", msg["error"])
			}
		}
	}
	if !found {
		t.Fatalf("dispatch did not answer hub_agent_list (输出:\n%s)", buf.String())
	}
}

// create 不认客户端 id/version/deleted：这些是服务端字段（§7.1.2「生成 id」）。
// 不带 payload 的话，删掉 create 里的 `p.Version = 1` 不会有任何测试失败（P1-03 复核）。
func TestHubAgentCreateIgnoresClientIDVersionAndDeleted(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"id": "ag_clientchosen", "name": "Forged", "provider": "deepseek",
			"model": "deepseek-v4-flash", "version": 7, "deleted": true,
		},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	p := agentFromData(t, data)
	if p.ID == "ag_clientchosen" {
		t.Fatal("create must allocate its own id, not accept the client's")
	}
	if !strings.HasPrefix(string(p.ID), "ag_") {
		t.Fatalf("id = %q, want ag_*", p.ID)
	}
	if p.Version != 1 || p.Deleted {
		t.Fatalf("version/deleted = %d/%v, want 1/false", p.Version, p.Deleted)
	}
}

// create 带一个已存在的 id：必须生成新 id，原智能体一个字节都不能变（P1-03 复核：
// 旧实现静默覆盖，version 归 1、不归档，记忆/DM 还挂在同一 id 上）。
func TestHubAgentCreateDoesNotOverwriteExistingID(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "Original", "provider": "deepseek", "model": "deepseek-v4-flash",
			"instructions": "keep me",
		},
	})
	if !ok {
		t.Fatalf("first create failed: %s", errMsg)
	}
	orig := agentFromData(t, data)

	ok, errMsg, data = hubCmd(t, m, buf, 2, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"id": string(orig.ID), "name": "Hijack", "provider": "deepseek", "model": "deepseek-v4-flash",
		},
	})
	if !ok {
		t.Fatalf("second create failed: %s", errMsg)
	}
	hijack := agentFromData(t, data)
	if hijack.ID == orig.ID {
		t.Fatalf("second create reused the existing id %q", orig.ID)
	}

	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_agent_get", map[string]any{"agent_id": string(orig.ID)})
	if !ok {
		t.Fatalf("get original failed: %s", errMsg)
	}
	got := agentFromData(t, data)
	if got.Name != "Original" || got.Instructions != "keep me" || got.Version != orig.Version {
		t.Fatalf("original agent was overwritten: %+v", got)
	}
}

// update 的 patch 不能污染归档的旧版本（切片底层数组共享的真实事故）。
func TestHubAgentUpdateArchiveNotPollutedByPatchSlices(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "Patcher", "provider": "deepseek", "model": "deepseek-v4-flash",
			"tools": map[string]any{"disable": []string{"tool_a", "tool_b", "tool_c"}},
		},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	created := agentFromData(t, data)

	ok, errMsg, data = hubCmd(t, m, buf, 2, "hub_agent_update", map[string]any{
		"agent_id": string(created.ID),
		"patch":    map[string]any{"tools": map[string]any{"disable": []string{"tool_x"}}},
	})
	if !ok {
		t.Fatalf("update failed: %s", errMsg)
	}
	updated := agentFromData(t, data)
	if len(updated.Tools.Disable) != 1 || updated.Tools.Disable[0] != "tool_x" {
		t.Fatalf("current disable = %v, want [tool_x]", updated.Tools.Disable)
	}

	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListProfileVersions(t.Context(), created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}
	old := versions[0].Profile.Tools.Disable
	if len(old) != 3 || old[0] != "tool_a" || old[1] != "tool_b" || old[2] != "tool_c" {
		t.Fatalf("archived old tools.disable polluted by patch: %v, want [tool_a tool_b tool_c]", old)
	}
}

// purge 幂等：目录已经没了再删一次仍然是成功（双击/重试不该弹 not found）。
func TestHubAgentPurgeIdempotent(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Purgeable", "provider": "deepseek", "model": "deepseek-v4-flash"},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	p := agentFromData(t, data)
	for i := 0; i < 2; i++ {
		ok, errMsg, _ := hubCmd(t, m, buf, 2+i, "hub_agent_delete", map[string]any{
			"agent_id": string(p.ID), "purge": true,
		})
		if !ok {
			t.Fatalf("purge #%d failed: %s", i+1, errMsg)
		}
	}
}

// 软删要记一版（version++ + 归档旧版本），否则回收站里的「恢复」没有历史。
func TestHubAgentSoftDeleteRecordsVersion(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Archived", "provider": "deepseek", "model": "deepseek-v4-flash"},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	p := agentFromData(t, data)
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_delete", map[string]any{"agent_id": string(p.ID)}); !ok {
		t.Fatalf("soft delete failed: %s", errMsg)
	}
	ok, _, data = hubCmd(t, m, buf, 3, "hub_agent_get", map[string]any{"agent_id": string(p.ID)})
	if !ok {
		t.Fatal("get after soft delete failed")
	}
	deleted := agentFromData(t, data)
	if !deleted.Deleted || deleted.Version != p.Version+1 {
		t.Fatalf("deleted/version = %v/%d, want true/%d", deleted.Deleted, deleted.Version, p.Version+1)
	}
	st, _ := m.hubStore()
	versions, err := st.ListProfileVersions(t.Context(), p.ID, 0)
	if err != nil || len(versions) != 1 {
		t.Fatalf("soft delete must archive one version, got %d (%v)", len(versions), err)
	}
	// 重复软删不再记版本（幂等）。
	if ok, _, _ := hubCmd(t, m, buf, 4, "hub_agent_delete", map[string]any{"agent_id": string(p.ID)}); !ok {
		t.Fatal("second soft delete failed")
	}
	versions, _ = st.ListProfileVersions(t.Context(), p.ID, 0)
	if len(versions) != 1 {
		t.Fatalf("idempotent soft delete added a version: %d", len(versions))
	}
}

// MaxAgents 上限只算未删的：满了拒绝，软删一个就放行。
func TestHubAgentCountLimitFreesOnSoftDelete(t *testing.T) {
	m, buf := testManager(t)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	// 直接落盘 50 个（走命令太慢，且不是本用例要测的东西）。
	// id 必须以 ag_ + 8 位 base32 小写（不含 0/1/8/9）。
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	seedID := func(n int) agenthub.AgentID {
		b := make([]byte, 8)
		for k := 7; k >= 0; k-- {
			b[k] = alphabet[n%32]
			n /= 32
		}
		return agenthub.AgentID("ag_" + string(b))
	}
	for i := 0; i < agenthub.MaxAgents; i++ {
		p := &agenthub.AgentProfile{
			ID:       seedID(i),
			Name:     fmt.Sprintf("Seed%d", i),
			Provider: "deepseek", Model: "deepseek-v4-flash",
		}
		if err := p.Normalize(); err != nil {
			t.Fatalf("normalize seed %d: %v", i, err)
		}
		p.Version = 1
		if err := st.SaveAgent(t.Context(), p); err != nil {
			t.Fatalf("save seed %d: %v", i, err)
		}
	}
	ok, errMsg, _ := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Overflow", "provider": "deepseek", "model": "deepseek-v4-flash"},
	})
	if ok || !strings.Contains(errMsg, "limit") {
		t.Fatalf("create beyond MaxAgents must fail, ok=%v err=%q", ok, errMsg)
	}
	// 软删一个 → 名额释放。
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_delete", map[string]any{"agent_id": string(seedID(0))}); !ok {
		t.Fatalf("soft delete seed failed: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Overflow", "provider": "deepseek", "model": "deepseek-v4-flash"},
	}); !ok {
		t.Fatalf("create after soft delete should succeed: %s", errMsg)
	}
}

// 软删可从设置恢复（§5.2）：恢复后回到列表、version++、归档一版；重名/重复恢复都有确定行为。
func TestHubAgentRestore(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Revivable", "provider": "deepseek", "model": "deepseek-v4-flash"},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	p := agentFromData(t, data)
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_delete", map[string]any{"agent_id": string(p.ID)}); !ok {
		t.Fatalf("soft delete failed: %s", errMsg)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_agent_restore", map[string]any{"agent_id": string(p.ID)})
	if !ok {
		t.Fatalf("restore failed: %s", errMsg)
	}
	restored := agentFromData(t, data)
	if restored.Deleted {
		t.Fatal("restored agent still marked deleted")
	}
	if restored.Version != p.Version+2 { // create=1，软删=2，恢复=3
		t.Fatalf("version = %d, want %d", restored.Version, p.Version+2)
	}
	ok, _, data = hubCmd(t, m, buf, 4, "hub_agent_list", map[string]any{})
	if !ok {
		t.Fatal("list failed")
	}
	if agents, _ := data["agents"].([]any); len(agents) != 1 {
		t.Fatalf("restored agent not back in list: %v", agents)
	}
	// 幂等：重复恢复不报错、不再加版本。
	ok, errMsg, data = hubCmd(t, m, buf, 5, "hub_agent_restore", map[string]any{"agent_id": string(p.ID)})
	if !ok {
		t.Fatalf("second restore failed: %s", errMsg)
	}
	if again := agentFromData(t, data); again.Version != restored.Version {
		t.Fatalf("idempotent restore bumped version: %d -> %d", restored.Version, again.Version)
	}
}

// 软删后同名重建 → 恢复必须报冲突（否则两个同名的「它」同时在列表里）。
func TestHubAgentRestoreNameConflict(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Twin", "provider": "deepseek", "model": "deepseek-v4-flash"},
	})
	if !ok {
		t.Fatalf("create failed: %s", errMsg)
	}
	p := agentFromData(t, data)
	if ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_agent_delete", map[string]any{"agent_id": string(p.ID)}); !ok {
		t.Fatalf("soft delete failed: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "Twin", "provider": "deepseek", "model": "deepseek-v4-flash"},
	}); !ok {
		t.Fatalf("recreate after soft delete failed: %s", errMsg)
	}
	ok, errMsg, _ = hubCmd(t, m, buf, 4, "hub_agent_restore", map[string]any{"agent_id": string(p.ID)})
	if ok || !strings.Contains(errMsg, "already used") {
		t.Fatalf("restore with name conflict must fail, ok=%v err=%q", ok, errMsg)
	}
}
