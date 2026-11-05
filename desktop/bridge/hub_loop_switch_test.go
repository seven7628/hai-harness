package main

// P1-05 复核收尾：Hub 会话的 loop 重建必须留在 Hub 装配上。
//
// switch_model/switch_effort/set_provider 都走 m.rebuildLoop(bs)，而 bs.persona=="hub"
// 不在 profiles 表里 —— 落到项目分支会拿到零值 personaProfile：L1/L2 丢失、base 回落
// DefaultSystemPrompt、工具面按项目重装。

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/agents"
)

// partChars 会话「下一次 Run」的静态分区字符数（system/skills/tools/mcp/memory）。
func partChars(bs *bridgeSession) map[string]int64 {
	bd := bs.s.ContextBreakdown(context.Background())
	out := map[string]int64{}
	for _, part := range bd.Parts {
		out[part.Key] = part.Chars
	}
	return out
}

func TestHubRebuildLoopKeepsHubAssembly(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Switch")
	bs, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	before := partChars(bs)
	if before[agents.ContextPartTools] == 0 || before[agents.ContextPartSystem] == 0 {
		t.Fatalf("baseline partitions missing: %v", before)
	}
	// switch_model 的路径：先改会话选择，再重建。
	bs.model, bs.provider = "deepseek-v4-pro", "deepseek"
	bs.effort = "high"
	m.rebuildLoop(bs)
	if got := bs.loop.Model(); got != "deepseek-v4-pro" {
		t.Fatalf("rebuilt loop model = %q, want deepseek-v4-pro", got)
	}
	after := partChars(bs)
	// 装配不变时工具面/skills/系统提示词逐字节相同；落到项目分支会整片变化。
	for _, key := range []string{agents.ContextPartSystem, agents.ContextPartSkills, agents.ContextPartTools, agents.ContextPartMCP} {
		if before[key] != after[key] {
			t.Fatalf("partition %q changed on switch_model: %d -> %d (Hub 会话被换成了项目装配)", key, before[key], after[key])
		}
	}
	if bs.s.Loop() != bs.loop {
		t.Fatal("session still runs the old loop")
	}
}

// 会话 ID 解析：rebuild/回滚/记忆都需要从 sid 反查 agentID 与作用域。
func TestHubSIDParsing(t *testing.T) {
	cases := []struct {
		sid   string
		agent string
		kind  string
	}{
		{"hub.ag_aaaaaaaa.dm", "ag_aaaaaaaa", "dm"},
		{"hub.ag_aaaaaaaa.g.gr_gggggggg", "ag_aaaaaaaa", "group"},
		{"project.session", "", "dm"},
		{"hub.", "", "dm"},
	}
	for _, tc := range cases {
		if got := hubAgentIDFromSID(tc.sid); got != tc.agent {
			t.Errorf("hubAgentIDFromSID(%q) = %q, want %q", tc.sid, got, tc.agent)
		}
		scope := hubScopeFromSID(tc.sid)
		want := "dm"
		if tc.kind == "group" {
			want = "group"
		}
		got := "dm"
		if scope.Kind == "group" {
			got = "group"
		}
		if got != want {
			t.Errorf("hubScopeFromSID(%q).Kind = %v, want %q", tc.sid, scope.Kind, want)
		}
	}
}
