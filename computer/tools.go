package computer

// tools.go：Computer Use 语义工具族（档 T/档 V 共用的 8 个工具）——
// 全部返回文本、模型只给文本目标（uid/应用/菜单），零坐标依赖。
// 实现是 Executor 决策方法的薄壳；描述自带用法契约（§8：snapshot 优先/
// uid 会失效/动作间隔 300ms），由 Computer Use 插件注册进引擎。
//
// 工具名（§4.1）：computer_snapshot / computer_find / computer_press /
//   computer_type / computer_key / computer_open_app / computer_activate /
//   computer_wait_verify
//
// 每个工具 CanParallel=false（桌面动作全局串行，§6.7）；读类动作（snapshot/
// find/wait_verify）标只读（plan 模式可见性）。

import (
	"context"
	"encoding/json"

	"github.com/seven7628/hai-harness/tools"
)

// SemTools 返回语义工具族（按固定顺序；插件 Enable 时注册）。
func SemTools(ex *Executor) []tools.Tool {
	return []tools.Tool{
		newSnapshotTool(ex),
		newFindTool(ex),
		newPressTool(ex),
		newTypeTool(ex),
		newKeyTool(ex),
		newOpenAppTool(ex),
		newActivateTool(ex),
		newWaitVerifyTool(ex),
	}
}

// ---- 参数解析辅助 ----

// toolBase 语义工具公共基座：ValidParams 宽松（参数错误在 Call 内转文本返回）。
// CanParallel=false 由 BaseTool 字段提供（构造时设 CanParallel_: false）。
type toolBase struct{}

func (toolBase) ValidParams(_ context.Context, _, _ string) error { return nil }

// requireArgs 解析参数并返回是否可继续（错误以文本返回，模型可见自愈）。
func requireArgs(arguments string, dst any) (string, bool) {
	if err := json.Unmarshal([]byte(arguments), dst); err != nil {
		return "invalid parameters: " + err.Error() + ". Check the parameter format.", false
	}
	return "", true
}

// ---- computer_snapshot ----

type snapshotTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newSnapshotTool(ex *Executor) *snapshotTool {
	return &snapshotTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_:        "computer_snapshot",
		Description_: `Get a semantic snapshot of the current screen: frontmost app, window list, accessibility tree (a11y) with uids. This is the starting point of every desktop task — snapshot first to get fresh uids, then act; uids go stale after any action, so snapshot again before the next one. Text models can read this directly; prefer it over screenshots.`,
		Params_:      tools.Obj(map[string]any{"app": tools.Str("Filter by app name (optional; default frontmost app)")}),
		CanParallel_: false,
		ReadOnly_:    true,
	}}
}

func (t *snapshotTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a struct {
		App string `json:"app"`
	}
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	return t.Executor.Snapshot(ctx, a.App, 6000)
}

// ---- computer_find ----

type findTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newFindTool(ex *Executor) *findTool {
	return &findTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_:        "computer_find",
		Description_: `Incrementally search elements in the latest snapshot (fallback when the tree was truncated). Give text/role, get candidate uid list. Then act with computer_press {uid}.`,
		Params_: tools.Obj(map[string]any{
			"text": tools.Str("Text to find (matches button/input title or value; required)"),
			"role": tools.Str("Restrict role: button/menuItem/textField/checkbox etc. (optional)"),
		}),
		CanParallel_: false,
		ReadOnly_:    true,
	}}
}

func (t *findTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a struct {
		Text string `json:"text"`
		Role string `json:"role"`
	}
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	if a.Text == "" {
		return "computer_find needs a text parameter (text to find).", nil
	}
	return t.Executor.Find(ctx, a.Text, a.Role, "")
}

// ---- computer_press ----

type pressTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newPressTool(ex *Executor) *pressTool {
	return &pressTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_: "computer_press",
		Description_: `Semantically press a target control (button/menu item/checkbox/switch/focus input). Give the target one of three ways:
1) uid: stable handle from the latest snapshot (recommended, most precise); 2) app + menuPath: menu path ["File","Save"];
3) title: match by on-screen text. The system performs the press — no coordinates. Auto-verifies after the action.`,
		Params_: tools.Obj(map[string]any{
			"uid":      tools.Str("Element uid from the snapshot tree (recommended; e.g. a:1:0:5)"),
			"app":      tools.Str("App name (used with menuPath)"),
			"menuPath": tools.ArrayStr("Menu path, e.g. [\"File\",\"Save\"]"),
			"title":    tools.Str("On-screen text (press the element whose title contains this text)"),
		}),
		CanParallel_: false,
	}}
}

type pressArgs struct {
	UID      string   `json:"uid"`
	App      string   `json:"app"`
	MenuPath []string `json:"menuPath"`
	Title    string   `json:"title"`
}

func (t *pressTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a pressArgs
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	if a.UID == "" && a.Title == "" && len(a.MenuPath) == 0 {
		return "computer_press needs one of uid / title / app+menuPath as the target.", nil
	}
	if len(a.MenuPath) > 0 {
		if a.App == "" {
			return "computer_press with menuPath also requires the app parameter.", nil
		}
		return t.Executor.PressMenu(ctx, a.App, a.MenuPath)
	}
	if a.UID != "" {
		return t.Executor.Press(ctx, a.UID)
	}
	return t.Executor.PressTitle(ctx, a.Title)
}

// ---- computer_type ----

type typeTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newTypeTool(ex *Executor) *typeTool {
	return &typeTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_:        "computer_type",
		Description_: `Type text into the current focus (e.g. an already-focused text field); Chinese/Unicode fine. Use target_uid to focus the target input first. Before typing, use computer_press / computer_snapshot to confirm focus.`,
		Params_: tools.Obj(map[string]any{
			"text":       tools.Str("Text to type (required)"),
			"target_uid": tools.Str("Target input uid (optional; focuses it before typing)"),
		}),
		CanParallel_: false,
	}}
}

type typeArgs struct {
	Text      string `json:"text"`
	TargetUID string `json:"target_uid"`
}

func (t *typeTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a typeArgs
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	if a.Text == "" {
		return "computer_type needs a text parameter.", nil
	}
	return t.Executor.Type(ctx, a.Text, a.TargetUID)
}

// ---- computer_key ----

type keyTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newKeyTool(ex *Executor) *keyTool {
	return &keyTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_:        "computer_key",
		Description_: `Send a keyboard shortcut or single key (e.g. cmd+shift+p, Return, Tab, Escape, ArrowDown). Keys go to the currently focused app. Modifiers: cmd/ctrl/alt/shift, combined with +.`,
		Params_: tools.Obj(map[string]any{
			"keys": tools.Str("Key combination (cmd+shift+p / Return / Tab…; required)"),
		}),
		CanParallel_: false,
	}}
}

func (t *keyTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a struct {
		Keys string `json:"keys"`
	}
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	if a.Keys == "" {
		return "computer_key needs a keys parameter.", nil
	}
	return t.Executor.Key(ctx, a.Keys)
}

// ---- computer_open_app ----

type openAppTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newOpenAppTool(ex *Executor) *openAppTool {
	return &openAppTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_:        "computer_open_app",
		Description_: `Open/activate an app (by name or path). E.g. open Calendar: {"name":"Calendar"}; Chinese-named apps use their display name, e.g. {"name":"提醒事项"}. If already running it is brought to the front.`,
		Params_: tools.Obj(map[string]any{
			"name": tools.Str("App name or .app path (required)"),
		}),
		CanParallel_: false,
	}}
}

func (t *openAppTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	if a.Name == "" {
		return "computer_open_app needs a name parameter.", nil
	}
	return t.Executor.OpenApp(ctx, a.Name)
}

// ---- computer_activate ----

type activateTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newActivateTool(ex *Executor) *activateTool {
	return &activateTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_:        "computer_activate",
		Description_: `Activate an app/window to the foreground (first step after switching apps). window_title optionally targets a specific window.`,
		Params_: tools.Obj(map[string]any{
			"app":          tools.Str("App name (required)"),
			"window_title": tools.Str("Window title (optional, activate by match)"),
		}),
		CanParallel_: false,
	}}
}

type activateArgs struct {
	App         string `json:"app"`
	WindowTitle string `json:"window_title"`
}

func (t *activateTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a activateArgs
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	if a.App == "" {
		return "computer_activate needs an app parameter.", nil
	}
	return t.Executor.Activate(ctx, a.App, a.WindowTitle)
}

// ---- computer_wait_verify ----

type waitVerifyTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newWaitVerifyTool(ex *Executor) *waitVerifyTool {
	return &waitVerifyTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_:        "computer_wait_verify",
		Description_: `Wait and verify UI state (confirm success/failure after an action). Polls until expected text appears/disappears or the target uid changes. Targets are user-visible texts; verification is the second line of the action loop.`,
		Params_: tools.Obj(map[string]any{
			"expect_text": tools.Str("Expected on-screen text (appears; or disappears with expect_gone)"),
			"expect_gone": tools.Bool("true = wait for the text to disappear"),
			"timeout_ms":  tools.Int("Max wait in ms (default 5000)"),
		}),
		CanParallel_: false,
	}}
}

type verifyArgs struct {
	ExpectText string `json:"expect_text"`
	ExpectGone bool   `json:"expect_gone"`
	TimeoutMS  int    `json:"timeout_ms"`
}

func (t *waitVerifyTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a verifyArgs
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	if a.TimeoutMS <= 0 {
		a.TimeoutMS = 5000
	}
	return t.Executor.WaitVerify(ctx, a.ExpectText, a.ExpectGone, a.TimeoutMS)
}
