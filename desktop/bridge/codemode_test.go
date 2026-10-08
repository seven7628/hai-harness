package main

// codemode_test.go：Wave 3 **装配**的钉子（配置读取 → 工具注册 → MCP 默认档翻转）。
//
// 这一层测的是「接进产品」这一步，不是工具本体（后者在 tools/builtin/codemode 里）。
// 每条断言都对着任务书里的一条可观测要求：
//
//   - 默认关闭：`codemode.enabled` 缺失/为假 → 工具表里**没有** codemode；
//   - 开启后：codemode 进 ToolParams（模型看得见）、描述里列出 direct 档工具、
//     含 searchTools 引导、mode:"on" 的 loadout 片段在**真装配**里生效；
//   - `mode:"only"` 被拒绝（半成品，装配不得使用）→ 按关闭处理 + 可读警告；
//   - 非法字段逐个退化到默认（不连累同段其他字段，也不让桥起不来）；
//   - store 落在**会话级**目录且 0600（装配方负责建目录 —— OpenStore 不建）；
//   - MCP「未声明档位」的默认落点随 codemode 开关翻转（§7.11：通道不在时落 deferred
//     等于把 MCP 对模型整体关闭）。
//
// 设置来自 settings.json（HOME 隔离到 t.TempDir()），与 browser_engine/cron/mesh 同一
// 条读取链（homeCfgPath）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/mcp"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin/codemode"
)

// ---- 装配/配置的测试脚手架 ----

// isolateHome 把 HOME 指到临时目录（settings.json 的唯一来源），先建好 ~/.go-code。
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeSettingsJSON 把 body 原样写进 settings.json（0600，同 Electron 侧）。
func writeSettingsJSON(t *testing.T, cfgDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newTestSessionEngine 建一个**真**会话引擎（工具注册的真实入口）。
// profile 用 work（不带 bash/agent 家族）：注册路径更短，但 codemode 的装配链完全相同。
func newTestSessionEngine(t *testing.T, ws, sid string, profile personaProfile, mcpm *mcp.Manager) *tools.Engine {
	t.Helper()
	sk, err := skills.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newSessionEngine(ws, sid, nil, loadProviderConfig(), "deepseek",
		loadProviderConfig().defaultModel(), string(provider.ReasoningEffortLevelLow),
		func() context.Context { return context.Background() }, subagent.NewRegistry(),
		events.NewQuestionWaiter(0), sk, buildSubagentRegistry(t.TempDir()),
		mcpm, nil, profile, nil, nil, nil, nil, nil)
	return eng
}

// schemaNamed 取 ToolParams 里的某条（找不到返回 nil）。
func schemaNamed(t *testing.T, eng *tools.Engine, name string) *toolSchemaView {
	t.Helper()
	schemas, err := eng.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schemas {
		if s.Name == name {
			return &toolSchemaView{Name: s.Name, Description: s.Description}
		}
	}
	return nil
}

// toolSchemaView 只留断言用得上的两个字段（避免把 core.ToolSchema 传得到处都是）。
type toolSchemaView struct {
	Name        string
	Description string
}

// ---- A1 settings.codemode 读取：默认 / 合法值 / 非法值逐个退化 ----

func TestLoadCodemodeSettingsDefaults(t *testing.T) {
	dir := isolateHome(t)
	cases := []struct {
		name string
		body string // "" = 不写 settings.json
	}{
		{"没有 settings.json", ""},
		{"整段缺失（老配置）", `{"theme":"dark"}`},
		{"空段", `{"codemode":{}}`},
		{"段为 null", `{"codemode":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Remove(filepath.Join(dir, "settings.json"))
			if c.body != "" {
				writeSettingsJSON(t, dir, c.body)
			}
			cs, warns := loadCodemodeSettings()
			if cs.Enabled {
				t.Fatal("默认必须**关闭**（对齐 pi defaultActive:false）")
			}
			if cs.Mode != codemode.ModeOn {
				t.Fatalf("默认 mode = %q, want %q", cs.Mode, codemode.ModeOn)
			}
			if cs.InlineBudget != 0 {
				t.Fatalf("默认 inline_budget = %d, want 0（= 用 DefaultInlineBudget）", cs.InlineBudget)
			}
			if want := codemode.DefaultScriptTimeoutMs * time.Millisecond; cs.ScriptTimeout != want {
				t.Fatalf("默认 budget = %v, want %v", cs.ScriptTimeout, want)
			}
			if len(warns) != 0 {
				t.Fatalf("缺省不是错误，不该有警告: %v", warns)
			}
		})
	}
}

func TestLoadCodemodeSettingsValidSection(t *testing.T) {
	dir := isolateHome(t)
	writeSettingsJSON(t, dir, `{"codemode":{"enabled":true,"mode":"on","inline_budget":-1,"budget_seconds":2.5}}`)
	cs, warns := loadCodemodeSettings()
	if len(warns) != 0 {
		t.Fatalf("合法配置不该有警告: %v", warns)
	}
	if !cs.Enabled || cs.Mode != codemode.ModeOn {
		t.Fatalf("enabled/mode = %v/%q, want true/%q", cs.Enabled, cs.Mode, codemode.ModeOn)
	}
	if cs.InlineBudget != -1 {
		t.Fatalf("inline_budget = %d, want -1（<0 = 不限，透传给 Options.InlineBudget）", cs.InlineBudget)
	}
	if cs.ScriptTimeout != 2500*time.Millisecond {
		t.Fatalf("budget_seconds 2.5 → %v, want 2.5s", cs.ScriptTimeout)
	}
}

// TestLoadCodemodeSettingsRejectsOnlyMode `mode:"only"` 是半成品（§7.16 边界 2：只兑现了
// 「描述改列全部」，没兑现「对模型隐藏」）——必须**拒绝**并按关闭处理，绝不能静默降级成 on
// （那会让用户以为工具已隐藏，实际全都在模型工具表里）。
func TestLoadCodemodeSettingsRejectsOnlyMode(t *testing.T) {
	dir := isolateHome(t)
	for _, mode := range []string{"only", "Only", " ONLY "} {
		t.Run(mode, func(t *testing.T) {
			writeSettingsJSON(t, dir, `{"codemode":{"enabled":true,"mode":`+jsonQuote(mode)+`}}`)
			cs, warns := loadCodemodeSettings()
			if cs.Enabled {
				t.Fatalf("mode=%q 必须按关闭处理（半成品不得激活）", mode)
			}
			if cs.Mode == codemode.ModeOnly {
				t.Fatal("Mode 不得落到 only（装配方不得使用它）")
			}
			if len(warns) == 0 || !strings.Contains(strings.Join(warns, "\n"), "only") {
				t.Fatalf("必须留下可读警告（用户要能看出为什么没生效）: %v", warns)
			}
			if !strings.Contains(strings.Join(warns, "\n"), "关闭") {
				t.Fatalf("警告要说明按关闭处理: %v", warns)
			}
		})
	}
}

// TestLoadCodemodeSettingsDegradesPerField 非法值逐字段退化：类型不对 / 越界 / 拼错
// 都只影响自己那一项（同段其他正确字段照常生效），且都留可读警告。
func TestLoadCodemodeSettingsDegradesPerField(t *testing.T) {
	dir := isolateHome(t)
	cases := []struct {
		name     string
		section  string
		enabled  bool
		budget   int
		timeout  time.Duration
		warnWord string // 期望警告里出现的关键词（"" = 不该有警告）
	}{
		{"enabled 类型不对 → 关闭", `{"enabled":"yes"}`, false, 0, 30 * time.Second, "enabled"},
		{"mode 类型不对 → 默认 on", `{"enabled":true,"mode":5}`, true, 0, 30 * time.Second, "mode"},
		{"mode 拼错 → 默认 on", `{"enabled":true,"mode":"onn"}`, true, 0, 30 * time.Second, "无法识别"},
		{"mode off → 关闭（合法值，无警告）", `{"enabled":true,"mode":"off"}`, false, 0, 30 * time.Second, ""},
		{"inline_budget 类型不对 → 默认", `{"enabled":true,"inline_budget":"many"}`, true, 0, 30 * time.Second, "inline_budget"},
		{"inline_budget 越界 → 默认", `{"enabled":true,"inline_budget":100001}`, true, 0, 30 * time.Second, "越界"},
		{"budget_seconds 类型不对 → 默认 30s", `{"enabled":true,"budget_seconds":"1m"}`, true, 0, 30 * time.Second, "budget_seconds"},
		{"budget_seconds 0 → 默认 30s（不生效为「立即超时」）", `{"enabled":true,"budget_seconds":0}`, true, 0, 30 * time.Second, "越界"},
		{"budget_seconds 超上限 → 默认 30s", `{"enabled":true,"budget_seconds":7200}`, true, 0, 30 * time.Second, "越界"},
		{"一个字段坏不连累另一个", `{"enabled":true,"inline_budget":500,"budget_seconds":"x"}`, true, 500, 30 * time.Second, "budget_seconds"},
		{"全合法 → 无警告", `{"enabled":true,"inline_budget":800,"budget_seconds":45}`, true, 800, 45 * time.Second, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeSettingsJSON(t, dir, `{"codemode":`+c.section+`}`)
			cs, warns := loadCodemodeSettings()
			if cs.Enabled != c.enabled {
				t.Fatalf("enabled = %v, want %v（警告: %v）", cs.Enabled, c.enabled, warns)
			}
			if cs.InlineBudget != c.budget {
				t.Fatalf("inline_budget = %d, want %d", cs.InlineBudget, c.budget)
			}
			if cs.ScriptTimeout != c.timeout {
				t.Fatalf("budget = %v, want %v", cs.ScriptTimeout, c.timeout)
			}
			joined := strings.Join(warns, "\n")
			if c.warnWord == "" {
				if len(warns) != 0 {
					t.Fatalf("不该有警告: %v", warns)
				}
				return
			}
			if !strings.Contains(joined, c.warnWord) {
				t.Fatalf("警告应提到 %q，实际: %v", c.warnWord, warns)
			}
		})
	}
}

// TestLoadCodemodeSettingsBadJSON 坏 JSON 只影响 codemode（按默认关闭）+ 明确警告，
// 绝不让桥启动失败。
func TestLoadCodemodeSettingsBadJSON(t *testing.T) {
	dir := isolateHome(t)
	writeSettingsJSON(t, dir, `{"codemode": {`)
	cs, warns := loadCodemodeSettings()
	if cs.Enabled {
		t.Fatal("坏 JSON 时按默认关闭（安全侧）")
	}
	if len(warns) == 0 {
		t.Fatal("坏 JSON 必须留可读警告")
	}
}

// jsonQuote 测试内的字符串字面量（避免手写转义）。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// codemodeTierStub 「可编排但不声明给模型」档位的替身：用来证明装配现场拿到的目录是
// **活的注册表**（Options.Tools 闭包），以及分层的两侧（描述里有、ToolParams 里没有）。
type codemodeTierStub struct {
	tools.BaseTool
}

func (s *codemodeTierStub) Call(context.Context, string, string) (string, error) { return "ok", nil }
func (s *codemodeTierStub) ValidParams(context.Context, string, string) error    { return nil }
func (s *codemodeTierStub) Exposure() tools.ToolExposure                         { return tools.ExposureCodemode }

// TestInlineBudgetZeroMeansDefaultNotZeroBudget 「没配 inline_budget」必须翻成 nil
// （codemode 的 DefaultInlineBudget），而**不是**「返回 0 的函数」—— 后者在 codemode 的
// 契约里是「一条都不列」的极端档，会把目录装配成永远空的（模型只能靠 searchTools 瞎搜）。
func TestInlineBudgetZeroMeansDefaultNotZeroBudget(t *testing.T) {
	if got := inlineBudgetFunc(0); got != nil {
		t.Fatalf("inline_budget=0 应翻成 nil（= 用默认预算），实际是函数（返回 %d）", got())
	}
	if fn := inlineBudgetFunc(-1); fn == nil || fn() != -1 {
		t.Fatalf("inline_budget=-1 应原样透传（不限），实际 fn=%v", fn != nil)
	}
	if fn := inlineBudgetFunc(800); fn == nil || fn() != 800 {
		t.Fatalf("inline_budget=800 应原样透传，实际 fn=%v", fn != nil)
	}
}

// ---- A2 settings 往返安全（多写者：Electron 与 bridge 各写自己那一段）----

// TestPatchJSONFilePreservesCodemodeSection settings.json 是**多写者**的：Electron 写
// provider/settings 段，bridge 写 mcpServers/hooks 段（config_patch.patchJSONFile）。
// 用户在设置面板里打开 codemode（或手写这一节）之后，一次 MCP 编辑绝不能把它抹掉 ——
// 那会表现成「开了 codemode，改个 MCP 服务器就又关了」，且毫无提示。
//
// 覆盖面（Electron 侧 readSettings 对未知键是 `{...默认, ...raw}` 的展开式合并，其
// 「不裁剪未知键」由 main.ts:372 的 spread 保证，无法在没有 Electron 运行时的情况下单测；
// 这里钉住的是**同一份文件的另一个写者**，也是实际最容易丢段的那一侧）。
func TestPatchJSONFilePreservesCodemodeSection(t *testing.T) {
	dir := isolateHome(t)
	path := filepath.Join(dir, "settings.json")
	writeSettingsJSON(t, dir, `{"codemode":{"enabled":true,"budget_seconds":45},"mcpServers":{"old":{"type":"stdio","command":"x"}}}`)

	// bridge 的 MCP 写入路径（全量替换 mcpServers 段）。
	if err := patchJSONFile(path, map[string]any{
		"mcpServers": map[string]any{"new": map[string]any{"type": "stdio", "command": "y"}},
	}); err != nil {
		t.Fatalf("patchJSONFile: %v", err)
	}

	// 读回：codemode 段还在，且**仍然能被设置读取链读到**（端到端，不只是字节还在）。
	cs, warns := loadCodemodeSettings()
	if !cs.Enabled || cs.ScriptTimeout != 45*time.Second {
		t.Fatalf("一次 MCP 写入后 codemode 设置丢了: %+v（警告: %v）", cs, warns)
	}
	// mcpServers 该被替换（不是"都保留"）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(obj["mcpServers"]), `"new"`) || strings.Contains(string(obj["mcpServers"]), `"old"`) {
		t.Fatalf("mcpServers 未被替换: %s", obj["mcpServers"])
	}
}

// ---- C1 MCP「未声明档位」的默认落点随 codemode 翻转（§7.11）----

func TestMCPDefaultExposureFollowsCodemode(t *testing.T) {
	if got := mcpDefaultExposure(true); got != tools.ExposureDeferred {
		t.Fatalf("codemode 开 → %q, want deferred（searchTools 是 deferred 的激活通道）", got)
	}
	if got := mcpDefaultExposure(false); got != tools.ExposureDirect {
		t.Fatalf("codemode 关 → %q, want direct（通道不在时 deferred = MCP 对模型整体不可达）", got)
	}
}

// TestApplyMCPDefaultExposureFollowsSettings 装配点真读设置：默认（无配置）必须是 direct
// （保持既有产品口径），打开 codemode 才翻 deferred。
func TestApplyMCPDefaultExposureFollowsSettings(t *testing.T) {
	dir := isolateHome(t)
	mgr := mcp.NewManagerWithSource(nil, nil)

	// ① 老配置（无 codemode 段）→ direct：不改变既有行为。
	writeSettingsJSON(t, dir, `{"theme":"dark"}`)
	applyMCPDefaultExposure(mgr)
	if mgr.DefaultExposure != tools.ExposureDirect {
		t.Fatalf("未开 codemode 时默认档 = %q, want direct", mgr.DefaultExposure)
	}
	// ② 打开 codemode → deferred。
	writeSettingsJSON(t, dir, `{"codemode":{"enabled":true}}`)
	applyMCPDefaultExposure(mgr)
	if mgr.DefaultExposure != tools.ExposureDeferred {
		t.Fatalf("开了 codemode 时默认档 = %q, want deferred", mgr.DefaultExposure)
	}
	// ③ 又关掉 → 必须翻回 direct（否则 deferred 失去激活通道 = MCP 对模型整体关闭）。
	writeSettingsJSON(t, dir, `{"codemode":{"enabled":false}}`)
	applyMCPDefaultExposure(mgr)
	if mgr.DefaultExposure != tools.ExposureDirect {
		t.Fatalf("关掉 codemode 后必须翻回 direct，实际 %q", mgr.DefaultExposure)
	}
}

// TestApplyMCPDefaultExposureAllWorkspaces reload_settings 路径：设置是全局的，运行态按
// 工作区一份 —— 必须每个工作区的 Manager 都被翻到同一个档位。
func TestApplyMCPDefaultExposureAllWorkspaces(t *testing.T) {
	m, _ := testManager(t)
	cfgDir := filepath.Join(os.Getenv("HOME"), ".go-code")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSettingsJSON(t, cfgDir, `{"codemode":{"enabled":true}}`)

	a, b := mcp.NewManagerWithSource(nil, nil), mcp.NewManagerWithSource(nil, nil)
	m.workspaces["/ws-a"] = &workspaceRuntime{path: "/ws-a", mcp: a}
	m.workspaces["/ws-b"] = &workspaceRuntime{path: "/ws-b", mcp: b}
	m.applyMCPDefaultExposureAllWorkspaces()
	if a.DefaultExposure != tools.ExposureDeferred || b.DefaultExposure != tools.ExposureDeferred {
		t.Fatalf("两个工作区都该翻成 deferred，实际 %q / %q", a.DefaultExposure, b.DefaultExposure)
	}

	writeSettingsJSON(t, cfgDir, `{"codemode":{"enabled":false}}`)
	m.applyMCPDefaultExposureAllWorkspaces()
	if a.DefaultExposure != tools.ExposureDirect || b.DefaultExposure != tools.ExposureDirect {
		t.Fatalf("关掉后两个工作区都该回 direct，实际 %q / %q", a.DefaultExposure, b.DefaultExposure)
	}
}

// ---- B 装配：注册与不注册 ----

// TestSessionEngineCodemodeRegisteredWhenEnabled 开启后的全链路可观测断言：
// 工具表里有 codemode、描述里列出 direct 档工具 + searchTools 引导、mode:"on" 的
// loadout 片段在真装配里追加到了已声明工具上、且 codemode 自己仍是 model-only（禁自嵌套）。
func TestSessionEngineCodemodeRegisteredWhenEnabled(t *testing.T) {
	dir := isolateHome(t)
	writeSettingsJSON(t, dir, `{"codemode":{"enabled":true}}`)
	ws := t.TempDir()
	eng := newTestSessionEngine(t, ws, "sid-codemode", profiles["work"], nil)

	tool, err := eng.GetTool(context.Background(), "codemode")
	if err != nil {
		t.Fatalf("codemode 未注册: %v", err)
	}
	if got := tools.ToolExposureOf(tool); got != tools.ExposureModelOnly {
		t.Fatalf("Exposure = %q, want model-only（脚本不得再起脚本）", got)
	}
	if tools.CallableFromScript(tool) {
		t.Fatal("codemode 不得可被脚本调用（自嵌套）")
	}

	// 模型工具表里有它，且描述是「教学正文 + 目录」而不是兜底文本。
	view := schemaNamed(t, eng, "codemode")
	if view == nil {
		t.Fatal("ToolParams 缺 codemode（模型看不到写脚本的入口）")
	}
	for _, want := range []string{"searchTools", "tools."} {
		if !strings.Contains(view.Description, want) {
			t.Fatalf("codemode 描述缺 %q（教学正文/引导没接上）:\n%s", want, view.Description)
		}
	}
	if strings.Contains(view.Description, "the tool catalog is unavailable") {
		t.Fatalf("目录生成硬失败（Options.Tools 没接对？）:\n%s", view.Description)
	}

	// 目录确实来自**活的注册表**（Options.Tools 闭包的接线）：
	//   - codemode 档的工具进描述（模拟「可编排但不声明给模型」那一档）；
	//   - direct 档的工具**不进**描述（Wave 2/pi 语义：mode:"on" 时它已经躺在模型的
	//     工具表里，再列一遍纯烧 3000 预算 —— 这条不是本波次能改的语义）。
	probe := &codemodeTierStub{BaseTool: tools.BaseTool{
		Name_: "codemode_probe", Description_: "A probe tool for the codemode catalog.",
		Params_: tools.Obj(map[string]any{}),
	}}
	eng.RegisterTool(context.Background(), probe)
	view = schemaNamed(t, eng, "codemode")
	if !strings.Contains(view.Description, "codemode_probe") {
		t.Fatalf("codemode 档的工具没进目录（Options.Tools 闭包没接到活注册表）:\n%s", view.Description)
	}
	if got := schemaNamed(t, eng, "codemode_probe"); got != nil {
		t.Fatal("codemode 档工具不该进 ToolParams（分层失效）")
	}

	// mode:"on" 的定义：给**已声明**且可编排的工具追一句「也能从脚本调用」。
	// read_file 是 direct 档（既有工具不实现 ExposureProvider），必须被追加。
	rf := schemaNamed(t, eng, "read_file")
	if rf == nil {
		t.Fatal("ToolParams 缺 read_file")
	}
	if !strings.Contains(rf.Description, "Also callable from a codemode script: tools.read_file({...})") {
		t.Fatalf("loadout 片段没追加到 read_file（mode:on 的收益没了）:\n%s", rf.Description)
	}
	// 反向：codemode 档的工具**不**该被追加（它不在模型工具表里，追加了也没人看）。
	if got := schemaNamed(t, eng, "codemode_probe"); got != nil {
		t.Fatalf("codemode 档工具不该出现在 ToolParams: %+v", got)
	}

	// store 落在会话级目录（装配方建目录 + OpenStore），权限 0600。
	storePath := filepath.Join(wsSessionsDir(ws), "sid-codemode", codemodeStoreDirName, codemode.StoreFileName)
	st, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("会话级 store 未落盘（%s）: %v", storePath, err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store 权限 = %o, want 600", perm)
	}
}

// TestSessionEngineCodemodeAbsentByDefault 默认（出厂）与显式关闭都必须**完全不注册**：
// ToolParams 里没有 codemode，GetTool 也取不到（不是「注册成 hidden」）。
func TestSessionEngineCodemodeAbsentByDefault(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
	}{
		{"默认（无配置）", `{"theme":"dark"}`},
		{"显式 enabled=false", `{"codemode":{"enabled":false}}`},
		{"mode=off", `{"codemode":{"enabled":true,"mode":"off"}}`},
		{"mode=only 被拒", `{"codemode":{"enabled":true,"mode":"only"}}`},
		{"enabled 类型不对", `{"codemode":{"enabled":"true"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := isolateHome(t)
			writeSettingsJSON(t, dir, c.body)
			ws := t.TempDir()
			eng := newTestSessionEngine(t, ws, "sid-off", profiles["work"], nil)

			if _, err := eng.GetTool(context.Background(), "codemode"); err == nil {
				t.Fatal("不该注册 codemode")
			}
			if view := schemaNamed(t, eng, "codemode"); view != nil {
				t.Fatal("ToolParams 里不该有 codemode")
			}
			// 连 loadout 片段也不该出现（它由 codemode 工具实例提供）。
			if rf := schemaNamed(t, eng, "read_file"); rf != nil &&
				strings.Contains(rf.Description, "Also callable from a codemode script") {
				t.Fatalf("没有 codemode 却追加了脚本调用片段:\n%s", rf.Description)
			}
			// 也不该有会话级 store 目录（没有工具就没有 store）。
			if _, err := os.Stat(filepath.Join(wsSessionsDir(ws), "sid-off", codemodeStoreDirName)); err == nil {
				t.Fatal("未激活却建了 codemode 目录")
			}
		})
	}
}

// TestSessionEngineCodemodeUsesSessionScopedStoreAcrossSessions 两个会话各自的 store 互不
// 干扰（同一 workspace 不同 sid）：会话级隔离是装配方给的路径保证。
func TestSessionEngineCodemodeUsesSessionScopedStoreAcrossSessions(t *testing.T) {
	dir := isolateHome(t)
	writeSettingsJSON(t, dir, `{"codemode":{"enabled":true}}`)
	ws := t.TempDir()
	for _, sid := range []string{"sid-a", "sid-b"} {
		eng := newTestSessionEngine(t, ws, sid, profiles["work"], nil)
		if _, err := eng.GetTool(context.Background(), "codemode"); err != nil {
			t.Fatalf("%s: codemode 未注册: %v", sid, err)
		}
		if _, err := os.Stat(filepath.Join(wsSessionsDir(ws), sid, codemodeStoreDirName, codemode.StoreFileName)); err != nil {
			t.Fatalf("%s: store 未落盘: %v", sid, err)
		}
	}
}

// TestSessionEngineCodemodeRegistersForWorkProfile Work（办公档）也要有：它是执行通道
// （同 run_python 的口径），不是 shell —— 真正的门是用户显式打开的 codemode.enabled。
func TestSessionEngineCodemodeRegistersForWorkProfile(t *testing.T) {
	dir := isolateHome(t)
	writeSettingsJSON(t, dir, `{"codemode":{"enabled":true}}`)
	for _, name := range []string{"work", "code"} {
		eng := newTestSessionEngine(t, t.TempDir(), "sid-"+name, profiles[name], nil)
		if _, err := eng.GetTool(context.Background(), "codemode"); err != nil {
			t.Fatalf("%s profile: codemode 未注册: %v", name, err)
		}
	}
}

// TestSessionEngineCodemodeInvalidSIDDegradesToMemoryStore sid 不可用（拿不到会话目录）时
// 仍要装配成功（内存 store），**不能**让会话建不起来。
func TestSessionEngineCodemodeInvalidSIDDegradesToMemoryStore(t *testing.T) {
	dir := isolateHome(t)
	writeSettingsJSON(t, dir, `{"codemode":{"enabled":true}}`)
	ws := t.TempDir()
	eng := newTestSessionEngine(t, ws, "bad/../sid", profiles["work"], nil)
	if _, err := eng.GetTool(context.Background(), "codemode"); err != nil {
		t.Fatalf("拿不到会话目录也必须能装配（内存 store）: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsSessionsDir(ws), "bad/../sid")); err == nil {
		t.Fatal("非法 sid 不该被用来建目录")
	}
}
