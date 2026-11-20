package main

// P1-02：Hub AgentLoop 装配（L0 关闭、显式 provider 采样、记忆注入）。

import (
	"context"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
)

// captureHubLoopRun 用 mock provider 跑一轮，返回真正发出去的 StreamRequest。
func captureHubLoopRun(t *testing.T, opts []agents.Option) *provider.StreamRequest {
	t.Helper()
	var captured *provider.StreamRequest
	mock := &bridgeMockProvider{rounds: []func(req *provider.StreamRequest, handler func(provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
			cp := *req
			captured = &cp
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	opts = append(opts, agents.WithProvider(mock), agents.WithMaxIterations(1))
	loop := agents.NewAgentLoop(opts...)
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"})},
		func(context.Context, events.Event) {})
	if ac == nil {
		t.Fatal("loop.Run returned nil AgentContext")
	}
	if captured == nil {
		t.Fatal("mock provider was not called")
	}
	return captured
}

func systemTextOf(req *provider.StreamRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role != core.System {
			continue
		}
		for _, c := range m.Content {
			b.WriteString(c.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func hubLoopFor(t *testing.T, m *manager, ws *workspaceRuntime, spec *hubSessionSpec) *provider.StreamRequest {
	t.Helper()
	eng, _ := m.buildHubEngine(ws, "s-hub-loop", spec, nil, subagent.NewRegistry(),
		events.NewQuestionWaiter(0), func() context.Context { return context.Background() })
	return captureHubLoopRun(t, m.hubLoopOptions(ws, "s-hub-loop", spec, eng))
}

// L0 必须真的关掉：system 里只有 L1 协议（+L2 身份），没有 SDK 的 DefaultSystemPrompt。
// WithBaseSystemPrompt("") 不传的话内核会补 L0 —— 这一条就是那个回归。
func TestHubLoopDropsL0KeepsL1(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_loop")
	spec := &hubSessionSpec{Agent: agenthub.AgentProfile{
		ID: "ag_loop", Name: "Researcher", Role: "竞品研究员",
		Provider: "deepseek", Model: "deepseek-v4-flash",
	}}
	req := hubLoopFor(t, m, ws, spec)
	system := systemTextOf(req)

	if !strings.Contains(system, "You are an agent in Agent Hub.") {
		t.Fatalf("L1 protocol prompt missing from system: %q", system)
	}
	for _, l0 := range []string{
		"You are an AI agent working in a long-running, event-driven agent harness",
		"Help the user by working iteratively",
	} {
		if strings.Contains(system, l0) {
			t.Fatalf("L0 leaked into hub system prompt (%q): %q", l0, system)
		}
	}
	want := agenthub.RenderSystem(agenthub.PromptSpec{Agent: &spec.Agent})
	if !strings.Contains(system, want) {
		t.Fatalf("system prompt does not contain RenderSystem output.\n--- want ---\n%s\n--- system ---\n%s", want, system)
	}
	// L2 身份层：名字/职责来自 Profile，而不是全局 Persona。
	if !strings.Contains(system, "Researcher") {
		t.Fatalf("identity layer missing from system: %q", system)
	}
}

// 显式 provider 的采样默认：DeepSeek 显式传温度/top_p，其它厂商不传（厂商默认）。
// 必须按**入参 provider** 取 —— 读全局 active 会让 A/B 两个智能体互串。
func TestHubLoopSamplingDefaultsFollowExplicitProvider(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_loop")

	ds := hubLoopFor(t, m, ws, &hubSessionSpec{Agent: agenthub.AgentProfile{
		ID: "ag_ds", Provider: "deepseek", Model: "deepseek-v4-flash",
	}})
	if ds.Config == nil || ds.Config.Temperature == nil || *ds.Config.Temperature != 1.0 {
		t.Fatalf("deepseek must get explicit temperature 1.0, got %+v", ds.Config)
	}
	if ds.Config.TopP == nil || *ds.Config.TopP != 0.95 {
		t.Fatalf("deepseek must get explicit top_p 0.95, got %+v", ds.Config)
	}

	oa := hubLoopFor(t, m, ws, &hubSessionSpec{Agent: agenthub.AgentProfile{
		ID: "ag_oa", Provider: "openai", Model: "gpt-5",
	}})
	if oa.Config != nil && oa.Config.Temperature != nil {
		t.Fatalf("openai must keep vendor defaults, got temperature=%v", *oa.Config.Temperature)
	}
	// 关键：装配智能体不得改动全局 active provider（否则 A=DeepSeek、B=Anthropic
	// 同时跑时，后装配的会把前一个的采样/模型带过去）。
	activeBefore := m.provCfg.providerName()
	oa2 := hubLoopFor(t, m, ws, &hubSessionSpec{Agent: agenthub.AgentProfile{
		ID: "ag_oa2", Provider: "openai", Model: "gpt-5",
	}})
	if oa2.Config != nil && oa2.Config.Temperature != nil {
		t.Fatalf("openai must not inherit deepseek sampling, got temperature=%v", *oa2.Config.Temperature)
	}
	if got := m.provCfg.providerName(); got != activeBefore {
		t.Fatalf("hub loop assembly changed global active provider: %q -> %q", activeBefore, got)
	}
}

// 记忆注入 + 压缩阈值 + provider/model 元数据。
func TestHubLoopMemoryAndCompress(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_loop")
	spec := &hubSessionSpec{
		Agent: agenthub.AgentProfile{
			ID: "ag_mem", Provider: "deepseek", Model: "deepseek-v4-flash",
			CompressThreshold: 0.7,
		},
		MemoryInjection: "## Memory\n- 记住：用户叫小王",
	}
	req := hubLoopFor(t, m, ws, spec)
	if req.Provider != "deepseek" || req.Model != "deepseek-v4-flash" {
		t.Fatalf("request provider/model = %q/%q", req.Provider, req.Model)
	}
	if !strings.Contains(systemTextOf(req), "用户叫小王") {
		t.Fatalf("memory injection missing from system: %q", systemTextOf(req))
	}
	// compress_threshold 是行为级可观测的（AgentLoop.CompressThreshold），
	// 不能只靠「选项构造时传了」——那和没测一样（P1-02 复核）。
	eng, _ := m.buildHubEngine(ws, "s-hub-loop", spec, nil, subagent.NewRegistry(),
		events.NewQuestionWaiter(0), func() context.Context { return context.Background() })
	if got := m.buildHubLoop(ws, "s-hub-loop", spec, eng).CompressThreshold(); got != 0.7 {
		t.Fatalf("compress threshold = %v, want 0.7", got)
	}
	// 零值 → 默认；越界 → 钳到 [0.5, 0.95]（装配层再钳一次，防手工 spec 绕过 Normalize）。
	for _, tc := range []struct{ in, want float64 }{
		{0, agenthub.DefaultCompressThreshold},
		{0.97, agenthub.MaxCompressThreshold},
		{0.1, agenthub.MinCompressThreshold},
	} {
		spec := &hubSessionSpec{Agent: agenthub.AgentProfile{
			ID: "ag_th", Provider: "deepseek", Model: "deepseek-v4-flash", CompressThreshold: tc.in,
		}}
		eng, _ := m.buildHubEngine(ws, "s-hub-loop", spec, nil, subagent.NewRegistry(),
			events.NewQuestionWaiter(0), func() context.Context { return context.Background() })
		if got := m.buildHubLoop(ws, "s-hub-loop", spec, eng).CompressThreshold(); got != tc.want {
			t.Fatalf("compress threshold(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// 零值 Profile 的 L2 层必须稳定（不追加空层）：两次装配的 system 字节一致。
func TestHubLoopPersonaEmptyIsStable(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_loop")
	spec := &hubSessionSpec{Agent: agenthub.AgentProfile{
		ID: "ag_stable", Provider: "deepseek", Model: "deepseek-v4-flash",
	}}
	a := systemTextOf(hubLoopFor(t, m, ws, spec))
	b := systemTextOf(hubLoopFor(t, m, ws, spec))
	if a != b {
		t.Fatalf("hub system prompt is not byte-stable with empty persona:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	// 「空层稳定」= 不追加空的身份层，而不只是两次结果相同（P1-02 复核：后者恒真）。
	// 无条件写一行 "## Identity" 也会让字节稳定，但那是错的。
	if strings.Contains(a, "## Identity") {
		t.Fatalf("empty persona must not emit an Identity layer:\n%s", a)
	}
	if strings.Contains(a, "\n\n\n") {
		t.Fatalf("empty persona must not leave a blank layer (triple newline):\n%s", a)
	}
}

// 群名册层必须用 Members 渲染出真名/职责，而不是退化成 "unknown member (ag_…)"。
// Hub 会话一旦挂 Group（P3a），缺 Members 会静默产出错名册（P1-02 复核）。
func TestHubLoopRosterUsesMembers(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_loop")
	owner := &agenthub.AgentProfile{ID: "ag_owner", Name: "Writer", Role: "长文写作"}
	member := &agenthub.AgentProfile{ID: "ag_member", Name: "Reviewer", Role: "代码评审"}
	spec := &hubSessionSpec{
		Agent: agenthub.AgentProfile{ID: "ag_owner", Name: "Writer", Provider: "deepseek", Model: "deepseek-v4-flash"},
		Group: &agenthub.Group{
			ID: "grp_x", Name: "写作组",
			Members: []agenthub.Member{
				{AgentID: "ag_owner", Role: agenthub.RoleOwner},
				{AgentID: "ag_member", Role: agenthub.RoleMember},
			},
		},
		Members: []*agenthub.AgentProfile{owner, member},
	}
	system := systemTextOf(hubLoopFor(t, m, ws, spec))
	for _, want := range []string{"Writer", "Reviewer", "长文写作", "代码评审"} {
		if !strings.Contains(system, want) {
			t.Fatalf("roster missing %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, "unknown member") {
		t.Fatalf("roster degraded to unknown member:\n%s", system)
	}
}
