package main

// skill_capability.go：按**会话选中模型 + 工具集**算技能能力面（2026-10）。
//
// 需求（用户口径）：「用 session 选中的模型来进行 skills 加载与内容生成」。拆成两件事，
// 本文件负责第一件：
//
//  1. **哪些技能对这个会话可见/可加载**：由选中模型的输入模态 + 本 persona 的工具集决定。
//     技能清单注入系统提示词时按它过滤，load_skill 与 /skills load 也按它拒绝 ——
//     三处同源（skills.Capabilities），避免「清单里没有、工具却能加载」的错位。
//  2. **技能内容由谁生成**：恒为本会话 loop 的模型。技能只是指令文本，进入上下文后由
//     当前模型执行；不需要任何额外接线。
//
// ⚠ 第 2 条恰好也是**不**照搬智谱 GLM-skills 的原因：那批技能的做法是让 Python 脚本
// 自己调 GLM-4V 接口做 OCR/看图，模型退化成「调脚本的人」—— 与本需求（用选中模型生成
// 内容）相反，且要求额外的 ZHIPU key 与 requests 依赖。本仓随包的 visual 技能走
// 「模型自己看图（read_file 回 image 块）+ 选中模型自己写产出」。
//
// 为什么能力面必须在**每次重建 loop** 时重算而不是缓存：switch_model 会换掉 loop
// （main.go rebuildLoop），能力面必须跟着换 —— 否则 glm-4.6 → glm-5v-turbo 之后清单
// 里仍然没有看图技能，或反过来纯文本模型看到看图技能（加载一段注定做不成的流程）。

import (
	"context"

	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/tools"
)

// skillCapabilitiesFor 按本会话的 provider/model 与工具引擎算技能能力面。
//
// 入参：
//   - pc      provider 配置（取模型元数据注册表；nil 时退化为「只有 text」）
//   - prov    provider id（如 zhipu）
//   - model   会话选中的模型（如 glm-5v-turbo）
//   - eng     本会话的工具引擎（Explore 子 agent 传的是它自己那份窄工具集）
//
// 模型不在注册表里（自定义端点新模型）时 **Input 只给 text**：conservative 默认，
// 与 provider.Registry.SupportsImageInput 的「未知模型默认不支持」同口径 ——
// 宁可让看图技能不出现（用户会去查模型配置），也不要让模型以为能看图却拿到占位文本。
//
// 恒非 nil 返回：调用方直接 WithSkillCapabilities/WithCapabilities，不必判空。
func skillCapabilitiesFor(pc *providerConfig, prov, model string, eng *tools.Engine) *skills.Capabilities {
	// info.Inputs 是注册表里的真实模态列表（text/image/video/pdf）；空/未知 → 只有 text。
	input := modelInputs(pc, prov, model)
	var toolNames []string
	if eng != nil {
		for _, t := range eng.Tools() {
			toolNames = append(toolNames, t.Name())
		}
	}
	return skills.NewCapabilities(sessionModelLabel(prov, model), input, toolNames)
}

// skillCapabilitiesForTools 只按**工具名清单**算能力面（不需要先建引擎的场合：
// 例如 Explore 子 agent 的窄工具集 —— 它的工具集是静态常量，
// 而 load_skill 自己不该参与 tools 需求判定，见 exploreAllowedTools 注释）。
// 模型信息仍从注册表取（Input 模态）。
func skillCapabilitiesForTools(pc *providerConfig, prov, model string, toolNames []string) *skills.Capabilities {
	return skills.NewCapabilities(sessionModelLabel(prov, model), modelInputs(pc, prov, model), toolNames)
}

// modelInputs 模型支持的输入模态（"text" 恒在；未知模型保守只给 text）。
// 抽出来给 skillCapabilitiesFor / skillCapabilitiesForTools 共用，避免两处各判一遍。
func modelInputs(pc *providerConfig, prov, model string) []string {
	if pc != nil && pc.registry != nil {
		if info, ok := pc.registry.Lookup(prov, model); ok && len(info.Inputs) > 0 {
			return append([]string(nil), info.Inputs...)
		}
	}
	return []string{"text"}
}

// sessionModelLabel 会话模型的人读标签（能力面错误文案用）："zhipu/glm-4.6"。
// provider 为空时只给模型名（自定义模型的常见形态）。
func sessionModelLabel(prov, model string) string {
	if prov == "" {
		return model
	}
	return prov + "/" + model
}

// registerLoadSkill 注册 load_skill 工具并把能力面绑进去（主 / Explore / persona 三处共用）。
//
// 单点注册的理由：能力面必须在 load_skill 与清单之间**逐字节同源**，两处各写一遍
// 迟早漂移（漂移的表现是「系统提示词里没有的技能，模型却加载成功了」，很难发现）。
// onLoad 为 nil 时不挂钩子。
func registerLoadSkill(ctx context.Context, eng *tools.Engine, sk *skills.Registry, caps *skills.Capabilities, onLoad func(string)) {
	if sk == nil || eng == nil {
		return
	}
	tool := skills.NewLoadSkillTool(sk).WithCapabilities(caps)
	if onLoad != nil {
		tool = tool.WithOnLoad(onLoad)
	}
	eng.RegisterTool(ctx, tool)
}
