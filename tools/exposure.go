package tools

// exposure.go：工具暴露档位与「声明给模型 / 可被编排」的分层（Phase 2 接口冻结）。
//
// 对齐 pi docs/extensions.md:154-176 的两层语义，**callable ≠ declared**：
//
//	档位          声明给模型(进 ToolParams)   可被编排(脚本内 tools.x)   进 codemode 描述
//	direct              ✅                        ✅                      ✅
//	model-only          ✅                        ❌（禁自嵌套靠它）        ❌
//	codemode            ❌                        ✅                      ✅
//	deferred            ❌                        ✅                      ❌（由 tool_search 现查）
//	hidden              ❌                        ❌（撤下工具的唯一手段）   ❌
//
// 未实现 ExposureProvider 的工具 = direct：既有工具零迁移成本，行为逐字节不变。

import (
	"context"

	"github.com/seven7628/hai-harness/core"
)

// ToolExposure 工具的暴露档位。
type ToolExposure string

const (
	ExposureDirect    ToolExposure = "direct"     // 声明给模型 + 可编排
	ExposureModelOnly ToolExposure = "model-only" // 声明给模型，永不可编排（codemode 自身）
	ExposureCodemode  ToolExposure = "codemode"   // 可编排 + 进 codemode 描述；不声明给模型
	ExposureDeferred  ToolExposure = "deferred"   // 可编排但不进 codemode 描述；可被 tool_search 加载
	ExposureHidden    ToolExposure = "hidden"     // 已注册但不可达（撤销手段：重注册覆盖）
)

// ExposureProvider 可选接口：工具声明自己的暴露档位。
type ExposureProvider interface {
	Exposure() ToolExposure
}

// ToolExposureOf 读工具的暴露档位；未实现 ExposureProvider 或返回空值 = direct。
func ToolExposureOf(t Tool) ToolExposure {
	if t == nil {
		return ExposureDirect
	}
	if ep, ok := t.(ExposureProvider); ok {
		switch e := ep.Exposure(); e {
		case ExposureDirect, ExposureModelOnly, ExposureCodemode, ExposureDeferred, ExposureHidden:
			return e
		}
		// 非法值（拼写错误等）按最保守处理：既不声明也不可编排，
		// 否则一个 typo 会让工具意外进模型上下文。
		return ExposureHidden
	}
	return ExposureDirect
}

// DeclaredToModel 该工具是否进模型的工具列表（ToolParams）。
func DeclaredToModel(t Tool) bool {
	switch ToolExposureOf(t) {
	case ExposureDirect, ExposureModelOnly:
		return true
	}
	return false
}

// CallableFromScript 该工具是否可被编排型工具（codemode）从脚本内调用。
func CallableFromScript(t Tool) bool {
	if t == nil {
		return false
	}
	switch ToolExposureOf(t) {
	case ExposureDirect, ExposureCodemode, ExposureDeferred:
		return true
	}
	return false
}

// ListedInCodemodeCatalog 该工具是否进 codemode 的描述目录（deferred 刻意不列举：
// 模型该用 searchTools() 现查，否则 MCP 工具会重新把描述撑爆 —— pi #10212）。
func ListedInCodemodeCatalog(t Tool) bool {
	if t == nil {
		return false
	}
	switch ToolExposureOf(t) {
	case ExposureDirect, ExposureCodemode:
		return true
	}
	return false
}

// ToolAnnotations 行为提示（对齐 MCP annotations，pi docs/extensions.md:164-176）。
// 未实现 AnnotationsProvider = 全部零值，按 MCP 默认解释（非只读、可能破坏、
// 可能触达开放世界）——保守安全。
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"read_only_hint,omitempty"`
	DestructiveHint bool `json:"destructive_hint,omitempty"`
	IdempotentHint  bool `json:"idempotent_hint,omitempty"`
	OpenWorldHint   bool `json:"open_world_hint,omitempty"`
}

// AnnotationsProvider 可选接口：工具声明行为提示。
type AnnotationsProvider interface {
	Annotations() ToolAnnotations
}

// AnnotationsOf 读行为提示；未实现 = 零值（MCP 默认语义）。
func AnnotationsOf(t Tool) ToolAnnotations {
	if ap, ok := t.(AnnotationsProvider); ok {
		return ap.Annotations()
	}
	return ToolAnnotations{}
}

// ToolNamespace 命名空间（MCP server 或工具组）：进 codemode 描述的分组标题下。
type ToolNamespace struct {
	Name string
	// Description 进 codemode 描述（分组标题下的一句话）。
	Description string
	// Instructions 较长使用指引；**不进描述**，由 describeNamespace() 按需读取
	// （描述预算只有 3000 估算 token，长文进去会把别的工具挤掉）。
	Instructions string
}

// NamespaceProvider 可选接口：工具声明自己所属命名空间。
type NamespaceProvider interface {
	Namespace() *ToolNamespace
}

// NamespaceOf 读命名空间；nil = 无归属（进描述时归入默认组）。
func NamespaceOf(t Tool) *ToolNamespace {
	if np, ok := t.(NamespaceProvider); ok {
		return np.Namespace()
	}
	return nil
}

// OutputSchemaProvider 可选接口：工具声明结构化返回契约。
//
// 声明后**编排路径**（脚本内 tools.x(...)）拿 structuredContent（JSON 原文）而非
// 拼接文本 —— 这是脚本里能做结构化过滤/聚合的前提；模型路径仍是文本（不变）。
//
// 取值时机与 ToolUsageProvider / ToolDiffProvider 同构：Call 内暂存、引擎在 Call
// 成功返回**之后**调用 StructuredContent()（并行调用同一工具的竞态顾虑与那两个先例一致）。
// 返回 nil = 退回文本路径（工具可以按本次参数决定不给结构化结果）。
type OutputSchemaProvider interface {
	// OutputSchema 结构化结果的 JSON Schema（描述生成用；nil = 不声明 schema）。
	OutputSchema() any
	// StructuredContent 本次调用的结构化结果原文；nil = 无（退回文本）。
	StructuredContent() []byte
}

// SkipTruncateProvider 可选接口：本次结果**不做**引擎的 defaultMaxResponseSize
// （20 KB）头尾截断。
//
// 只给编排型工具实现（codemode 的输出预算由它自己按 max_output_tokens + spill 管理，
// 被引擎再砍一刀会让「脚本已按模型要求筛过」的输出反而缺尾）。**普通工具不要实现** ——
// 模型路径的 20 KB 上限是刻意设计，放开会把上下文炸掉。
type SkipTruncateProvider interface {
	SkipTruncate() bool
}

// LoadoutProvider 可选接口：loadout 级钩子（工具集的「模型最终看到什么」由工具自己改写）。
//
// 用途（对齐 pi prepareLoadout）：mode:"on" 的定义就是「给**已声明**工具的描述追加
// codemode 用法片段」——否则模型不知道 read_file 也能从脚本里调，收益直接打折。
//
// 契约：
//   - 入参 = 引擎按 exposure 过滤后的声明集（顺序稳定：引擎已按名字排序）；
//   - 返回值 = 改写后的集合（可改 Description/Parameters，不改 Name 集合的语义由实现自负）；
//   - **必须字节稳定**：MCP 连/断、tool_search 加载、mode 切换都不得改变输出字节
//     （provider 侧缓存对 schema 抖动极敏感，见 engine.ToolParams 注释）。
type LoadoutProvider interface {
	PrepareLoadout(ctx context.Context, declared []core.ToolSchema) []core.ToolSchema
}
