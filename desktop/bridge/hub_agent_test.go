package main

// P1-03：智能体 CRUD 命令（create/get/list/update/delete + 校验 + 版本 + 事件）。

import (
	"encoding/json"
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
