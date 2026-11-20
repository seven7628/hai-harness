package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
)

// writeBridgeSubagent 写一个自定义 subagent 定义文件到 {base}/agents/ 目录。
func writeBridgeSubagent(t *testing.T, base, name, desc, body string) {
	t.Helper()
	dir := filepath.Join(base, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bridgeReqCaptureProvider 捕获请求消息/档位并正常结束一轮（bridge 测试本地实现）。
type bridgeReqCaptureProvider struct {
	messages []core.Message
	efforts  []provider.ReasoningEffortLevel
}

func (p *bridgeReqCaptureProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	p.messages = req.Messages
	if req.Config != nil {
		p.efforts = append(p.efforts, req.Config.Effort)
	}
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

// TestSubagentEffortFollowsMainLoop 子 agent 追随主 loop（2026-09 用户决策）：
// switch_effort 只改主 loop、不重建会话，所以子 agent 必须由持有者同步 ——
// 共享 loop（agent_spawn / subagent_explore）就地 SetReasoningEffort，下一次请求生效；
// 文件化 subagent 的 per-def loop 是调用时新建的，由工厂读 current() 构造。
func TestSubagentEffortFollowsMainLoop(t *testing.T) {
	p := &bridgeReqCaptureProvider{}
	in := []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}
	run := func(l *agents.AgentLoop) provider.ReasoningEffortLevel {
		t.Helper()
		if ac := l.Run(context.Background(), in, nil); ac.Err != nil {
			t.Fatalf("run: %v", ac.Err)
		}
		return p.efforts[len(p.efforts)-1]
	}

	// 装配期：共享子 agent loop 与主 loop 同档（newSessionEngine 用 effort 构建）
	ref := newSubagentEffortRef(provider.ReasoningEffortLevelLow)
	shared := agents.NewAgentLoop(agents.WithProvider(p), agents.WithReasoningEffort(provider.ReasoningEffortLevelLow))
	ref.track(shared)
	if got := run(shared); got != provider.ReasoningEffortLevelLow {
		t.Fatalf("装配期子 agent 档位 = %q, want low", got)
	}

	// 主 loop 切到 max → 共享子 agent loop 立即同档（无需重建会话/引擎）
	ref.set(provider.ReasoningEffortLevelMax)
	if got := run(shared); got != provider.ReasoningEffortLevelMax {
		t.Fatalf("切档后子 agent 档位 = %q, want max（子 agent 追随主 loop）", got)
	}

	// 切档后新建的文件化 subagent loop（工厂读 current()）也是新档位
	perDef := agents.NewAgentLoop(agents.WithProvider(p), agents.WithReasoningEffort(ref.current()))
	if got := run(perDef); got != provider.ReasoningEffortLevelMax {
		t.Fatalf("新建人设 loop 档位 = %q, want max", got)
	}
	// track 幂等：登记时按当前档位对齐（构建期传旧值也不会残留）
	stale := agents.NewAgentLoop(agents.WithProvider(p), agents.WithReasoningEffort(provider.ReasoningEffortLevelLow))
	ref.track(stale)
	if got := run(stale); got != provider.ReasoningEffortLevelMax {
		t.Fatalf("track 后档位 = %q, want max（登记即对齐）", got)
	}
}

// TestBuildSubagentRegistry 注册表构建：工作区 agents/ 定义可见。
func TestBuildSubagentRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	writeBridgeSubagent(t, ws, "ws-only", "工作区独有", "指令")

	reg := buildSubagentRegistry(ws)
	list := reg.List()
	if len(list) != 1 || list[0].Name != "ws-only" {
		t.Fatalf("工作区注册表应只有 ws-only: %+v", list)
	}
}

// TestBuildSubagentRegistryLayers 六层发现 + 优先级（低 → 高）：
// ~/.claude/agents < ~/.go-code/agents < ~/.agents/agents < {ws}/.claude/agents
// < {ws}/agents < {ws}/.agents/agents。
func TestBuildSubagentRegistryLayers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()

	// 全局 Claude Code 兼容层（最低；本机真实形态之一：无 frontmatter）
	claudeGlobal := filepath.Join(home, ".claude", "agents")
	os.MkdirAll(claudeGlobal, 0o755)
	os.WriteFile(filepath.Join(claudeGlobal, "shared.md"), []byte("---\nname: shared\ndescription: claude-global\n---\n指令\n"), 0o644)
	os.WriteFile(filepath.Join(claudeGlobal, "only-claude-global.md"), []byte("# Claude 全局独有\n\n正文。\n"), 0o644)
	// 全局 go-code 命名空间
	gcGlobal := filepath.Join(home, ".go-code", "agents")
	os.MkdirAll(gcGlobal, 0o755)
	os.WriteFile(filepath.Join(gcGlobal, "shared.md"), []byte("---\nname: shared\ndescription: gocode-global\n---\n指令\n"), 0o644)
	os.WriteFile(filepath.Join(gcGlobal, "only-gc-global.md"), []byte("---\nname: only-gc-global\ndescription: 独有\n---\n指令\n"), 0o644)
	// 全局通用协议命名空间
	agGlobal := filepath.Join(home, ".agents", "agents")
	os.MkdirAll(agGlobal, 0o755)
	os.WriteFile(filepath.Join(agGlobal, "shared.md"), []byte("---\nname: shared\ndescription: agents-global\n---\n指令\n"), 0o644)
	os.WriteFile(filepath.Join(agGlobal, "only-ag-global.md"), []byte("---\nname: only-ag-global\ndescription: 独有\n---\n指令\n"), 0o644)
	// 工作区 Claude Code 兼容层
	os.MkdirAll(filepath.Join(ws, ".claude", "agents"), 0o755)
	os.WriteFile(filepath.Join(ws, ".claude", "agents", "shared.md"), []byte("---\nname: shared\ndescription: claude-ws\n---\n指令\n"), 0o644)
	os.WriteFile(filepath.Join(ws, ".claude", "agents", "only-claude-ws.md"), []byte("# 工作区 Claude 独有\n\n正文。\n"), 0o644)
	// 工作区 go-code 命名空间
	os.MkdirAll(filepath.Join(ws, "agents"), 0o755)
	os.WriteFile(filepath.Join(ws, "agents", "shared.md"), []byte("---\nname: shared\ndescription: gocode-ws\n---\n指令\n"), 0o644)
	// 工作区通用协议命名空间（最高）
	os.MkdirAll(filepath.Join(ws, ".agents", "agents"), 0o755)
	os.WriteFile(filepath.Join(ws, ".agents", "agents", "shared.md"), []byte("---\nname: shared\ndescription: agents-ws\n---\n指令\n"), 0o644)
	os.WriteFile(filepath.Join(ws, ".agents", "agents", "only-ag-ws.md"), []byte("---\nname: only-ag-ws\ndescription: 独有\n---\n指令\n"), 0o644)

	reg := buildSubagentRegistry(ws)
	shared, err := reg.Load("shared")
	if err != nil {
		t.Fatal(err)
	}
	if shared.Description != "agents-ws" {
		t.Fatalf("同名应取最高层 {ws}/.agents/agents: %+v", shared)
	}
	// 各层独有都可见（含两个 Claude 兼容层；无 frontmatter 的按文件名/首行补齐）
	for _, n := range []string{"only-claude-global", "only-gc-global", "only-ag-global", "only-claude-ws", "only-ag-ws"} {
		if _, err := reg.Load(n); err != nil {
			t.Fatalf("独有定义 %s 应可见: %v", n, err)
		}
	}
	// Claude 兼容层优先级最低：自家全局定义已覆盖同名
	if def, err := reg.Load("only-claude-global"); err != nil || def.Description != "Claude 全局独有" {
		t.Fatalf("无 frontmatter 的 claude 文件应按首行补 description: %+v (%v)", def, err)
	}
}

// TestBuildSubagentRegistryClaudePrecedence Claude 兼容层与其他命名空间冲突时的
// 排序规则：**作用域优先于命名空间**（工作区 > 全局），同作用域内自家命名空间
// （.agents > .go-code）> claude 兼容层 —— 与既有「通用协议优先、工作区优先」一致。
func TestBuildSubagentRegistryClaudePrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()

	writeDef := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fm := func(name, desc string) string {
		return "---\nname: " + name + "\ndescription: " + desc + "\n---\n指令\n"
	}
	// 全局同名：claude 兼容层 vs .agents → 自家 .agents 赢
	writeDef(filepath.Join(home, ".claude", "agents", "global-clash.md"), "# claude 全局\n\n正文\n")
	writeDef(filepath.Join(home, ".agents", "agents", "global-clash.md"), fm("global-clash", "agents-global"))
	// 工作区同名：claude 兼容层 vs {ws}/agents（go-code 工作区命名空间）→ 自家赢
	writeDef(filepath.Join(ws, ".claude", "agents", "ws-clash.md"), "# claude 工作区\n\n正文\n")
	writeDef(filepath.Join(ws, "agents", "ws-clash.md"), fm("ws-clash", "gocode-ws"))
	// 跨作用域：工作区 claude 兼容层 vs 全局 .agents → 工作区优先（作用域优先）
	writeDef(filepath.Join(home, ".agents", "agents", "scope-clash.md"), fm("scope-clash", "agents-global"))
	writeDef(filepath.Join(ws, ".claude", "agents", "scope-clash.md"), "# claude 工作区\n\n正文\n")

	reg := buildSubagentRegistry(ws)
	for name, want := range map[string]string{
		"global-clash": "agents-global",
		"ws-clash":     "gocode-ws",
		"scope-clash":  "claude 工作区",
	} {
		def, err := reg.Load(name)
		if err != nil {
			t.Fatalf("%s 应可见: %v", name, err)
		}
		if def.Description != want {
			t.Fatalf("%s 应取 %q，got %q", name, want, def.Description)
		}
	}
}

// TestSpawnAgentToolRegistered agent_spawn 已注册（Code profile），且文件化模式已接线
// （defReg/loopForDef 注入——task 省略时可按 name 加载 *.md 定义，2026-08-30 合并 spawn_agent）。
func TestSpawnAgentToolRegistered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, _ := testManager(t)
	ws := t.TempDir()
	writeBridgeSubagent(t, ws, "code-reviewer", "审查代码变更", "你是资深代码审查员")
	eng, _ := newSessionEngine(ws, "s1", nil, nil, m.provCfg, "deepseek", m.provCfg.defaultModel(), string(provider.ReasoningEffortLevelLow),
		func() context.Context { return context.Background() }, subagent.NewRegistry(), events.NewQuestionWaiter(0), buildSkillsRegistry(ws), buildSubagentRegistry(ws), nil, nil, profiles["code"], nil, nil, nil, nil, nil)
	tool, err := eng.GetTool(context.Background(), "agent_spawn")
	if err != nil {
		t.Fatalf("agent_spawn 应已注册: %v", err)
	}
	// 文件化模式已接线：内部 defReg 可用（能加载工作区定义）——通过 ValidParams 验证
	//（task 省略时不再报 "task is required"；defReg 已配置则放行）。
	if err := tool.ValidParams(context.Background(), "agent_spawn", `{"name":"code-reviewer"}`); err != nil {
		t.Fatalf("agent_spawn 文件化模式（task 省略）应通过校验: %v", err)
	}
	// 动态模式不受影响
	if err := tool.ValidParams(context.Background(), "agent_spawn", `{"name":"x","task":"do it"}`); err != nil {
		t.Fatalf("agent_spawn 动态模式应通过校验: %v", err)
	}
}

// TestSubagentManifestInSystemPrompt 主 loop 系统提示词含 subagent 清单（渐进披露）。
func TestSubagentManifestInSystemPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, _ := testManager(t)
	_ = m // testManager 隔离 HOME；loop 直接构造，无需 manager
	ws := t.TempDir()
	writeBridgeSubagent(t, ws, "code-reviewer", "审查代码变更", "你是资深代码审查员")

	sk := buildSkillsRegistry(ws)
	reg := buildSubagentRegistry(ws)
	p := &bridgeReqCaptureProvider{}
	loop := agents.NewAgentLoop(
		agents.WithProvider(p),
		agents.WithSystemPrompt("你是助手"),
		agents.WithSkills(sk),
		agents.WithSubagents(reg),
	)
	_ = loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, nil)

	if len(p.messages) == 0 || p.messages[0].Role != core.System {
		t.Fatalf("system prompt should be first: %+v", p.messages)
	}
	sp := p.messages[0].Content[0].Content
	if !strings.Contains(sp, "Available subagents") || !strings.Contains(sp, "code-reviewer: 审查代码变更") {
		t.Fatalf("系统提示词应含 subagent 清单: %q", sp)
	}
}

// TestCommandReloadSubagents /reload_skills 重扫 subagent 目录：新增定义立即生效。
func TestCommandReloadSubagents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	if _, err := m.create(ws, "s1", ""); err != nil {
		t.Fatal(err)
	}

	// 运行态已建好（此时 subagent 注册表为空）；模拟「之后才放定义文件」
	writeBridgeSubagent(t, ws, "late-agent", "迟到定义", "指令")

	m.dispatch(cmd(0, "command", map[string]any{"workspace": ws, "session_id": "s1", "line": "/reload_skills"}))
	if ok, _ := waitSkillResult(t, buf, "reload_skills", 5*time.Second); !ok {
		t.Fatal("/reload_skills 应被接受")
	}

	// 重扫后定义应可见
	if _, err := m.runtime(ws).subagents.Load("late-agent"); err != nil {
		t.Fatalf("重扫后应可见 late-agent: %v", err)
	}

	m.shutdownWorkspace(ws)
}

// TestAgentToolsPoolWiring 装配断言：agent_spawn 走 Registry 主池且深度上限 = 2
// （主 → sub → sub，再深拒绝）；subagent_explore 走独立辅池。
// 这三项都属于「看起来有、其实悄悄失效」的高危接线（C7 同类）：把 Slots/MaxDepth
// 改回默认（nil/0）不会有编译错误，只会让主 Agent 又能在槽满时整批卡死 —— 先红。
func TestAgentToolsPoolWiring(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, _ := testManager(t)
	ws := t.TempDir()
	reg := subagent.NewRegistry()
	eng, _ := newSessionEngine(ws, "s1", nil, nil, m.provCfg, "deepseek", m.provCfg.defaultModel(), string(provider.ReasoningEffortLevelLow),
		func() context.Context { return context.Background() }, reg, events.NewQuestionWaiter(0), buildSkillsRegistry(ws), buildSubagentRegistry(ws), nil, nil, profiles["code"], nil, nil, nil, nil, nil)

	spawnTool, err := eng.GetTool(context.Background(), "agent_spawn")
	if err != nil {
		t.Fatalf("agent_spawn 应已注册: %v", err)
	}
	spawn, ok := spawnTool.(*subagent.Tool)
	if !ok {
		t.Fatalf("agent_spawn 类型 = %T, want *subagent.Tool", spawnTool)
	}
	if got := spawn.MaxDepthLimit(); got != 2 {
		t.Fatalf("agent_spawn 深度上限 = %d, want 2（主 → sub → sub；0 = 无限会递归自锁）", got)
	}
	if spawn.SlotPool() != nil {
		t.Fatal("agent_spawn 必须走 Registry 主池（Spec.Slots = nil）")
	}

	exploreTool, err := eng.GetTool(context.Background(), "subagent_explore")
	if err != nil {
		t.Fatalf("subagent_explore 应已注册: %v", err)
	}
	explore, ok := exploreTool.(*subagent.Tool)
	if !ok {
		t.Fatalf("subagent_explore 类型 = %T, want *subagent.Tool", exploreTool)
	}
	if explore.SlotPool() != reg.AuxSlots() {
		t.Fatal("subagent_explore 必须用独立辅池（reg.AuxSlots()）——否则与 agent_spawn 互抢槽")
	}
}

// TestSetMaxSubagentsAppliesLive set_max_subagents 命令：热更新所有已创建会话的槽池容量
// （主池 + 辅池），只改容量不重建会话；非法值（0/负数）拒绝且不改变现有容量。
func TestSetMaxSubagentsAppliesLive(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("会话未创建")
	}
	reg := bs.s.Registry()

	m.dispatch(cmd(1, "set_max_subagents", map[string]any{"max_subagents": 7, "max_explore_subagents": 3}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("set_max_subagents 未 ok")
	}
	if got := reg.Concurrency(); got != 7 {
		t.Fatalf("主池容量未热更新: want 7, got %d", got)
	}
	if got := reg.AuxConcurrency(); got != 3 {
		t.Fatalf("辅池容量未热更新: want 3, got %d", got)
	}

	for _, bad := range []int{0, -1} {
		m.dispatch(cmd(2, "set_max_subagents", map[string]any{"max_subagents": bad, "max_explore_subagents": 3}))
		if lastRespOK(t, buf, 2) {
			t.Fatalf("max_subagents=%d 应被拒绝", bad)
		}
		if got := reg.Concurrency(); got != 7 {
			t.Fatalf("拒绝后容量不得变化: want 7, got %d", got)
		}
	}
}
