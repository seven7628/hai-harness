package main

// 场景1 收尾：hub_agent_prompt_preview 的作用域。
//
// L1 协议与 L2 名册都按作用域分叉（DM 变体没有 @/assign_task），而预览原先只传
// PromptSpec{Agent: p} —— Group 恒 nil，所以群会话的预览也永远显示 DM 变体。
// payload 现在可选带 group_id：带上走群变体 + 名册，不带保持旧行为（DM 变体）。

import (
	"strings"
	"testing"
)

func promptLayerText(data map[string]any, key string) string {
	layers, _ := data["layers"].([]any)
	for _, raw := range layers {
		layer, _ := raw.(map[string]any)
		if str(layer, "key") == key {
			return str(layer, "text")
		}
	}
	return ""
}

func promptLayerKeys(data map[string]any) string {
	layers, _ := data["layers"].([]any)
	keys := make([]string, 0, len(layers))
	for _, raw := range layers {
		layer, _ := raw.(map[string]any)
		keys = append(keys, str(layer, "key"))
	}
	return strings.Join(keys, ",")
}

func promptLayerTexts(data map[string]any) []string {
	layers, _ := data["layers"].([]any)
	out := make([]string, 0, len(layers))
	for _, raw := range layers {
		layer, _ := raw.(map[string]any)
		out = append(out, str(layer, "text"))
	}
	return out
}

func TestHubPromptPreviewScope(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "PreviewOwner")
	b := createHubAgent(t, m, buf, 2, "PreviewPeer")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name": "预览群", "members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	g := groupFromData(t, data)

	// 1) 旧调用形状（只传 agent_id）：DM 变体，且不含群工具名。
	ok, errMsg, dmData := hubCmd(t, m, buf, 4, "hub_agent_prompt_preview", map[string]any{
		"agent_id": string(a.ID),
	})
	if !ok {
		t.Fatalf("dm preview: %s", errMsg)
	}
	if got := str(dmData, "scope"); got != "dm" {
		t.Fatalf("dm preview scope = %q, want dm", got)
	}
	dmL1 := promptLayerText(dmData, "protocol")
	if dmL1 == "" {
		t.Fatalf("dm preview has no protocol layer: %v", dmData)
	}
	for _, unwanted := range []string{"group_members", "group_history", "assign_task", "@Name"} {
		if strings.Contains(dmL1, unwanted) {
			t.Fatalf("dm preview L1 must not mention %q:\n%s", unwanted, dmL1)
		}
	}
	// workspace 段 DM 也适用：预览里必须还在（不能因为去群工具把这块一起删掉）。
	if !strings.Contains(dmL1, "session_send()") || !strings.Contains(dmL1, "task_status()") {
		t.Fatalf("dm preview L1 lost the workspaces section:\n%s", dmL1)
	}

	// 2) 带 group_id：群变体 L1 + 名册层。
	ok, errMsg, gData := hubCmd(t, m, buf, 5, "hub_agent_prompt_preview", map[string]any{
		"agent_id": string(a.ID), "group_id": string(g.ID),
	})
	if !ok {
		t.Fatalf("group preview: %s", errMsg)
	}
	if got := str(gData, "scope"); got != "group" {
		t.Fatalf("group preview scope = %q, want group", got)
	}
	gL1 := promptLayerText(gData, "protocol")
	for _, want := range []string{"## Who is here", "@Name", "assign_task", "group_members()"} {
		if !strings.Contains(gL1, want) {
			t.Fatalf("group preview L1 missing %q:\n%s", want, gL1)
		}
	}
	if gL1 == dmL1 {
		t.Fatal("group preview still renders the DM variant")
	}
	if keys := promptLayerKeys(gData); keys != "protocol,roster,identity,environment,memory" {
		t.Fatalf("group preview layers = %s, want protocol,roster,identity,environment,memory", keys)
	}
	if roster := promptLayerText(gData, "roster"); !strings.Contains(roster, "PreviewPeer") {
		t.Fatalf("group preview roster missing the other member:\n%s", roster)
	}

	// 3) 与真正下发的提示词同源：各层按 "\n\n" 拼起来 == 该作用域会话的完整系统提示词。
	//    这条是「预览不能另写一份文案」的硬断言，也顺带锁住 L1 变体的选择口径。
	bsGroup, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(promptLayerTexts(gData), "\n\n"), bsGroup.loop.SystemPromptText(); got != want {
		t.Fatalf("group preview != composed system prompt (len %d vs %d)", len(got), len(want))
	}
	bsDM, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(promptLayerTexts(dmData), "\n\n"), bsDM.loop.SystemPromptText(); got != want {
		t.Fatalf("dm preview != composed system prompt (len %d vs %d)", len(got), len(want))
	}

	// 4) 未知 group_id 明确报错，不静默退回 DM 变体（否则设置页会显示错的层）。
	if ok, _, _ := hubCmd(t, m, buf, 6, "hub_agent_prompt_preview", map[string]any{
		"agent_id": string(a.ID), "group_id": "gr_doesnotexist",
	}); ok {
		t.Fatal("preview with unknown group_id must fail")
	}
}
