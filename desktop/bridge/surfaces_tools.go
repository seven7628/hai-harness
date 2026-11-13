// surfaces_tools.go —— `surface_install` / `surface_remove` / `surface_list`（PR-13 B）。
//
// 形状照 ui_tools.go：BaseTool + OutputSchema（结构化槽，脚本路径拿对象）+ 注册函数。
// 三条纪律：
//
//  1. **受开关控制**（`settings.features.surfaceTools`，缺省 false；PR-14）：工具「在不在」
//     与「能不能装」是两件事 —— 后者由审批与校验回答。开关的读法与默认值见
//     surface_settings.go。
//  2. **失败不返回 Go error**（`status:"rejected"` 是正常回执）：模型据此改参数比吃掉
//     一轮重试便宜；只有参数本身不是合法 JSON 才走 Go error（那是签名错误，重试无意义）。
//     审批拒绝仍然由引擎的 Block 路径表达（含 `rejected` 子串），措辞不变。
//  3. **安装/卸载成功才推 `surfaces_changed`**：PR-12 建好的通道，形状一个字不改；
//     PR-14 起渲染层收到它就跑一次 loader.syncPlugins（见 src/surfaces/plugin/loader.ts）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// ---- surface_install ----

type surfaceInstallTool struct {
	tools.BaseTool
	store *surfaceStore
	push  func() // 安装成功后的通知（生产：manager.PushSurfacesChanged）
}

func newSurfaceInstallTool(store *surfaceStore, push func()) *surfaceInstallTool {
	return &surfaceInstallTool{
		BaseTool: tools.BaseTool{
			Name_: "surface_install",
			// ≤120 字（常驻 schema 的成本约束，见 03-code-plan §5.1）。
			Description_: "安装本地目录里的 Surface 面板插件：校验 manifest → 算 sha256 → 原子写 index → 通知界面。" +
				"status=applied 才算生效；校验失败给 rejected+reason。需审批。",
			Params_: tools.Obj(map[string]any{
				"target": tools.Str("插件目录的绝对路径（必须含 surface.json）"),
				"enable": tools.Map{
					"type":        "boolean",
					"default":     true,
					"description": "装完是否启用（默认 true）",
				},
			}, "target"),
			CanParallel_: false, // 写类：与其它写工具串行，index 的顺序才可解释
			ReadOnly_:    false,
		},
		store: store,
		push:  push,
	}
}

// RequiresApproval 逐次要批准（01-design §5.5：与 DSH 的 plugin_manager 同一条纪律）。
func (t *surfaceInstallTool) RequiresApproval(context.Context, core.ToolCall) bool { return true }

// OutputSchema 声明结构化返回契约（见 tools/exposure.go 的 OutputSchemaProvider）。
func (t *surfaceInstallTool) OutputSchema() any { return surfaceReceiptSchema() }

func (t *surfaceInstallTool) ValidParams(_ context.Context, _, args string) error {
	var a struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(args)), &a); err != nil {
		return fmt.Errorf("surface_install: 参数不是合法 JSON: %v", err)
	}
	if strings.TrimSpace(a.Target) == "" {
		return fmt.Errorf("surface_install: target 不能为空（插件目录的绝对路径）")
	}
	return nil
}

func (t *surfaceInstallTool) Call(ctx context.Context, _ string, args string) (string, error) {
	var a struct {
		Target string `json:"target"`
		Enable *bool  `json:"enable"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("surface_install: 参数不是合法 JSON: %v", err)
	}
	enable := true
	if a.Enable != nil {
		enable = *a.Enable
	}
	rec := t.store.Install(a.Target, enable)
	if rec.Status == surfaceApplied && t.push != nil {
		t.push() // 只有真的落盘了才通知（rejected/error 改了什么的说法都不成立）
	}
	return surfaceJSON(ctx, rec)
}

// ---- surface_remove ----

type surfaceRemoveTool struct {
	tools.BaseTool
	store *surfaceStore
	push  func()
}

func newSurfaceRemoveTool(store *surfaceStore, push func()) *surfaceRemoveTool {
	return &surfaceRemoveTool{
		BaseTool: tools.BaseTool{
			Name_: "surface_remove",
			Description_: "卸载 Surface 面板插件：index 去条目、目录移进 .trash（可恢复，不直接删）。" +
				"幂等，不存在返回 cleaned。需审批。",
			Params_: tools.Obj(map[string]any{
				"id": tools.Str("要卸载的插件 id（见 surface_list）"),
			}, "id"),
			CanParallel_: false,
			ReadOnly_:    false,
		},
		store: store,
		push:  push,
	}
}

// RequiresApproval 与安装同级：卸载同样会改变用户的插件清单。
func (t *surfaceRemoveTool) RequiresApproval(context.Context, core.ToolCall) bool { return true }

func (t *surfaceRemoveTool) OutputSchema() any { return surfaceReceiptSchema() }

func (t *surfaceRemoveTool) ValidParams(_ context.Context, _, args string) error {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(args)), &a); err != nil {
		return fmt.Errorf("surface_remove: 参数不是合法 JSON: %v", err)
	}
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("surface_remove: id 不能为空")
	}
	return nil
}

func (t *surfaceRemoveTool) Call(ctx context.Context, _ string, args string) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("surface_remove: 参数不是合法 JSON: %v", err)
	}
	rec := t.store.Remove(a.ID)
	// cleaned 也可能改了 index（清掉幽灵记录），所以两者都通知。
	if (rec.Status == surfaceApplied || rec.Status == surfaceCleaned) && t.push != nil {
		t.push()
	}
	return surfaceJSON(ctx, rec)
}

// ---- surface_list ----

type surfaceListTool struct {
	tools.BaseTool
	store *surfaceStore
}

func newSurfaceListTool(store *surfaceStore) *surfaceListTool {
	return &surfaceListTool{
		BaseTool: tools.BaseTool{
			Name_: "surface_list",
			Description_: "只读：列出已安装的 Surface 插件（id/版本/sha256/启用状态）。" +
				"这是 index 里的记录，不代表面板此刻已渲染。",
			Params_: tools.Obj(map[string]any{
				"enabled_only": tools.Bool("只看启用的（默认 false = 全部）"),
			}),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		store: store,
	}
}

func (t *surfaceListTool) ValidParams(_ context.Context, _, args string) error {
	var a struct {
		EnabledOnly *bool `json:"enabled_only"`
	}
	if s := strings.TrimSpace(args); s == "" || s == "null" {
		return nil
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(args)), &a); err != nil {
		return fmt.Errorf("surface_list: 参数不是合法 JSON: %v", err)
	}
	return nil
}

// RequiresApproval 显式声明「只读、不审批」。
//
// 不实现接口 = 引擎按「未声明」处理（现状路径），结果一样；显式写 false 是为了让
// 这三个工具的审批面在同一个文件里一眼能读完（install/remove true、list false），
// 而不是靠「这个类型恰好没实现某个接口」来推断。
func (t *surfaceListTool) RequiresApproval(context.Context, core.ToolCall) bool { return false }

func (t *surfaceListTool) OutputSchema() any {
	return tools.Obj(map[string]any{
		"status": tools.Str("applied | error"),
		"reason": tools.Str("失败时的可读原因"),
		"items": tools.Map{
			"type":        "array",
			"description": "已安装的插件（顺序即 index 顺序；空态是 []，不是 null）",
			"items":       surfaceEntrySchema(),
		},
	}, "status", "items")
}

func (t *surfaceListTool) Call(ctx context.Context, _ string, args string) (string, error) {
	var a struct {
		EnabledOnly bool `json:"enabled_only"`
	}
	if s := strings.TrimSpace(args); s != "" && s != "null" {
		_ = json.Unmarshal([]byte(s), &a) // 合法性由 ValidParams 把关
	}
	return surfaceJSON(ctx, t.store.List(a.EnabledOnly))
}

// ---- 共用 ----

// surfaceJSON 序列化回执：文本给模型，结构化槽给脚本（History：脚本拿到的是对象）。
//
// 只在脚本调用时写槽（00-conventions §2.1 C3）：模型路径照常拿到同一段文本，
// 多写一份结构化结果只是白占一次内存。
func surfaceJSON(ctx context.Context, v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("面板回执序列化失败: %w", err)
	}
	if tools.IsScriptCall(ctx) {
		if sink := tools.StructuredSinkFrom(ctx); sink != nil {
			sink.Set(raw)
		}
	}
	return string(raw), nil
}

func surfaceReceiptSchema() any {
	return tools.Obj(map[string]any{
		"status":  tools.Str("applied | already_installed | cleaned | rejected | error"),
		"reason":  tools.Str("rejected/error 时的可读原因（指向具体问题）"),
		"id":      tools.Str("插件 id"),
		"version": tools.Str("manifest 里的版本"),
		"hash":    tools.Str("sha256(入口文件) 的十六进制"),
	}, "status")
}

func surfaceEntrySchema() any {
	return tools.Obj(map[string]any{
		"id":             tools.Str("插件 id"),
		"version":        tools.Str("manifest 版本"),
		"hash":           tools.Str("sha256(入口文件)"),
		"source":         tools.Str("安装来源目录的绝对路径"),
		"enabled":        tools.Bool("是否启用"),
		"installedAt":    tools.Str("这一版落盘的时刻（RFC3339）"),
		"disabledReason": tools.Str("被禁用的原因（可选）"),
	}, "id", "version", "hash")
}

// registerSurfaceTools 注册三个内置工具，受 `settings.features.surfaceTools` 控制（PR-14 E）。
//
// 关（缺省）→ **一个都不注册**：工具面里没有 surface_install/remove/list，模型也就
// 不会试图装插件（它连这个能力都不存在）。开 → 三个都在，行为与本轮之前逐字相同。
//
// 为什么门开在注册函数里而不是调用点（main.go）：调用点有一处（newSessionEngine 之后），
// 而「读设置 + 决定注册什么」是一个整体；摆在外面就成了两处各自解释同一个开关。
// 注册点与 registerUITools 同一处，不改签名 —— 工具需要的是宿主能力（store 与 UI 通道），
// 不是引擎装配的一部分。
//
// **ui_inspect 不在这里**：它无条件注册（见 registerUITools），因为它是只读观测面 ——
// 「关掉插件能力」不该顺带让人看不见现在的界面。
func (m *manager) registerSurfaceTools(eng *tools.Engine) {
	if m == nil || eng == nil {
		return
	}
	if !m.surfaceToolsEnabled() {
		return
	}
	store := &m.surfaces
	eng.RegisterTool(context.Background(), newSurfaceInstallTool(store, m.PushSurfacesChanged))
	eng.RegisterTool(context.Background(), newSurfaceRemoveTool(store, m.PushSurfacesChanged))
	eng.RegisterTool(context.Background(), newSurfaceListTool(store))
}
