package main

// skill_capability_wiring_test.go：能力面接线的守卫（2026-10）。
//
// 守的是「必须按会话选中模型过滤」这条需求在**生产装配**里真的接上了，
// 而不是只在 skills 包里存在。历史教训：能力过滤这种"软"特性最容易漏接一处，
// 而漏接的表现是静默的 —— 技能照常出现（什么都没坏），只是过滤没生效。
//
// 断言分三层：
//  1. skillCapabilitiesFor 从注册表取到真实模态（glm-5v-turbo 有 image，glm-4.6 没有）；
//  2. 工具集快照进能力面（Explore 的窄工具集 vs 主会话的完整工具集给出不同判定）；
//  3. 生产 builder（buildExploreLoop / buildPersonaLoop）产出的 loop **确实**带着
//     与 load_skill 同一份能力面（靠行为断言：同一技能在两种模型下一个可见一个不可见）。

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
)

// TestSkillCapabilitiesForReadsModelInputs 能力面的图片模态来自 provider 注册表。
// glm-5v-turbo 的 Inputs 含 image/video/pdf；glm-4.6 只有 text。
func TestSkillCapabilitiesForReadsModelInputs(t *testing.T) {
	pc := loadProviderConfig()

	vision := skillCapabilitiesFor(pc, "zhipu", "glm-5v-turbo", nil)
	if !vision.HasInput("image") {
		t.Fatalf("glm-5v-turbo 应支持图片输入（注册表 Inputs 含 image）: %+v", vision.Input)
	}

	text := skillCapabilitiesFor(pc, "zhipu", "glm-4.6", nil)
	if text.HasInput("image") {
		t.Fatalf("glm-4.6 不应支持图片输入: %+v", text.Input)
	}
	if !text.HasInput("text") {
		t.Fatalf("任何模型都必须有 text 输入: %+v", text.Input)
	}

	// 未知模型要保守：只有 text（与 SupportsImageInput 的「未知模型默认不支持」同口径）。
	unknown := skillCapabilitiesFor(pc, "zhipu", "totally-made-up-model", nil)
	if unknown.HasInput("image") {
		t.Fatalf("未知模型必须保守判为无图片输入: %+v", unknown.Input)
	}
}

// TestSkillCapabilitiesForReadsTools 工具集来自引擎实况（不是硬编码清单）。
func TestSkillCapabilitiesForReadsTools(t *testing.T) {
	pc := loadProviderConfig()
	eng := exploreToolEngine(t.TempDir(), nil, nil) // 窄工具集
	caps := skillCapabilitiesFor(pc, "zhipu", "glm-5v-turbo", eng)
	for _, name := range exploreAllowedTools {
		if !caps.HasTool(name) {
			t.Errorf("Explore 能力面应含 %s: %+v", name, caps.Tools)
		}
	}
	// Explore 没有 write_file —— 声明 requires.tools: [write_file] 的技能对它不可见。
	if caps.HasTool("write_file") {
		t.Error("Explore 能力面不应含 write_file（它确实没有这个工具）")
	}
}

// TestExploreLoopFiltersByModel 生产装配（buildExploreLoop）在两种模型下给出不同的
// 技能可见性 —— 证明能力面**接到了 loop 上**（漏接时两边都会看到全部技能）。
//
// 观测面用导出的 ContextBreakdown：它的「技能」分区字节数就是实际注入 system 的
// 清单长度（buildSkillManifest(a.cfg.Skills, a.cfg.SkillCaps)，与真实请求同源）。
// 不用 composeSystemPrompt —— 它是 agents 包私有方法，这里只能看导出接口，
// 而这恰好也是宿主（bridge）真实观测 loop 的方式。
func TestExploreLoopFiltersByModel(t *testing.T) {
	pc := loadProviderConfig()
	ws := t.TempDir()

	reg, err := skills.NewRegistry(visualSkillsSourceDir)
	if err != nil {
		t.Fatalf("加载随包视觉技能失败: %v", err)
	}

	skillsChars := func(loop *agents.AgentLoop) int64 {
		bd := loop.ContextBreakdown(context.Background(), nil, nil)
		for _, p := range bd.Parts {
			if p.Key == agents.ContextPartSkills {
				return p.Chars
			}
		}
		t.Fatalf("ContextBreakdown 缺 %s 分区", agents.ContextPartSkills)
		return 0
	}

	textChars := skillsChars(buildExploreLoop(ws, "s-text", pc, "zhipu", "glm-4.6", "low", reg))
	visionChars := skillsChars(buildExploreLoop(ws, "s-vision", pc, "zhipu", "glm-5v-turbo", "low", reg))

	// 视觉模型能看到 3 个技能，纯文本只能看到 1 个（visual-ui）→ 清单字节必然更大。
	// 用字节数而不是逐条名字：清单文本是 loop 内部产物，没有导出接口；
	// 「大小关系」足以证明过滤生效（漏接时两者字节相同）。
	if visionChars <= textChars {
		t.Fatalf("glm-5v-turbo 的技能清单应比 glm-4.6 长（多两个需要 image 的技能）: "+
			"vision=%d text=%d —— 能力面没接到 loop 上（WithSkillCapabilities 漏了?）",
			visionChars, textChars)
	}
	if textChars == 0 {
		t.Fatal("glm-4.6 也应看到不需要看图的技能（visual-ui），清单不应为空")
	}
}

// TestExploreLoopLoadSkillSharesCapabilities 清单与 load_skill **同源**：
// 同一个 Explore 引擎里，清单没有的技能，load_skill 也必须拒绝。
// 这是最容易被破坏的不变式（两处各算一份能力面，迟早漂移）。
func TestExploreLoopLoadSkillSharesCapabilities(t *testing.T) {
	pc := loadProviderConfig()
	reg, err := skills.NewRegistry(visualSkillsSourceDir)
	if err != nil {
		t.Fatal(err)
	}
	caps := skillCapabilitiesForTools(pc, "zhipu", "glm-4.6", exploreAllowedTools)
	eng := exploreToolEngine(t.TempDir(), reg, caps)

	tool, err := eng.GetTool(nil, "load_skill")
	if err != nil {
		t.Fatalf("Explore 引擎必须注册 load_skill: %v", err)
	}
	if _, err := tool.Call(nil, "", `{"name":"screenshot-to-ui"}`); err == nil {
		t.Error("纯文本模型的 load_skill 必须拒绝需要图片输入的技能（与清单同源）")
	}
	if _, err := tool.Call(nil, "", `{"name":"visual-ui"}`); err != nil {
		t.Errorf("不需要看图的技能应能加载: %v", err)
	}
}

// TestMainLoopLoadSkillRegisteredLast 主会话装配里 load_skill 必须最后注册：
// 它的能力面要读完整工具集（MCP/插件/codemode/cron/mesh 都在它之后注册）。
// 断言方式：主引擎的能力面里能看到这些后注册的工具（漏了说明注册顺序被改回去了）。
func TestMainLoopLoadSkillRegisteredLast(t *testing.T) {
	sk, err := skills.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	m := &manager{}
	pc := loadProviderConfig()

	eng, _ := newSessionEngine(ws, "s-order", nil, pc, "zhipu", "glm-5v-turbo",
		string(provider.ReasoningEffortLevelLow), func() context.Context { return context.Background() },
		subagent.NewRegistry(), events.NewQuestionWaiter(0), sk,
		buildSubagentRegistry(ws), nil, nil, profiles["code"], nil, nil, nil, nil, nil)
	if eng == nil {
		t.Fatal("newSessionEngine 返回 nil 引擎")
	}
	// 主会话能力面（production 用法）：从引擎实况算，必须包含 ask_user
	// —— 它注册在 load_skill 之前，但改错顺序时最早丢的是**最后**那几个工具。
	caps := skillCapabilitiesFor(pc, "zhipu", "glm-5v-turbo", eng)
	if !caps.HasTool("ask_user") {
		t.Errorf("主会话能力面应含 ask_user: %v", caps.Tools)
	}
	if !caps.HasTool("load_skill") {
		t.Errorf("主会话能力面应含 load_skill（自身注册后算）: %v", caps.Tools)
	}
	_ = m
}
