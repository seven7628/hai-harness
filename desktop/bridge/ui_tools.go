// ui_tools.go —— `ui_inspect` 工具（PR-12）：让模型读「运行中的桌面端现在长什么样」。
//
// 为什么需要它：改 UI 之前先查（哪个面板装着、当前标签是哪个、右栏开着没有），
// 比让模型猜或让用户复述可靠得多；装上插件之后也要能自证「真的挂上了」。
//
// 与 office 工具的关键差别：**无条件注册**。
//   - office 的 kit 不可用时不注册（注册了必然失败，只白耗轮次）；
//   - ui_inspect 恰恰要在「渲染层还没连上/刚启动」时可用 —— 那种情况的正确答案是
//     `{ok:false, reason:"no_snapshot"}`（可编程的状态），而不是「工具不存在」
//     （模型会以为这个产品没有这个能力，转而瞎猜）。
//
// CodeMode（00-conventions §2）：实现 OutputSchemaProvider 并把同一份 JSON 写进
// StructuredSink —— 脚本路径拿到的是**对象**（`s.data.tabs`），不是 20 KB 文本。
// 这条有真跑测试（ui_tools_test.go 的 TestUIInspectInRunCodeReturnsObject），
// 因为它是最容易退化成 string 的一条路径。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/tools"
)

// uiInspectFields `what` 的合法取值 —— 唯一清单（schema enum、校验、Call 的取值共用）。
//
// `all` 是缺省：模型最常见的问题是「现在是什么情况」，一次拿全会比来回问三次便宜。
// 其余取值与快照的顶层字段一一对应（渲染层 lib/uiSnapshot.ts 的 UISnapshot）。
var uiInspectFields = []string{"all", "layout", "surfaces", "tabs", "editors", "theme", "viewport"}

// uiInspectTool 只读查询渲染层快照。
//
// BaseTool 的 CanParallel_/ReadOnly_ 都是 true：读一份 mutex 保护的缓存，
// 既不与写类工具抢串行门（CodeMode 的 Promise.all 不会被它卡住），也不需要审批。
type uiInspectTool struct {
	tools.BaseTool
	state *uiState
}

func newUIInspectTool(state *uiState) *uiInspectTool {
	return &uiInspectTool{
		BaseTool: tools.BaseTool{
			Name_: "ui_inspect",
			// ≤120 字（常驻 schema 的成本约束，见 03-code-plan §5.1）。
			Description_: "只读：桌面端 UI 快照（布局/面板/标签/编辑器/主题/视口）。ageMs>5000 标 stale，应重试。",
			Params_: tools.Obj(map[string]any{
				"what": tools.Map{
					"type":        "string",
					"enum":        uiInspectFields,
					"default":     "all",
					"description": "要看的部分：all（缺省）/layout/surfaces/tabs/editors/theme/viewport",
				},
			}),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		state: state,
	}
}

func (t *uiInspectTool) ValidParams(_ context.Context, _, args string) error {
	var a struct {
		What string `json:"what"`
	}
	if s := strings.TrimSpace(args); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &a); err != nil {
			return fmt.Errorf("ui_inspect: 参数不是合法 JSON: %v", err)
		}
	}
	if a.What == "" {
		return nil // 缺省 all
	}
	for _, f := range uiInspectFields {
		if f == a.What {
			return nil
		}
	}
	return fmt.Errorf("ui_inspect: what 不合法：%q（可选 %s）", a.What, strings.Join(uiInspectFields, "/"))
}

// OutputSchema 声明结构化返回契约（引擎据此把 StructuredSink 的内容交给脚本）。
func (t *uiInspectTool) OutputSchema() any { return map[string]any{"type": "object"} }

func (t *uiInspectTool) Call(ctx context.Context, _ string, args string) (string, error) {
	var a struct {
		What string `json:"what"`
	}
	if s := strings.TrimSpace(args); s != "" && s != "null" {
		_ = json.Unmarshal([]byte(s), &a) // 合法性由 ValidParams 把关；这里只取值
	}
	what := a.What
	if what == "" {
		what = "all"
	}
	if !uiInspectFieldOK(what) {
		what = "all" // 直接调用（不经 ValidParams）时不制造第二种失败形态
	}

	raw, age, ok := t.state.Snapshot()
	if !ok {
		// 没有快照**不是错误**：渲染层可能刚启动/正在重连。用可编程状态表达，
		// 模型据此决定「稍后重试」还是「先做别的」，比一次工具失败轮次便宜。
		out, _ := json.Marshal(map[string]any{"ok": false, "reason": "no_snapshot"})
		writeUIInspectStructured(ctx, out)
		return string(out), nil
	}

	var data json.RawMessage = json.RawMessage("null")
	if what == "all" {
		data = json.RawMessage(raw)
	} else {
		var sections map[string]json.RawMessage
		if err := json.Unmarshal(raw, &sections); err == nil {
			if sec, has := sections[what]; has {
				data = sec
			}
		}
	}

	// data 是权威字段；value 是 03-code-plan §3.4 的旧名（脚本示例写的是 s.value.tabs），
	// 两条路径同一份内容 —— 让「按设计稿写的脚本」和「按本 PR 的 schema 写的脚本」都读得到，
	// 而不是让调用方去猜这一版用的是哪个名字。
	out, err := json.Marshal(map[string]any{
		"ok":    true,
		"ageMs": age.Milliseconds(),
		"stale": age > uiStaleAfter,
		"data":  data,
		"value": data,
	})
	if err != nil {
		return "", fmt.Errorf("ui_inspect: 组装结果失败: %w", err)
	}
	writeUIInspectStructured(ctx, out)
	return string(out), nil
}

// writeUIInspectStructured 把结构化结果交给本次调用的槽位（非脚本路径为 nil，忽略）。
func writeUIInspectStructured(ctx context.Context, raw []byte) {
	if sink := tools.StructuredSinkFrom(ctx); sink != nil {
		sink.Set(raw)
	}
}

func uiInspectFieldOK(what string) bool {
	for _, f := range uiInspectFields {
		if f == what {
			return true
		}
	}
	return false
}

// registerUITools 注册 ui_inspect（**无条件**，见文件头注释）。
//
// 注册点与 office 同一处（main.go 建 loop 时），但不做可用性判断：这个工具回答的是
// 「渲染层现在是什么状态」，工具本身不依赖任何宿主能力。
func (m *manager) registerUITools(eng *tools.Engine) {
	if m == nil || eng == nil {
		return
	}
	eng.RegisterTool(context.Background(), newUIInspectTool(&m.ui))
}
