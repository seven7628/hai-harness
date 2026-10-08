package main

// codemode.go —— codemode（脚本编排工具）的**宿主装配**（Wave 3）。
//
// 这一层只做三件事，别在这里塞第四件：
//
//  1. 读 settings.json 的 codemode 段（`loadCodemodeSettings`）：默认**关闭**
//     （对齐 pi `defaultActive:false`），非法值一律退化到默认并留可读警告 ——
//     一个写错的配置项不该让桥起不来，但也不许静默生效。
//  2. 按设置注册工具本体（`registerCodemode`）：enabled=false **完全不注册**
//     （不是「注册成 hidden」—— 模型会在工具表里看到 hidden 与看不到是同一件事，
//     但注册了就等于留了一个可被脚本摸到的入口，而 enabled=false 的语义是「没有」）。
//  3. 给 MCP 的「未声明档位」定默认落点（`mcpDefaultExposure`）：codemode 激活时
//     deferred（激活通道是它的 searchTools()），未激活时 direct（否则 MCP 工具对模型
//     **整体不可达** —— 既不在 ToolParams，也没有任何东西能把它加载回来，§7.11）。
//
// # 配置形状（settings.json 顶层，自由 JSON 加顶层字段的先例：browser_engine/cron/mesh）
//
//	{"codemode": {"enabled": true, "mode": "on", "inline_budget": 3000, "budget_seconds": 60}}
//
// 字段语义见 codemodeSettings 的注释与 §7.17 的配置表；缺失 → 默认（不报错）。
//
// # 热读的边界（诚实交代）
//
// 工具注册跟着 loop 重建走（`reload_settings` → `rebuildAll` → `newSessionEngine`），
// 所以 codemode.enabled/inline_budget/budget_seconds 改完发一次 reload_settings 即生效。
// **MCP 默认档位不是**：它在「服务器连接、工具适配器建立」的那一刻被读走
// （`Manager.Tools` → `newAdapterWithDefault`），故 `applyMCPDefaultExposure` 在启动与
// reload_settings 两处调用，把两侧拉齐；除此之外没有热重注册通道（本波次明确不做）。
// 单台服务器仍可用面板的「模型可见性」即时覆盖。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/execproc"
	"github.com/seven7628/hai-harness/mcp"
	"github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/session"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin/codemode"
)

const (
	// maxCodemodeInlineBudget inline_budget 上限（估算 token）。超了不是「更好」而是单位
	// 写错（3000 是 token，30 万 token ≈ 1.2 MB 描述，装不进任何模型的上下文）——
	// 与「越界即退化默认」同一条纪律，宁可不生效也不要按字节数去撑爆工具 schema。
	maxCodemodeInlineBudget = 100_000
	// codemodeStoreDirName 会话目录下的 codemode 目录（store 文件落在它下面）。
	// 与 session 层的 `<sessions>/<wsKey>/<sid>/agents/` 同构：一个功能一个子目录。
	codemodeStoreDirName = "codemode"
)

// codemodeSettings settings.json 的 codemode 段（**已校验、已补齐默认**的形态）。
//
// 字段与默认值（三条都由 loadCodemodeSettings 保证不会出现第三种值）：
//
//	Enabled      默认 false（出厂关闭）
//	Mode         只会是 codemode.ModeOn（"only" 被拒绝：Wave 2 只做了一半，见 §7.16）
//	InlineBudget 估算 token：0 = 用 codemode 的 DefaultInlineBudget（**装配时必须翻成
//	             Options 的 nil**，不能给 0 —— 在 codemode 的契约里 0 是「一条都不列」）；
//	             <0 = 不限；>0 覆盖
//	ScriptTimeout >0，默认 codemode.DefaultScriptTimeoutMs（30s）
type codemodeSettings struct {
	Enabled       bool
	Mode          codemode.Mode
	InlineBudget  int
	ScriptTimeout time.Duration
}

// defaultCodemodeSettings 出厂默认：关闭 + ModeOn + 目录用默认预算 + 墙钟 30s。
//
// 为什么 Mode 仍然给 "on"：enabled=false 时它不被使用（工具根本不注册）；而一旦用户
// 打开开关，默认模式照 pi 的 `mode ?? "on"` 就是 on —— "only" 是半成品，装配不得使用。
func defaultCodemodeSettings() codemodeSettings {
	return codemodeSettings{
		Enabled:       false,
		Mode:          codemode.ModeOn,
		InlineBudget:  0,
		ScriptTimeout: codemode.DefaultScriptTimeoutMs * time.Millisecond,
	}
}

// loadCodemodeSettings 读 settings.json 的 codemode 段。
//
// 第二个返回值是**给宿主日志的可读警告**（每个被拒的字段一条，调用方负责打印）：
// 让「配置被拒」这件事在桥的 stderr 上看得见，而不是变成「我明明开了却没生效」。
// 本函数**永不返回 error**：配置错误只影响它自己那一项，其余照常生效。
//
// 逐字段解码（而不是整体 Unmarshal 进结构体）：一个字段类型写错（"enabled": "yes"）
// 不该连累同一节里其它正确的字段 —— 整体解析失败会把整段退化成默认。
func loadCodemodeSettings() (codemodeSettings, []string) {
	out := defaultCodemodeSettings()
	raw, err := os.ReadFile(homeCfgPath("settings.json"))
	if err != nil {
		return out, nil // 没配置文件 = 全默认（不是错误）
	}
	var outer struct {
		Codemode json.RawMessage `json:"codemode"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return out, []string{fmt.Sprintf("settings.json 不是合法 JSON（%v），codemode 段按默认关闭处理", err)}
	}
	if len(outer.Codemode) == 0 {
		return out, nil // 整段缺失 = 全默认
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(outer.Codemode, &fields); err != nil {
		return out, []string{fmt.Sprintf("codemode 段不是对象（%v），按默认关闭处理", err)}
	}
	var warns []string
	warn := func(format string, args ...any) { warns = append(warns, fmt.Sprintf(format, args...)) }

	// enabled：唯一决定「注册 / 不注册」的开关。类型不对 → 默认关闭（安全侧），
	// 不按「能解析出真值就当 true」的模糊规则来。
	if v, ok := fields["enabled"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			warn("codemode.enabled 不是布尔值（%s），按关闭处理", strings.TrimSpace(string(v)))
		} else {
			out.Enabled = b
		}
	}

	// mode：只认 "on"/"off"。"only" 是**半成品**（只兑现了「描述改列全部 direct 工具」，
	// 「对模型隐藏已声明工具」需要 ToolParams 的 hiddenDeclarations 语义，见 §7.16 边界 2）
	// —— 接受它 = 静默把用户要的「工具对模型隐藏」降级成「工具照旧可见」，故整段关闭
	// 并明确说出原因（用户想用 only 就不会得到一个假装成功的 on）。
	if v, ok := fields["mode"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			warn("codemode.mode 不是字符串（%s），按默认 %q 处理", strings.TrimSpace(string(v)), codemode.ModeOn)
		} else {
			// 大小写不敏感（"ON"/"Only" 与 "on"/"only" 同一语义）：配置是人手写的，
			// 大小写不该改变「开 / 关 / 拒绝」的判定（MCP exposure 同一口径）。
			switch strings.ToLower(strings.TrimSpace(s)) {
			case string(codemode.ModeOn):
				out.Mode = codemode.ModeOn
			case "off":
				out.Enabled = false // 显式关：与 enabled=false 同义（两个开关都关才算关）
			case string(codemode.ModeOnly):
				out.Enabled = false
				warn("codemode.mode=\"only\" 尚未实现（已声明工具不会对模型隐藏），" +
					"本次按**关闭**处理；请用 \"on\" 或 \"off\"")
			case "":
				out.Mode = codemode.ModeOn // 空串 = 没写 → 默认
			default:
				warn("codemode.mode=%q 无法识别（只接受 \"on\"/\"off\"），按默认 %q 处理",
					strings.TrimSpace(s), codemode.ModeOn)
			}
		}
	}

	// inline_budget：目录描述的估算 token 上限。>0 覆盖；<0 = 不限；0/缺省 = 用默认。
	if v, ok := fields["inline_budget"]; ok {
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			warn("codemode.inline_budget 不是整数（%s），用默认预算", strings.TrimSpace(string(v)))
		} else if n > maxCodemodeInlineBudget {
			warn("codemode.inline_budget=%d 越界（上限 %d，单位是估算 token），用默认预算",
				n, maxCodemodeInlineBudget)
		} else {
			out.InlineBudget = n
		}
	}

	// budget_seconds：脚本未在首行 @options 声明 timeout_ms 时的默认墙钟上限（秒）。
	// 取值域与 @options.timeout_ms 对齐（>0 且 ≤ 1 小时）——两层同一口径，模型与宿主
	// 看到的边界不会打架。
	if v, ok := fields["budget_seconds"]; ok {
		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			warn("codemode.budget_seconds 不是数字（%s），用默认 %s",
				strings.TrimSpace(string(v)), defaultCodemodeDurationText())
		} else if ms := f * 1000; ms < 1 || ms > codemode.MaxScriptTimeoutMs {
			warn("codemode.budget_seconds=%v 越界（须在 (0, %d] 秒内），用默认 %s",
				f, codemode.MaxScriptTimeoutMs/1000, defaultCodemodeDurationText())
		} else {
			out.ScriptTimeout = time.Duration(ms) * time.Millisecond
		}
	}
	return out, warns
}

// defaultCodemodeDurationText 默认墙钟上限的可读文本（日志用；不引入第二个数字来源）。
func defaultCodemodeDurationText() string {
	return (codemode.DefaultScriptTimeoutMs * time.Millisecond).String()
}

// loadCodemodeSettingsQuiet loadCodemodeSettings 的日志封装：把被拒字段的警告打到
// stderr 后返回设置。装配点用这个（它们只需要值，不想每处都写两行）。
func loadCodemodeSettingsQuiet() codemodeSettings {
	cs, warns := loadCodemodeSettings()
	logCodemodeWarnings(warns)
	return cs
}

// codemodeNodeRuntime 进程级 node 探测器（与 execproc 内部的默认探测器同款：只解析配置，
// 探测发生在首次 Ensure，结果在实例内按锁缓存）。
//
// 放在这里而不是每个会话建一个：探测会 spawn `node --version`，多个会话各建一个实例
// 就等于冷启动时反复探测（execproc 的实例缓存救不了跨 Executor 的重复探测）。
var codemodeNodeRuntime = runtime.NewNodeRuntime(runtime.NodeConfig{})

// codemodeStore 打开会话级 store。
//
// 路径：<appData>/sessions/<wsKey>/<sid>/codemode/codemode-store.jsonl（与 subagent
// journal 的 <sid>/agents/ 同构）。目录不存在时由**本函数**建（codemode.OpenStore 刻意
// 不建目录：store 不住在别人的目录所有权里，装配方才是目录 owner）。
//
// 拿不到合适路径时（sid 非法/空、建目录失败、OpenStore 失败）退化为**进程内内存 store**
// 并返回一条可读说明：不落盘也要能用（跨调用可见），但绝不允许静默半残 —— 调用方必须
// 把说明写进日志。第二个返回值 "" 表示正常落盘。
func codemodeStore(workspace, sid string) (*codemode.Store, string) {
	degrade := func(why string) (*codemode.Store, string) {
		st, err := codemode.OpenStore("") // "" = 纯内存
		if err != nil {
			// OpenStore("") 只可能是「不会发生」的构造失败；真发生时也不能 panic 装配。
			return nil, fmt.Sprintf("codemode store 初始化失败（%v），store 不可用: %s", err, why)
		}
		return st, fmt.Sprintf("codemode store 退化为内存 store（跨调用可见、重启即失）: %s", why)
	}
	if err := session.ValidateSessionID(sid); err != nil {
		return degrade(fmt.Sprintf("会话 id 不可用（%v）", err))
	}
	dir := filepath.Join(wsSessionsDir(workspace), sid, codemodeStoreDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil { // 会话数据含脚本产物，0700（同 session 层）
		return degrade(fmt.Sprintf("创建会话目录 %s 失败（%v）", dir, err))
	}
	st, err := codemode.OpenStore(codemode.StorePath(dir))
	if err != nil {
		return degrade(fmt.Sprintf("打开 %s 失败（%v）", dir, err))
	}
	return st, ""
}

// mcpDefaultExposure MCP「未声明档位」的默认落点：codemode 激活 → deferred，否则 direct。
//
// 为什么必须条件化（§7.11 反复强调的坑）：deferred 的**激活通道**是 codemode 的
// searchTools()/tool_search。通道不在（codemode 关掉）而默认落 deferred，等于把 MCP
// 工具对模型整体关闭 —— 既不进 ToolParams，也没有任何东西能把它加载回来。
func mcpDefaultExposure(codemodeEnabled bool) tools.ToolExposure {
	if codemodeEnabled {
		return tools.ExposureDeferred
	}
	return tools.ExposureDirect
}

// applyMCPDefaultExposure 读 codemode 设置并写进**这个** MCP 管理器（装配决策）。
//
// 调用点：新工作区运行态建立时（紧跟在 mcp.NewManagerWithSource 之后）—— 每个工作区
// 一个 Manager，各自都要定档。
// 这样两侧永远一致：codemode 关掉后如果 MCP 还停在 deferred，就是上面那条「整体不可达」。
func applyMCPDefaultExposure(mcpm *mcp.Manager) {
	if mcpm == nil {
		return
	}
	cs, warns := loadCodemodeSettings()
	mcpm.DefaultExposure = mcpDefaultExposure(cs.Enabled)
	logCodemodeWarnings(warns)
}

// applyMCPDefaultExposureAllWorkspaces reload_settings 用：读一次设置，应用到**全部**
// 已建工作区的 Manager（与 refreshComputerApprovedApps 同款：设置是全局的，运行态是按
// 工作区一份），再打一轮警告。必须在 rebuildAll **之前**调用 —— 适配器在
// newSessionEngine → Manager.Tools 时按这个值定型。
func (m *manager) applyMCPDefaultExposureAllWorkspaces() {
	cs, warns := loadCodemodeSettings()
	exp := mcpDefaultExposure(cs.Enabled)
	m.mu.Lock()
	wss := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wss = append(wss, ws)
	}
	m.mu.Unlock()
	for _, ws := range wss {
		if ws != nil && ws.mcp != nil {
			ws.mcp.DefaultExposure = exp
		}
	}
	logCodemodeWarnings(warns)
}

// inlineBudgetFunc 把设置里的 inline_budget 翻成 Options.InlineBudget。
//
// 关键：值 0（= 缺省/用默认）必须翻成 **nil**（= codemode 的 DefaultInlineBudget），
// 不能翻成「返回 0 的函数」—— 在 codemode 的契约里 0 是「一条都不列」的极端档
// （SelectCatalog：预算 0 时连组标题都不渲染，描述只剩一句「用 searchTools 找」），
// 于是「用户没配这一项」会被装配成「目录永远是空的」。>0 覆盖；<0 = 不限。
func inlineBudgetFunc(v int) func() int {
	if v == 0 {
		return nil
	}
	return func() int { return v }
}

// registerCodemode 注册 codemode 工具本体（在 newSessionEngine 里调用，Wave 3 装配）。
//
// enabled=false → **完全不注册**（不注册成 hidden，见文件头）；返回 false。
//
// 装配的四个必需项（Options 的注释是契约）：
//   - Engine：正在装配的引擎（嵌套调用走完整管线：审批 → 校验 → 时限 → 记录 → 用量）；
//   - Tools：返回**全部**已注册工具（用闭包读引擎，注册顺序无关；分层是 BuildCatalog
//     的职责，这里不许过滤）。本工具自己会被 snapshot 跳过，无需特判；
//   - Exec：与 bash/run_python **同一个** sandbox 实例（同一套 Seatbelt 策略）+ 进程级
//     node 探测器；Cwd = 工作区根（与 bash/run_python 同锚）；
//   - Store：会话级 append-only store（拿不到路径则内存 store + 日志）。
//
// 只在这里注册一次：profile 不参与门控（与 run_python 同款口径 —— 它是执行通道而不是
// shell，Work 档也要能编排）；真正的门是用户显式打开的 codemode.enabled。
func registerCodemode(eng *tools.Engine, workspace, sid string, sbx sandbox.Sandbox, cfg codemodeSettings) bool {
	if eng == nil || !cfg.Enabled {
		return false
	}
	st, note := codemodeStore(workspace, sid)
	if note != "" {
		logCodemodeWarnings([]string{note})
	}
	eng.RegisterTool(context.Background(), codemode.New(codemode.Options{
		Engine: eng,
		Tools:  func() []tools.Tool { return eng.Tools() },
		// Mode/InlineBudget/ScriptTimeout 都接装配时读到的设置值：本工具实例随 loop
		// 重建而重建（reload_settings → rebuildAll），故捕获值不会比设置旧。
		Mode:          func() codemode.Mode { return cfg.Mode },
		InlineBudget:  inlineBudgetFunc(cfg.InlineBudget),
		ScriptTimeout: func() time.Duration { return cfg.ScriptTimeout },
		// Namespaces 传 nil：分组标题与 describeTool 的 instructions 由**工具自己**的
		// NamespaceProvider 提供（MCP 工具就实现了它：名字 = server 名、说明 = server 自述；
		// 连接上的服务器还给 instructions）。这一层的缺口只影响「宿主额外注入的 namespace」
		// 这种用法（目前没有），而补它要调 Manager.Instructions（会触发惰性连接、最长 15s
		// 的 initialize），不值得在每次描述生成里做。
		Namespaces: nil,
		Store:      st,
		Exec:       execproc.New(sbx, codemodeNodeRuntime),
		Cwd:        workspace,
	}))
	return true
}

// logCodemodeWarnings 打印设置警告（沿用桥的 stderr 告警形态）。
//
// 为什么不去重：同一进程里 loadCodemodeSettings 会被调用两次（启动定 MCP 默认档 +
// 每个会话装配各一次），把「配置写错了」重复说三遍的代价，远小于「用户以为开了却没开」
// 的排查成本；何况这些行只在配置真有错时出现。
func logCodemodeWarnings(warns []string) {
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "⚠ codemode 设置: %s\n", w)
	}
}
