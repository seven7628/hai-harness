package tools

import (
	"context"
	"github.com/seven7628/hai-harness/core"
)

// BaseTool 工具公共实现（元数据 + 默认拦截），内置工具嵌入复用：
//
//	type myTool struct {
//	    tools.BaseTool
//	}
//
// 构造时填 Name_/Description_/Params_/CanParallel_/ReadOnly_；需要自定义行为的
// 方法（CanParallel/BeforeCall/AfterCall）可覆盖。ReadOnly_ = true 时自动满足
// ReadOnlyTool 接口（plan 模式可见性判定，见 types.go）。
type BaseTool struct {
	Name_        string
	Description_ string
	Params_      any
	CanParallel_ bool
	ReadOnly_    bool
}

func (b *BaseTool) Name() string        { return b.Name_ }
func (b *BaseTool) Description() string { return b.Description_ }
func (b *BaseTool) Parameters() any     { return b.Params_ }
func (b *BaseTool) CanParallel() bool   { return b.CanParallel_ }
func (b *BaseTool) ReadOnly() bool      { return b.ReadOnly_ }
func (b *BaseTool) BeforeCall(context.Context, core.ToolCall) BeforeToolCallResponse {
	return BeforeToolCallResponse{}
}
func (b *BaseTool) AfterCall(context.Context, core.ToolCall) {}

// Map 工具参数属性的显式 schema 节点（map[string]any 的类型别名，二者完全互通）。
//
// 约定：凡带附加 JSON Schema 关键字（enum / default / minimum / maximum /
// maxLength / minItems / items …）的属性字段，一律用 Map 字面量显式书写，
// 且必须携带 "type"（字段类型）与 "description"（字段作用及取值说明）两个键；
// 纯「类型 + 描述」的简单字段继续用 Str/Bool/Int/Number/ArrayStr 等 helper，
// 二者产出等价。守护测试见 internal/integration 的 schema 全量巡检
// （每个属性节点必须有 type+description，数组必须有 items）。
type Map = map[string]any

// Str / Bool / Obj / ArrayStr / Int / Number 工具 schema 构造 helper（Parameters() 返回的厂商无关描述）。
func Str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func Bool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func Int(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func Number(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

// Obj 构造一个封闭的 JSON Schema object。
//
// 工具参数通常是一个小而固定的对象：把 additionalProperties 明确设为 false
// 可以阻止模型把自然语言中的字段名误当成参数，减少“看起来调用成功但参数被
// 静默忽略”的情况。required 只在确实存在必填字段时写入；没有 required 字段
// 在 JSON Schema 中等价于 required=[]，并不是 schema 丢失。
func Obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		// 复制切片，避免调用方之后修改 schema 的 required 内容。
		o["required"] = append([]string(nil), required...)
	}
	return o
}

func ArrayStr(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
