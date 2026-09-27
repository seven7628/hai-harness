// hai-bridge：桌面端宿主层（Go stdio NDJSON 宿主，多工作区；SDK 纯库零改动）。
//
// 单进程多 workspace：一个 bridge 进程承载所有工作区 × 所有会话。
// workspace 是协议的一等维度（会话级命令 payload 带 workspace + session_id），
// 会话在同一进程内按工作区隔离（storeDir/FileTools 根均按 workspace）。
//
// 协议（stdio NDJSON）：
//
//	stdin  一行一个命令 {"id": int, "type": string, "payload": {...}}
//	stdout 一行一个事件（SDK Event 平铺 + session_id + workspace 路由键），以及命令响应
//	       {"event_type":"command_response", "id", "ok", "error", "data", "workspace"}
//
// 命令集：new_session / list / ask / ask_batch / interrupt / approve / answer_question / compact /

//	external_skills_status / external_skills_import /
//
//	switch_mode / switch_effort / switch_persona / switch_model / set_provider /
//
//	set_goal_alignment_rounds / set_max_subagents / list_models / reload_settings / file_preview / xlsx_preview / skills / skill_toggle / skill_get / load_skill /
//	skill_install / skill_browse / skill_update / skill_uninstall /
//	market_add / market_remove / market_list / skill_update_market /
//	mcp_list / mcp_set / mcp_refresh / plugin_list / plugin_install / plugin_uninstall /
//	plugin_enable / plugin_disable / browser_status / browser_pages / browser_screenshot /
//	browser_close /
//	command / metrics / delete_session /
//	delete_workspace / shutdown
//	git_snapshot / git_file_diff / git_history / git_commit_diff（只读）
//	git_stage / git_unstage / git_discard / git_commit（P1 写操作；路径限制在 workspace 内）
//	（会话级命令均要求 payload.workspace；command = 斜杠命令 /xxx 解析执行）
//
// 运行：go run ./desktop/bridge（--workspace 已废弃，随命令 payload 传入）
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/computer"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/cron"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/hooks"
	"github.com/seven7628/hai-harness/mcp"
	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/plugin/browser"
	computerplugin "github.com/seven7628/hai-harness/plugin/computer"
	implugin "github.com/seven7628/hai-harness/plugin/im"
	"github.com/seven7628/hai-harness/provider"
	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/session"
	meshremote "github.com/seven7628/hai-harness/session/remote"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/skills/remote"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/todo"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
	"github.com/seven7628/hai-harness/tools/question"
)

const (
	// 桌面端 persona 系统提示词（英文、分节）见 agents/prompt/product.go。
	// Code：2026-08-25 起用唯一简洁的 DesktopCodePromptV2（自带 Harness/Background
	// Tasks 契约），注入时停用 SDK Base 防条款重复拼接（见 profiles.replaceBase）。
	// Work：仍为 DefaultSystemPrompt + DesktopWork 双层拼接。
	// 注：agent_spawn 子 Agent 与主会话共用 DesktopCodePromptV2（WithBaseSystemPrompt
	// 注入，见 buildSpawnLoop），角色/任务由主 agent 的 task 参数动态生成。
	systemPrompt = prompt.DesktopCodePromptV2
	workPrompt   = prompt.DesktopWork
)

// computerPromptGuide Computer Use 插件启用时追加的系统提示词段（§8 落地）。
// 真机 LLM E2E 教训（2026-09）：模型自由决策时会固执重试失败调用、用 bash 越界
// 自救（open/osascript/killall）、不听任务里的路径提示、点添加按钮后不知键入——
// 用行为准则约束，把隐含 UI 状态（选中列表/输入框出现）变显式。
//
// 授权清单动态注入（2026-09 补）：已授权 App（settings.json
// permissions.computer_approved_apps）随提示词列出——模型 open_app/activate
// 只在这些 App 上放行，清单内用的是授权时记录的准确名字（如 "WPS Office"），
// 避免模型拿用户口语简称（"WPS"）去猜。清单为空 = 全部拒绝，明示引导。
const computerPromptGuide = `
## Desktop control (Computer Use)
- computer_* tools control the real macOS desktop. Work in small steps: computer_snapshot → act → verify via snapshot.
- Prefer semantic targets (uid / menu path / text) from the snapshot; never invent coordinates or menu names — read them from the snapshot (‹› = accessibility description, [selected]/[focused] = selection/focus state).
- After any action, uid can go stale — take a fresh snapshot before the next action.
- If a tool fails or the UI looks different than expected: take a fresh snapshot and adapt. Do NOT blindly retry the same call, and do NOT switch to bash (open/osascript/killall/pkill) to operate apps or the system — that bypasses the controlled desktop tooling and is disallowed for desktop tasks.
- When the user names an app that is not open, use computer_open_app (it resolves localized names); if it reports not found, try the English name once, then report back rather than looping.
- Text entry goes to the focused field; click the field first when needed, and verify the typed text appears in the next snapshot before submitting.
- Platform habits: menu bars (computer_press with app+menuPath) are the most reliable path for commands like New/Save. Shortcuts like cmd+n often require a list/item to be selected first ([selected]) — select a target from the snapshot, then press. After pressing an "add/new" control, an input field usually appears — type into it right away (a tool result may say "New input field detected").
- If the frontmost app is not the one you want (snapshot header shows another app), call computer_open_app / computer_activate to bring yours forward, then snapshot again before acting.
`

// computerAppsGuide 已授权 App 动态清单段（追加在 computerPromptGuide 之后）。
// 目的（真机教训 2026-09）：用户说「打开 WPS」时模型不知道授权名是 "WPS Office"，
// 清单把授权时记录的准确显示名给模型 —— 唤起/激活时优先用清单内的名字，而不是
// 用户口语简称或猜英文名。每次 buildLoop 现读 settings.json（热更新即时生效）。
func computerAppsGuide() string {
	apps := loadComputerApprovedApps()
	names := make([]string, 0, len(apps))
	for a := range apps {
		names = append(names, a)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("\n- Approved apps for computer_open_app / computer_activate: ")
	if len(names) == 0 {
		b.WriteString("none yet — desktop control is currently denied for all apps. If the user wants a desktop task, tell them to add the app under Settings → Plugins → Computer Use → Approved Apps (or ask them to do so), then retry.")
		return b.String()
	}
	b.WriteString(strings.Join(names, ", ") + ".")
	b.WriteString(` Only these apps can be opened/activated; anything else is rejected with guidance. When the user names an app in conversation, map it to the exact approved name above (e.g. user says "WPS" but the approved name is "WPS Office" — use "WPS Office").`)
	return b.String()
}

// personaProfile 会话 Persona：系统提示词 + 工具集（同一内核不同 profile，见 DESKTOP.md §5.6）。
type personaProfile struct {
	name         string
	systemPrompt string
	withBash     bool // Work 无 bash（办公助理不跑命令）
	withAgent    bool // Work 无 agent_*/plan_submit（不派子 agent / 不规划）
	replaceBase  bool // systemPrompt 自带行为契约时停用 SDK DefaultSystemPrompt（防重复拼接）
}

var profiles = map[string]personaProfile{
	"code": {name: "code", systemPrompt: systemPrompt, withBash: true, withAgent: true, replaceBase: true},
	"work": {name: "work", systemPrompt: workPrompt, withBash: false, withAgent: false},
}

// workBinaryHint read_file 拒绝二进制文件（docx/xlsx/pdf 等）时 Work persona 的尾句。
// 参数 = 文件路径（钩子语义：宿主可按扩展名细分格式），withPython = 本会话是否注册了
// run_python（受管运行时未装配时不注册，见 newSessionEngine —— 漏传会让提示又指向不
// 存在的工具）。
//
// SDK 默认尾句指向 shell，Work 不注册它 → 建议指向不存在的工具（模型照着试、白耗轮次）。
// 2026-09 修订：**办公能力已随包就位**（run_python 受管运行时 + docx/xlsx/pptx/pdf 四个
// 技能），旧句仍写着「二进制格式在这里无法转换…请导出成 txt」，模型据此回用户「本会话
// 不支持」——把已具备的能力说成没有（TECH_PLAN §3.6：错误信息必须指向真实存在的能力）。
//
// 因此只写本会话真有的路径，顺序即引导顺序：load_skill（拿到该格式的 gotcha 与推荐流程）
// → run_python（受管 Python，办公库全预装）。命中办公四格式给具体技能名与库；zip/媒体
// 容器等**没有随包技能**的格式才走兜底 —— 先看脚本（标准库也算）能否解决，确实无通道才
// 如实告知并建议用系统程序打开/导出文本。既不承诺「任何二进制都有通道」，也不把「无通道」
// 的结论提前套到办公格式上（这正是本次修复的缺陷）。
func workBinaryHint(path string, withPython bool) string {
	if !withPython {
		// 没有 run_python（未装配受管运行时）：不得提它，否则又是指向不存在的工具。
		return "There is no shell tool in this session and no managed Python runtime, so this format cannot be converted here. " +
			"Tell the user plainly that this file cannot be read in this session, and ask them to open it with a desktop app and export it as text (txt/md/csv), or paste the content."
	}
	const capability = "There is no shell tool in this session, but run_python is the execution channel here: python-docx, openpyxl, python-pptx, pypdf, pypdfium2 and mammoth are preinstalled, " +
		"and the docx/xlsx/pptx/pdf skills describe the recommended workflows."
	var skill, lib string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".docx", ".docm":
		skill, lib = "docx", "python-docx"
	case ".xlsx", ".xlsm":
		skill, lib = "xlsx", "openpyxl"
	case ".pptx", ".pptm":
		skill, lib = "pptx", "python-pptx"
	case ".pdf":
		skill, lib = "pdf", "pypdf"
	}
	if skill == "" {
		return capability + " No bundled skill covers this format: use run_python first if a script (the standard library is available too) can extract what you need; " +
			"only if nothing can, tell the user this format has no direct channel in this session and suggest opening it with a desktop app or exporting it as text (txt/md/csv)."
	}
	return fmt.Sprintf("%s For this file, call load_skill(%q) first for that format's gotchas and workflow, then read or convert it with %s via run_python.", capability, skill, lib)
}

// 输出：带锁行缓冲写 stdout（多会话并发事件行不交错）。
// 微批量 flush：普通事件攒缓冲（bufio 4KB 自动 + 定时 5ms），减少系统调用；
// 关键行（command_response 等同步点）经 writeLine 立即 flush，保证保序与实时性。
// 流式 chunk 延迟 ≤5ms（人眼无感），系统调用从每事件 1 次降到每秒 ~200 次。
const (
	flushInterval = 5 * time.Millisecond
	flushMinBytes = 4 << 10 // bufio 默认 4KB 自动 flush 阈值
)

type out struct {
	mu sync.Mutex
	w  *bufio.Writer
	// isFile 标记底层是否为 os.File（生产 stdout）：是 → 批量 flush（定时）；
	// 否（测试内存 buffer）→ 每行立即 flush，避免测试读空。
	isFile bool

	flushTimer *time.Timer // 定时 flush（懒启动；有未 flush 数据时重置）
	stopCh     chan struct{}
	stopOnce   sync.Once
}

func newOut(w io.Writer) *out {
	_, isFile := w.(*os.File)
	return &out{w: bufio.NewWriterSize(w, flushMinBytes), isFile: isFile, stopCh: make(chan struct{})}
}

// writeLine 写一行并立即 flush（命令响应 / 同步点：保序 + 前端依赖它作同步锚）。
func (o *out) writeLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_, _ = o.w.Write(b)
	_ = o.w.WriteByte('\n')
	_ = o.w.Flush()
}

// writeRaw 原样写一行（事件行已完整 JSON，emit 直接输出不重新 marshal/注入）。
// 不立即 flush：bufio 4KB 自动 + 定时 5ms 兜底（流式 chunk 延迟 ≤5ms）。
// 测试场景（底层为内存 buffer）立即 flush，避免测试读空（syncBuf 非 os.File）。
func (o *out) writeRaw(line []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, _ = o.w.Write(line)
	_ = o.w.WriteByte('\n')
	if !o.isFile {
		_ = o.w.Flush() // 内存 buffer（测试）：立即可见
		return
	}
	o.scheduleFlushLocked()
}

// flushNow 立即冲刷缓冲（进程退出 / 测试同步点用）。
func (o *out) flushNow() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.flushTimer != nil {
		o.flushTimer.Stop()
		o.flushTimer = nil
	}
	_ = o.w.Flush()
}

// Close 停止定时 flush 并冲刷剩余（进程退出调用）。
func (o *out) Close() {
	o.stopOnce.Do(func() { close(o.stopCh) })
	o.flushNow()
}

// scheduleFlushLocked 确保定时 flush 已启动（持锁调用）。
func (o *out) scheduleFlushLocked() {
	if o.flushTimer != nil {
		return // 已有定时器在途
	}
	o.flushTimer = time.AfterFunc(flushInterval, o.flushTick)
}

// flushTick 定时 flush：冲刷缓冲并复位定时器（若期间有新数据）。
func (o *out) flushTick() {
	o.mu.Lock()
	o.flushTimer = nil
	_ = o.w.Flush()
	// 检查是否有新数据（写回时 scheduleFlushLocked 会重建定时器）
	o.mu.Unlock()
	select {
	case <-o.stopCh:
	default:
	}
}

// bridgeSession：一个会话 + 常驻 run goroutine（消费队列，队列空即返回）
type bridgeSession struct {
	id     string
	s      *session.Session
	loop   *agents.AgentLoop // per-session loop（model/effort 运行时可切；persona 切换重建）
	effort string
	// subagentEffort 子 agent 档位持有者（spawn/explore/文件化人设 loop）：switch_effort
	// 时与主 loop 同步 —— 子 agent 追随主 loop（2026-09 决策）。rebuildLoop 换代。
	subagentEffort *subagentEffortRef
	model          string // 当前模型（switch_model 设置；下一次请求生效）
	provider       string // 当前 provider（set_provider 重建时更新；emit 打标价表用）
	// providerExplicit 本会话 Provider 是否由用户显式指定（switch_model 携带 provider /
	// 新会话 UI 指定）。true 时 rebuildAll 不再按模型名全局字母序重解析归属——
	// 避免「glm-5.3-flash 同时存在于 opencode+zhipu 时被字母序改写到 opencode」
	//（2026-09 修复：会话 Provider 归属以用户选择为准，不用模型名猜）。
	providerExplicit bool
	mode             *agents.Mode
	persona          string // 当前 Persona（"code"/"work"；switch_persona 重建 loop）
	ctx              context.Context
	cancel           context.CancelFunc
	wake             chan struct{}
	out              *out

	workspace string            // 工作区根（审批 diff 读取原文件用）
	metrics   *FileMetricsStore // 跨会话 usage/成本指标（每 llm_end 记录一条；全局共享）
	// im IM 事件桥（IM 插件启用后装配；emit 全事件漏斗里转发给 Renderer → 飞书推送）
	im *imBridge
	// 注意：字段用具体类型而非 MetricsStore 接口 —— m.metrics（*FileMetricsStore）为 nil 时
	// 赋值进接口会变成「类型非 nil 的 nil 指针」，`bs.metrics != nil` 判空失效 → Record 崩。
	eventsDir string     // 事件日志目录（{ws}/.go-code/events/）
	eventsMu  sync.Mutex // 保护事件日志文件（子 agent 并发事件流写同一文件）
	eventsF   *os.File   // 懒打开的事件日志文件（O_APPEND）

	questions *events.QuestionWaiter // ask_user 提问等待器（工具与 Session 共享；answer_question 命令注入）

	// ViewReducer + checkpoint commit coordinator（目标恢复架构，见 checkpoint.go）：
	// emit 是所有 SDK 事件的唯一漏斗——observe 先 Apply 进 reducer，再发 stdout/IPC；
	// 语义边界触发异步 CommitRecord（SessionRecord = sdk_state + view_checkpoint）。
	reducer *ViewReducer
	coord   *checkpointCoordinator
	wsKey   string // workspaceKey（record 路径维度）

	// 序列化缓存：session_id/workspace 的 JSON 字节（会话级不变，emit 高频复用，
	// 避免每个事件（尤其流式 chunk）重复 json.Marshal 这两个常量键值）。
	sidKV []byte
	wsKV  []byte

	// dispatchMu 把「Apply → marshal → stdout → 事件日志 → 提交请求」串行化为
	// per-session 单一序列（docs §3.1.3 step 3-5 的 ordered dispatcher）：
	// 否则并发事件（子 agent / PushTaskResult 与主 run）会在 observe 之后交错写 stdout，
	// 导致前端事件顺序与 reducer/checkpoint 顺序不一致。
	dispatchMu sync.Mutex

	// cron 派生会话标记：非空 = 本会话是某定时任务触发时新建的。
	// 该会话结束时（AgentEnd）据此回写运行账本并推 cron_run_status 事件。
	cronRun     string       // 触发 run_id（账本主键）
	cronLedger  *cron.Ledger // 共享运行账本（nil = 未启用）
	cronStore   *cron.Store  // 共享任务库（finishCronRun 回写 LastStatus 用）
	cronFiredAt time.Time    // 触发时间（Finish 计算耗时用）
	cronJobID   string       // 所属任务 id（事件打标用）
	cronEvDir   string       // 非空 = 事件日志独立目录（cron_sessions/<wsKey>/<jobID>；不落工作区 events）

	// AgentEnd 快照归档暂存：emit 的 AgentEnd 分支先记录状态/结果，待事件落盘后
	//（logEventLine 之后）调用 finishCronRun 归档（保证 ExtractMessages 读到完整对话）。
	cronEndStatus  cron.RunStatus
	cronEndErr     string
	cronEndContent string

	// isCron 标记本会话为定时任务派生会话：不出现在 workspace 会话列表，
	// 不被 workspace 级操作（中断全部/删除工作区/重建）误伤，独立归 Cron 管理。
	isCron bool

	// 注：沙箱放行确认已抽象到工具层审批通道（askSandboxApproval → ToolApprovalRequested
	// + approve 命令，见 sandboxReleaseGate），不再有独立的 sandbox_blocked 模态。

	lastActive atomic.Int64 // 最近活跃 unix ms（list updated_at 排序依据；run goroutine 写入、主 goroutine 读，用 atomic 免锁）
}

// isStreamingChunkType 流式增量事件（content_chunk / reasoning_chunk）——不落盘。
// 按**事件类型**判定（emit 手上就是 typed event），不扫描序列化后的行：
// 事件行的 JSON key 按字母序排列（content 在 event_type 之前），长 chunk 会把
// event_type 推到很后面，靠行首子串匹配会漏判；反过来在工具结果里搜子串又会误判。
func isStreamingChunkType(t events.EventType) bool {
	return t == events.ContentChunkType || t == events.ReasoningChunkType
}

// logEventLine 把转发给前端的事件行旁路追加到该会话事件日志（展示时间线，可重放重建链路）。
// 定时任务派生会话的事件日志重定向到 cronEvDir（独立记录，不进工作区会话历史）。
// 不 fsync：可重建的展示层数据，丢最新几行不致命；fsync 每行会拖慢事件转发。
//
// 流式增量不落盘（2026-09-10，见 docs/SESSION_JSONL_STATE_O2_FIX.md）：
// content_chunk / reasoning_chunk 是**可重建**的增量——实测占事件日志体积 78.5%
// （某会话 330,613 行 / 83 MB），而最终文本已由 llm_end.content / llm_end.reasoning
// 携带（reducer 有显式兜底 `b.Text == "" && e.Content != ""`；cron ExtractMessages
// 主用 agent_end.content）。落盘只保留事件**时间线**（run_id + timestamp + 工具起止
// + usage/cost），既省 78% 体积，又保住历史 trace 的唯一数据源。
func (bs *bridgeSession) logEventLine(line []byte) {
	dir := bs.eventsDir
	if bs.cronEvDir != "" {
		dir = bs.cronEvDir
	}
	if dir == "" {
		return
	}
	bs.eventsMu.Lock()
	defer bs.eventsMu.Unlock()
	if bs.eventsF == nil {
		f, err := os.OpenFile(filepath.Join(dir, bs.id+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // 事件日志含敏感内容，0600
		if err != nil {
			return
		}
		bs.eventsF = f
	}
	_, _ = bs.eventsF.Write(append(line, '\n'))
}

// sandboxReleaseGate 沙箱拦截放行门（装配期注入 WithRelease）。
// 决策统一抽象到工具层审批通道（2026-08-28 用户决策）：沙箱层不弹独立确认框，
// 命中拦截时按会话权限模式分派：
//   - full-access / all-pass：无沙箱直通（防御分支，正常不经本门——后端已切换直通）；
//   - bash 本就在工具层审批的模式（accept-edits/manual/hitl/normal/未设模式：
//     bash 自声明 RequiresApproval + 注入 approver → 执行前已预审）→ 自动放行重跑，
//     不重复打扰用户；
//   - 其余（auto 等 bash 全自动模式，无预审）→ 发起工具层审批请求
//     （ToolApprovalRequested 事件，UI 展示命令 + 被拒路径；用户经 approve 命令
//     决策；超时 /= 会话关闭 = 拒绝，安全默认）。
type sandboxReleaseGate struct {
	m  *manager
	ws string
	id string
}

func (g *sandboxReleaseGate) Release(ctx context.Context, spec sandbox.ExecSpec, blockHint string) bool {
	bs := g.m.get(g.ws, g.id)
	if bs == nil {
		return false
	}
	// full-access/all-pass：无沙箱直通，不弹审批
	if bs.mode != nil {
		switch modeName(bs.mode) {
		case "full-access", "all-pass":
			return true
		}
	}
	// bash 在工具层已预审（ApprovalRequired / 未设模式走工具自声明 + 注入 approver）
	// → 不重复问，自动放行重跑（用户已在执行前审过该命令）。
	if bs.mode == nil || bs.mode.Approve == nil ||
		bs.mode.Approve(core.ToolCall{Name: "bash"}) != agents.ApprovalAuto {
		return true
	}
	// auto 等 bash 全自动模式：沙箱拦截 → 工具层审批通道
	return bs.askSandboxApproval(ctx, g.m, spec, blockHint)
}

// askSandboxApproval 沙箱拦截 → 工具层审批通道：先注册审批请求（注册先行语义，
// 拿到 wait 再发事件）→ 发 ToolApprovalRequested（携带命令 + 被拒路径 hint）→
// 阻塞等用户决策（approve 命令经 Session.Approve 注入；超时/取消 = 拒绝）。
func (bs *bridgeSession) askSandboxApproval(ctx context.Context, m *manager, spec sandbox.ExecSpec, blockHint string) bool {
	cmdJSON, _ := json.Marshal(spec.Command)
	args := `{"command":` + string(cmdJSON) + `}`
	call := core.ToolCall{Name: "bash", Id: "sbx-" + bs.id + "-" + fmt.Sprint(time.Now().UnixNano()), Arguments: args}
	wait, err := bs.s.RequestApproval(ctx, call)
	if err != nil {
		return false // 审批通道不可用/会话关闭：安全默认拒绝
	}
	bs.out.writeLine(map[string]any{
		"event_type": events.ToolApprovalType,
		"id":         call.Id,
		"name":       call.Name,
		"arguments":  args,
		"hint":       blockHint, // 被拒路径/操作详情（UI 审批弹窗展示）
		"timestamp":  time.Now(),
		"session_id": bs.id,
		"workspace":  bs.workspace,
	})
	ok, err := wait(ctx)
	return err == nil && ok
}

func (bs *bridgeSession) wakeRun() {
	select {
	case bs.wake <- struct{}{}:
	default:
	}
}

// emit SDK 事件 → stdout（注入 session_id + workspace 路由键，事件自包含）
func (bs *bridgeSession) emit(e events.Event) {
	// per-session ordered dispatcher：Apply、marshal、stdout、日志、commit 请求
	// 必须按同一序列执行（并发子 agent / 任务结果事件不得与主 run 交错）。
	bs.dispatchMu.Lock()
	defer bs.dispatchMu.Unlock()

	// IM 镜像推送：会话事件 → Renderer → 飞书（有镜像关联的会话）。
	// 必须在 dispatchMu 内：与 stdout/日志同序（IM 消息不早于对应 UI 事件）。
	// 注意：RunStream 层事件（agent_start/content_chunk/llm_end）只经 emit 发出，
	// WithEventHandler（Session 层）收不到它们——IM 推送必须挂在这里（原挂在
	// main.go WithEventHandler，导致回复内容永远到不了 Renderer → IM 无回复）。
	if bs.im != nil {
		bs.im.OnSessionEvent(bs.workspace, bs.id, e)
	}

	// checkpoint 漏斗先行：reducer.Apply（稳定视图）+ 提交边界检测。
	// 必须在 marshal/写 stdout 之前——checkpoint 与实时事件保持同一顺序。
	bs.observe(e)
	// 性能：避免 marshal→unmarshal→marshal 三次序列化。直接 marshal 一次，
	// 在原始字节上注入 session_id/workspace（JSON 对象首部插入键），
	// 宿主附加字段（cost_usd/todos/diff）用 map 只对"需要注入"的事件构造。
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	// 在 {"event_type":"...","run_id":"..."} 首部注入 session_id/workspace
	//（保持合法 JSON：在第一个 { 后插入键值对）。键值字节已缓存（会话级不变）。
	injected, err := injectJSONKeys(b, []jsonKeyVal{
		{Key: "session_id", Value: bs.sidKV},
		{Key: "workspace", Value: bs.wsKV},
	})
	if err != nil {
		return
	}
	// 宿主附加字段（按事件类型）
	var extras map[string]any
	switch ev := e.(type) {
	case *events.LLMEnd:
		bs.lastActive.Store(time.Now().UnixMilli()) // 有产出 → 更新最近活跃
		if ev.Usage != nil {
			prov := bs.provider
			// 成本金额已由 provider 填充进 Usage.Cost（µUSD，见 provider.FillUsageCost）
			// 并随 MetricsEntry 持久化；cost_usd 浮点字段仅供前端展示（USD）。
			costUSD := costUsd(prov, ev.Model, *ev.Usage)
			if bs.metrics != nil {
				_ = bs.metrics.Record(MetricsEntry{
					Timestamp: ev.Timestamp, Workspace: bs.workspace,
					SessionID: bs.id, Provider: prov, Model: ev.Model, Usage: *ev.Usage,
				})
			}
			extras = map[string]any{"cost_usd": costUSD}
		}
	case *events.LLMError:
		// 失败尝试也要进账（2026-09-23 决策）：metrics 以 Kind=attempt_failed 单列
		// （调用数只数 call，重试浪费看 RetryCostUsd）；cost_usd 供前端会话成本累计 +
		// 重试块展示。用量可能为 nil（OpenAI 系协议 usage 只在收尾帧 → 中途失败拿不到）
		// —— 此时仍记一行（FailedAttempts 计数用），不污染 token/成本合计。
		entry := MetricsEntry{
			Timestamp: ev.Timestamp, Workspace: bs.workspace, SessionID: bs.id,
			Provider: bs.provider, Model: ev.Model, Kind: MetricsKindAttemptFailed,
		}
		if ev.Usage != nil {
			entry.Usage = *ev.Usage
			extras = map[string]any{"cost_usd": costUsd(bs.provider, ev.Model, *ev.Usage)}
		}
		if bs.metrics != nil {
			_ = bs.metrics.Record(entry)
		}
	case *events.ToolResponse:
		bs.lastActive.Store(time.Now().UnixMilli()) // 有产出 → 更新最近活跃
		if strings.HasPrefix(ev.Name, "todo_") {
			extras = map[string]any{"todos": bs.s.TodoItems()}
		}
		if strings.HasPrefix(ev.Name, "Cron") {
			bs.out.writeLine(map[string]any{"event_type": "cron_changed", "workspace": bs.workspace, "session_id": bs.id})
		}
	case *events.AgentEnd:
		// 注意：快照归档（finishCronRun 内 ExtractMessages 读事件日志）必须在本事件
		// 写入日志之后执行，否则 agent_end 尚未落盘，最后一条助手回复会丢失。
		// 因此这里只记录状态，归档推迟到 logEventLine 之后（见 emit 尾部）。
		bs.cronEndStatus = cron.StatusSuccess
		bs.cronEndErr = ""
		if ev.FinishReason == core.FinishReasonError || ev.Error != "" {
			bs.cronEndStatus = cron.StatusError
			bs.cronEndErr = ev.Error
		}
		bs.cronEndContent = ev.Content
	case *events.ToolApprovalRequested:
		// RunId 已由 SDK 携带（事件自包含）；再补审批预演 diff（文件变更预览）
		extras = bs.approvalExtras(ev)
	case *events.CompressEnd:
		// 压缩调用用量/成本计入跨会话指标（llm_end 不覆盖压缩轮；压缩成本此前在
		// 指标层缺失）。仅记录成功压缩（Usage 非 nil；失败降级无产出不计费）。
		if ev.Usage != nil && ev.Usage.Cost.Total > 0 && bs.metrics != nil {
			_ = bs.metrics.Record(MetricsEntry{
				Timestamp: time.Now(), Workspace: bs.workspace,
				SessionID: bs.id, Provider: bs.provider, Model: ev.Model, Usage: *ev.Usage,
			})
		}
		extras = map[string]any{"todos": bs.s.TodoItems()}
	}
	if len(extras) > 0 {
		// 直接构造 jsonKeyVal（避免 marshal→unmarshal 中间 map）
		exKVs := make([]jsonKeyVal, 0, len(extras))
		for k, v := range extras {
			vb, err := json.Marshal(v)
			if err != nil {
				continue
			}
			exKVs = append(exKVs, jsonKeyVal{Key: k, Value: vb})
		}
		if injected, err = injectJSONKeys(injected, exKVs); err != nil {
			return
		}
	}

	// 写 stdout 与事件日志用同一行（事件自包含，含注入的 session_id/workspace/run_id/diff/todos）
	bs.out.writeRaw(injected)
	// 流式增量只走 stdout（前端实时渲染），不落盘：可重建（llm_end 携带最终文本），
	// 实测占事件日志体积 78.5%。落盘只保留时间线（run_id/timestamp/工具起止/usage）。
	if !isStreamingChunkType(e.Type()) {
		bs.logEventLine(injected)
	}
	// AgentEnd 快照归档推迟到这里：事件已落盘，ExtractMessages 能读到完整对话
	//（含本条 agent_end 的最终回复）。只对定时任务派生会话生效（cronLedger 非空）。
	if e.Type() == events.AgentEndType && bs.cronLedger != nil && bs.cronRun != "" {
		bs.finishCronRun(bs.cronEndStatus, bs.cronEndErr, bs.cronEndContent)
	}
}

// approvalWindow 审批等待窗口（与提问窗口 events.MaxQuestionTimeout 同值：最长 30 分钟）。
// 单一事实来源：既用于 session.WithApprovalTimeout，也作为 timeout_ms 注入审批事件（UI 倒计时口径）。
const approvalWindow = 30 * time.Minute

// mcpInstructionsTimeout 装配期取 MCP server instructions 的等待预算（构建系统提示词时）。
// 取短值：它只影响"这一轮能不能拿到引导文本"，而 MCP 服务器可能在冷启动（首次 spawn），
// 不该为了它阻塞首字延迟；没赶上的服务器下次 loop 重建时补上。
const mcpInstructionsTimeout = 2 * time.Second

func (bs *bridgeSession) approvalExtras(e *events.ToolApprovalRequested) map[string]any {
	// timeout_ms：UI（审批面板）据此显示「剩余决策时间」倒计时，与后端等待窗口同源。
	extras := map[string]any{"timeout_ms": approvalWindow.Milliseconds()}
	if e.Name != "write_file" && e.Name != "edit_file" {
		return extras
	}
	var a struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if json.Unmarshal([]byte(e.Arguments), &a) != nil || a.Path == "" {
		return extras // 参数不可解析：不预演 diff，倒计时照旧
	}
	oldBytes, _ := os.ReadFile(filepath.Join(bs.workspace, a.Path))
	oldStr := string(oldBytes)
	newStr := a.Content
	if e.Name == "edit_file" {
		if a.OldString == "" {
			return extras
		}
		newStr = strings.Replace(oldStr, a.OldString, a.NewString, 1)
	}
	added, removed, unified, truncated := unifiedDiff(a.Path, oldStr, newStr)
	extras["diff"] = map[string]any{
		"path":      a.Path,
		"added":     added,
		"removed":   removed,
		"unified":   unified,
		"truncated": truncated,
	}
	return extras
}

const maxPreviewBytes = 1 << 20

func (bs *bridgeSession) handler() events.EventHandler {
	return func(_ context.Context, e events.Event) { bs.emit(e) }
}

func (bs *bridgeSession) runLoop() {
	go func() {
		for {
			select {
			case <-bs.ctx.Done():
				return
			case <-bs.wake:
			}
			if err := bs.s.Run(bs.ctx, bs.handler()); err != nil {
				if errors.Is(err, session.ErrSessionClosed) || bs.ctx.Err() != nil {
					return
				}
				// 运行异常：SDK 已发 SessionRunError；继续等待新输入（可重试）
			}
		}
	}()
}

// manager：进程内多 workspace 注册表（一个进程承载所有工作区的会话）
type manager struct {
	mu          sync.Mutex
	workspaces  map[string]*workspaceRuntime // key = workspace 绝对路径
	provCfg     *providerConfig              // 全局 provider 配置（热切换；set_provider 更新）
	out         *out
	metrics     *FileMetricsStore // 全局指标（跨 workspace，~/.go-code/metrics.jsonl）
	seq         int
	embed       embedBridge // M2：嵌入式浏览器 CDP 转发器（跨工作区共享单个内嵌视图）
	cron        *cron.Store // 全局定时任务库（跨会话；~/.go-code/cron.json + 运行账本）
	cronCtx     context.Context
	cronCancel  context.CancelFunc
	oauthWaiter oauthPromptWaiter // OAuth 登录 manual 输入等待表（oauth_prompt_answer 命令回填）
	// imPlugin 全局 IM 集成插件（单例：连接/路由/安全跨 workspace 唯一；
	// 每个 workspace 的 registry 共享同一实例，plugin_list 均可见）。
	imPlugin *implugin.Plugin
	// imBridge IM 事件桥（插件 Enable 后装配：事件流→IM 推送；IM 消息→会话驱动）。
	imBridge *imBridge
	// lastModel 最近一次 switch_model 的模型（IM 发起会话的默认模型来源；
	// 与前端 globalPrefs「最近一次选择」语义一致——IM 会话应复用桌面端当前选择）。
	lastModel string
	// hub Session Mesh Hub（本机会话注册表 + 远程多节点寻址；settings mesh 段开启后注册工具）。
	hub *session.Hub
	// meshGW Session Mesh 远程网关（remote_enabled 时启动；nil = 未开启）。启停见 applyMeshSettings。
	meshGW *meshremote.Gateway
	// meshWired Hub 宿主侧回调是否已接线（唤醒/标题；进程内一次）。
	meshWired bool
	// lastMeshCfg 最近一次成功启动网关用的 remote 配置（meshCfgEquals 对比用）。
	lastMeshCfg meshremote.Settings
	// titleGenMu 保护 titleInFlight（AI 标题生成去重：同一会话只允许一个在途任务）。
	titleGenMu    sync.Mutex
	titleInFlight map[string]bool // sid → 生成中（防并发首次输入重复触发）
	// browserEngine 浏览器引擎开关（mcp|ego；settings.json 顶层 browser_engine，
	// 独立 browserEngineMu 保护——见 browser_engine.go）。启动/热读刷新。
	browserEngine BrowserEngine
	// pyRT 受管 Python 运行时（run_python 的固定解释器来源；懒创建——New 只解析配置
	// 不做 I/O，建 venv 是首次调用时 Ensure 的职责）。全局单例：运行时根在 home
	// （~/.go-code/runtime/python），与工作区无关；Ensure 自身按 Root 串行 + flock 去重。
	pyRT *goruntime.Runtime
	// xlsxPV 工作簿预览转换器（xlsx_preview 命令；与 run_python 共用 pyRT）。
	// 全局单例（同 pyRT 的理由：运行时不按工作区隔离），懒创建 —— 新建只是构造一个
	// 结构体（无 I/O），真正的重活在首次 Convert 时才发生（Ensure + 跑 openpyxl）。
	xlsxPV *builtin.XlsxPreviewer
	// docxPV / pptxPV 文档与演示文稿预览转换器（docx_preview / pptx_preview 命令）。
	// 单例与懒创建的理由同上；三条通路共用同一个 pyRT 与同一套沙箱工厂，只是跑不同的
	// 固定脚本（mammoth / python-pptx）。
	docxPV *builtin.DocxPreviewer
	pptxPV *builtin.PptxPreviewer
	// hookTrust 进程内共享的 hooks 信任库（~/.go-code/hooks-state.json；懒读一次，
	// 见 hookTrustStore）。**必须共享同一实例**：信任判定发生在每次派发时，面板的
	// hooks_trust 写的就是它 —— 否则新信任要重建会话才生效（契约 §5.5 要求即刻生效）。
	hookTrustMu sync.Mutex
	hookTrust   *hooks.TrustStore
}

// xlsxPreviewer 取（或懒创建）工作簿预览转换器单例。
// pyRT 为空（SDK 层未装配运行时）时仍可构造 —— Convert 会给出「运行时不可用」的明确
// 错误（bridge 侧在调用前已判定并回错误，这里只是兜底，不 panic）。
func (m *manager) xlsxPreviewer(pyRT *goruntime.Runtime) *builtin.XlsxPreviewer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.xlsxPV == nil {
		var py builtin.PythonRuntime
		if pyRT != nil {
			py = pyRT
		}
		// 沙箱按工作区构造（Seatbelt 的读写根在构造时注入，而本转换器是进程级单例）。
		m.xlsxPV = builtin.NewXlsxPreviewer(py, m.xlsxPreviewSandbox)
	}
	return m.xlsxPV
}

// docxPreviewer 取（或懒创建）文档预览转换器单例（理由同 xlsxPreviewer）。
func (m *manager) docxPreviewer(pyRT *goruntime.Runtime) *builtin.DocxPreviewer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.docxPV == nil {
		var py builtin.PythonRuntime
		if pyRT != nil {
			py = pyRT
		}
		m.docxPV = builtin.NewDocxPreviewer(py, m.officePreviewSandbox)
	}
	return m.docxPV
}

// pptxPreviewer 取（或懒创建）演示文稿预览转换器单例（理由同 xlsxPreviewer）。
func (m *manager) pptxPreviewer(pyRT *goruntime.Runtime) *builtin.PptxPreviewer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pptxPV == nil {
		var py builtin.PythonRuntime
		if pyRT != nil {
			py = pyRT
		}
		m.pptxPV = builtin.NewPptxPreviewer(py, m.officePreviewSandbox)
	}
	return m.pptxPV
}

// workspaceRuntime：单个工作区的会话集合 + 持久化目录 + 技能注册表 + 自定义 subagent 注册表 + MCP 管理器（按工作区隔离）
type workspaceRuntime struct {
	path       string
	storeDir   string
	eventsDir  string
	skills     *skills.Registry             // 分层技能注册表（全局 .agents/.go-code + 工作区 .agents/.go-code，工作区优先）
	subagents  *subagent.DefinitionRegistry // 自定义 subagent 定义注册表（全局 ~/.go-code/agents + 工作区 agents，工作区优先）
	mcp        *mcp.Manager                 // MCP 服务器管理器（每工作区共享连接；配置分层：用户 settings.json + 项目 .mcp.json/.go-code/settings.json）
	mcpCfg     *mcp.Loader                  // 分层加载器（每层 last-good：坏文件沿用上次有效，见 mcp/loader.go）
	mcpLayers  []mcp.LayerState             // 最近一次分层加载状态（mcp_list 展示：stale/err/skipped）
	mcpApplied map[string]mcp.ServerConfig  // 已应用的合并配置（watcher 变化检测：相同则零副作用）
	mcpWatch   *mcpWatcher                  // 配置文件监听（fsnotify；delete_workspace/shutdown 停止）
	plugins    *plugin.Registry             // 插件注册表（每工作区：Browser Use 私有连接隔离；组件目录全局共享）
	// pluginsAuto 本次进程内是否已做过「自动恢复启用」判定（每工作区一次，避免每次会话创建重复尝试）。
	pluginsAuto bool
	mu          sync.Mutex
	sessions    map[string]*bridgeSession
}

func (m *manager) get(wsPath, id string) *bridgeSession {
	ws := m.runtime(wsPath)
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.sessions[id]
}

// interruptAllIn 中断指定工作区全部会话（Session.Interrupt 幂等：无运行中 run 时无事发生）。
// 返回「当前确有运行中 run」的会话数（无运行 = 0，前端据此判断是否为纯兜底命中）。
// 用途：interrupt 命令路由失配（session_id/workspace 对不上）时的兜底，杜绝静默 no-op。
func (m *manager) interruptAllIn(wsPath string) int {
	ws := m.runtime(wsPath)
	ws.mu.Lock()
	sessions := make([]*bridgeSession, 0, len(ws.sessions))
	for _, bs := range ws.sessions {
		if bs.isCron {
			continue // 定时任务派生会话不受用户「中断全部」影响
		}
		sessions = append(sessions, bs)
	}
	ws.mu.Unlock()
	n := 0
	for _, bs := range sessions {
		if bs.s.IsRunning() {
			bs.s.Interrupt()
			n++
		}
	}
	return n
}

// workspaceList 当前全部已创建 workspace 运行态（快照切片；引擎级命令遍历用）。
func (m *manager) workspaceList() []*workspaceRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		out = append(out, ws)
	}
	return out
}

// pythonRuntime 取受管 Python 运行时单例（run_python 的解释器来源）。
//
// 为什么放 manager 而不是每次 newSessionEngine 现造：运行时根在 home（与工作区无关）、
// Ensure 自身幂等（进程内按 Root 串行 + 跨进程 flock），但**每次会话重建都 new 一个
// Runtime 会让「同一进程内共享 lock 表」的意图落空**，也没必要——Runtime 只含配置，无状态。
//
// 放 manager 而非 workspaceRuntime：运行时不按工作区隔离（~/.go-code/runtime/python 是
// home 级单例，所有工作区共用一份 venv，与 skills/subagents 的按工作区分层不同）。
//
// 注入方式：manager 字段（懒创建），经 buildLoop → newSessionEngine 的装配链传入。
// 不新加 newSessionEngine 参数的原因是它已有 18 个参数、10 处调用点（含 6 个测试文件），
// 加参会让每个调用点都要造一个假运行时；改由 m.buildLoop（唯一的会话装配入口）就地取。
func (m *manager) pythonRuntime() *goruntime.Runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pyRT == nil {
		m.pyRT = goruntime.New(goruntime.Config{}) // 零值 = 生产默认（Root/超时/解释器探测）
	}
	return m.pyRT
}

// needsExecSandbox 本会话是否需要执行通道的沙箱后端：有 bash（code）**或**有 run_python
// （受管运行时已装配）即需要 —— 两个通道共用同一个 sandbox.Sandbox 实例，谁也不比谁安全。
//
// 为什么单独成函数：这是「work persona 会跑脚本」这一事实的接线点。2026-09 前的装配只按
// withBash 判定，work 下 run_python 会静默落到 NoSandbox（工具描述承诺的文件边界落空），
// 且该失配不会报错、只在真机上表现为「沙箱没拦住工作区外写入」。做成纯函数便于单测钉住。
func needsExecSandbox(profile personaProfile, pyRT *goruntime.Runtime) bool {
	return profile.withBash || pyRT != nil
}

// execSandbox 构造本会话执行通道的沙箱后端（bash 与 run_python 共用；nil = 降级 NoSandbox）。
//
// 形态：settings 沙箱模式 = seatbelt 且工具链存在（darwin）→ Seatbelt + WithRelease 放行门
// + 运行期后端切换；构造失败（缺 sandbox-exec / 策略探测 SIGABRT）→ 降级 NoSandbox +
// sandbox_status 事件（命令不中断）。放行分派（2026-08-28）：沙箱拦截的确认决策统一走
// 工具层审批通道（sandboxReleaseGate → ToolApprovalRequested + approve），沙箱层不弹独立框。
//
// 运行期切换（每次 Run 重选）：full-access/all-pass 会话直通 —— 不进沙箱、无拦截、无标注；
// 其余模式（auto/accept-edits/manual/normal）走 Seatbelt，拦截经放行门 → 工具层审批通道。
func (m *manager) execSandbox(workspace, sid string, profile personaProfile, gate sandbox.ReleaseGate, pyRT *goruntime.Runtime) sandbox.Sandbox {
	if !needsExecSandbox(profile, pyRT) {
		return nil
	}
	// TEMP-STARTUP A7：loadSandboxSettings
	sbStart := time.Now()
	ss := loadSandboxSettings()
	startupLog("A7.loadSandboxSettings", sbStart)
	if ss.Mode != "seatbelt" || !sandbox.Available() {
		return nil
	}
	var opts []sandbox.SeatbeltOption
	if len(ss.SensitivePaths) > 0 {
		opts = append(opts, sandbox.WithSensitivePaths(ss.SensitivePaths))
	}
	// 用户级包安装放行（pip/npm/go/cargo）：home 可写；敏感路径仍 deny 读写兜底
	if ss.WriteHome == nil || *ss.WriteHome {
		opts = append(opts, sandbox.WithHomeWrite())
	}
	// TEMP-STARTUP A7：NewSeatbelt（含 /usr/bin/true 子进程探测）
	seatStart := time.Now()
	sb, err := sandbox.NewSeatbelt(workspace, opts...)
	if err != nil {
		startupLog("A7.sandbox.NewSeatbelt(失败)", seatStart)
		m.out.writeLine(map[string]any{"event_type": "sandbox_status", "status": "degraded", "error": err.Error(), "workspace": workspace})
		return nil
	}
	startupLog("A7.sandbox.NewSeatbelt", seatStart)
	strict := sandbox.WithRelease(sb, gate)
	return sandbox.WithBackendSelector(func() sandbox.Sandbox {
		if bs := m.get(workspace, sid); bs != nil && bs.mode != nil {
			switch modeName(bs.mode) {
			case "full-access", "all-pass":
				return sandbox.NoSandbox{}
			}
		}
		return strict
	})
}

// xlsxPreviewSandbox 工作簿预览转换用的沙箱后端（xlsx_preview 命令）。
//
// 与 execSandbox 的差别是**故意的**，两条边界必须分开看：
//   - execSandbox 是「会话级执行通道」（bash/run_python）：随会话权限模式在
//     沙箱/直通之间切换，且带 WithRelease 放行门（拦住了就弹确认框）。
//   - 本函数是「命令级只读转换」：进程是**我们自己的固定脚本**（无模型/用户输入），
//     且输入文件在转换前已由 bridge 侧校验为工作区内或绝对路径（~/.go-code 拒绝），
//     故不受会话权限模式影响、也没有放行门 —— 但**仍然进沙箱**：脚本执行的毕竟是
//     第三方解析库（openpyxl + lxml 读用户文件），文件边界照旧由内核兜底，
//     不该因为「是我们的脚本」就把整条命令放出去。
//
// 代价：一条命令一个 Seatbelt 实例（构造含 /usr/bin/true 探测，实测毫秒级）；
// 预视图是用户点击触发的低频动作，不值得为它维护沙箱池。
//
// 失败降级：sandbox-exec 不可用/策略探测失败 → nil（= NoSandbox 直通）——
// 与 execSandbox 的降级语义一致，预览不能因为沙箱不可用就完全打不开。
func (m *manager) xlsxPreviewSandbox(workspace string) sandbox.Sandbox {
	return m.readOnlyConvertSandbox(workspace, "xlsx_preview")
}

// officePreviewSandbox 文档/演示文稿预览转换用的沙箱后端（docx_preview / pptx_preview）。
//
// 为什么与 xlsxPreviewSandbox 是两个函数而不是合一个（两者现在都指向同一条边界）：
// 前者的 scope 会进 sandbox_status 事件（用于排查「是哪条通路降级了」），而 scope 是
// 事件里唯一能区分格式的信息 —— 合成一个会让三条通路在诊断输出里长得一模一样。
// 边界本身共用一条（readOnlyConvertSandbox）：三者性质完全相同，见其注释。
func (m *manager) officePreviewSandbox(workspace string) sandbox.Sandbox {
	return m.readOnlyConvertSandbox(workspace, "office_preview")
}

// readOnlyConvertSandbox 「命令级只读转换」共用的沙箱后端（xlsx_preview 与 @引用
// PDF 文本抽取）。scope 只用于 sandbox_status 事件标注，便于区分是哪条通路降级的。
//
// 为什么两者共用一条边界：它们的性质完全相同 —— 进程是我们自己的固定脚本（无模型/
// 用户输入）、输入是用户明确的单个文件、需要读工作区外的位置（预览/引用的文件常在
// ~/Desktop、~/Downloads）。会话级执行通道（execSandbox）那条边界不适用：它随权限
// 模式切换、带放行门，而这两条通路与模式无关（不体现在对话里、用户点一下就要看到结果）。
func (m *manager) readOnlyConvertSandbox(workspace, scope string) sandbox.Sandbox {
	ss := loadSandboxSettings()
	if ss.Mode != "seatbelt" || !sandbox.Available() {
		return nil
	}
	var opts []sandbox.SeatbeltOption
	if len(ss.SensitivePaths) > 0 {
		opts = append(opts, sandbox.WithSensitivePaths(ss.SensitivePaths))
	}
	// 故意不继承 WithHomeWrite（execSandbox 为 pip/npm 而开 home 可写）：这两条通路只读
	// 输入文件 + 写自己的临时目录，没有「写 home」的需要。不开这一项，即使脚本出事也
	// 改不了 home 下任何东西 —— 比会话执行通道更紧的边界，这才配得上「只读转换」。
	sb, err := sandbox.NewSeatbelt(workspace, opts...)
	if err != nil {
		m.out.writeLine(map[string]any{"event_type": "sandbox_status", "status": "degraded", "error": err.Error(), "workspace": workspace, "scope": scope})
		return nil
	}
	return sb
}

// runtime 取（或懒创建）workspace 运行态：storeDir 与会话均按工作区隔离
func (m *manager) runtime(path string) *workspaceRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ws, ok := m.workspaces[path]; ok {
		return ws
	}
	// 办公技能随包分发同步（2026-09）：内置技能集合（dev = desktop/vendor/office-skills；
	// 打包 = Resources/office-skills）下的每个技能子目录 → ~/.agents/skills/<name>
	//（不存在才复制，逐技能判断：新增技能目录能到达已装用户，改既有技能内容则到不了）。
	// 此前这些技能只活在工作区 .agents/skills——换 workspace
	// 就看不到、打包也不分发；同步到全局层后跨工作区可用。
	// ⚠ 位置必须在下方的 buildSkillsRegistry **之前**：注册表只扫一次目录，若先建表
	// 再同步，首次启动技能已落盘却不在清单里（E2E 实测：registry skills = []），
	// 用户得手动 /reload_skills 才能 load_skill。失败不阻断启动。
	officeSyncStart := time.Now()
	if err := ensureOfficeSkillsSynced(); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 办公技能同步失败: %v\n", err)
	}
	startupLog("A5.officeSkillsSynced", officeSyncStart)
	// ego-browser skill 随包分发同步（2026-09）：内置 skill（Resources/ego-browser-skill）
	// → ~/.agents/skills/ego-browser（不存在才复制）。保证未装 ego lite 的机器上
	// 打包版也能在技能面板看到 ego-browser（对齐 browser/computer 的 vendor 同步）。
	// ⚠ 与上方 office 同步同理，必须在下方的 buildSkillsRegistry **之前**：注册表只扫
	// 一次目录，晚于建表则首次启动技能已落盘却不在清单里（E2E 实测：registry skills = []），
	// 而未装 ego lite 的机器正是唯一要走这条分发的场景。
	egoSyncStart := time.Now()
	if err := ensureEgoSkillSynced(); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ ego-browser skill 同步失败: %v\n", err)
	}
	startupLog("A5.egoSkillSynced", egoSyncStart)
	// TEMP-STARTUP A5：runtime 懒初始化分段打点
	skillsStart := time.Now()
	skillsReg := buildSkillsRegistry(path)
	startupLog("A5.buildSkillsRegistry", skillsStart)
	subagentsStart := time.Now()
	subagentsReg := buildSubagentRegistry(path)
	startupLog("A5.buildSubagentRegistry", subagentsStart)
	mcpStart := time.Now()
	// MCP 配置分层（低 → 高）：用户 ~/.go-code/settings.json < 项目 {ws}/.mcp.json
	// < 项目 {ws}/.go-code/settings.json —— 详见 mcpConfigLayers。
	// Loader（有状态）：坏 JSON / 半写 / 读失败沿用上次有效内容（mcp/loader.go），恢复后切回。
	mcpLoader := &mcp.Loader{}
	mcpRes := mcpLoader.Load(mcpConfigLayers(path)...)
	mcpm := mcp.NewManagerWithSource(mcpRes.Config, mcpRes.Source)
	startupLog("A5.mcp.NewManager", mcpStart)
	plgStart := time.Now()
	plg := buildPlugins(m.imPlugin) // 插件注册表（内建 Browser Use + 全局 IM 单例）
	startupLog("A5.buildPlugins", plgStart)
	// 启动预置内置组件（2026-09）：Browser Use 组件随应用分发（vendor），启动即从
	// 资源复制到 ~/.go-code/plugins/browser/ —— 用户无需「安装」，只需 node 环境
	//（启用时检测）。失败不阻断启动：插件 State 报 error，UI 可见原因。
	browserSyncStart := time.Now()
	if cap, ok := plg.Get(browser.ID); ok {
		if syncer, ok2 := cap.(interface{ EnsureSynced() error }); ok2 {
			if err := syncer.EnsureSynced(); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ browser 内置组件同步失败（UI 插件卡可见原因）: %v\n", err)
			}
		}
	}
	startupLog("A5.browserEnsureSynced", browserSyncStart)
	// Computer Use helper 同步（2026-09 补分发缺口）：dev/打包都从源目录复制
	// computer-helper 到 ~/.go-code/plugins/computer/。此前 helper 只在仓库相对路径
	// 命中——应用打包/重启后 cwd 变化 → 「computer-helper 不存在」。失败不阻断启动
	//（State 报 error，UI 可见原因 + 构建引导）。
	cuSyncStart := time.Now()
	if cap, ok := plg.Get(computerplugin.ID); ok {
		if syncer, ok2 := cap.(interface{ EnsureSynced() error }); ok2 {
			if err := syncer.EnsureSynced(); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ computer helper 同步失败（UI 插件卡可见原因）: %v\n", err)
			}
		}
	}
	startupLog("A5.computerEnsureSynced", cuSyncStart)
	ws := &workspaceRuntime{
		path:       path,
		storeDir:   wsSessionsDir(path), // APP 级：~/.go-code/sessions/<wsKey>（跨 workspace，工作区下只留 skills）
		eventsDir:  wsEventsDir(path),   // APP 级：~/.go-code/events/<wsKey>
		skills:     skillsReg,
		subagents:  subagentsReg,
		mcp:        mcpm,
		mcpCfg:     mcpLoader,     // 分层加载器（含每层 last-good 快照）
		mcpLayers:  mcpRes.Layers, // 逐层状态（mcp_list 展示 stale/err）
		mcpApplied: mcpRes.Config, // 已应用配置（watcher 变化检测基准）
		plugins:    plg,
		sessions:   map[string]*bridgeSession{},
	}
	mkStart := time.Now()
	_ = os.MkdirAll(ws.storeDir, 0o700) // 会话/事件目录含敏感内容，0700
	_ = os.MkdirAll(ws.eventsDir, 0o700)
	startupLog("A5.MkdirAll", mkStart)
	m.workspaces[path] = ws
	// 配置监听（fsnotify）：登记进 m.workspaces 之后再启动——watcher 的 apply 回调会经
	// m.runtime(path) 取运行态，早于登记会新建一份（重复运行态）。
	m.startMCPWatch(ws)
	return ws
}

func (m *manager) create(wsPath, id, effort string) (*bridgeSession, error) {
	return m.createOpts(wsPath, id, effort, sessionOpts{})
}

// sessionOpts 会话创建附加选项（定时任务派生会话用：事件/会话存储独立目录）。
type sessionOpts struct {
	storeDir  string // 非空 = 会话存储独立目录（cron_sessions/<wsKey>/<jobID>）
	eventsDir string // 非空 = 事件日志独立目录（同上；不进工作区 events）
	cronEvDir string // 同 eventsDir（cron 语义别名，供快照归档用）
	wsKey     string // workspaceKey（快照路径维度）
	noRecord  bool   // true = 不装配 checkpoint record（触发即跑、跑完归档，无需崩溃恢复）
}

func (m *manager) createOpts(wsPath, id, effort string, opts sessionOpts) (*bridgeSession, error) {
	if effort == "" {
		effort = string(provider.ReasoningEffortLevelLow)
	}
	ws := m.runtime(wsPath)
	// 会话首次创建前自动恢复「上次已启用」的插件（持久化标记；本进程每工作区一次）。
	// 未安装/启用失败仅记日志，不阻断会话创建。
	// TEMP-STARTUP A6：插件启用（同步，头号嫌疑——Enable ≤15s + embed 2s）
	plgStart := time.Now()
	m.autoEnablePlugins(ws)
	startupLog("A6.autoEnablePlugins", plgStart)
	// 会话已在进程内（切走再切回 / 后台仍活跃）→ 复用，不重建：避免丢内存态与 goroutine 泄漏
	ws.mu.Lock()
	if existing, ok := ws.sessions[id]; ok {
		ws.mu.Unlock()
		return existing, nil
	}
	ws.mu.Unlock()

	storeDir := ws.storeDir
	eventsDir := ws.eventsDir
	if opts.storeDir != "" {
		storeDir = opts.storeDir
	}
	if opts.eventsDir != "" {
		eventsDir = opts.eventsDir
	}
	_ = os.MkdirAll(storeDir, 0o700) // 会话/事件目录含敏感内容，0700
	_ = os.MkdirAll(eventsDir, 0o700)

	// 全新会话（无磁盘持久化）→ 落 lastActive=now（updated_at 排序依据）。
	// 冷恢复会话不刷新：排序回退 events/sessions 文件 mtime（真实最后活跃时间）。
	_, statErr := os.Stat(filepath.Join(storeDir, id+".jsonl"))
	isNewSession := os.IsNotExist(statErr)

	storeStart := time.Now()
	store, err := session.NewFileStore(storeDir)
	if err != nil {
		return nil, fmt.Errorf("file store: %w", err)
	}
	startupLog("A6.NewFileStore", storeStart)

	// checkpoint 装配（必须先于 NewSession：record.sdk_state 是 SDK 恢复源，
	// view_checkpoint 用于 hydrate reducer）。磁盘有合法 record → 冷恢复；
	// 无 record = 新会话/旧数据 → 空 reducer + legacy store 恢复。
	// 定时任务派生会话（opts.noRecord）跳过装配：触发即跑、跑完归档，无需崩溃恢复。
	recStart := time.Now()
	wsKey := workspaceKey(wsPath)
	if opts.wsKey != "" {
		wsKey = opts.wsKey
	}
	reducer := NewViewReducer()
	var recoveryState *session.State // 冷恢复时从 record.sdk_state 注入 SDK 恢复源
	var recordStore *session.FileStorage
	var recordSetupErr error
	if !opts.noRecord {
		if recordStore, recordSetupErr = newRecordStore(); recordSetupErr == nil {
			if rec, lerr := recordStore.LoadRecord(context.Background(), wsKey, id); lerr == nil {
				if cp, perr := parseCheckpoint(rec.ViewCheckpoint); perr == nil {
					if err := reducer.Restore(cp); err == nil {
						reducer.NormalizeForRestore() // 重启后 running → 终态（done/abandoned）
					}
				}
				// checkpoint-first SDK 恢复：record.sdk_state 作为权威恢复源
				//（含 pending 积压；session.NewSession 的 WithRecoveryState 分支使用）
				stateCopy := rec.SDKState
				recoveryState = &stateCopy
			}
		}
	}
	startupLog("A6.record恢复", recStart)

	// 后台任务注册表 + 会话级 ctx 闭包：agent_* 工具与 Session 共享同一实例
	// （bgCtx 闭包引用 s，NewSession 赋值前不调用——工具运行时 s 已就绪）。
	reg := subagent.NewRegistry()
	// 并发槽上限（settings.json agent 段，缺省 subagent.DefaultConcurrency = 1000）：
	// 主池给 agent_spawn（写型子 agent），辅池给只读的 subagent_explore（分池 = 两者互不饿死）。
	// 2026-09-20 加固：此前硬编码 4，槽满时 agent_spawn 在工具调用栈上同步阻塞
	// （ToolTimeout=-1 + 批超时豁免 + 自动摘离默认关 → 任何超时都解不开），主 Agent 整批挂死。
	ag := loadAgentSettings()
	reg.SetConcurrency(ag.maxSubagents())
	reg.SetAuxConcurrency(ag.maxExploreSubagents())
	// 后台子 agent 任务执行上限：默认 12h（subagent.defaultAsyncTaskTimeout）。
	// 长时运行任务需要更大空间时在此放大——reg.SetAsyncTimeout(d)；
	// 超时按「时限到点」上报（非普通失败），并保留已完成轮次的 journal 供 TaskOutput 复查。
	// （2026-09-18：上限原硬编码 1h，仍在推进的长任务会被砍，与长时运行定位矛盾。）
	// ask_user 提问等待器：桌面端 UI 强制「必须全部作答才能继续」（面板按钮禁用直到全答）。
	// 等待窗口 = 上限 30 分钟（events.MaxQuestionTimeout）——UI 显示「剩余决策时间」倒计时，
	// 到点工具结果**明确告知模型「用户未作答」**，模型按最佳判断继续（不再无限期挂住本轮）。
	// （2026-08-17 曾置 -1 无限制；2026-09-20 用户拍板：延长到 30 分钟上限 + 倒计时 + 超时告知。
	//  恢复「无限等待」= 这一行改回 events.NewQuestionWaiter(-1)。）
	qw := events.NewQuestionWaiter(events.MaxQuestionTimeout)
	var bs *bridgeSession // WithEventHandler 闭包引用（emit 依赖 bs.s，Run 期间已赋值）
	var s *session.Session
	sk := m.runtime(wsPath).skills // 工作区技能注册表（WithSkills + load_skill 共享）
	loopStart := time.Now()
	loop, effortRef := m.buildLoop(wsPath, id, m.provCfg.providerName(), m.preferredModel(), effort,
		func() context.Context { return s.Context() }, reg, qw, sk, profiles["code"], &sandboxReleaseGate{m: m, ws: wsPath, id: id}) // 默认 Persona = code
	startupLog("A6.buildLoop", loopStart)
	nsStart := time.Now()
	// AI 标题恢复：冷启动从会话 jsonl meta 尾扫（进程内 titles store 未命中时兜底；
	// 与 titles.json 双写保持同步——jsonl 是权威源，重启后 titles.json 丢失也可恢复）
	restoredTitle := ""
	if mr, rerr := store.ReadMeta(context.Background(), id); rerr == nil && mr != nil {
		restoredTitle = mr.Title
	}
	// hooks 能力（外部命令 hook，2026-09-20）：装配配置 + 信任库 + 派发器。
	// 无配置 / disableAllHooks → nil → session.WithHooks(nil) → 行为与接线前完全一致
	// （内核所有扩展钩子为 nil）。设计见 docs/graft-integration/05-hooks-能力设计规范.md。
	// 2026-09-21：抽成 hooksOption —— 与 hooks_set 的热替换（reloadWorkspaceHooks）共用
	// 同一条装配链，避免"新建的会话"和"改过配置的会话"拿到不同 hooks。
	s, err = session.NewSession(id, loop, store,
		// 启用 HITL 审批并设置等待窗口：与提问窗口同值（30 分钟上限）。
		// 2026-09-20（用户拍板）：原 60s → 30min —— 审批面板已改成停靠（用户会边翻对话历史/
		// 读 diff 边决定），60 秒根本来不及；窗口同上限，前端据 approvalExtras 的 timeout_ms 显示倒计时。
		session.WithApprovalTimeout(approvalWindow),
		session.WithRegistry(reg),                // 与工具注册共享（异步多 agent）
		session.WithQuestionWaiter(qw),           // 与 ask_user 工具注册共享
		session.WithRecoveryState(recoveryState), // checkpoint-first SDK 恢复源（nil = legacy）
		// hooks：cwd 供外部命令拿项目根（同时导出 GOCODE_PROJECT_DIR，兼容 CLAUDE_PROJECT_DIR）；
		// 派发器为 nil 时不装配（零开销路径）。SessionStart/UserPromptSubmit 由 Session 内部
		// 从同一 ToolHooks 取用，无需在此额外传参。
		session.WithHookCwd(wsPath),
		m.hooksOption(wsPath, id, bs.warnHook),
		session.WithAITitle(restoredTitle), // 恢复 AI 标题（空 = 未生成/新会话）
		session.WithEventHandler(func(_ context.Context, e events.Event) {
			bs.emit(e) // 转发会话级事件（session_closed 等）
			// 摘离工具任务完成 → TaskResultDelivered（toolTaskSink → PushTaskResult 发出）：
			// 唤醒 run loop 消费 task_result 消息（模型下轮续跑）。与 reg.SetOnTaskDone 的
			// wakeRun 同效（后者保留无害——异步子 agent 结果同样走这里）。
			if _, ok := e.(*events.TaskResultDelivered); ok {
				bs.wakeRun()
			}
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	startupLog("A6.NewSession", nsStart)
	ctx, cancel := context.WithCancel(context.Background())
	bs = &bridgeSession{id: id, s: s, loop: loop, effort: effort, subagentEffort: effortRef, model: m.preferredModel(), provider: m.provCfg.providerName(), persona: "code", ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), out: m.out, workspace: wsPath, metrics: m.metrics, eventsDir: eventsDir, questions: qw}
	if opts.cronEvDir != "" {
		bs.cronEvDir = opts.cronEvDir
	}
	// 序列化缓存：session_id/workspace 的 JSON 字节（emit 高频复用）
	bs.sidKV, _ = json.Marshal(id)
	bs.wsKV, _ = json.Marshal(wsPath)

	// checkpoint 装配段：bs 装配 reducer/coord（record 已提前解析）
	if !opts.noRecord && recordStore != nil {
		bs.reducer = reducer
		bs.wsKey = wsKey
		// 测试隔离环境（appDataDirOverride 非空）用同步提交，避免异步 worker
		// 与测试 TempDir 清理竞态；生产保持异步 + 关闭时 drain。
		bs.coord = newCheckpointCoordinator(wsKey, id, recordStore, reducer, s, ctx.Done(), appDataDirOverride != "")
		// 持久化失败可观察（V2 P1-PERSIST-04）：提交失败发结构化事件行（前端可提示
		// "保存失败"），而不是静默吞掉（用户会误以为已保存）。
		bs.coord.onError = func(err error) {
			bs.out.writeLine(map[string]any{
				"event_type": "session_persist_error",
				"session_id": id,
				"workspace":  wsKey,
				"error":      err.Error(),
				"timestamp":  time.Now().UnixMilli(),
			})
		}
		// TEMP-DEBUG: LoadRecord 失败诊断（区分"无 record"与"record 损坏/路径错"）
		if rec, lerr := recordStore.LoadRecord(context.Background(), wsKey, id); lerr != nil {
			fmt.Fprintf(os.Stderr, "[checkpoint-debug] LoadRecord sid=%s wsKey=%s err=%v\n", id, wsKey, lerr)
		} else if rec != nil {
			fmt.Fprintf(os.Stderr, "[checkpoint-debug] record 存在 sid=%s rev=%d\n", id, rec.Revision)
		}
	} else if recordSetupErr != nil {
		// TEMP-DEBUG: record 装配失败诊断
		fmt.Fprintf(os.Stderr, "[checkpoint-debug] newRecordStore 失败: %v\n", recordSetupErr)
	}
	// 默认模式 = auto（全自动：bash/write/edit 不弹审批、沙箱拦截自动放行）——
	// 与前端默认 mode='auto' 对齐，避免「UI 显示 auto、bridge 实为 nil(normal 语义)」脱节
	//（此前导致默认场景仍弹审批/沙箱确认）。switch_mode 由用户切换覆盖。
	bs.mode = parseMode("auto")
	s.SetMode(bs.mode)
	// 后台任务完成 → 自动回传主 Runtime：结果推入主会话（模型下轮可见续跑）+ 发事件（客户端渲染）。
	// 队列内消息按 FIFO 消费；与 interrupt 同时触发时 abort 立即生效（对运行中 ctx 直接作用），
	// 推送只入队、无竞争——无需特殊排序代码。
	reg.SetOnTaskDone(func(taskID, name, result string, status subagent.TaskStatus, usage core.Usage, err error) {
		// usage 随结果一并回传（TaskResultDelivered.Usage）：任务卡片展示 tokens + 成本，
		// 会话级累计另经 Registry.TotalUsage 合成（不重复记账）。
		if perr := s.PushTaskResultUsage(taskID, name, result, string(status), usage, err); perr != nil {
			// inbox 满 / 预算超 / 会话关闭：结果仍在 Registry，宿主 Wait 可补取；仅记日志不中断
			fmt.Fprintln(os.Stderr, "✗ push task result", taskID, perr)
		}
		bs.wakeRun() // 空闲续跑：run loop 已回（队列空）→ 唤醒再跑取走任务结果
	})
	ws.mu.Lock()
	ws.sessions[id] = bs
	ws.mu.Unlock()
	// Session Mesh 注册（本机会话入 Hub 注册表；重复注册幂等忽略——进程内复用路径已挡）
	if m.hub != nil {
		if err := m.hub.Register(s); err != nil {
			// M3：跨工作区同 sid 碰撞（极罕见：genID 纳秒+序号进程内唯一，但磁盘
			// 冷恢复可能复用旧 sid）。不静默——记录诊断，避免 session_send 寻址歧义难查。
			fmt.Fprintf(os.Stderr, "✗ mesh register session %s: %v（同 id 会话已注册，跨工作区寻址可能歧义）\n", id, err)
		} else {
			m.pushMeshSessions() // 新会话注册 → 立即同步对端 session_list（不等 30s 周期）
		}
	}
	// IM 事件桥回填：IM 已启用时新会话直接挂上（emit 全事件漏斗转发用）
	bs.im = m.imBridge
	rlStart := time.Now()
	bs.runLoop()
	startupLog("A6.runLoop", rlStart)
	if isNewSession {
		bs.lastActive.Store(time.Now().UnixMilli())
	}
	return bs, nil
}

func (m *manager) genID() string {
	m.seq++
	return fmt.Sprintf("s-%d-%d", time.Now().UnixNano(), m.seq)
}

// 命令响应（回填 workspace，前端按活跃 workspace 门禁，防跨工作区响应串台）
func (m *manager) resp(c command, ok bool, errMsg string, data map[string]any) {
	m.respCode(c, ok, errMsg, "", data)
}

// respCode 同 resp，但携带稳定错误码（V2 P2-PROTOCOL-05）：前端可据 code 区分
// 认证失败/配置错误/超时/限流/永久错误，而非解析 error 字符串。code 为空 = 兼容旧行为。
// 高频语义错误（认证/超时/配置/限流/not_found）在调用点显式传 code。
func (m *manager) respCode(c command, ok bool, errMsg, code string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	line := map[string]any{"event_type": "command_response", "id": c.Id, "ok": ok, "error": errMsg, "data": data}
	if code != "" {
		line["code"] = code
	}
	if ws := str(c.Payload, "workspace"); ws != "" {
		line["workspace"] = ws
	}
	m.out.writeLine(line)
}

func (m *manager) dispatch(c command) {
	// TEMP-STARTUP A9：命令耗时（仅启动相关命令，避免全量噪音）
	cmdStart := time.Now()
	defer func() {
		switch c.Type {
		case "new_session", "workspace_list", "browser_embed_start", "browser_embed_attach", "plugin_list", "plugin_enable", "session_list":
			startupLog("A9."+c.Type.String(), cmdStart)
		}
	}()
	ws := str(c.Payload, "workspace")
	sid := str(c.Payload, "session_id")
	switch c.Type {
	case "external_skills_status": // 检测 ~/.codex / ~/.claude 外部 Skills 来源
		home, err := os.UserHomeDir()
		if err != nil {
			m.resp(c, false, fmt.Errorf("获取用户主目录: %w", err).Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"status": skills.DetectExternalSkills(home)})

	case "external_skills_import": // 导入外部 Skills 到全局 ~/.agents/skills（主路径）
		home, err := os.UserHomeDir()
		if err != nil {
			m.resp(c, false, fmt.Errorf("获取用户主目录: %w", err).Error(), nil)
			return
		}
		report, err := skills.ImportExternalSkills(home, agentSkillsDir())
		if err != nil {
			m.resp(c, false, err.Error(), map[string]any{"status": report.Status, "imported": report.Imported})
			return
		}
		// 全局目录变化后，所有已存在工作区都要重新扫描，确保当前及后台工作区
		// 的会话 loop 与 Skills UI 立即看到导入结果。
		m.rebuildAllSkills()
		m.resp(c, true, "", map[string]any{"status": report.Status, "imported": report.Imported})

	case "git_snapshot":
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		snap, err := gitSnapshot(context.Background(), ws)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		parseAheadBehind(context.Background(), ws, &snap)
		if snap.Files == nil {
			snap.Files = []gitFile{} // 干净仓库返回空数组而非 null，协议更干净
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "git": snap, "files": snap.Files})
	case "git_file_diff":
		if ws == "" || str(c.Payload, "path") == "" {
			m.resp(c, false, "workspace/path 为空", nil)
			return
		}
		diff, err := gitDiff(context.Background(), ws, str(c.Payload, "path"), c.Payload["staged"] == true)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"path": str(c.Payload, "path"), "diff": diff})

	case "git_history":
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		commits, err := gitLog(context.Background(), ws, intNum(c.Payload, "limit", 30), intNum(c.Payload, "skip", 0))
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		// has_more 粗判：返回条数 == limit 视为可能还有更多（恰好整除时多一次空页点击，可接受）
		limit := intNum(c.Payload, "limit", 30)
		if limit < 1 || limit > 100 {
			limit = 30
		}
		if commits == nil {
			commits = []gitCommit{} // 空历史（如 unborn HEAD）返回空数组而非 null，协议更干净
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "commits": commits, "has_more": len(commits) == limit})
	case "git_stage": // 暂存文件（P1 写操作；路径经 sanitizePaths 限制在 workspace 内）
		n, err := gitStage(context.Background(), ws, strSlice(c.Payload, "paths"))
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "staged": n})
	case "git_unstage": // 取消暂存（无 HEAD 时报错，前端提示）
		n, err := gitUnstage(context.Background(), ws, strSlice(c.Payload, "paths"))
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "unstaged": n})
	case "git_discard": // 丢弃变更（破坏性：前端必须二次确认）
		n, err := gitDiscard(context.Background(), ws, strSlice(c.Payload, "paths"), c.Payload["untracked"] == true)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "discarded": n})
	case "git_commit": // 提交（message 必填 ≤2000 字符；stage_all=true 先 add -A）
		hash, err := gitCommitWorktree(context.Background(), ws, str(c.Payload, "message"), c.Payload["stage_all"] == true)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "hash": hash})
	case "git_branch_list": // 本地分支清单（P2 分支切换器；unborn HEAD 返回空数组）
		branches, err := gitBranchList(context.Background(), ws)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		if branches == nil {
			branches = []gitBranchInfo{}
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "branches": branches})
	case "git_checkout": // 切换本地分支（分支名经校验，防 flag 注入；由前端走 gitWriteOp 统一 busy/重拉）
		name, err := gitCheckoutBranch(context.Background(), ws, str(c.Payload, "branch"))
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "branch": name})
	case "git_init": // 在工作区初始化仓库（已是仓库时报错；非仓库空态引导入口）
		created, err := gitInitRepo(context.Background(), ws)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "created": created})
	case "git_commit_diff":
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		diff, err := gitCommitDiff(context.Background(), ws, str(c.Payload, "hash"))
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"session_id": sid, "hash": str(c.Payload, "hash"), "diff": diff})

	case "list":
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		m.resp(c, true, "", map[string]any{"sessions": m.listSessions(ws)})

	case "metrics": // 全局聚合指标（不要求 session_id；payload: from/to/project 可选过滤）
		// 注意不用 payload.workspace 做过滤：send() 会给所有命令注入活跃 workspace，
		// 用它过滤会把看板锁死到当前项目。过滤用独立 key `project`（前端显式传）。
		q := MetricsQuery{Workspace: str(c.Payload, "project")}
		if f := str(c.Payload, "from"); f != "" {
			if t, err := time.Parse("2006-01-02", f); err == nil {
				q.From = &t
			}
		}
		if t := str(c.Payload, "to"); t != "" {
			if t, err := time.Parse("2006-01-02", t); err == nil {
				q.To = &t
			}
		}
		rep, err := m.metrics.Aggregate(q)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"report": rep})

	case "refresh_prices": // 实时价源补价（不需 session_id；payload: provider?/source?/overwrite?）
		// 内置价表是编译期快照（go:embed），网关新模型/改价不发版就是 0 成本 ——
		// 本命令拉实时价目补「当前无价」的已配置模型（详见 price_refresh.go）。
		m.refreshPrices(c)

	case "trace": // 历史执行链路投影（链路 tab 数据源；见 trace.go）
		// 不带 id → 摘要列表（~150 B/条）；带 id → 该 trace 的完整 span 列表。
		// 数据源 = 事件日志（零新增存储），无日志 → 空列表（前端回退现有渲染）。
		limit := 0
		if v, ok := c.Payload["limit"].(float64); ok {
			limit = int(v)
		}
		// latest=1：列表请求顺带回最新一条 trace 的详情（前端刷新兜底，见 trace.go）
		wantLatest := c.Payload["latest"] == float64(1) || c.Payload["latest"] == true
		m.resp(c, true, "", m.traceData(ws, sid, str(c.Payload, "run_id"), limit, wantLatest))

	case "new_session":
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		id := str(c.Payload, "id")
		if id == "" {
			id = m.genID()
		}
		// 前端显式携带 Provider（2026-09：新建会话归属以 UI 选择的 Provider 为准，
		// 不再依赖全局 active 猜测——多 Provider 同名模型时避免会话发到错误 Provider）。
		newProv := str(c.Payload, "provider")
		if newProv != "" && m.provCfg.providerName() != newProv {
			m.provCfg.setActive(newProv)
		}
		// 区分进程内复用（后台任务仍真实运行）与磁盘冷恢复。客户端快照只能在冷恢复时
		// 收敛残留 running 状态；warm session 必须保留 agent/tool/task 的运行态。
		wasLive := id != "" && m.get(ws, id) != nil
		// restore 协议标识（问题四）：renderer 据此丢弃陈旧响应（迟到的旧 new_session
		// 响应不得覆盖新会话）。缺失（旧 renderer）时前端回退请求 id 校验。
		restoreID := str(c.Payload, "restore_id")
		attempt := int64(0)
		if a, ok := c.Payload["attempt"].(float64); ok {
			attempt = int64(a)
		}
		bs, err := m.create(ws, id, "") // 进程内已存在 → 复用；新从磁盘载入 → 恢复有效上下文
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		respData := map[string]any{
			"session_id": id,
			"sessions":   m.listSessions(ws),
			"todos":      bs.s.TodoItems(), // 恢复会话时带当前待办（宿主 TODO 面板）
			"live":       wasLive,          // true=进程内复用，后台运行态仍有效；false=磁盘冷恢复
		}
		if restoreID != "" {
			respData["restore_id"] = restoreID // 回显：前端按 会话+请求+attempt 校验
		}
		if attempt > 0 {
			respData["attempt"] = attempt
		}
		// checkpoint-first 恢复（目标主路径）：优先用进程内 reducer 快照直接 hydrate，
		// 不再扫描 raw events。冷恢复（wasLive=false）从磁盘 record hydrate reducer；
		// warm（wasLive=true，进程内已有）直接用内存 reducer 快照（秒出，不扫 events）。
		// 仅当 reducer 无内容（新会话/无 record/旧数据）才回退 legacy 分支。
		if bs.coord != nil {
			rec, rerr := bs.coord.Load(context.Background())
			if rerr == nil {
				if _, perr := parseCheckpoint(rec.ViewCheckpoint); perr == nil {
					// 进程内 reducer 已 hydrate（create 时 Restore + NormalizeForRestore）；
					// 响应使用同一归一化快照，保证响应与进程内状态一致。
					if viewRaw, merr := bs.reducer.Marshal(); merr == nil {
						respData["restored"] = "checkpoint"
						respData["checkpoint_version"] = checkpointSchemaVersion
						respData["revision"] = rec.Revision
						respData["reducer_seq"] = rec.ReducerSeq
						respData["clear_generation"] = rec.ClearGeneration
						respData["degraded"] = bs.reducer.IsDegraded()
						respData["view_checkpoint"] = json.RawMessage(viewRaw)
						m.resp(c, true, "", respData)
						return
					}
				}
			} else {
				// 无 record：懒迁移（旧会话首次访问）——读 events 重放生成 checkpoint。
				// 同步完成（首次一次性成本），成功后响应 checkpoint；失败回退 legacy。
				if !wasLive && bs.migrateLegacyToCheckpoint() {
					if rec2, rerr2 := bs.coord.Load(context.Background()); rerr2 == nil {
						if viewRaw, merr := bs.reducer.Marshal(); merr == nil {
							respData["restored"] = "checkpoint"
							respData["checkpoint_version"] = checkpointSchemaVersion
							respData["revision"] = rec2.Revision
							respData["reducer_seq"] = rec2.ReducerSeq
							respData["clear_generation"] = rec2.ClearGeneration
							respData["degraded"] = bs.reducer.IsDegraded()
							respData["view_checkpoint"] = json.RawMessage(viewRaw)
							m.resp(c, true, "", respData)
							return
						}
					}
				}
			}
		}
		// 有事件日志 → 聚合为单行 command_response 内的有序快照（替代逐行重放：
		// 前端单次同步遍历事件数组重建视图，避免 stdout 洪峰与逐行驱动状态）。
		// 无日志（新会话/旧会话）降级 messages 文本；两者皆空 → 空视图（新会话）。
		if snap, ok := m.buildSnapshot(ws, id); ok {
			respData["restored"] = "snapshot"
			respData["snapshot"] = snap
		} else if msgs := restoredMessages(bs.s); len(msgs) > 0 {
			respData["restored"] = "messages"
			respData["messages"] = msgs
		}
		m.resp(c, true, "", respData)

	case CmdAsk:
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		text := str(c.Payload, "text")
		if err := bs.s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: text})); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		bs.lastActive.Store(time.Now().UnixMilli()) // 用户输入 → 更新最近活跃
		bs.wakeRun()
		m.maybeGenSessionTitle(ws, bs, text) // 首次用户输入 → 异步生成 AI 标题
		m.resp(c, true, "", nil)

	case CmdAskBatch:
		// 批量推送用户输入（客户端暂存队列一次性推送）。每条输入是一条 user 消息，
		// 消息 = 内容块数组（对齐 core.Content：text / image，图文可混排，可携带文件/图片附件）。
		// 逐条经 Session.Ask 入 inbox —— 下一轮 takeBatch / 插话 Poll 一起成批消费，
		// 并以 UserInputsConsumed 事件确认（前端据此把多条输入移入对话页）。
		// payload: { "messages": [ { "content": [ {"type":"text","content":"..."} | {"type":"image","content":"data:...","mime_type":"..."} ] }, ... ] }
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		rawMsgs, ok := c.Payload["messages"].([]any)
		if !ok {
			m.resp(c, false, "messages must be an array", nil)
			return
		}
		sent := 0
		var firstText string
		for mi, it := range rawMsgs {
			mObj, ok := it.(map[string]any)
			if !ok {
				continue
			}
			rawBlocks, _ := mObj["content"].([]any)
			var blocks []core.Content
			for _, b := range rawBlocks {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				blocks = append(blocks, core.Content{
					Type:     str(bm, "type"),
					Content:  str(bm, "content"),
					MimeType: str(bm, "mime_type"),
				})
			}
			// 图片体积闸门（P0）：单条消息的图片总量必须留在 stdin 单行上限内，
			// 否则整批 ask_batch 会被「command line too long」拒绝——那条错误
			// 用户完全无法行动（不知道是图太大还是消息太多）。此处按条精确报错 + 专属
			// code（前端据 code 提示「图片过大，请压缩后重发」并保留该条待发送）。
			if over, detail := imageBudgetExceeded(blocks, maxImageBlockBytes); over {
				m.respCode(c, false, fmt.Sprintf(
					"message %d: images total %s, exceeds the per-message limit %s; compress or remove images and resend",
					mi+1, humanBytes(detail), humanBytes(maxImageBlockBytes)),
					"image_too_large", map[string]any{"message_index": mi})
				return
			}
			// 空消息（无内容块 / 全是纯空文本）跳过：与单条 ask 的空文本语义一致
			if len(blocks) == 0 || isEmptyContent(blocks) {
				continue
			}
			if err := bs.s.Ask(context.Background(), core.NewUserMessage(blocks...)); err != nil {
				m.resp(c, false, err.Error(), nil)
				return
			}
			if firstText == "" {
				firstText = firstTextOf(blocks) // 首条有效文本（AI 标题输入）
			}
			sent++
		}
		if sent > 0 {
			bs.lastActive.Store(time.Now().UnixMilli()) // 有实际输入 → 更新最近活跃
			bs.wakeRun()
			m.maybeGenSessionTitle(ws, bs, firstText) // 首次用户输入 → 异步生成 AI 标题
			m.resp(c, true, "", nil)
			return
		}
		// sent==0（全部被跳过：payload 形状不识别 / 纯空消息）必须显式失败——
		// 假成功会让客户端把消息标成「已推送」且不可编辑，消息从此滞留暂存队列
		// 无任何消费路径（问题十死局形态之一）。
		m.resp(c, false, "no valid messages in ask_batch payload (all skipped)", nil)

	case CmdInterrupt:
		bs := m.get(ws, sid)
		if bs == nil {
			// 路由兜底（2026-08-17 修复）：session_id/workspace 失配时绝不能静默 no-op——
			// 否则用户点「停止」无效果、LLM 一路流式输出到结束（线上实证：多会长 run
			// 全部 finish_reason=stop 自然结束、无一条 abort）。兜底中断该工作区全部
			// 会话（Session.Interrupt 幂等：无运行中 run 时无事发生）。响应 ok=true
			//（请求已处理），interrupted=中断的会话数，error 仅信息性提示。
			n := m.interruptAllIn(ws)
			// 响应带 code=interrupt_fallback：前端可区分「精确中断」与「兜底全停」
			//（错误/过期 session_id 时的安全兜底，V2 #5：不静默 no-op 也不误伤）。
			m.respCode(c, true, fmt.Sprintf("session %q not found; interrupted %d running session(s) in workspace", sid, n), "interrupt_fallback", map[string]any{"interrupted": n, "fallback": true})
			return
		}
		// 硬停（2026-08-17）：只 abort 当前 run，不入队"请停止"消息、不开新 run ——
		// 此前软停语义下模型把中断消息当普通输入继续执行工具（事件日志实证），
		// 用户看到"点了停止还在输出"。中断后会话 idle，下一条用户输入才恢复。
		bs.s.Interrupt()
		// 用户「停止」= 只停主会话（主 run 的 abort）。后台异步任务（agent_spawn 派生的
		// 子 agent、promoted 的后台工具任务）设计上脱离主 run 存活，**不受主停止影响**
		//（2026-08-30 用户决策：停止对话不影响异步任务；单个异步任务的停止统一走
		// interrupt_task 按 taskId 精确中断，与对话页/右栏任务卡同一入口）。
		// S3-B 开关保留：显式配置 runtime.stop_background_on_interrupt=true 时才级联停后台。
		if stopBackgroundOnInterrupt() {
			for _, ti := range bs.s.Registry().List() {
				if ti.Status == subagent.TaskRunning {
					_ = bs.s.Registry().Interrupt(ti.ID) // 非失败终止（AgentEnd finishReason=abort）
				}
			}
		}
		bs.wakeRun() // 保险：run loop 若正阻塞在 Run 入口的 takeBatch 之外，唤醒后自然退出
		m.resp(c, true, "", nil)

	case CmdInterruptTask:
		// 按 taskId 精确中断异步任务（右侧任务栏「停止」）：Session 统一路由
		// task-* → 子 agent 注册表；tooltask-* → 工具批控制器（promoted 长工具）。
		// 只影响该任务自身（取消其执行 ctx），不中断主会话、不影响其他任务。
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		taskID := str(c.Payload, "task_id")
		if err := bs.s.InterruptTask(taskID); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"task_id": taskID})

	case CmdApprove:
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		toolID := str(c.Payload, "id")
		approve := boolVal(c.Payload["approve"])
		if err := bs.s.Approve(toolID, approve); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", nil)

	case "promote_task": // 把运行中的工具调用摘离为后台任务（主 loop 立即拿占位继续；后台完成自动回传）
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		taskID := str(c.Payload, "task_id")
		if err := bs.s.PromoteTask(taskID); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", nil)

	case "answer_question": // 用户回答 ask_user 提问批次（batch_id 为 AskUserQuestion 事件携带的锚）
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		batchID := str(c.Payload, "batch_id")
		var answers []events.QuestionAnswer
		if raw, ok := c.Payload["answers"].([]any); ok {
			for _, it := range raw {
				if m, ok := it.(map[string]any); ok {
					answers = append(answers, events.QuestionAnswer{QuestionId: str(m, "question_id"), Answer: str(m, "answer")})
				}
			}
		}
		if err := bs.s.AnswerQuestions(batchID, answers); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		bs.lastActive.Store(time.Now().UnixMilli()) // 用户作答 → 更新最近活跃
		m.resp(c, true, "", nil)

	case CmdCompact:
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		if err := bs.s.Command(context.Background(), "compact", ""); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		bs.wakeRun()
		m.resp(c, true, "", nil)

	case CmdSwitchMode:
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		mode := parseMode(str(c.Payload, "mode"))
		bs.mode = mode
		bs.s.SetMode(mode) // 快照语义：下一次 Run 生效
		m.resp(c, true, "", map[string]any{"mode": modeName(mode)})

	case CmdSwitchEffort:
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		eff := str(c.Payload, "effort")
		if eff == "" {
			m.resp(c, false, "effort 为空", nil)
			return
		}
		bs.loop.SetReasoningEffort(provider.ReasoningEffortLevel(eff)) // 下一次 LLM 请求生效
		bs.effort = eff
		// 子 agent 追随主 loop（2026-09 决策）：共享 spawn/explore loop 就地同步，
		// 之后派生的文件化 subagent 由工厂读新档位（见 subagentEffortRef）。
		if bs.subagentEffort != nil {
			bs.subagentEffort.set(provider.ReasoningEffortLevel(eff))
		}
		m.resp(c, true, "", map[string]any{"effort": eff})

	case CmdSwitchPersona:
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		name := str(c.Payload, "persona")
		if _, ok := profiles[name]; !ok {
			m.respCode(c, false, "unknown persona: "+name, "unknown_persona", nil)
			return
		}
		// 重建 loop（新提示词/工具集）；保留当前 effort/model 与历史
		bs.persona = name
		m.rebuildLoop(bs)
		m.resp(c, true, "", map[string]any{"persona": name})

	case CmdSwitchModel:
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		name := str(c.Payload, "model")
		// 模型可来自任意已配置 Provider（对话窗口的模型列表不受「活跃 Provider」限制）：
		// 1) payload 显式 provider 优先（前端跨 Provider 选中时连带切）；
		// 2) 否则在所有 Provider 的模型表里按名解析（providerForModel），自动以该 Provider 为准。
		prov := str(c.Payload, "provider")
		if prov != "" && !m.provCfg.hasModelIn(prov, name) {
			// 显式 provider 下暂时查无此模型：先重载 settings.json（用户刚 add provider /
			// 拉模型列表落盘，bridge 内存可能滞后于落盘），再查一次。
			m.provCfg.loadSettings()
			m.provCfg.syncRegistry()
			if !m.provCfg.hasModelIn(prov, name) {
				// 仍查不到 → 显式报错，绝不静默回退按名全局解析（字母序会把 zhipu 的
				// glm-5.3-flash 猜成 opencode → 请求打到错误 Provider 401/429，2026-09）。
				m.respCode(c, false, "provider "+prov+" has no model "+name, "unknown_model", nil)
				return
			}
		}
		// 显式 provider（前端跨 Provider 分组选中模型连带切）→ 会话归属以用户选择为准，
		// 置 providerExplicit 防 rebuildAll 字母序改写（2026-09：多 Provider 同名模型
		// 如 glm-5.3-flash 存在于 opencode+zhipu，不能按字母序猜归属）。
		if prov == "" {
			prov = m.provCfg.providerForModel(name)
		}
		if prov == "" {
			m.respCode(c, false, "unknown model: "+name, "unknown_model", nil)
			return
		}
		// 目标 Provider ≠ 当前活跃 → 自动把活跃 Provider 切到该模型所属 Provider
		//（模型窗口/输出上限/压缩器都按它解析），无需依赖前端 set_provider 时序。
		if prov != m.provCfg.providerName() {
			m.provCfg.setActive(prov)
		}
		// model/provider/max_tokens/contextWindow 一起切：窗口与输出上限按目标模型重解析
		// （Provider 设置已持久化；重载 settings.json 拿最新值）。
		m.provCfg.loadSettings()
		// settings.json 的 active 可能仍是旧值（客户端 provider:save 异步落盘，与
		// 命令到达存在竞态）—— 重载后无条件把活跃 Provider 固定回本会话模型所属
		// Provider，避免会话 loop 用错 Provider 表/地址（跨 Provider 切模型时）。
		m.provCfg.setActive(prov)
		bs.model = name
		bs.provider = prov // 价表打标：本会话按所属 Provider 记账（不随全局活跃漂移）
		if str(c.Payload, "provider") != "" && bs.provider == prov {
			bs.providerExplicit = true // 用户显式选定 Provider → 后续 rebuildAll 保留归属
		}
		// 记录全局"最近一次模型选择"：IM 发起的新会话用它（而非 defaultModel）
		m.mu.Lock()
		m.lastModel = name
		m.mu.Unlock()
		// 下一轮 LLM 生效：当前 loop 就地切 model/max_tokens/contextWindow ——
		// 运行中的 Run 下一轮 LLM 立即读到新值（streamOnce/maybeCompact 按 current 取）。
		// 窗口/输出上限按目标 Provider 显式解析（问题六：不依赖 active 漂移）
		bs.loop.SetModel(name)
		bs.loop.SetMaxTokens(m.provCfg.maxTokensForIn(prov, name))
		bs.loop.SetContextWindow(m.provCfg.windowForIn(prov, name))
		m.rebuildLoop(bs) // 下一次 Run 生效：新模型绑定的压缩器/工具集重建
		m.resp(c, true, "", map[string]any{"model": name, "provider": prov})

	case CmdSetProvider: // 热切换：Provider 设置面板 → 更新配置 + 重建全部会话 loop（不重启 bridge）
		providerName := str(c.Payload, "provider")
		baseURL := str(c.Payload, "base_url")
		apiKey := str(c.Payload, "api_key")
		protocol := str(c.Payload, "protocol")
		// 仅当 payload 显式携带 active（前端「设为当前」/ providerSave active 字段）才切换
		// 全局活跃 Provider。纯「保存配置」（改 key/协议/模型窗口）不切 active——
		// 2026-09：此前无条件 setActive 导致「添加/编辑 Provider 保存」就全局跳 active，
		// 新建会话默认 Provider 跟着变，多 Provider 场景下请求串到刚保存的 Provider。
		if a := str(c.Payload, "active"); a != "" {
			if err := m.provCfg.setActive(a); err != nil {
				m.resp(c, false, err.Error(), nil)
				return
			}
		} else if providerName != "" && m.provCfg.active == "" {
			// 兜底：无 active 记录（异常初始态）且明确保存某 provider → 设为该 provider
			if err := m.provCfg.setActive(providerName); err != nil {
				m.resp(c, false, err.Error(), nil)
				return
			}
		}
		// 预设初始化：用户从下拉选中内置预设添加（新 provider 且未配置 base_url/protocol 时，
		// 从预设自动补官方端点/协议/模型清单——前端只需传 name+key 即可生效）。
		if preset := provider.FindProviderPreset(providerName); preset != nil {
			if baseURL == "" && m.provCfg.baseURL[providerName] == "" {
				baseURL = preset.BaseURL
			}
			if protocol == "" && m.provCfg.protocol[providerName] == "" {
				protocol = string(preset.Protocol)
			}
			// 模型清单：新 provider 且前端未传 models 时，从预设（注册表真实窗口）初始化
			if _, ok := c.Payload["models"]; !ok && len(m.provCfg.models[providerName]) == 0 {
				reg := m.provCfg.registry
				if reg == nil {
					reg = provider.NewRegistry()
				}
				init := make(map[string]int64, len(preset.Models))
				for _, mid := range preset.Models {
					if w := reg.ContextWindowFor(providerName, mid); w > 0 {
						init[mid] = w
					}
				}
				if len(init) > 0 {
					m.provCfg.setModels(providerName, init)
				}
			}
		}
		if baseURL != "" {
			m.provCfg.setBaseURL(providerName, baseURL)
		}
		if apiKey != "" {
			m.provCfg.setKey(providerName, apiKey)
		} else if _, ok := c.Payload["api_key"]; ok {
			// payload 显式带空 api_key（前端保存未填 key / 删 key）：清内存旧值，
			// 防止旧 key 残留 → 新会话 401（回退 env/.env 兜底）。
			m.provCfg.setKey(providerName, "")
		}
		if protocol != "" {
			m.provCfg.setProtocol(providerName, protocol) // payload 兜底 settings.json 落盘时序
		}
		if authType := str(c.Payload, "auth_type"); authType != "" {
			m.provCfg.setAuthType(providerName, authType) // OAuth 鉴权方式（payload 兜底落盘时序）
		}
		// usage 口径标注（2026-09-21）：payload 显式带该键才动 —— bool = 标注
		//（true = input_tokens 已含缓存 / false = 未含），null = 清除回自动判定。
		// 未带键（改价格/窗口等其它保存路径）保持现状，避免误清用户标注。
		if raw, ok := c.Payload["usage_input_includes_cache"]; ok {
			if v, isBool := raw.(bool); isBool {
				m.provCfg.setUsageIncl(providerName, &v)
			} else {
				m.provCfg.setUsageIncl(providerName, nil)
			}
		}
		// 缓存 TTL 标注（同三态）：bool = 标注（true 1h / false 5m），null = 清除回端点默认。
		if raw, ok := c.Payload["cache_ttl_1h"]; ok {
			if v, isBool := raw.(bool); isBool {
				m.provCfg.setCacheTTL1h(providerName, &v)
			} else {
				m.provCfg.setCacheTTL1h(providerName, nil)
			}
		}
		// 重载 settings.json（模型窗口/max_tokens/input_types/协议/删除标记）。
		// 顺序：loadSettings 在前（基础值 + 其它 provider 的最新落盘值），payload 显式字段
		// 随后覆盖 —— payload 是「本次操作的期望值」，永远胜出；loadSettings 只补 payload
		// 未涉及的字段（如其它 provider 的改动）。前端 saveProvider 已改为 await 落盘后
		// 发送，落盘值 = payload 值，两轮覆盖结果一致。
		m.provCfg.loadSettings()
		if raw, ok := c.Payload["openai_compat"].(map[string]any); ok {
			m.provCfg.setCompat(providerName, parseCompatCapabilities(raw))
		}
		// models（窗口）与 max_tokens 直接经 payload 覆盖：客户端已持久化，payload 兜底
		// settings.json 落盘时序（窗口/输出上限即时生效，不依赖 IPC 写盘完成）
		if raw, ok := c.Payload["models"].(map[string]any); ok {
			parsed := make(map[string]int64, len(raw))
			for name, v := range raw {
				if n, ok := int64Val(v); ok {
					parsed[name] = n
				}
			}
			if len(parsed) > 0 {
				m.provCfg.setModels(providerName, parsed)
			}
		}
		if raw, ok := c.Payload["max_tokens"].(map[string]any); ok {
			parsed := make(map[string]int64, len(raw))
			for name, v := range raw {
				if n, ok := int64Val(v); ok {
					parsed[name] = n
				}
			}
			if len(parsed) > 0 {
				m.provCfg.setMaxTokens(providerName, parsed)
			}
		}
		// input_types（前端 Provider 面板逐模型勾选图片 → ["text","image"]）：bridge 现开始消费
		if raw, ok := c.Payload["input_types"].(map[string]any); ok {
			parsed := make(map[string][]string, len(raw))
			for model, v := range raw {
				if types, ok := v.([]any); ok {
					ss := make([]string, 0, len(types))
					for _, t := range types {
						if s, ok := t.(string); ok {
							ss = append(ss, s)
						}
					}
					parsed[model] = ss
				}
			}
			if len(parsed) > 0 {
				m.provCfg.setInputTypes(providerName, parsed)
			}
		}
		// prices（前端 Provider 面板每模型单价 → USD/1M tokens）：payload 兜底 settings.json
		// 落盘时序。写入全局价表 + 注册表覆盖（setProviderPrices 内做），随后 syncRegistry
		// 会把新价注入全部模型 override（info.Cost）。
		if raw, ok := c.Payload["prices"].(map[string]any); ok {
			parsed := map[string]provider.ModelPrice{}
			for model, v := range raw {
				if pm, ok := v.(map[string]any); ok {
					price := func(k string) float64 {
						if f, ok := pm[k].(float64); ok {
							return f
						}
						if i, ok := int64Val(pm[k]); ok {
							return float64(i) // 整数值（JSON 解码可能走 int64 分支的兜底）
						}
						return 0
					}
					parsed[model] = provider.ModelPrice{Input: price("input"), CacheRead: price("cache_read"), CacheWrite: price("cache_write"), Output: price("output")}
				}
			}
			if len(parsed) > 0 {
				m.provCfg.setPrices(providerName, parsed)
			}
		}
		m.provCfg.syncRegistry() // 热更新注册表（窗口/上限/价/input_types 覆盖）
		m.rebuildAll()
		m.resp(c, true, "", map[string]any{"active": m.provCfg.providerName()})

	case CmdOAuthLogin: // OAuth 订阅登录（P2）：发起登录流程（事件推送 + manual 通道）。
		// 必须异步执行：flow.Login 的 Prompt 会阻塞等待 oauth_prompt_answer 命令，
		// 而该命令要靠 dispatch 循环处理——同步执行会死锁（桌面端整体卡死直到超时）。
		go m.handleOAuthLogin(c)

	case CmdOAuthLogout: // OAuth 登出：删除凭证 + 回退 api_key
		m.handleOAuthLogout(c)

	case CmdOAuthStatus: // OAuth 登录状态查询（前端面板展示；不含 token）
		m.handleOAuthStatus(c)

	case CmdOAuthPromptAnswer: // 前端回填 manual 输入（粘贴授权码/重定向 URL）
		providerName := str(c.Payload, "provider")
		value := str(c.Payload, "value")
		requestID := str(c.Payload, "request_id")
		if providerName != "" && m.answerOAuthPrompt(requestID, providerName, value) {
			m.resp(c, true, "", nil)
		} else {
			m.respCode(c, false, "no pending OAuth input (may have timed out or been cancelled)", "oauth_no_pending", nil)
		}

	case CmdSetUILang: // 界面语言热通知：只刷新 Hub mesh 文案语言（不重建 loop）
		if m.hub != nil {
			m.hub.SetUILang(str(c.Payload, "lang"))
		}
		m.resp(c, true, "", nil)

	case CmdReloadSettings: // 重载 settings.json 并重建全部会话 loop（沙箱模式等顶层设置变更后热切换，不重启）
		m.provCfg.loadSettings()
		m.provCfg.syncRegistry()
		m.refreshComputerApprovedApps() // Computer Use 已授权 App 热更新（无需重启插件）
		m.applyMeshSettings()           // Session Mesh 开关热更新（Hub 门 + 远程网关 + rebuildAll 对齐工具）
		m.refreshBrowserEngine()        // Browser 引擎开关热更新（browser_engine: mcp|ego）
		m.applySubagentConcurrency()    // 子 agent 并发槽上限热更新（settings.json agent 段）
		m.rebuildAll()                  // 现有：重建全部 loop（兜底对齐 system 工具清单）
		m.resp(c, true, "", nil)

	case CmdMeshStatus: // 查询 Session Mesh 状态（设置页「别人怎么连我」/ 排障）
		// 先主动向对端拉取最新在线会话（list_req；错过推送也能立即对齐），
		// 再短暂等待回帧落地（≤500ms），让本次响应就带上新会话/标题。
		if m.meshGW != nil {
			m.meshGW.RequestPeerSessions()
			time.Sleep(400 * time.Millisecond)
		}
		m.resp(c, true, "", m.meshStatus())

	case CmdMeshPing: // 主动 ping 一个节点（连接检测：已连直连 / 配置出站 peer）
		target := str(c.Payload, "node_id")
		if target == "" {
			m.respCode(c, false, "node_id 为空", "mesh_ping_no_target", nil)
			return
		}
		if m.meshGW == nil {
			m.respCode(c, false, "远程网关未运行（remote 未开启或启动失败）", "mesh_disabled", nil)
			return
		}
		rtt, err := m.meshGW.PingNode(context.Background(), target)
		if err != nil {
			m.resp(c, false, err.Error(), map[string]any{"node_id": target, "ok": false})
			return
		}
		m.resp(c, true, "", map[string]any{"node_id": target, "ok": true, "rtt_ms": rtt.Milliseconds()})

	case "set_goal_alignment_rounds": // 仅更新现有会话 AgentLoop 的提醒阈值，不重建 loop/会话
		rounds := intNum(c.Payload, "reminder_rounds", -1)
		if rounds < 0 {
			m.resp(c, false, "reminder_rounds 必须是非负整数", nil)
			return
		}
		m.setGoalAlignmentReminderRounds(rounds)
		m.resp(c, true, "", map[string]any{"reminder_rounds": rounds})

	case "set_max_subagents": // 子 agent 并发槽上限热更新（主池/辅池）：只改容量，不动在途任务
		spawn := intNum(c.Payload, "max_subagents", -1)
		explore := intNum(c.Payload, "max_explore_subagents", -1)
		if spawn < 1 || explore < 1 {
			m.resp(c, false, "max_subagents / max_explore_subagents 必须是正整数", nil)
			return
		}
		m.setSubagentConcurrency(spawn, explore)
		m.resp(c, true, "", map[string]any{"max_subagents": spawn, "max_explore_subagents": explore})

	case CmdListModels: // 拉取某 provider 模型列表（Provider 设置面板「拉取模型列表」）
		providerName := str(c.Payload, "provider")
		if providerName == "" {
			providerName = m.provCfg.providerName()
		}
		models, err := m.provCfg.listModels(providerName)
		if err != nil {
			code := "provider_error"
			switch {
			case strings.Contains(err.Error(), "no API key"):
				code = "provider_no_key"
			case strings.Contains(err.Error(), "OAuth"):
				code = "provider_oauth_not_logged_in"
			}
			m.respCode(c, false, err.Error(), code, nil)
			return
		}
		// 附带模型能力（注册表真实 inputs/窗口/上限/reasoning）—— 前端渲染「支持图片」标记
		caps := m.provCfg.modelCapabilities(providerName, models)
		m.resp(c, true, "", map[string]any{"provider": providerName, "models": models, "capabilities": caps})

	case CmdTestConnection: // 测试连接（Provider 面板：GET /models 探活，瞬态——用待保存的
		// base_url + key 直接探测，不落盘、不修改 providerConfig）
		providerName := str(c.Payload, "provider")
		baseURL := str(c.Payload, "base_url")
		apiKey := str(c.Payload, "api_key")
		if baseURL == "" {
			m.resp(c, false, "base_url 为空", nil)
			return
		}
		models, err := m.provCfg.testConnection(baseURL, apiKey)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"provider": providerName, "base_url": baseURL, "models": models, "count": len(models)})

	case CmdListProviderPresets: // 内置 Provider 预设列表（前端下拉可选；用户添加后才生效）
		// 返回全部预设（含已配置的默认 5 家 + P1 可选）；前端据此渲染「添加 Provider」下拉。
		// 每个预设带 name/base_url/protocol/models（模型清单来自注册表内置真实数据）。
		// 2026-09：presets 数据源统一为 provider/providers.json（embed 单一事实源），
		// 前端不再硬编码预设表；oauth 声明一并下发（登录入口渲染）。
		// 2026-10：models_cost 一并下发 —— 预设各模型的内置价（$USD/1M tokens，注册表
		// providers.json + 用户覆盖的生效价）。纯本地注册表读取（无网络），前端打开设置
		// 即能回显内置价；此前只有 list_models（拉取模型列表，依赖网络与用户点击）才带价。
		presets := provider.BuiltinProviderPresets
		out := make([]map[string]any, 0, len(presets))
		modelsCost := map[string]map[string]any{}
		for _, p := range presets {
			entry := map[string]any{
				"name": p.Name, "id": p.ID, "base_url": p.BaseURL,
				"protocol": string(p.Protocol), "models": p.Models,
			}
			if p.OAuth != nil {
				oauth := map[string]any{"name": p.OAuth.Name, "flow": p.OAuth.Flow}
				if p.OAuth.IsSubscription {
					oauth["is_subscription"] = true
				}
				if p.OAuth.LoginLabel != "" {
					oauth["login_label"] = p.OAuth.LoginLabel
				}
				if p.OAuth.KeyInstead {
					oauth["key_instead"] = true
				}
				entry["oauth"] = oauth
			}
			out = append(out, entry)
			// 预设模型内置价（注册表 Lookup：用户覆盖优先、内置兜底 —— 与运行时
			// FillUsageCost 同源）。未知模型/无价 → 不下发（前端保持空覆盖占位）。
			costs := map[string]any{}
			reg := m.provCfg.registry
			if reg == nil {
				reg = provider.NewRegistry()
			}
			for _, mid := range p.Models {
				if info, ok := reg.Lookup(p.ID, mid); ok &&
					(info.Cost.Input > 0 || info.Cost.Output > 0 || info.Cost.CacheRead > 0 || info.Cost.CacheWrite > 0) {
					costs[mid] = map[string]any{
						"input": info.Cost.Input, "cache_read": info.Cost.CacheRead,
						"cache_write": info.Cost.CacheWrite, "output": info.Cost.Output,
					}
				}
			}
			if len(costs) > 0 {
				modelsCost[p.ID] = costs
			}
		}
		m.resp(c, true, "", map[string]any{"presets": out, "models_cost": modelsCost})

	case "file_preview":
		path := str(c.Payload, "path")
		if path == "" {
			m.resp(c, false, "path 为空", nil)
			return
		}
		// 绝对路径不依赖 workspace（工作区外统一 open-file）；仅相对路径需要锚定。
		if ws == "" && !filepath.IsAbs(path) {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		content, lines, truncated, binary, err := readFilePreview(ws, path)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		// binary=true 时 content 为空串：前端据此改为「该格式不适用文本预览」提示，
		// 而不是把二进制喂给 shiki（乱码 + 白耗渲染）。
		m.resp(c, true, "", map[string]any{"path": path, "content": content, "lines": lines, "truncated": truncated, "binary": binary})

	case CmdXlsxPreview: // .xlsx 工作簿 → HTML（受管运行时跑固定 openpyxl 脚本）→ 前端送内嵌浏览器 file://
		// 为什么在 bridge 而不是前端做：xlsx 是二进制容器（zip + XML），前端解析要引新依赖
		// （SheetJS 等），而不加依赖是硬约束；受管运行时里 openpyxl 已就位，且转换跑在
		// 沙箱内（见 xlsxPreviewSandbox）。前端只拿产物路径 → 复用内嵌浏览器的 file:// 通道
		// （与 PDF 预览同一条，实测可渲染）。
		//
		// 产物是临时文件（受管运行时 tmp 子目录，见 builtin.writeTempFile），不是用户资产：
		// 不落工作区、不进产出卡片、不写 metrics。生命周期由转换器自己管（下一次转换时清上一条）。
		path := str(c.Payload, "path")
		abs, ok := m.previewInputPath(c, ws, path, "xlsx")
		if !ok {
			return
		}
		pyRT := m.pythonRuntime()
		if pyRT == nil { // 理论上不会（pythonRuntime 恒返回非 nil），留作装配变化的兜底
			m.resp(c, false, "受管 Python 运行时未装配，无法预览 xlsx", nil)
			return
		}
		// 转换（openpyxl 解析 + 渲染）：秒级，但**首次调用可能先 bootstrap 运行时**
		// （建 venv + pip 装约 42MB），故放 goroutine —— 与 oauth_login 同理，不能阻塞
		// dispatch 主循环（那会让整个桌面端在此期间无响应）。响应经 m.resp 回写（线程安全）。
		go func() {
			res, cerr := m.xlsxPreviewer(pyRT).Convert(context.Background(), abs, ws)
			if cerr != nil {
				m.resp(c, false, cerr.Error(), nil)
				return
			}
			m.resp(c, true, "", map[string]any{"path": path, "html_path": res.HTMLPath})
		}()

	case CmdDocxPreview: // .docx 文档 → HTML（受管运行时跑固定 mammoth 脚本）→ 内嵌浏览器 file://
		// 与 xlsx_preview 逐字同构：为什么在 bridge 做、为什么产物落临时目录、路径口径，
		// 全部同 CmdXlsxPreview 的长注释（三条预览通路是一条通路，不该各自演化）。
		//
		// 与 xlsx 的唯一差别是转换器：Word 文档要的是**语义结构**（标题层级/列表/表格/
		// 内嵌图片），mammoth 正是这个定位（builtin.DocxPreviewer 有详细取舍）。
		path := str(c.Payload, "path")
		abs, ok := m.previewInputPath(c, ws, path, "docx")
		if !ok {
			return
		}
		pyRT := m.pythonRuntime()
		if pyRT == nil {
			m.resp(c, false, "受管 Python 运行时未装配，无法预览 docx", nil)
			return
		}
		go func() {
			res, cerr := m.docxPreviewer(pyRT).Convert(context.Background(), abs, ws)
			if cerr != nil {
				m.resp(c, false, cerr.Error(), nil)
				return
			}
			m.resp(c, true, "", map[string]any{"path": path, "html_path": res.HTMLPath})
		}()

	case CmdPptxPreview: // .pptx 演示文稿 → HTML（受管运行时跑固定 python-pptx 脚本）→ 内嵌浏览器 file://
		// 同上。注意这条通路的产物是**近似版式重建**而不是渲染（本机没有 LibreOffice，
		// python-pptx 不能渲染）—— 做不到的几件事印在页面顶部，见 builtin.PptxPreviewer。
		path := str(c.Payload, "path")
		abs, ok := m.previewInputPath(c, ws, path, "pptx")
		if !ok {
			return
		}
		pyRT := m.pythonRuntime()
		if pyRT == nil {
			m.resp(c, false, "受管 Python 运行时未装配，无法预览 pptx", nil)
			return
		}
		go func() {
			res, cerr := m.pptxPreviewer(pyRT).Convert(context.Background(), abs, ws)
			if cerr != nil {
				m.resp(c, false, cerr.Error(), nil)
				return
			}
			m.resp(c, true, "", map[string]any{"path": path, "html_path": res.HTMLPath})
		}()

	case CmdContextBreakdown: // 上下文构成面板（Composer ctx 环 hover 数据源）
		// 静态分区（系统提示词/技能/工具/MCP/其他）按请求组装字节固定展示，消息分区
		// 取「锚点 − 静态合计」残差 —— 静态块不随会话内容涨落，总量与 ctx 环逐字一致。
		// 锚点口径（真实 usage / 压缩后估算）由 reducer 给出，面板照实标注。
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		bd := bs.s.ContextBreakdown(context.Background())
		tokens, estimated := bs.reducer.CtxAnchor()
		m.resp(c, true, "", ctxBreakdownPayload(bd, ctxAnchor{Tokens: tokens, Estimated: estimated}))

	case "skills": // 技能清单（SkillsPage + /skill 面板数据源；全部技能含启用标志）
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		m.resp(c, true, "", map[string]any{"skills": m.skillList(ws)})

	case "skill_toggle": // 启用/禁用技能（真过滤：发现清单 + load_skill 立即生效）
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		name := str(c.Payload, "name")
		on := boolVal(c.Payload["enabled"])
		if err := m.runtime(ws).skills.SetEnabled(name, on); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.persistSkillsDisabled(ws)
		m.resp(c, true, "", nil)

	case "skill_get": // 技能详情（SKILL.md 指令 + 资源清单；SkillsPage 预览，不过滤禁用）
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		s, err := m.runtime(ws).skills.Skill(str(c.Payload, "name"))
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		data := map[string]any{"name": s.Name, "instructions": s.Instructions, "resources": s.Resources}
		if src, ok := m.remoteSources()[s.Name]; ok {
			data["source"] = src
		}
		m.resp(c, true, "", data)

	case "skill_install": // 远程技能下载（exec git）：payload {url, scope: global|workspace, name?}；
		// name 可选：市场浏览后精确安装单个技能（缺省 = 全部）；装完重建技能注册表
		// （新技能进发现清单 + 会话工具集）。安装只复制文件，不执行。
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		url := str(c.Payload, "url")
		scope := str(c.Payload, "scope")
		if scope == "" {
			scope = "global"
		}
		name := str(c.Payload, "name")
		results, err := remote.Install(context.Background(), url, remote.InstallOptions{Scope: scope, Workspace: ws, Select: name}, m.remoteEnv())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		items := []map[string]any{}
		for _, r := range results {
			items = append(items, map[string]any{"name": r.Name, "scope": r.Scope, "target": r.Target})
		}
		if scope == "global" {
			m.rebuildAllSkills() // 全局技能：所有工作区可见
		} else {
			m.rebuildSkills(ws)
		}
		m.resp(c, true, "", map[string]any{"skills": items})

	case "skill_browse": // 浏览远程仓库技能（只读，不安装）：payload {url}；返回技能名/描述/已装标记
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		url := str(c.Payload, "url")
		items, err := remote.Browse(context.Background(), url, m.remoteEnv())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		list := []map[string]any{}
		for _, it := range items {
			list = append(list, map[string]any{"name": it.Name, "description": it.Description, "installed": it.Installed, "update_available": it.UpdateAvailable})
		}
		m.resp(c, true, "", map[string]any{"skills": list})

	case "skill_update": // 重拉最新 ref 更新已安装技能（清单来源原样重解析）
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		name := str(c.Payload, "name")
		if err := remote.Update(context.Background(), name, m.remoteEnv()); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.rebuildAllSkills()
		m.resp(c, true, "", nil)

	case "skill_uninstall": // 卸载已安装远程技能（删目录 + 清单条目）
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		name := str(c.Payload, "name")
		if err := remote.Uninstall(name, m.remoteEnv()); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.rebuildAllSkills()
		m.resp(c, true, "", nil)

	case "market_add": // 保存市场 URL（免手填；幂等）：payload {url}；返回 {market}
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		mk, err := remote.AddMarketplace(str(c.Payload, "url"), m.remoteEnv())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"market": map[string]any{"name": mk.Name, "url": mk.URL, "added_at": mk.AddedAt}})

	case "market_remove": // 移除已保存市场：payload {url}
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		if err := remote.RemoveMarketplace(str(c.Payload, "url"), m.remoteEnv()); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", nil)

	case "market_list": // 已保存市场清单：{markets: [{name,url,added_at}]}
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		markets := []map[string]any{}
		for _, mk := range remote.ListMarketplaces(m.remoteEnv()) {
			markets = append(markets, map[string]any{"name": mk.Name, "url": mk.URL, "added_at": mk.AddedAt})
		}
		m.resp(c, true, "", map[string]any{"markets": markets})

	case "skill_update_market": // 按市场更新该市场全部已装技能：payload {url}；返回 {updated:[names]}
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		updated, err := remote.UpdateMarket(context.Background(), str(c.Payload, "url"), m.remoteEnv())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.rebuildAllSkills()
		m.resp(c, true, "", map[string]any{"updated": updated})

	case "load_skill": // 产品主动加载技能进上下文（/skill 面板）：走 SDK 命令框架 skills load（下一轮生效）
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		name := str(c.Payload, "name")
		if err := bs.s.Command(context.Background(), "skills", "load "+name); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		bs.wakeRun()
		m.resp(c, true, "", nil)

	case "command": // 斜杠命令（/xxx）：/技能名→加载技能；/summary→压缩；/clear→清空上下文；/reload_skills→重扫技能；其余→SDK unknown
		bs := m.get(ws, sid)
		if bs == nil {
			m.respCode(c, false, "session not found", "session_not_found", nil)
			return
		}
		raw := strings.TrimPrefix(str(c.Payload, "line"), "/")
		if raw == "" {
			m.resp(c, false, "命令为空", nil)
			return
		}
		var cmdErr error
		switch raw {
		case "summary": // 会话摘要 = 压缩（自动/主动压缩统一 LLMCompressor）
			cmdErr = bs.s.Command(context.Background(), "compact", "")
		case "clear": // 清空上下文（SDK 内核命令：清历史保留 system + Stop）
			cmdErr = bs.s.Command(context.Background(), "clear", "")
		case "reload_skills": // 重扫工作区技能目录 + 自定义 subagent 目录 + 重建会话 loop（新增/修改无需重启生效）
			m.rebuildSkills(ws)
			m.rebuildSubagents(ws)
			n := len(m.runtime(ws).skills.All())
			na := len(m.runtime(ws).subagents.List())
			bs.emit(&events.CommandResult{
				Name:      "reload_skills",
				Result:    fmt.Sprintf("已重载技能与子代理注册表（全局 ~/.agents/skills + ~/.go-code/skills + 工作区 .agents/skills + .go-code/skills + 全局 ~/.agents/agents + ~/.go-code/agents + ~/.claude/agents + 工作区 .agents/agents + agents + .claude/agents），技能 %d 个、子代理 %d 个", n, na),
				Timestamp: time.Now(),
				EventType: events.CommandResultType,
			})
			cmdErr = nil
		default: // /技能名 [正文...] → 加载技能；正文作为普通消息进上下文；未识别 → 文本回退
			// 切分命令名与参数（问题十一）：此前用「去 / 整行」查技能表，带正文的
			// /技能名 args 必然 miss → unknown command。现按首个空白切分。
			name, args := raw, ""
			if i := strings.IndexAny(raw, " \t"); i >= 0 {
				name, args = raw[:i], strings.TrimSpace(raw[i+1:])
			}
			fallbackText := func() error {
				// 命令识别失败 → 按普通用户文本进上下文（约定回退）：模型看到原文
				// 自行处理（如经 load_skill 工具加载技能后执行正文）。
				return bs.s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: str(c.Payload, "line")}))
			}
			if _, skerr := m.runtime(ws).skills.Skill(name); skerr == nil {
				cmdErr = bs.s.Command(context.Background(), "skills", "load "+name)
				if cmdErr != nil {
					// 加载失败（禁用技能等）→ 文本回退，不报错死局
					cmdErr = fallbackText()
				} else if args != "" {
					// 技能已加载 + 有正文 → 正文作为普通用户消息进上下文（模型带技能干活）
					if aerr := bs.s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: args})); aerr != nil {
						cmdErr = aerr
					}
				}
			} else {
				cmdErr = fallbackText() // 未识别命令 → 文本回退（问题十一约定）
			}
		}
		if cmdErr != nil {
			m.resp(c, false, cmdErr.Error(), nil)
			return
		}
		bs.wakeRun()
		m.resp(c, true, "", nil)

	case "mcp_list": // MCP 服务器清单 + 连接状态（SettingsModal MCP tab；每工作区连接，须 workspace）
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		servers := []map[string]any{}
		for _, st := range m.runtime(ws).mcp.Status() {
			servers = append(servers, map[string]any{
				"name": st.Name, "type": st.Type, "command": st.Command,
				"enabled": st.Enabled, "state": st.State,
				"tool_count": st.ToolCount, "error": st.Error,
				"source": st.Source, // 分层来源（"user"/"project"；空 = 未标注）
			})
		}
		// 逐层状态（含 stale/err/skipped）：配置文件坏掉时「沿用上次有效」这件事对用户可见。
		m.resp(c, true, "", map[string]any{"servers": servers, "layers": m.mcpLayerStates(ws)})

	case "mcp_set": // 全量更新 MCP 配置（payload.servers = 期望的完整**用户层** map）：叠加各工作区项目层后应用
		// 管理器 + 重建会话 loop（工具集变化立即生效）。持久化由 Electron mcpSave 负责
		// （bridge 不写 settings.json，遵循 set_provider 模式：客户端已落盘，payload 兜底热应用）。
		cfg, err := parseMCPConfig(c.Payload["servers"])
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.applyMCPConfig(cfg)
		m.rebuildAll()
		m.resp(c, true, "", nil)

	case "mcp_refresh": // 强制重连单个 MCP 服务器（断线重试/手动刷新）；重建本工作区会话
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		name := str(c.Payload, "name")
		if err := m.runtime(ws).mcp.Refresh(context.Background(), name); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.rebuildWorkspace(ws)
		m.resp(c, true, "", nil)

	case CmdMCPReload: // 重读 MCP 配置分层（用户 settings.json + 项目 .mcp.json/.go-code/settings.json）
		// 用途：手动重读（配置文件监听失败时的兜底 / 免等 debounce）。内容无变化则零副作用。
		// 只作用于该工作区（项目层本就是每工作区独立）。
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		m.reloadMCP(ws)
		m.resp(c, true, "", nil)

	case CmdMCPProjects: // 多项目项目层 MCP 汇总（MCP 页「项目」区；payload.workspaces = 前端绑定的项目）
		// 为什么不走 mcp_list：mcp_list 服务"当前工作区 + 连接状态"，必须先有运行态；
		// 本命令服务"任意多个已绑定项目"，**打开设置页不得产生副作用**（不建会话、不连
		// MCP、不起 watcher）——有运行态的项目才附连接状态/工具数（见 mcp_projects.go）。
		// 目录已删的项目也返回（exists=false），由前端过滤：面板要能解释"为什么没显示"。
		m.resp(c, true, "", map[string]any{"projects": m.mcpProjectsData(strSlice(c.Payload, "workspaces"))})

	case CmdMCPProjectSet: // 写某项目的项目层 MCP（{ws}/.go-code/settings.json 的 mcpServers 段）
		// 校验同 mcp_set（parseMCPConfig，非法拒绝）；空 map → 删键；有运行态 → 热应用。
		m.handleMCPProjectSet(c)

	case CmdHooksList: // hooks 来源汇总（钩子页数据源；payload.workspaces = 前端绑定的项目）
		// 同 mcp_projects：不创建运行态；钩子页与"项目是否打开"无关（纯读文件 + 合并视角）。
		m.handleHooksList(c)

	case CmdHooksSet: // 写钩子配置（user → ~/.go-code/settings.json 的 hooks 段；project → {ws}/.go-code/hooks.json）
		// 严格校验（未知事件/非 command 类型/负超时/非法 matcher 正则一律拒绝）+ 热替换
		// （Session.SetHooks，下一次 Run 生效）。
		m.handleHooksSet(c)

	case CmdHooksTrust: // 记录命令信任（~/.go-code/hooks-state.json；内容哈希）
		// 即刻生效：信任判定在每次派发时查共享的信任库实例，不需要重建会话。
		m.handleHooksTrust(c)

	case "plugin_list", "plugin_install", "plugin_uninstall", "plugin_enable", "plugin_disable":
		// 插件命令（通用，按 id 寻址）：插件列表/受管安装/卸载/启用/禁用（Browser Use 首个实例）
		m.handlePluginCommand(c)

	case "im_status", "im_chat_list", "im_schema_list", "im_config_get", "im_config_set",
		"im_route_list", "im_route_set", "im_route_del",
		"im_mirror_list", "im_mirror_set", "im_mirror_del",
		"im_bind_confirm":
		// IM 集成命令（外部 IM Gateway 管理面：状态/配置/路由/镜像/绑定确认）
		m.handleIMCommand(c)

	case "browser_status", "browser_pages", "browser_screenshot", "browser_close", "browser_config_get", "browser_config_set":
		// 浏览器命令（web tab 数据源 + 插件配置：状态/页列表/截图/关闭页面/配置读改写；走浏览器插件私有连接）
		m.handleBrowserCommand(c)

	case "browser_embed_start", "browser_embed_attach", "browser_embed_stop":
		// M2/M6：内嵌浏览器端点生命周期（起转发器 ws 端点 / attach 视图调试代理 / 停止）。
		// 注意：三个 case 实现在 handleBrowserCommand（与 browser_pages 同族），勿改路由到
		// handlePluginCommand——其 switch 无此类型又无 default，会静默吞命令（历史 bug）。
		m.handleBrowserCommand(c)

	case "browser_engine_get": // 查询浏览器引擎开关（引擎级，不依赖 ws）
		// payload.refresh_ego=true（安装完成后）→ 先清检测缓存，立即反映磁盘现状并落持久化记录。
		if b, ok := c.Payload["refresh_ego"].(bool); ok && b {
			resetEgoInstallCache()
		}
		m.resp(c, true, "", map[string]any{
			"browser_engine": string(m.getBrowserEngine()),
			"ego":            egoStatusToMap(checkEgoStatus()),
		})
		return

	case "browser_engine_set": // 切换浏览器引擎（mcp|ego）——引擎级互斥：切 ego 禁用 browser
		// 插件（工具不注册），切 mcp 恢复。落盘由客户端负责（payload 兜底热应用，对齐
		// set_provider 模式）；bridge 只在内存切 + 编排插件启停。
		eng := normalizeBrowserEngine(str(c.Payload, "engine"))
		changed := m.getBrowserEngine() != eng
		m.setBrowserEngine(eng)
		// 引擎变更 → 编排受影响工作区的 browser 插件：
		//   - 切 ego：禁用已启用的 browser（工具从 agent 摘除）
		//   - 切 mcp：恢复自动启用（有 WasEnabled 标记则 Enable——异步，不阻塞切换响应）
		ctx := context.Background()
		for _, ws := range m.workspaceList() {
			plg := ws.plugins
			cap, ok := plg.Get(browser.ID)
			if !ok {
				continue
			}
			isEnabled := false
			if ec, ok := cap.(interface{ IsEnabled() bool }); ok {
				isEnabled = ec.IsEnabled()
			}
			if eng == EngineEgo && isEnabled {
				// 引擎切换是编排性停用，非用户显式禁用：Registry.ClosePreserve
				// （摘工具缓存 + 释放连接，保留 .enabled 持久标记）——切回 mcp 可自动恢复。
				if err := plg.ClosePreserve(ctx, browser.ID); err != nil {
					fmt.Fprintf(os.Stderr, "✗ 切 ego ClosePreserve browser 插件(%s): %v\n", ws.path, err)
					continue
				}
				m.rebuildWorkspace(ws.path)
				m.emitPluginStatus(ws.path)
			}
			if eng == EngineMCP && !isEnabled {
				if we, ok := cap.(interface{ WasEnabled() bool }); ok && we.WasEnabled() {
					wsPath := ws.path
					go func() {
						if _, err := ws.plugins.Enable(ctx, browser.ID); err != nil {
							fmt.Fprintf(os.Stderr, "✗ 切 mcp 恢复 browser 插件(%s): %v\n", wsPath, err)
							return
						}
						m.rebuildWorkspace(wsPath)
						m.emitPluginStatus(wsPath)
					}()
				}
			}
		}
		m.resp(c, true, "", map[string]any{
			"browser_engine": string(eng),
			"changed":        changed,
		})
		return

	case "ego_status": // ego lite 可用性检测（pgrep + CLI --version；秒级，无网络）
		m.resp(c, true, "", egoStatusToMap(checkEgoStatus()))
		return

	case "cron_list": // 定时任务清单 + 开关态（CronPage 数据源；上游为宿主级，不要求 session_id）
		m.resp(c, true, "", m.cronListData())

	case "cron_runs": // 运行账本历史（payload.job_id 可选过滤；CronPage「运行历史」）
		m.resp(c, true, "", map[string]any{"runs": m.cronRunsData(str(c.Payload, "job_id"))})

	case "cron_runs_detail": // 某次运行的本地化历史快照（CronPage → 详情页数据源）
		m.resp(c, true, "", m.cronRunDetailData(str(c.Payload, "job_id"), str(c.Payload, "run_id")))

	case "cron_create": // 宿主侧新建（桌面页手动添加；复用 Store.Add，编辑后可改成 host 表单）
		if m.cron == nil {
			m.resp(c, false, "cron 不可用", nil)
			return
		}
		ws = str(c.Payload, "workspace")
		expr := str(c.Payload, "cron")
		prm := str(c.Payload, "prompt")
		rec := true
		if v, ok := c.Payload["recurring"].(bool); ok {
			rec = v
		}
		j, err := m.cron.Add(expr, prm, rec, ws, str(c.Payload, "session_id"), time.Now())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		// IM 推送目标（可选）：cron_create 带 im_chat 时绑定
		if ic := parseCronIMChat(c.Payload["im_chat"]); ic != nil {
			if _, err := m.cron.SetIMChat(j.ID, ic); err != nil {
				m.resp(c, false, err.Error(), nil)
				return
			}
		}
		m.emitCronEvent("cron_changed", nil)
		m.resp(c, true, "", map[string]any{"id": j.ID})

	case "cron_update": // 宿主侧编辑（prompt/cron/recurring/paused 可选）
		if m.cron == nil {
			m.resp(c, false, "cron 不可用", nil)
			return
		}
		var (
			prm *string
			exp *string
			r   *bool
			p   *bool
		)
		if v := str(c.Payload, "prompt"); v != "" {
			prm = &v
		}
		if v := str(c.Payload, "cron"); v != "" {
			exp = &v
		}
		if v, ok := c.Payload["recurring"].(bool); ok {
			r = &v
		}
		if v, ok := c.Payload["paused"].(bool); ok {
			p = &v
		}
		j, err := m.cron.Update(str(c.Payload, "id"), prm, exp, r, p, time.Now())
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		// IM 推送目标（可选）：payload 有 im_chat 键时设置/清除
		if raw, ok := c.Payload["im_chat"]; ok {
			ic := parseCronIMChat(raw)
			if j, err = m.cron.SetIMChat(j.ID, ic); err != nil {
				m.resp(c, false, err.Error(), nil)
				return
			}
		}
		m.emitCronEvent("cron_changed", nil)
		m.resp(c, true, "", map[string]any{"id": j.ID, "next_run": j.NextRun})

	case "cron_delete": // 宿主侧删除（CronPage 删除按钮；连带清理该任务独立历史目录）
		if m.cron == nil {
			m.resp(c, false, "cron 不可用", nil)
			return
		}
		if err := m.cron.Delete(str(c.Payload, "id")); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		// 独立历史目录清理由 Store.OnDelete 钩子完成（工具与宿主命令共用同一路径）
		m.emitCronEvent("cron_changed", nil)
		m.resp(c, true, "", nil)

	case "cron_clean": // 手动触发一次派生会话清理（返回清理数）
		m.resp(c, true, "", map[string]any{"cleaned": m.cronClean(time.Now())})

	case CmdDeleteSession: // 删除单个会话：停进程内会话 + 删磁盘 sessions/events 日志
		if ws == "" || sid == "" {
			m.resp(c, false, "workspace/session_id 为空", nil)
			return
		}
		m.deleteSession(ws, sid)
		m.resp(c, true, "", map[string]any{"sessions": m.listSessions(ws)})

	case CmdSessionPin: // 置顶/取消置顶会话（持久化到 ~/.go-code/sessions/<wsKey>/sessions.json）
		if ws == "" || sid == "" {
			m.resp(c, false, "workspace/session_id 为空", nil)
			return
		}
		if err := session.ValidateSessionID(sid); err != nil { // 统一入口校验（防异常 sid 进入 pin 存储）
			m.resp(c, false, err.Error(), nil)
			return
		}
		pinned := c.Payload["pinned"] == true
		pins, ok := sessionPins.set(workspaceKey(ws), sid, pinned)
		if !ok {
			m.resp(c, false, "置顶会话已达上限（5 个）", map[string]any{"pinned": pins})
			return
		}
		m.resp(c, true, "", map[string]any{"pinned": pins})

	case "delete_workspace": // 删除工作区：停全部会话 + 删 harness 元数据（skills.json；用户文件与项目配置保留）
		if ws == "" {
			m.resp(c, false, "workspace 为空", nil)
			return
		}
		m.deleteWorkspace(ws)
		m.resp(c, true, "", nil)

	case CmdShutdown:
		if ws != "" {
			m.shutdownWorkspace(ws) // 关闭该工作区全部会话（状态落盘）
		} else {
			m.shutdownAll() // 兜底：全部工作区
		}
		m.resp(c, true, "", nil)

	default:
		m.resp(c, false, "unknown command: "+c.Type.String(), nil)
	}
}

// shutdownWorkspace：优雅关闭该工作区全部会话（状态落盘）。
// 幂等守卫：已 cancel 的会话（上一次 shutdown 已关 / 进程内已取消）跳过——
// shutdown 命令 + stdin EOF 兜底（shutdownAll）会重复调用，不重复 Shutdown
// （SDK Shutdown 幂等但会再次发 SessionClosed 事件）。
func (m *manager) shutdownWorkspace(wsPath string) {
	// 已从 manager 摘除的 workspace（deleteWorkspace 后）不重建 runtime——
	// m.runtime 会新建空 workspace（nil mcp/plugins），cleanup 调用时 panic。
	m.mu.Lock()
	_, registered := m.workspaces[wsPath]
	m.mu.Unlock()
	if !registered {
		return
	}
	ws := m.runtime(wsPath)
	ws.mu.Lock()
	items := make([]*bridgeSession, 0, len(ws.sessions))
	for _, bs := range ws.sessions {
		if bs.isCron {
			continue // 定时任务派生会话归 Cron 管理，不随 workspace 关闭
		}
		items = append(items, bs)
	}
	ws.mu.Unlock()
	for _, bs := range items {
		if bs.ctx.Err() != nil {
			continue // 已关闭，跳过（避免重复 SessionClosed）
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = bs.s.Shutdown(ctx)
		cancel()
		bs.cancel()
		if m.hub != nil {
			m.hub.Unregister(bs.id) // Session Mesh 注销
		}
	}
	if m.hub != nil {
		m.pushMeshSessions() // 会话批量注销 → 对端 session_list 立即剔除
	}
	ws.mcp.Close()                         // 关闭工作区 MCP 服务器连接（子进程清理；会话复用按需重连）
	ws.plugins.Close(context.Background()) // 关闭插件私有连接（Browser Use 子进程清理）
}

// shutdownAll：stdin EOF 兜底，全部工作区优雅关闭
func (m *manager) shutdownAll() {
	m.mu.Lock()
	paths := make([]string, 0, len(m.workspaces))
	for p := range m.workspaces {
		paths = append(paths, p)
	}
	m.mu.Unlock()
	for _, p := range paths {
		m.shutdownWorkspace(p)
	}
	// MCP 配置监听停止（进程退出前显式收尾：watcher goroutine + fd）。
	// 此处不持 m.mu（stop 会等 apply 收尾，而 apply 要取 m.mu）。
	m.mu.Lock()
	wss := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wss = append(wss, ws)
	}
	m.mu.Unlock()
	for _, ws := range wss {
		ws.stopMCPWatch()
	}
	// Session Mesh 远程网关关闭（mesh 常驻 manager 级，不随工作区）
	if m.meshGW != nil {
		m.meshGW.Stop()
		m.meshGW = nil
		m.hub.SetRemote(false, nil)
	}
}

// deleteSession 删除一个会话：停进程内会话（不泄漏 goroutine）+ 删磁盘 sessions/events 日志
// + 清理子任务输出 journal（<sid>/agents，docs/SUBAGENT_OUTPUT_JOURNAL_TASKOUTPUT.md §3.6）。
// 工作区可能从未在此进程注册（冷启动后直接删），用 m.workspaces 查而非 runtime()（后者会建目录）。
func (m *manager) deleteSession(wsPath, id string) {
	// 路径安全（V2 SECURITY-02）：id 参与文件删除路径拼接，必须校验（防 `..`/绝对路径穿越）。
	if err := session.ValidateSessionID(id); err != nil {
		m.out.writeLine(map[string]any{"event_type": "command_response", "id": -1, "ok": false, "error": err.Error()})
		return
	}
	m.mu.Lock()
	ws, ok := m.workspaces[wsPath]
	m.mu.Unlock()
	if ok {
		ws.mu.Lock()
		bs := ws.sessions[id]
		delete(ws.sessions, id)
		ws.mu.Unlock()
		if m.hub != nil {
			m.hub.Unregister(id) // Session Mesh 注销
			m.pushMeshSessions() // 会话删除 → 对端 session_list 立即剔除
		}
		if bs != nil {
			stopSession(bs)
		}
	}
	_ = os.Remove(filepath.Join(wsSessionsDir(wsPath), id+".jsonl"))
	_ = os.Remove(filepath.Join(wsEventsDir(wsPath), id+".jsonl"))
	// record 文件（checkpoint-first 目标恢复源）+ 其 .bak 一并删除；coord 已 drain 完毕
	_ = os.Remove(filepath.Join(wsSessionsDir(wsPath), id+".record.json"))
	_ = os.Remove(filepath.Join(wsSessionsDir(wsPath), id+".record.json.bak"))
	removeSubagentJournals(wsSessionsDir(wsPath), id)
	// 清理该会话的置顶标记（防 pin 文件残留已删除会话）
	sessionPins.set(workspaceKey(wsPath), id, false)
}

// subagentsBase 子任务输出 journal 根目录（D1 路径规则：sessions/<wsKey>/<sid>/agents）。
// 子目录命名空间（与 record 文件 <sid>.record.json 平级、互不冲突）：
//   - listSessions 只扫 storeDir 顶层 *.jsonl，<sid>/ 是目录 → 跳过不误列
//   - delete_session 删 <sid>/agents 连代清理，不影响 <sid>.record.json
//   - 工作区级 RemoveAll(wsSessionsDir) 自动连带清理
func subagentsBase(storeDir, sid string) string {
	return filepath.Join(storeDir, sid, "agents")
}

// journalPathFor 解析子任务 journal 路径并确保父目录存在。
// 布局：sessions/<wsKey>/<sid>/agents/<taskId>.jsonl。
// taskId 强校验（^task-[a-z0-9]{8}$）为防路径穿越的第二道保险（第一道在 SDK 生成侧）；
// sid 同样校验（与 record 路径同源 validatePathComponent）。
func journalPathFor(storeDir, sid, taskID string) (string, error) {
	if !subagent.ValidTaskID(taskID) {
		return "", fmt.Errorf("invalid task id %q", taskID)
	}
	if err := session.ValidateSessionID(sid); err != nil {
		return "", fmt.Errorf("journal path: %w", err)
	}
	dir := subagentsBase(storeDir, sid)
	if err := os.MkdirAll(dir, 0o700); err != nil { // subagent journal 含敏感内容，0700
		return "", fmt.Errorf("mkdir subagent journal dir: %w", err)
	}
	return filepath.Join(dir, taskID+".jsonl"), nil
}

// removeSubagentJournals 删除某会话的全部子任务 journal（幂等；delete_session 与 cron 清理共用）。
// 只删 <sid>/agents 子目录，绝不动 <sid>.record.json 与 <sid>.jsonl（会话主体数据）。
func removeSubagentJournals(storeDir, sid string) {
	if err := session.ValidateSessionID(sid); err != nil {
		return
	}
	_ = os.RemoveAll(subagentsBase(storeDir, sid))
}

// deleteWorkspace 删除一个工作区的全部 HAI 元数据（{ws}/.go-code），保留用户工作区文件本身
// （那是用户真实项目，绝不代删）。停全部会话 → 摘除运行态 → 删元数据目录。
func (m *manager) deleteWorkspace(wsPath string) {
	m.mu.Lock()
	ws, ok := m.workspaces[wsPath]
	if ok {
		delete(m.workspaces, wsPath) // 先摘除运行态，阻止新命令路由到该工作区
	}
	m.mu.Unlock()
	if ok {
		ws.mu.Lock()
		items := make([]*bridgeSession, 0, len(ws.sessions))
		kept := map[string]*bridgeSession{}
		for id, bs := range ws.sessions {
			if bs.isCron {
				kept[id] = bs // 定时任务派生会话保留：归 Cron 管理（独立存储于 ~/.go-code/cron_sessions）
				continue
			}
			items = append(items, bs)
		}
		ws.sessions = kept
		ws.mu.Unlock()
		for _, bs := range items {
			stopSession(bs)
			if m.hub != nil {
				m.hub.Unregister(bs.id) // Session Mesh 注销
			}
		}
		if m.hub != nil {
			m.pushMeshSessions() // 工作区删除 → 对端 session_list 立即剔除
		}
		ws.mcp.Close()                         // 关闭工作区 MCP 服务器连接（子进程清理）
		ws.plugins.Close(context.Background()) // 关闭插件私有连接（Browser Use 子进程清理）
		ws.stopMCPWatch()                      // 停止配置监听（goroutine + fd 收尾；不得持锁调用）
	}
	// 只删 harness 自有元数据（工作区 .go-code 下的 skills.json），**保留**用户文件：
	// 项目级配置 {ws}/.go-code/settings.json（手写的 MCP 等）、skills/ 项目技能、
	// worktrees/ 都是用户数据。目录仅在清空后为空时移除（原实现 RemoveAll 整目录会把
	// 用户手写的项目配置一起删掉——分层加载引入项目级配置文件后这成了数据丢失）。
	wsMeta := filepath.Join(wsPath, ".go-code")
	_ = os.Remove(filepath.Join(wsMeta, "skills.json"))
	_ = os.Remove(wsMeta) // 非空 → 失败即保留（忽略：保留用户文件是预期结果）
	// 清理 APP 级（跨 workspace）的该工作区会话/事件数据
	_ = os.RemoveAll(wsSessionsDir(wsPath))
	_ = os.RemoveAll(wsEventsDir(wsPath))
}

// stopSession 优雅停止单个会话：Shutdown 落盘（随后即删，无碍）+ cancel 停 runLoop +
// 关闭事件日志句柄（防 fd 泄漏）+ 等待 checkpoint coordinator drain（防止删除后
// 异步 commit 又把 record 写回）。幂等：已 cancel 的跳过 Shutdown。
func stopSession(bs *bridgeSession) {
	if bs.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = bs.s.Shutdown(ctx)
		cancel()
		bs.cancel()
	}
	if bs.coord != nil {
		<-bs.coord.done // 等待剩余 checkpoint commit 完成（drain）
	}
	bs.eventsMu.Lock()
	if bs.eventsF != nil {
		_ = bs.eventsF.Close()
		bs.eventsF = nil
	}
	bs.eventsMu.Unlock()
}

// 扫描 store 目录：会话列表（磁盘 + 进程内活跃会话并集）
// 活跃会话可能尚未落盘（新建未跑 / 切走未关），必须并集否则 list 丢会话。
// 每条带 updated_at（unix ms，最近活跃），按 updated_at 降序、id 降序兜底 —— 最新在最上。
func (m *manager) listSessions(wsPath string) []map[string]any {
	ws := m.runtime(wsPath)
	seen := map[string]bool{}
	out := []map[string]any{}
	wsKey := workspaceKey(wsPath)

	ws.mu.Lock()
	for id, bs := range ws.sessions {
		if seen[id] {
			continue
		}
		if bs.isCron {
			continue // 定时任务派生会话独立归 Cron 管理，不出现在 workspace 会话列表
		}
		seen[id] = true
		out = append(out, map[string]any{
			"id":         id,
			"title":      resolveLiveSessionTitle(wsKey, bs, id),
			"updated_at": sessionUpdatedAt(ws, id, bs.lastActive.Load()),
			"pinned":     sessionPins.isPinned(wsKey, id),
		})
	}
	ws.mu.Unlock()

	entries, err := os.ReadDir(ws.storeDir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, map[string]any{
				"id":         id,
				"title":      resolveDiskSessionTitle(wsKey, filepath.Join(ws.storeDir, name), id),
				"updated_at": sessionUpdatedAt(ws, id, 0), // 冷磁盘会话：lastActive 无 → 文件 mtime 回退
				"pinned":     sessionPins.isPinned(wsKey, id),
			})
		}
	}
	// updated_at 降序；相同 → id 降序（前端只防御性再排）
	sort.Slice(out, func(i, j int) bool {
		ui := toInt64(out[i]["updated_at"])
		uj := toInt64(out[j]["updated_at"])
		if ui != uj {
			return ui > uj
		}
		return str(out[i], "id") > str(out[j], "id")
	})
	return out
}

func sessionUpdatedAt(ws *workspaceRuntime, id string, lastActive int64) int64 {
	if lastActive > 0 {
		return lastActive
	}
	if fi, err := os.Stat(filepath.Join(ws.eventsDir, id+".jsonl")); err == nil {
		return fi.ModTime().UnixMilli()
	}
	if fi, err := os.Stat(filepath.Join(ws.storeDir, id+".jsonl")); err == nil {
		return fi.ModTime().UnixMilli()
	}
	return 0
}

// resolveLiveSessionTitle 活跃会话标题（优先级：Session 内存 AI 标题（含恢复注入）
// > titles.json 缓存 > 首条 user 文本截断 > "New Chat"）。供 listSessions 与 mesh
// 标题桥共用。
func resolveLiveSessionTitle(wsKey string, bs *bridgeSession, id string) string {
	if bs != nil && bs.s != nil {
		if ai := bs.s.AITitle(); ai != "" {
			return ai
		}
	}
	if ai := sessionTitles.get(wsKey, id); ai != "" {
		return ai
	}
	if bs == nil {
		return defaultSessionTitle
	}
	first := firstUserText(bs.s.Messages())
	if first != "" {
		return truncateTitle(first)
	}
	return defaultSessionTitle
}

// resolveDiskSessionTitle 冷磁盘会话标题（无进程内 Session；读 jsonl 首条 user 文本
// 与 meta 里的 AI 标题）。优先级同 resolveLiveSessionTitle：AI 标题（缓存 → jsonl meta）
// > 首条 user 文本截断 > "New Chat"。
func resolveDiskSessionTitle(wsKey, path, id string) string {
	if ai := sessionTitles.get(wsKey, id); ai != "" {
		return ai
	}
	// jsonl meta 尾扫（AI 标题权威源；titles.json 缓存丢失/未同步时兜底）
	if title := readMetaTitleFromFile(path); title != "" {
		return title
	}
	first := firstUserTextFromFile(path)
	if first != "" {
		return truncateTitle(first)
	}
	return defaultSessionTitle
}

// readMetaTitleFromFile 读 jsonl meta 里的 AI 标题（session 包导出实现）。
func readMetaTitleFromFile(path string) string {
	return session.ReadMetaFromFile(path)
}

// liveTitle 进程内活跃会话标题（读内存不读盘）。保留签名供 mesh 标题桥使用；
// 标题优先 AI 生成（wsKey 从 bs.workspace 反推）。
func liveTitle(bs *bridgeSession, id string) string {
	if bs == nil {
		return defaultSessionTitle
	}
	return resolveLiveSessionTitle(workspaceKey(bs.workspace), bs, id)
}

// firstUserText 取消息列表里第一条非空 user 文本（AI 标题触发判定与标题回退共用）。
func firstUserText(msgs []core.Message) string {
	for _, m := range msgs {
		if m.Role != core.User {
			continue
		}
		for _, c := range m.Content {
			if c.Type == core.ContentTypeText {
				if t := strings.TrimSpace(c.Content); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// truncateTitle 标题截断（24 字符 + 省略号；AI 标题由生成侧限 15 字符）。
func truncateTitle(t string) string {
	if len(t) > 24 {
		t = t[:24] + "…"
	}
	return t
}

// restoredMessages：恢复会话时把 Messages() 转成 UI 可渲染形状（user/assistant 文本轮 +
// 图片内容块）。2026-08-22：携带完整图片块（type/content/mime_type）—— 此前只拼文本，
// 纯图片消息还会被整条丢弃（Text==""），导致「历史恢复时图片丢失」。
func restoredMessages(s *session.Session) []map[string]any {
	var out []map[string]any
	for _, m := range s.Messages() {
		if m.Role != core.User && m.Role != core.Assistant {
			continue
		}
		var text string
		var contents []map[string]any
		for _, c := range m.Content {
			switch c.Type {
			case core.ContentTypeText:
				text += c.Content
			case core.ContentTypeImage:
				contents = append(contents, map[string]any{"type": "image", "content": c.Content, "mime_type": c.MimeType})
			}
		}
		if len(m.ToolCalls) > 0 {
			continue
		}
		if strings.TrimSpace(text) == "" && len(contents) == 0 {
			continue
		}
		msg := map[string]any{"role": string(m.Role), "text": text}
		if len(contents) > 0 {
			msg["contents"] = contents
		}
		out = append(out, msg)
	}
	return out
}

// buildSubagentRegistry 构建工作区自定义 subagent 注册表（低 → 高，同名高层覆盖）：
//
//	~/.claude/agents        Claude Code 兼容层（宽松解析，最低：外部产品只读）
//	~/.go-code/agents       全局 go-code 命名空间（兼容旧路径）
//	~/.agents/agents        全局通用 Agent Skills 命名空间（跨工作区）
//	{ws}/.claude/agents     工作区 Claude Code 兼容层（宽松解析）
//	{ws}/agents             工作区 go-code 命名空间（兼容旧路径）
//	{ws}/.agents/agents     工作区通用 Agent Skills 命名空间（最高）
//
// 优先级：工作区 > 全局；同作用域内通用协议（.agents）> 自家旧路径（.go-code）>
// 外部兼容层（.claude，永不覆盖自家定义）。
func buildSubagentRegistry(ws string) *subagent.DefinitionRegistry {
	reg, err := subagent.NewLayeredDefinitionRegistry(
		subagent.Layer{Dir: claudeSubagentsDir(), Lenient: true},
		subagent.Layer{Dir: globalSubagentsDir()},
		subagent.Layer{Dir: globalAgentSubagentsDir()},
		subagent.Layer{Dir: filepath.Join(ws, ".claude", "agents"), Lenient: true},
		subagent.Layer{Dir: filepath.Join(ws, "agents")},
		subagent.Layer{Dir: filepath.Join(ws, skills.AgentDirName, "agents")},
	)
	if err != nil {
		return reg // 目录不可读：空注册表（功能降级，不影响其他命令）
	}
	return reg
}

// skillList 取工作区注册表全部技能元数据（含 Enabled 标志，宿主 UI 展示；远端技能附来源）。
func (m *manager) skillList(wsPath string) []map[string]any {
	reg := m.runtime(wsPath).skills
	src := m.remoteSources()
	out := []map[string]any{}
	for _, d := range reg.All() {
		item := map[string]any{"name": d.Name, "description": d.Description, "enabled": d.Enabled}
		if s, ok := src[d.Name]; ok {
			item["source"] = s // 远程安装的技能：展示来源（SkillsPage 区分远端）
		}
		out = append(out, item)
	}
	return out
}

// persistSkillsDisabled 从注册表当前状态同步持久化禁用集（skill_toggle 后调用）。
// 写盘失败（目录权限等）只记日志：开关本身已生效，不该因为持久化问题回滚用户操作；
// 但必须可见——静默失败会让「禁用」在重启后失效且无从排查。
func (m *manager) persistSkillsDisabled(wsPath string) {
	reg := m.runtime(wsPath).skills
	var disabled []string
	for _, d := range reg.All() {
		if !d.Enabled {
			disabled = append(disabled, d.Name)
		}
	}
	if err := saveSkillsDisabled(wsPath, disabled); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 技能禁用集持久化失败（%s）: %v\n", skillsEnabledPath(wsPath), err)
	}
}

// —— 远程技能下载（2026-08-15）：exec git 拉取 + 全局/工作区双作用域 ——
// remoteEnv 远程技能安装环境（全局技能根双路径 + 来源清单 + 市场清单）。
func (m *manager) remoteEnv() remote.Env {
	return remote.Env{
		AgentSkillsDir:  agentSkillsDir(),
		GlobalSkillsDir: globalSkillsDir(),
		ManifestPath:    homeCfgPath("remote-skills.json"),
		MarketplacePath: homeCfgPath("marketplaces.json"),
	}
}

// remoteSources 已安装远程技能名 → 来源 URL（skillList/skill_get 展示来源）。
func (m *manager) remoteSources() map[string]string {
	out := map[string]string{}
	for _, it := range remote.List(m.remoteEnv()) {
		out[it.Name] = it.Source
	}
	return out
}

// rebuildSkills 重建单工作区技能注册表 + 会话 loop（工作区作用域安装/卸载后生效）。
func (m *manager) rebuildSkills(wsPath string) {
	ws := m.runtime(wsPath)
	ws.mu.Lock()
	ws.skills = buildSkillsRegistry(wsPath)
	ws.mu.Unlock()
	m.rebuildWorkspace(wsPath)
}

// rebuildSubagents 重建单工作区自定义 subagent 注册表 + 会话 loop
// （工作区新增/修改定义文件后生效，/reload_skills 触发）。
func (m *manager) rebuildSubagents(wsPath string) {
	ws := m.runtime(wsPath)
	ws.mu.Lock()
	ws.subagents = buildSubagentRegistry(wsPath)
	ws.mu.Unlock()
	m.rebuildWorkspace(wsPath)
}

// rebuildAllSubagents 全局自定义 subagent 变更：全部工作区重新扫描 + 重建。
func (m *manager) rebuildAllSubagents() {
	m.mu.Lock()
	paths := make([]string, 0, len(m.workspaces))
	for p := range m.workspaces {
		paths = append(paths, p)
	}
	m.mu.Unlock()
	for _, p := range paths {
		m.rebuildSubagents(p)
	}
}

// rebuildAllSkills 全局技能安装/卸载：全部工作区重新扫描 + 重建（新技能全局可见）。
func (m *manager) rebuildAllSkills() {
	m.mu.Lock()
	paths := make([]string, 0, len(m.workspaces))
	for p := range m.workspaces {
		paths = append(paths, p)
	}
	m.mu.Unlock()
	for _, p := range paths {
		m.rebuildSkills(p)
	}
}

// —— MCP（2026-08-15）：每工作区 Manager + mcp_list/set/refresh ——
// parseMCPConfig 解析 mcp_set 的 servers payload（map[string]any → 配置表，校验归一化）。
func parseMCPConfig(raw any) (map[string]mcp.ServerConfig, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("mcp: servers 解析失败: %w", err)
	}
	var cfg map[string]mcp.ServerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("mcp: servers 非法 JSON: %w", err)
	}
	out := make(map[string]mcp.ServerConfig, len(cfg))
	for name, c := range cfg {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("mcp: 服务器名不能为空")
		}
		norm, err := c.Validate()
		if err != nil {
			return nil, err
		}
		out[name] = norm
	}
	return out, nil
}

// MCP 配置来源标签（Layer.Source → ServerStatus.Source → mcp_list 的 source 字段）。
const (
	mcpLayerUser    = "user"    // 用户级：~/.go-code/settings.json（MCP tab 管理，跨工作区）
	mcpLayerProject = "project" // 项目级：{ws}/.mcp.json 与 {ws}/.go-code/settings.json
)

// mcpConfigLayers MCP 配置分层（低 → 高优先级，同名后层覆盖前层）：
//  1. 用户级 ~/.go-code/settings.json（Electron main 写盘；MCP tab 全量保存的来源）
//  2. 项目级 {ws}/.mcp.json（Claude Code 项目级约定，跨工具可复用）
//  3. 项目级 {ws}/.go-code/settings.json（go-code 原生项目配置，最高优先；
//     为将来项目级 settings 打底——后续新增段（hooks/plugins 等）沿用这条读取链）
//
// 项目层既能整体覆盖用户层同名服务器，也能用 "enabled": false 关闭用户层某台服务器。
//
// 变更自动生效：三层文件由 fsnotify 监听（mcp_watch.go）——新建/改写/原子替换都热应用；
// 坏文件沿用上次有效内容（mcp/loader.go 的 Loader）。
//
// ⚠ 安全语义：项目层配置文件随仓库分发，其中的 stdio command 会在连接时**直接执行**
// 且继承本进程环境变量（含 provider key）。当前**无 project trust 门禁**——打开某仓库
// 即接受其 MCP 配置（用户决策 2026-09：v1 先打通分层；门禁/批准 UI 留作后续）。
// 与 Claude Code 的 `.mcp.json` 需批准不同，此处是「clone 即可执行」；同理 agent 的文件
// 工具也能写这两个文件 → 下轮会话即生效（配置变更无「需再批准」这道闸）。改进先于此处增门禁。
func mcpConfigLayers(ws string) []mcp.Layer {
	layers := []mcp.Layer{{Path: homeCfgPath("settings.json"), Source: mcpLayerUser}}
	return append(layers, projectMCPLayers(ws)...)
}

// projectMCPLayers 单个工作区的项目层（低 → 高：.mcp.json < .go-code/settings.json）。
func projectMCPLayers(ws string) []mcp.Layer {
	return []mcp.Layer{
		{Path: filepath.Join(ws, ".mcp.json"), Source: mcpLayerProject},
		{Path: filepath.Join(ws, ".go-code", "settings.json"), Source: mcpLayerProject},
	}
}

// applyMCPConfig 把新配置应用到全部已存在工作区的管理器：payload = **用户层**全量
// （MCP tab 保存内容），每工作区再叠加该项目自己的分层——项目层依旧覆盖用户层。
// 不可直接 SetConfig(payload)：那会把项目层服务器从活管理器里抹掉（要重启才回来）。
// 新工作区由 runtime() 读同一套分层获得。
//
// 走 Loader（同 watcher 一条链）：项目层坏文件沿用上次有效；内容与上次相同时不碰连接。
// 调用方随后 rebuildAll（工具面变更立即生效）。
func (m *manager) applyMCPConfig(cfg map[string]mcp.ServerConfig) {
	m.mu.Lock()
	wss := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wss = append(wss, ws)
	}
	m.mu.Unlock()
	for _, ws := range wss {
		m.syncMCPLayers(ws, cfg, mcpLayerUser, projectMCPLayers(ws.path)...)
	}
}

// buildSpawnLoop 子 agent 运行时（agent_spawn 的后台执行器）：
// 继承主 Agent 的全部工具（共享同一 ToolEngine 实例 eng——任何注册进主引擎的工具，
// 包括 bash/write/edit/agent_*/todo 等，子任务同样可见可用）。不再构建独立只读工具集，
// 对齐"Spawn 继承主 Agent 所有工具"。model/effort 复用主会话当前配置：
// SubAgent 与主 Agent 同模型同强度（switch_model/switch_effort/set_provider
// 重建时随 buildLoop 同步跟随）。
//
// 子 agent 的"人设/任务"完全由主 agent 生成的 agent_spawn task 参数承载（任务即 Prompt，
// 工具描述强制其自包含、含角色/约束/输出格式）。2026-08-25 起系统提示词与主会话统一：
// WithBaseSystemPrompt(DesktopCodePromptV2) 替换 SDK DefaultSystemPrompt——即
// "简洁契约（Harness/Background Tasks/Change discipline/Reporting）+ 动态 TaskPrompt"，
// 子 agent 的 system = DesktopCodePromptV2 + 工作目录/AGENTS.md/技能 + task（第一条 User 消息）。
//
// 2026-08-29：等价重构为 buildPersonaLoop(persona="")——agent_spawn 无 persona
// 参数，行为逐字节不变（见 buildPersonaLoop）。
// 2026-08-30：注入技能清单（sk）——子 agent 可用技能（load_skill 工具已在共享主引擎）；
// 不注入 subagent 清单（防递归：子 agent 不派生子 agent）。
func buildSpawnLoop(eng *tools.Engine, workspace, sid string, pc *providerConfig, prov, model, effort string, sk *skills.Registry) *agents.AgentLoop {
	return buildPersonaLoop(eng, workspace, sid, pc, prov, model, effort, "", sk)
}

// codeAgentHost 三个 loop 装配点的可寻址标识（C7 装配断言按宿主逐个断言，缺一即红）。
type codeAgentHost string

const (
	hostMainLoop    codeAgentHost = "buildLoop"
	hostSpawnLoop   codeAgentHost = "buildPersonaLoop"
	hostExploreLoop codeAgentHost = "buildExploreLoop"
)

// codeAgentCompressThreshold 自动压缩触发阈值（占上下文窗口比例）。**钉住 0.8**：
// 2026-08-30 D1/D2 曾试 0.8→0.72 提前压缩，实测子 Agent 高频压缩 + 重读循环，已回退；
// 用户 2026-09-18 再次明确「压缩阈值不改，就保持」。三宿主共用同一常量（此前是三处
// 字面量 0.8）——C7 的装配断言钉住它：谁想再动，必须先让测试红起来，而不是悄悄改一处。
const codeAgentCompressThreshold = 0.8

// codeAgentTuning 三宿主 loop 装配点上「易失的可调项」值类型（C7）。纯值 + 纯函数：
// 装配点只经 options() 生成 agents.Option，测试可直接断言字段与应用结果（无需
// agents 暴露 getter —— AgentLoop 的 Config 私有持有，装配后读不回来）。
//
// 为何把这三项单独提出来（它们都是「曾经在生产悄悄失效」的类型）：
//   - postToolNudge：agents.WithPostToolNudgeEnabled 入库后**只有测试接线、生产从未开启**
//     （2026-09-18 第三批 C1/A1a 才接上）⇒「看起来有、实际失效」的第三类样本本身
//     （2026-09-18 第三批的教训：宁可少一个开关，也不要一个「看起来有、实际失效」的
//     开关；B4 收尾门控正因它的前车之鉴而拒绝加
//     Config 开关）。触发条件极窄：仅「刚发生工具轮 + 本回归模型内容为空」时注入一次
//     （per-Run 仅一次）；空回复轮在任何宿主都不是合法结果 ⇒ 不给正常 run 增加轮次。
//   - goalAlignmentRounds：主会话接线到 settings 的 loadAgentSettings().ReminderRounds
//     （值随设置变）；子宿主**刻意不接线**（保持 agents 缺省 0 = 禁用）。
//   - compressThreshold：见 codeAgentCompressThreshold 注释。
type codeAgentTuning struct {
	postToolNudge bool
	// goalAlignmentRounds 非 nil = 本宿主接线 WithGoalAlignmentReminderRounds(该值)；
	// nil = 不传该选项（保持 agents 缺省 0 = 禁用）。用指针而非 0 值哨兵：0 本身是
	// 合法取值（显式禁用），与「本宿主不接线」必须可区分。
	goalAlignmentRounds *int
	compressThreshold   float64
}

// codeAgentTuningFor 返回某宿主的可调项取值（单一事实源）。
// 与提取前逐项一致：postToolNudge 三宿主全开；goalAlignmentRounds 仅主会话（值 =
// 装配时读 settings 的 loadAgentSettings().ReminderRounds，与提取前同一时点）；
// 压缩阈值三宿主同一常量。
func codeAgentTuningFor(host codeAgentHost) codeAgentTuning {
	t := codeAgentTuning{
		postToolNudge:     true,
		compressThreshold: codeAgentCompressThreshold,
	}
	if host == hostMainLoop {
		rounds := loadAgentSettings().ReminderRounds
		t.goalAlignmentRounds = &rounds
	}
	return t
}

// options 把可调项值翻译成 agents.Option —— 装配点生成这三项选项的唯一入口。
// 提炼安全性（重构等价性）：agents 的每个 With* 都是纯 setter（只写自己的 Config
// 字段、不读其它字段），故选项在切片中的先后次序不影响最终 Config；与其余 With*
// 的相对位置同样无关。逐项对应关系：postToolNudge → WithPostToolNudgeEnabled()；
// goalAlignmentRounds → WithGoalAlignmentReminderRounds(n)（仅接线时）；
// compressThreshold → WithCompressThreshold(t)。
func (t codeAgentTuning) options() []agents.Option {
	opts := make([]agents.Option, 0, 3)
	if t.postToolNudge {
		opts = append(opts, agents.WithPostToolNudgeEnabled())
	}
	if t.goalAlignmentRounds != nil {
		opts = append(opts, agents.WithGoalAlignmentReminderRounds(*t.goalAlignmentRounds))
	}
	opts = append(opts, agents.WithCompressThreshold(t.compressThreshold))
	return opts
}

// buildPersonaLoop 子 agent 运行时（spawn_agent 的按定义执行器）：
// 在 buildSpawnLoop 全部配置基础上，persona 非空时经 WithSystemPrompt 注入
// 产品层（自定义 subagent 定义文件正文 = 人设）。Base 层仍为
// DesktopCodePromptV2（简洁契约）——与 agent_spawn 完全同构，仅多一层人设；
// persona="" 时与旧 buildSpawnLoop 逐字节等价（不追加任何 Option）。
// sk 为技能注册表（与主会话同一实例）：注入技能发现清单（渐进披露），
// 子 agent 经 load_skill 激活技能；不注入 subagent 清单（防递归）。
func buildPersonaLoop(eng *tools.Engine, workspace, sid string, pc *providerConfig, prov, model, effort, persona string, sk *skills.Registry) *agents.AgentLoop {
	if effort == "" {
		effort = string(provider.ReasoningEffortLevelLow)
	}
	opts := []agents.Option{
		agents.WithProvider(pc.newProvider(prov)),
		agents.WithProviderName(prov),
		agents.WithModel(model),
		agents.WithToolEngine(eng),                              // 共享主 Agent 引擎 → 继承其全部工具
		agents.WithBaseSystemPrompt(prompt.DesktopCodePromptV2), // 与主会话共用简洁契约
		agents.WithReasoningEffort(provider.ReasoningEffortLevel(effort)),
		// 2026-09-18 统一超时/重试策略（与主 loop 一致）：首字预算 30s（agents 层），
		// HTTP 层 60s 仅等响应头的次级兜底
		// + 不限总时限（LLMTimeout 0）+ 首次+10 重试固定 1s 退避。见 buildLoop 注释。
		agents.WithLLMTimeout(0),
		agents.WithMaxRetries(11),
		agents.WithRetryBackoff(agents.FixedBackoff(time.Second)),
		agents.WithAgentMDDir(workspace), // 子任务共享工作区 AGENTS.md/CLAUDE.md
		agents.WithWorkingDir(workspace), // Current Workspace 环境层：声明子任务文件操作根目录
		agents.WithSkills(sk),            // 技能发现清单注入 system（渐进披露；与主会话同实例，启用过滤一致）
		// 2026-08-22：拷贝主 Agent 的上下文窗口与压缩配置（**拷贝而非共享**）——
		// 此前子 loop 缺省（无窗口/无压缩器），长任务上下文无限增长、必然撞超限。
		// 独立 NewLLMCompressor 实例：与主 loop 的压缩器互不影响（各自状态/调用互不干扰）。
		agents.WithMaxTokens(pc.maxTokensForIn(prov, model)),
		agents.WithContextWindow(pc.windowForIn(prov, model)),
		// 压缩配置（V2 的 A/B 对照见 docs/summary/压缩PromptV2优化分析-2026-08-30.md
		// 附录 C）+ SummaryPromptV3 + 缺节带反馈重试 + C5 近窗裁剪窗口注入。
		agents.WithCompressor(agents.NewLLMCompressor(pc.newProvider(prov), model,
			// 2026-09：压缩与主会话同策略（主会话的单次上界 = 首字预算 30s，在 agents
			// streamOnce 里生效；压缩调用是非流式，故这里靠自身超时档位）
			// —— 不限总时限（0）+ 首次+10 重试（11）固定 1s 退避。
			agents.WithCompressTimeout(0), agents.WithCompressMaxRetries(11), agents.WithCompressBackoff(agents.FixedBackoff(time.Second)), agents.WithKeepRecentMessages(10),
			agents.WithSummaryVersion(3), agents.WithCompressorRetryOnMissingSection(true),
			agents.WithSummaryMaxTokens(pc.maxTokensForIn(prov, model)),
			agents.WithSummaryContextWindow(pc.windowForIn(prov, model)),
			// 子 agent 与父会话同会话 id：OpenCode Go 网关要求辅助/子请求同样带
			// x-opencode-session（缺失 400），同 id 才能复用会话路由与缓存前缀。
			agents.WithCompressProviderName(prov), agents.WithCompressSessionID(sid))),
		// 子 agent 的会话标识（与父会话同 id；Responses prompt cache key +
		// OpenCode Go 网关 x-opencode-session 用）。
		agents.WithSessionID(sid),
	}
	// C7：易失可调项（postToolNudge / 压缩阈值）——装配点只经该值类型生成选项，
	// 装配断言（assembly_wiring_test.go）钉住「机制不会被悄悄关掉」。理由见 codeAgentTuning。
	opts = append(opts, codeAgentTuningFor(hostSpawnLoop).options()...)
	// 产品层：人设 + Computer Use 准则（若共享引擎含 computer_snapshot = 插件已启用，
	// 子 agent 继承全部工具 → 与主会话 buildLoop 对齐注入同一段桌面操控契约。
	// 2026-09 教训：子 agent 无契约时越界 bash 自救/盲试）。合并为单层 WithSystemPrompt。
	systemLayer := persona
	if eng != nil {
		if _, err := eng.GetTool(context.Background(), "computer_snapshot"); err == nil {
			systemLayer += computerPromptGuide + computerAppsGuide()
		}
	}
	if systemLayer != "" {
		opts = append(opts, agents.WithSystemPrompt(systemLayer))
	}
	// 采样默认（宿主决策 2026-08）：DeepSeek 显式 top_p=0.95 / temperature=1.0；OpenAI 厂商默认
	if temperature, topP := pc.samplingDefaults(); temperature != nil {
		opts = append(opts, agents.WithTemperature(*temperature), agents.WithTopP(*topP))
	}
	return agents.NewAgentLoop(opts...)
}

// exploreToolDescription subagent_explore 工具描述（专职用户问题分析）。
const exploreToolDescription = "Run a dedicated Explore subagent to analyze a user question against the workspace. " +
	"It has a restricted, analysis-only toolset (read_file / grep / bash / glob) and does NOT write or change any files. " +
	"\nWHEN TO USE: use this when you need to UNDERSTAND / RESEARCH before deciding — e.g. a user asks a question about the codebase, or you need to analyze requirements, trace how a feature works, or gather facts to form a plan. It returns immediately with a task_id (background, same as agent_spawn); the analysis is pushed back to this session automatically when done, so keep working on other things instead of waiting." +
	"\nWHEN NOT TO USE: use agent_spawn instead when you want a subagent that inherits the FULL toolset (can edit/write/bash) to actually DO a task. " +
	"Parameters: name (short label for the analysis, e.g. \"analyze api flow\"), task (the user question / what to analyze)."

// exploreToolEngine Explore 子 agent 的受限工具引擎：注入 read_file / grep / glob / bash
// + load_skill（技能渐进披露激活；2026-08-30 起 Explore 也能用技能，清单经 WithSkills 注入）。
// 不注入写变更工具（专职问题分析，不落写变更）。暴露为函数便于单测守卫工具集。
func exploreToolEngine(workspace string, sk *skills.Registry) *tools.Engine {
	allowed := map[string]bool{"read_file": true, "grep": true, "glob": true, "bash": true}
	eng := tools.NewToolEngine()
	for _, t := range builtin.NewFileTools(workspace).Tools() {
		if allowed[t.Name()] {
			eng.RegisterTool(context.Background(), t)
		}
	}
	if sk != nil {
		eng.RegisterTool(context.Background(), skills.NewLoadSkillTool(sk)) // load_skill：技能激活（禁用技能拒绝）
	}
	return eng
}

// buildExploreLoop Explore 子 agent 运行时（subagent_explore 的执行器）：独立 SystemPrompt +
// 受限工具集（read_file / grep / bash / glob + load_skill，仅分析不落写变更）。model/effort
// 复用主会话当前配置。sk 为技能注册表（与主会话同一实例）：注入技能发现清单（渐进披露）。
func buildExploreLoop(workspace, sid string, pc *providerConfig, prov, model, effort string, sk *skills.Registry) *agents.AgentLoop {
	if effort == "" {
		effort = string(provider.ReasoningEffortLevelLow)
	}
	opts := []agents.Option{
		agents.WithProvider(pc.newProvider(prov)),
		agents.WithProviderName(prov),
		agents.WithModel(model),
		agents.WithToolEngine(exploreToolEngine(workspace, sk)),
		agents.WithReasoningEffort(provider.ReasoningEffortLevel(effort)),
		// 2026-09-18 统一超时/重试策略（与主 loop 一致）：首字预算 30s（agents 层），
		// HTTP 层 60s 次级兜底 + 不限
		// 总时限（LLMTimeout 0）+ 首次+10 重试固定 1s 退避。
		agents.WithLLMTimeout(0),
		agents.WithMaxRetries(11),
		agents.WithRetryBackoff(agents.FixedBackoff(time.Second)),
		agents.WithSystemPrompt(prompt.DesktopExploreV2),
		agents.WithAgentMDDir(workspace), // 共享工作区 AGENTS.md/CLAUDE.md
		agents.WithWorkingDir(workspace), // Current Workspace 环境层
		agents.WithSkills(sk),            // 技能发现清单注入 system（渐进披露；与主会话同实例）
		// 2026-08-22：拷贝主 Agent 的上下文窗口与压缩配置（**拷贝而非共享**）——
		// 此前 Explore 子 loop 缺省（无窗口/无压缩器），长分析（大 grep/多文件）上下文
		// 无限增长、必然撞模型窗口。独立 NewLLMCompressor 实例：与主 loop 互不影响。
		agents.WithMaxTokens(pc.maxTokensForIn(prov, model)),
		agents.WithContextWindow(pc.windowForIn(prov, model)),
		// 压缩器参数与主会话同步（阈值/V3/缺节重试/近窗窗口）+ 2026-09 超时/重试
		// 同策略（不限总时限 / 首字预算 30s / 首次+10 重试固定 1s 退避）。
		agents.WithCompressor(agents.NewLLMCompressor(pc.newProvider(prov), model,
			agents.WithCompressTimeout(0), agents.WithCompressMaxRetries(11), agents.WithCompressBackoff(agents.FixedBackoff(time.Second)), agents.WithKeepRecentMessages(10),
			agents.WithSummaryVersion(3), agents.WithCompressorRetryOnMissingSection(true),
			agents.WithSummaryMaxTokens(pc.maxTokensForIn(prov, model)),
			agents.WithSummaryContextWindow(pc.windowForIn(prov, model)),
			agents.WithCompressProviderName(prov), agents.WithCompressSessionID(sid))),
		// Explore 与父会话同会话 id（OpenCode Go 网关 x-opencode-session 用）。
		agents.WithSessionID(sid),
	}
	// C7：易失可调项（postToolNudge / 压缩阈值）——同上，装配断言钉住，理由见 codeAgentTuning。
	opts = append(opts, codeAgentTuningFor(hostExploreLoop).options()...)
	if temperature, topP := pc.samplingDefaults(); temperature != nil {
		opts = append(opts, agents.WithTemperature(*temperature), agents.WithTopP(*topP))
	}
	return agents.NewAgentLoop(opts...)
}

// newSessionEngine 主会话工具引擎：文件工具 + todo + agent_* + plan_submit + ask_user。
// bgCtx 提供 Session 级 ctx（后台任务生命周期），reg 为 Session 共享的任务注册表
// （NewAgentTools 与 session.WithRegistry 必须同一实例），qw 为 ask_user 提问等待器
// （与 session.WithQuestionWaiter 同一实例）。按 persona profile 决定工具集：
// Work 无 bash（办公助理不跑命令）、无 agent_*/plan_submit（不派子 agent / 不规划）。
// newSessionEngine 主会话工具引擎：文件工具 + todo + agent_* + plan_submit + ask_user + load_skill。
// bgCtx 提供 Session 级 ctx（后台任务生命周期），reg 为 Session 共享的任务注册表
// （NewAgentTools 与 session.WithRegistry 必须同一实例），qw 为 ask_user 提问等待器
// （与 session.WithQuestionWaiter 同一实例），sk 为工作区技能注册表（load_skill 激活；
// subagentEffortRef 会话内子 agent 的档位持有者（2026-09 用户决策：子 agent 追随主 loop）。
//
// 为什么需要单独持有：switch_effort 只作用于主 loop（不重建会话），而子 agent 的 loop
// 是装配期创建的 —— 共享 loop（agent_spawn / subagent_explore 各一个，被所有派生任务复用）
// 可以就地 SetReasoningEffort；文件化 subagent 的 per-def loop 是每次调用时新建的，
// 由工厂读 current() 构造，天然拿到新档位。
//
// 装配点：newSessionEngine（track 共享 loop）→ buildLoop 返回给 bridgeSession →
// switch_effort 调 set 同步全部；loop 重建（切 model/persona/provider）时随新引擎换新实例。
type subagentEffortRef struct {
	mu    sync.RWMutex
	level provider.ReasoningEffortLevel
	loops []*agents.AgentLoop
}

func newSubagentEffortRef(level provider.ReasoningEffortLevel) *subagentEffortRef {
	return &subagentEffortRef{level: level}
}

// current 当前档位（新建 per-def loop 时读）。
func (r *subagentEffortRef) current() provider.ReasoningEffortLevel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.level
}

// track 登记共享子 agent loop：登记即按当前档位对齐（构建期传值可能滞后）。
func (r *subagentEffortRef) track(loops ...*agents.AgentLoop) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range loops {
		if l == nil {
			continue
		}
		l.SetReasoningEffort(r.level) // 幂等：与构建期档位一致时不改变行为
		r.loops = append(r.loops, l)
	}
}

// set 切换档位：登记过的共享 loop 立即同步（各自下一次 LLM 请求生效）。
func (r *subagentEffortRef) set(level provider.ReasoningEffortLevel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.level = level
	for _, l := range r.loops {
		l.SetReasoningEffort(level)
	}
}

// 与 agents.WithSkills 同一实例，启用过滤一致）。subagentsReg 为工作区自定义 subagent
// 定义注册表（spawn_agent 派发；与 agents.WithSubagents 同一实例）。mcpm 为工作区 MCP
// 管理器（跨会话共享连接；nil = 不注册 MCP 工具）。pyRT 为受管 Python 运行时
// （run_python 的固定解释器来源；nil = 不注册 run_python —— 未装配运行时的 SDK 用法
// 不注册比注册一个必然失败的工具更好，诊断留给宿主）。按 persona profile 决定工具集：
// Work 无 bash（办公助理不跑命令）、无 agent_*/plan_submit（不派子 agent / 不规划）。
func newSessionEngine(workspace, sid string, cs *cron.Store, pc *providerConfig, prov, model, effort string, bgCtx func() context.Context, reg *subagent.Registry, qw *events.QuestionWaiter, sk *skills.Registry, subagentsReg *subagent.DefinitionRegistry, mcpm *mcp.Manager, plg *plugin.Registry, profile personaProfile, sbx sandbox.Sandbox, hub *session.Hub, pyRT *goruntime.Runtime, egoOnLoad func(string), egoBashBeforeExec func(string)) (*tools.Engine, *subagentEffortRef) {
	if effort == "" {
		effort = string(provider.ReasoningEffortLevelLow)
	}
	// 子 agent 档位持有者：装配期与主 loop 同档，之后由 switch_effort 同步（见类型注释）。
	effortRef := newSubagentEffortRef(provider.ReasoningEffortLevel(effort))
	eng := tools.NewToolEngine()
	ft := builtin.NewFileTools(workspace)
	if sbx != nil {
		// bash 沙箱后端（macOS Seatbelt + WithRelease 放行门）：注入后 bash 命令在沙箱内执行。
		ft = ft.WithBashSandbox(sbx)
	}
	if !profile.withBash {
		// Work（无 bash）：read_file 拒绝二进制文件时的提示改走本会话真有的路径 ——
		// 工具集是 per-profile 装配的，只有这一层知道「本会话有没有 bash」。
		// pyRT 同理由传进提示：未装配受管运行时 → 下面的 run_python 不注册 → 提示里
		// 也就不能提它（否则又是一条指向不存在工具的建议）。
		ft = ft.WithBinaryHint(func(p string) string { return workBinaryHint(p, pyRT != nil) })
	}
	for _, t := range ft.Tools() {
		if bt, isBash := t.(*builtin.BashTool); isBash {
			if !profile.withBash {
				continue // Work：不注册 bash
			}
			// ego lite 激活钩子（2026-09）：bash 执行 ego-browser CLI 且 app 刚被自拉起
			// → 请求激活窗口（覆盖「用户中途关 app，技能已加载不再触发 load_skill」场景）。
			if egoBashBeforeExec != nil {
				bt.BeforeExec = egoBashBeforeExec
			}
		}
		eng.RegisterTool(context.Background(), t)
	}
	for _, t := range todo.Tools() { // 会话级待办（todo_add/update/list）；Store 由 Session 注入
		eng.RegisterTool(context.Background(), t)
	}
	// run_python：办公/data 执行通道（受管 Python 运行时 + 与 bash 同一个沙箱后端）。
	// work 与 code 都注册 —— 办公能力不是 work 专属；对 work 而言这是唯一执行通道
	//（bash 被 withBash 门控剔掉，见上）。pyRT 为 nil 时（SDK 层未装配运行时）不注册：
	// 注册一个必然失败的工具只会让模型白耗轮次。
	// 不经 FileTools.Tools() 注册：那份清单被多处复用（Explore 白名单、schema 巡检、
	// 测试断言），往里加会让 Explore 子 agent 也拿到执行通道。
	if pyRT != nil {
		eng.RegisterTool(context.Background(), builtin.NewRunPythonTool(workspace, pyRT, sbx))
	}
	if profile.withAgent {
		// Spawn 子 agent 继承主 Agent 的全部工具：共享同一 ToolEngine 实例（而非独立只读
		// 工具集），子任务可读可写可执行，对齐"继承全部工具"（见 docs/DESKTOP.md §5.6）。
		spawnLoop := buildSpawnLoop(eng, workspace, sid, pc, prov, model, effort, sk)
		effortRef.track(spawnLoop) // 主 loop 切档时同步（子 agent 追随主 loop）
		// 子任务输出 journal 路径解析器（D1）：sessions/<wsKey>/<sid>/agents/<taskId>.jsonl。
		// 解析失败（含 taskId 非法）降级不落盘——journal 是尽力而为的旁路，绝不影响 spawn。
		journalPath := func(taskID string) (string, error) {
			return journalPathFor(wsSessionsDir(workspace), sid, taskID)
		}
		for _, t := range subagent.NewAgentTools(spawnLoop, bgCtx, reg, journalPath, subagentsReg,
			func(def *subagent.AgentDefinition) *agents.AgentLoop {
				// 文件化 subagent（agent_spawn task 省略时按 name 加载）：人设 loop
				// 每次调用新建 → 读当前档位（switch_effort 后派生的子 agent 立即同档）。
				return buildPersonaLoop(eng, workspace, sid, pc, prov, model, string(effortRef.current()), def.Instructions, sk)
			}) {
			eng.RegisterTool(context.Background(), t) // agent_spawn（动态 + 文件化双模式）/send/interrupt/list/output
		}
		// Explore 子 agent：专职用户问题分析，仅 read_file/grep/bash/glob 四个工具。
		// Slots = 注册表辅池（只读子 agent 独立上限）：explore 占满不饿死主池的 agent_spawn。
		exploreLoop := buildExploreLoop(workspace, sid, pc, prov, model, effort, sk)
		effortRef.track(exploreLoop) // 主 loop 切档时同步
		eng.RegisterTool(context.Background(), subagent.NewBackgroundTool(subagent.Spec{
			Name: "subagent_explore", Description: exploreToolDescription, Loop: exploreLoop, Background: true, JournalPath: journalPath, Slots: reg.AuxSlots(),
		}, bgCtx, reg))
		// plan_submit 不再注册：计划/文档使用 write_file，任务拆分使用 todo 工具。
	}
	// Git Worktree 工具：仅在 Code 模式（withBash）引入；worktree 统一放 {workspace}/.go-code/worktrees，
	// 目录自动创建，创建时需指定 worktree 名字（name）。
	if profile.withBash {
		eng.RegisterTool(context.Background(), builtin.NewGitWorktreeTool(workspace))
	}
	eng.RegisterTool(context.Background(), question.NewTool(qw)) // ask_user：模型→用户提问（HITL 反方向）
	loadSkill := skills.NewLoadSkillTool(sk)                     // load_skill：技能渐进披露激活（禁用技能拒绝）
	// ego lite 激活钩子（2026-09）：加载 ego-browser 技能（引擎=ego）→ 首次把 ego lite
	// 窗口带到前台 + UI 提示（模型决定用 ego 浏览器的语义起点，比 bash 触发更精准）。
	if egoOnLoad != nil {
		loadSkill.WithOnLoad(egoOnLoad)
	}
	eng.RegisterTool(context.Background(), loadSkill)
	if mcpm != nil {
		// MCP 工具（mcp__<server>__<tool>）：惰性连接（connectTimeout 内），连接失败
		// 的服务器跳过（状态经 mcp_list 暴露，retryCooldown 防反复阻塞）。
		for _, t := range mcpm.Tools(context.Background()) {
			eng.RegisterTool(context.Background(), t)
		}
	}
	if plg != nil {
		// 已启用插件的工具面（Browser Use：私有连接，browser 包 Enable 时建立）。
		for _, t := range plg.EnabledTools(context.Background()) {
			eng.RegisterTool(context.Background(), t)
		}
		// 看图工具门控（档 V，见 docs/COMPUTER_USE_DESIGN.md §2.1）：仅当当前模型
		// 支持图片输入（注册表 SupportsImage）才注册 computer_screenshot —— 纯文本
		// 模型注册了也用不上（图片会被翻译层降级成占位文本），反而多占一份工具 schema。
		if pc != nil && pc.registry != nil && pc.registry.SupportsImageInput(prov, model) {
			if cap, ok := plg.Get(computerplugin.ID); ok {
				if ep, ok := cap.(interface{ Executor() *computer.Executor }); ok {
					if exec := ep.Executor(); exec != nil {
						for _, t := range computer.VisionTools(exec) {
							eng.RegisterTool(context.Background(), t)
						}
					}
				}
			}
		}
	}
	// 定时任务工具门控：设置 cron.enabled=true 才注册（CronCreate/CronList/CronUpdate/CronDelete）。
	// 任务定义/账本在宿主级 Store；本会话绑定自己的 workspace/sid 作为触发目标。
	if cs != nil && loadCronSettings().Enabled {
		for _, t := range cron.NewTools(cs, cron.ToolBound{Workspace: workspace, SessionID: sid}) {
			eng.RegisterTool(context.Background(), t)
		}
	}
	// Session Mesh 工具门控：设置 mesh.session_enabled=true 才注册（session_send/session_list）。
	// 默认关闭 = 零侵入；开关热更新见 applyMeshSettings（rebuildAll 兜底移除）。
	if hub != nil && loadMeshSettings().SessionEnabled {
		for _, t := range session.NewMeshTools(hub, func() *session.Session { return hub.Get(sid) }) {
			eng.RegisterTool(context.Background(), t)
		}
	}
	return eng, effortRef
}

// 组装：指定 provider + 文件工具（workspace 为根）+ agent loop（per-session，effort 构建期）。
// profile 决定系统提示词 + 工具集（Persona 切换重建 loop 时复用）；qw 与 Session 共享提问等待器；
// sk 与 newSessionEngine 同一实例（技能发现清单注入 system + /skills 命令；启用过滤一致）。
// 模型窗口（WithContextWindow）与输出上限（WithMaxTokens）按 prov 显式解析（问题六：
// 不随全局活跃 Provider 漂移——跨 Provider 会话重建时窗口/端点/价表保持本会话归属）。
// Provider 面板标记 1M / 配置 max_tokens 的模型 → 真 1M 上下文 + 对应单次输出上限。
func (m *manager) buildLoop(workspace, sid, prov, model, effort string, bgCtx func() context.Context, reg *subagent.Registry, qw *events.QuestionWaiter, sk *skills.Registry, profile personaProfile, gate sandbox.ReleaseGate) (*agents.AgentLoop, *subagentEffortRef) {
	if effort == "" {
		effort = string(provider.ReasoningEffortLevelLow)
	}
	// 受管 Python 运行时（run_python 的解释器来源；manager 级单例，见 pythonRuntime 注释）。
	// 在此就地取而不加 newSessionEngine 参数：buildLoop 是唯一的会话装配入口。
	pyRT := m.pythonRuntime()
	// Computer Use 插件启用 → 系统提示词追加桌面操控行为准则 + 已授权 App 清单
	// （§8 落地）：引导模型「语义快照优先、失败重新观察而非 bash 自救、只给文本
	// 目标」；清单让模型用授权时记录的准确显示名唤起/激活（而非用户口语简称猜名）。
	// 模型自由决策时易固执重试失败调用/蔓延 bash（真机 LLM E2E 教训 2026-09）。
	sysPrompt := profile.systemPrompt
	if plg := m.runtime(workspace).plugins; plg != nil {
		if cap, ok := plg.Get(computerplugin.ID); ok {
			if ec, ok2 := cap.(interface{ IsEnabled() bool }); ok2 && ec.IsEnabled() {
				sysPrompt = sysPrompt + computerPromptGuide + computerAppsGuide()
			}
		}
	}
	// MCP servers 自述的 instructions（"什么时候该用哪个工具"）→ 系统提示词。
	// 背景：MCP 的 initialize 响应可携带 instructions，Claude Code / Codex 都会把它注入
	// 系统提示词；本仓库早期把它丢掉了（mcp/server.go 的 `if _, err := c.Initialize(...)`），
	// 模型因此只看到一堆同名工具。例：graft 的 instructions 有 953 字符，说明"一次调用通常
	// 就够、不要链式调用"——丢掉它，第三方工具的会用率会明显下降。
	// 装配期取一次（会触发服务器惰性首连）；2s 预算内没连上的服务器本轮不贡献该层，
	// 下次 loop 重建时补上（其工具本来也尚不可用）。
	if mm := m.runtime(workspace).mcp; mm != nil {
		ictx, icancel := context.WithTimeout(context.Background(), mcpInstructionsTimeout)
		if txt := mcp.InstructionsText(mm.Instructions(ictx)); txt != "" {
			sysPrompt = sysPrompt + "\n\n## MCP servers\n" + txt
		}
		icancel()
	}

	// 执行通道沙箱装配（见 execSandbox 注释）。
	sbx := m.execSandbox(workspace, sid, profile, gate, pyRT)

	// TEMP-STARTUP A7：newSessionEngine（工具注册）
	engStart := time.Now()
	eng, effortRef := newSessionEngine(workspace, sid, m.cron, m.provCfg, prov, model, effort, bgCtx, reg, qw, sk, m.runtime(workspace).subagents, m.runtime(workspace).mcp, m.runtime(workspace).plugins, profile, sbx, m.hub, pyRT, m.loadSkillOnLoadHook(), m.bashEgoBeforeExec())
	startupLog("A7.newSessionEngine", engStart)
	// TEMP-STARTUP A7：newProvider
	pvStart := time.Now()
	p := m.provCfg.newProvider(prov)
	startupLog("A7.newProvider", pvStart)
	opts := []agents.Option{
		agents.WithProvider(p),
		agents.WithProviderName(prov),
		agents.WithModel(model),
		agents.WithToolEngine(eng),
		agents.WithReasoningEffort(provider.ReasoningEffortLevel(effort)),
		agents.WithMaxTokens(m.provCfg.maxTokensForIn(prov, model)),
		// 2026-09-18 超时/重试策略（用户决策；三层各管一段，勿混用）：
		//   - 首字预算：agents.FirstChunkTimeout（默认 30s，本装配未覆盖）—— **单次尝试**
		//     从请求发出到收到模型首个流事件的上限；首个事件一到即解除，流开始后
		//     **不限总时长**（reasoning 数分钟 / 大文档生成不受切）。命中 = 上游无响应
		//     → 记一次失败并重试（可重试的上游侧错误）。
		//   - LLMTimeout(0) = 不设整轮总时限（同上：流开始后不限制）。
		//   - 流内停滞预算：agents.StreamStallTimeout（默认 3min，本装配未覆盖）—— 首个
		//     事件之后「相邻两个事件之间的静默」上限；命中 = 上游连接挂死（TCP 不断开也
		//     不再发字节）→ 记一次失败并重试。只界静默、不界总时长：持续产出的长 reasoning /
		//     大文档生成每个事件都重置计时，命中不了（反馈样本：GLM 流中段静默 >5min 无输出，
		//     此前只能等对端 RST 或用户手动停止）。
		//   - HTTP 层 ResponseHeaderTimeout=60s（providers.go llmHTTPClient）：次级兜底，
		//     只在「等响应头」这一段；策略本体是上面的 30s 首字预算。
		//   - 重试：首次 + 10 次重试（MaxRetries=11 含首次），每次失败固定退避 1s 快速重试；
		//     耗尽不再重试（streamWithRetry willRetry=false 终止）。最坏等待 ≈ 11×(30s首字+1s退避)。
		agents.WithLLMTimeout(0), // 不限整轮总时限（单次的上界是首字预算 30s）
		agents.WithMaxRetries(11),
		agents.WithRetryBackoff(agents.FixedBackoff(time.Second)),
		agents.WithContextWindow(m.provCfg.windowForIn(prov, model)),
		agents.WithSystemPrompt(sysPrompt),
	}
	// C7：易失可调项（postToolNudge / goalAlignmentRounds / 压缩阈值）——装配点只经该值
	// 类型生成选项，装配断言（assembly_wiring_test.go）钉住「机制不会被悄悄关掉」；
	// 三者各自的失效史与理由见 codeAgentTuning。
	opts = append(opts, codeAgentTuningFor(hostMainLoop).options()...)
	// Code（DesktopCodePromptV2 自带 Harness/Background Tasks 契约）：停用 SDK Base，
	// 避免同一条款在 system 首条重复出现；Work 保留 DefaultSystemPrompt + DesktopWork 双层。
	if profile.replaceBase {
		opts = append(opts, agents.WithBaseSystemPrompt(""))
	}
	opts = append(opts,
		agents.WithSkills(sk),                                // 技能发现清单注入 system（渐进披露）+ /skills 命令
		agents.WithSubagents(m.runtime(workspace).subagents), // 自定义 subagent 清单注入 system（渐进披露）+ spawn_agent
		agents.WithAgentMDDir(workspace),                     // 工作区 AGENTS.md/CLAUDE.md 递归发现（工作记忆层）
		agents.WithWorkingDir(workspace),                     // Current Workspace 环境层：声明本会话文件操作根目录
		// @引用 PDF 文本抽取（2026-09）：PDF 结构层是纯 ASCII，agents 侧的二进制判定
		// 读不出正文（压缩流 → 报「二进制不读内容」；未压缩 → 把 PDF 源码当正文注入）。
		// 抽取要 pypdf + 受管运行时 + 沙箱，属宿主能力面 → 经钩子注入（agents 包不
		// import tools/runtime）。pyRT 恒非 nil（pythonRuntime 懒创建），未 bootstrap
		// 时闭包内 Ensure 报错 → 调用方回退既有行为，不影响其它格式的引用展开。
		agents.WithRefExtractor(newPDFExtractor(pyRT, func(ws string) sandbox.Sandbox {
			return m.readOnlyConvertSandbox(ws, "pdf_extract")
		}, workspace)),
		// 自动压缩 + 主动压缩（compact / summary 命令）统一走 LLMCompressor。
		// KeepRecentMessages=10（问题三）：滚动近窗原文保留最近交换，工具回执
		// 不再只靠摘要转述；splitMessages 按完整 assistant→tool 组截断防悬空配对。
		// 阈值与 SummaryPromptV3（2026-08-30 GPN 交接单重构）+ 缺节带反馈重试 +
		// C5 近窗裁剪窗口注入；阈值取值见 codeAgentCompressThreshold。
		agents.WithCompressor(agents.NewLLMCompressor(p, model,
			// 2026-09-18：压缩与主会话同策略 —— 不限总时限（0）+ 首字预算 30s
			//（SDK 默认，agents.FirstChunkTimeout）+ 流内停滞预算 3min（SDK 默认，
			// agents.LLMCompressor.StallTimeout）+ 首次+10 重试固定 1s 退避
			//（HTTP 层 ResponseHeaderTimeout=60s 仅次级兜底）。
			agents.WithCompressTimeout(0), agents.WithCompressMaxRetries(11), agents.WithCompressBackoff(agents.FixedBackoff(time.Second)), agents.WithKeepRecentMessages(10),
			agents.WithSummaryVersion(3), agents.WithCompressorRetryOnMissingSection(true),
			// 摘要输出上限 = 模型真实 max_tokens（与主对话一致）：默认 8192 对
			// 1M 窗口 / 560K 输入的手动 summary 必然 finish_reason=length 截断失败。
			agents.WithSummaryMaxTokens(m.provCfg.maxTokensForIn(prov, model)),
			agents.WithSummaryContextWindow(m.provCfg.windowForIn(prov, model)),
			agents.WithCompressProviderName(prov), agents.WithCompressSessionID(sid))),
	)
	// D3（方案 B2/C3.2）：stop 检查点在途任务提醒（Work 无 agent_* 工具时快照恒空
	// = 天然 no-op）+ 自动压缩失败退避 3min（防失控重试，实测 211k/231k 样本）。
	opts = append(opts,
		agents.WithPendingTasksNudge(),
		agents.WithCompactBackoff(3*time.Minute),
		// 会话稳定标识：Responses prompt cache key + session affinity 头（对齐 pi
		// options.sessionId —— 同一会话多轮共享缓存前缀；空 sid 不启用）。
		agents.WithSessionID(sid),
	)
	// 采样默认（宿主决策 2026-08）：DeepSeek 显式 top_p=0.95 / temperature=1.0；OpenAI 厂商默认
	if temperature, topP := m.provCfg.samplingDefaults(); temperature != nil {
		opts = append(opts, agents.WithTemperature(*temperature), agents.WithTopP(*topP))
	}
	// 推理内容全量回传（宿主决策 2026-08-18）：OpenCode Go 网关对全部思考模型要求
	// reasoning_content 原样回传（工具调用轮缺失即 400）。autoRetainReasoning 只按
	// 模型名覆盖 deepseek/kimi/glm/qwen…，对网关未知/边缘模型（如 gpt-5.6-luna）会漏，
	// 此处按 provider 全量强制回传兜底。
	if m.provCfg.isOpenCodeGateway() {
		opts = append(opts, agents.WithRetainReasoning(true))
	}
	// TEMP-STARTUP A7：NewAgentLoop
	loopStart := time.Now()
	loop := agents.NewAgentLoop(opts...)
	startupLog("A7.NewAgentLoop", loopStart)
	// 同时返回子 agent 档位持有者：bridgeSession 保存它，switch_effort 据此同步
	// 子 agent（spawn/explore/文件化人设 loop）—— 子 agent 追随主 loop。
	return loop, effortRef
}

// rebuildLoop 按当前 persona/model/effort 重建会话 loop（switch_model/switch_persona/set_provider 共用）。
// loop 构建期捕获 provider + 模型窗口 + 输出上限（按 bs.provider 显式解析，问题六：
// 跨 Provider 会话不随全局活跃漂移），重建即应用新配置（热切换不重启 bridge）。
func (m *manager) rebuildLoop(bs *bridgeSession) {
	profile := profiles[bs.persona]
	loop, effortRef := m.buildLoop(bs.workspace, bs.id, bs.provider, bs.model, bs.effort,
		func() context.Context { return bs.s.Context() }, bs.s.Registry(), bs.questions, m.runtime(bs.workspace).skills, profile, &sandboxReleaseGate{m: m, ws: bs.workspace, id: bs.id})
	bs.s.SetLoop(loop)
	bs.loop = loop
	bs.subagentEffort = effortRef // 子 agent 档位持有者随新引擎换代（旧引擎的 loop 已废弃）
}

// setGoalAlignmentReminderRounds 把运行中的提醒阈值同步到所有已创建会话。
// 只调用 AgentLoop setter，不替换 Session loop；当前运行下一次 Provider 请求读取新值，
// 会话历史、AgentContext、后台任务和上下文均保持不变。
func (m *manager) setGoalAlignmentReminderRounds(rounds int) {
	if rounds < 0 {
		rounds = 0
	}
	m.mu.Lock()
	wsList := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wsList = append(wsList, ws)
	}
	m.mu.Unlock()
	for _, ws := range wsList {
		ws.mu.Lock()
		sessions := make([]*bridgeSession, 0, len(ws.sessions))
		for _, bs := range ws.sessions {
			sessions = append(sessions, bs)
		}
		ws.mu.Unlock()
		for _, bs := range sessions {
			if bs.loop != nil {
				bs.loop.SetGoalAlignmentReminderRounds(rounds)
			}
		}
	}
}

// setSubagentConcurrency 把子 agent 并发槽上限同步到所有已创建会话（主池 + 辅池）。
// 只改容量（SlotPool.SetCapacity）：在途任务继续持有旧池的槽并归还给旧池，此后新派发
// 立即按新上限；不重建 loop/会话，不打断任何正在跑的子 agent。
func (m *manager) setSubagentConcurrency(spawn, explore int) {
	m.mu.Lock()
	wsList := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wsList = append(wsList, ws)
	}
	m.mu.Unlock()
	for _, ws := range wsList {
		ws.mu.Lock()
		sessions := make([]*bridgeSession, 0, len(ws.sessions))
		for _, bs := range ws.sessions {
			sessions = append(sessions, bs)
		}
		ws.mu.Unlock()
		for _, bs := range sessions {
			if bs.s == nil {
				continue
			}
			reg := bs.s.Registry()
			if reg == nil {
				continue
			}
			reg.SetConcurrency(spawn)
			reg.SetAuxConcurrency(explore)
		}
	}
}

// applySubagentConcurrency 从 settings.json（agent 段）重读并发槽上限并热应用：
// reload_settings 用（手改配置文件后无需重启；前端设置面板走 set_max_subagents 命令）。
func (m *manager) applySubagentConcurrency() {
	ag := loadAgentSettings()
	m.setSubagentConcurrency(ag.maxSubagents(), ag.maxExploreSubagents())
}

// applyMeshSettings 热应用 Session Mesh 设置（CmdReloadSettings 调用；不重启进程）。
// 流程：Hub 能力门（C1/C2）→ 远程网关启停 → rebuildAll（重建 loop 时 newSessionEngine
// 按新开关决定注册/移除 mesh 工具——Engine 无 UnregisterTool，移除工具靠重建 loop）。
// 幂等：同配置重复调用无副作用（Hub 门/网关启停幂等，rebuildAll 本就重建）。
func (m *manager) applyMeshSettings() {
	if m.hub == nil {
		return
	}
	ms := loadMeshSettings()
	// 界面语言（reload_settings 热切换时刷新 mesh_from 文案语言）
	if m.hub != nil {
		m.hub.SetUILang(loadUILang())
	}
	// C1：Hub 工具门（session_* 可用性；工具注册由 rebuildAll → newSessionEngine 决定）
	m.hub.SetToolsEnabled(ms.SessionEnabled)
	// 接线（幂等）：投递成功 → 唤醒目标会话 run loop；会话标题 → session_list Name
	m.wireMeshHub()
	// C2：远程网关启停（remote_enabled=true 才启动；幂等：同配置重复调用无副作用）。
	// peers/监听配置变化（保存节点后热生效）：网关在跑且配置已变 → Stop 重建
	//（dialLoop 只在 Start 时按 cfg.Peers 起拨号，旧网关不会连新增 peer——原缺陷）。
	if ms.RemoteEnabled && m.meshGW != nil && !m.meshCfgEquals(ms.Remote) {
		m.meshGW.Stop()
		m.meshGW = nil
		m.hub.SetRemote(false, nil)
	}
	if ms.RemoteEnabled && m.meshGW == nil {
		// 收到远程事件 → 投递到本机会话（hub.Send 统一寻址：SessionId 即本机 sid）。
		// SessionId 为空（node://<node> 不带 sid 的自动建会话请求）→ 在默认工作区新建会话
		// 并把消息投进去；新建的 sid 经 msg.SessionId 回填，由网关 ack 带回发送方。
		onEvent := func(ctx context.Context, fromNodeID string, msg *meshremote.InboundMessage) {
			if msg == nil {
				return
			}
			if msg.SessionId == "" {
				ws := m.firstWorkspace()
				if ws == "" {
					return // 无任何工作区：无处建会话（发送方将 ack 超时）
				}
				bs, err := m.create(ws, m.genID(), "") // 显式生成新会话 id
				if err != nil {
					return
				}
				msg.SessionId = bs.id // 回填：网关 ack 据此带 created_session
				_, _ = m.hub.Send(ctx, bs.id, msg.Body, *msg)
				// 自动建会话也走 AI 标题生成（与用户首条 query 同路径；正文纯净不含来源前缀）
				m.maybeGenSessionTitle(ws, bs, msg.Body)
				return
			}
			_, _ = m.hub.Send(ctx, msg.SessionId, msg.Body, *msg)
		}
		gw := meshremote.NewGateway(ms.Remote, m.hub.NodeID(), onEvent)
		// N7 宿主接线：本机会话摘要广播给远程（对端 session_list 可见本机会话）
		gw.SetSessionProvider(func() []meshremote.SessionInfo {
			if m.hub == nil {
				return nil
			}
			// Hub.List 含远程部分，这里只需本机——Hub 无独立本机查询，直接用 List 后滤远程
			items := m.hub.List(context.Background())
			out := make([]meshremote.SessionInfo, 0, len(items))
			for _, it := range items {
				if it.Node == m.hub.NodeID() {
					out = append(out, it)
				}
			}
			return out
		})
		if err := gw.Start(context.Background()); err != nil {
			m.hub.SetRemote(false, nil) // 明确：网关未起，node:// 走 ErrRemoteDisabled（防误导）
			m.out.writeLine(map[string]any{
				"event_type": "mesh_status", "ok": false,
				"error": err.Error(), "workspace": "", "session_id": "",
			})
			m.meshGW = nil
		} else {
			m.meshGW = gw
			m.lastMeshCfg = ms.Remote // 记录本次生效配置（peers 变化对比基线）
			m.hub.SetRemote(true, gw)
			m.out.writeLine(map[string]any{"event_type": "mesh_status", "ok": true})
		}
	} else if !ms.RemoteEnabled && m.meshGW != nil {
		m.meshGW.Stop()
		m.meshGW = nil
		m.hub.SetRemote(false, nil)
		m.lastMeshCfg = meshremote.Settings{} // 清基线：下次开启按全新配置启动
	}
	// 注：hub.SetRemote(true, gw) 幂等（同网关重复注入无害）；开关翻转见上。
}

// meshCfgEquals 判断本次 remote 配置与上次启动网关时是否一致（只比较影响拨号/监听的
// 字段：peers 增删/地址、监听、token、TLS；NodeID 参与握手不变不重启）。
func (m *manager) meshCfgEquals(cur meshremote.Settings) bool {
	if m.meshGW == nil {
		return false
	}
	return reflect.DeepEqual(cur.Peers, m.lastMeshCfg.Peers) &&
		cur.ListenAddr == m.lastMeshCfg.ListenAddr &&
		cur.AuthToken == m.lastMeshCfg.AuthToken &&
		cur.TLSCert == m.lastMeshCfg.TLSCert &&
		cur.TLSKey == m.lastMeshCfg.TLSKey
}

// wireMeshHub 把 Hub 的宿主侧回调接线（唤醒 + 标题）。幂等（重复调用只覆盖闭包）。
//   - onDelivered：消息入队成功 → 按 sid 全工作区找 bridgeSession → wakeRun（空闲续跑）。
//   - titleFn：取会话第一条 user 文本作 Name（与桌面列表 liveTitle 同逻辑；跨包借由
//     Session.Messages 实现，避免 desktop→session 反向依赖问题——Hub 回调本就是注入面）。
func (m *manager) wireMeshHub() {
	if m.hub == nil || m.meshWired {
		return
	}
	m.hub.SetOnDelivered(func(sid string) {
		if bs := m.sessionByID(sid); bs != nil {
			bs.wakeRun() // 关键修复：mesh 消息送达后唤醒空闲会话 run loop（原只 Ask 入队）
		}
	})
	m.hub.SetTitleProvider(func(sid string) string {
		if bs := m.sessionByID(sid); bs != nil {
			return liveTitle(bs, sid)
		}
		return ""
	})
	m.meshWired = true
}

// sessionByID 全工作区按 sid 找 bridgeSession（mesh 唤醒/标题用；sid 进程内唯一）。
func (m *manager) sessionByID(sid string) *bridgeSession {
	if sid == "" {
		return nil
	}
	m.mu.Lock()
	wss := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wss = append(wss, ws)
	}
	m.mu.Unlock()
	for _, ws := range wss {
		ws.mu.Lock()
		bs := ws.sessions[sid]
		ws.mu.Unlock()
		if bs != nil {
			return bs
		}
	}
	return nil
}

// firstWorkspace 取第一个注册的工作区路径（mesh 自动建会话的默认归属；无 → ""）。
func (m *manager) firstWorkspace() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for p := range m.workspaces {
		return p // map 无序：任一工作区即可（自动建会话无强归属语义）
	}
	return ""
}

// firstTextOf 取内容块里第一段非空文本（AI 标题输入；无文本返回 ""——纯图片消息不触发）。
func firstTextOf(blocks []core.Content) string {
	for _, b := range blocks {
		if b.Type == core.ContentTypeText {
			if t := strings.TrimSpace(b.Content); t != "" {
				return t
			}
		}
	}
	return ""
}

// maybeGenSessionTitle 会话首次用户输入后触发异步 AI 标题生成（仅一次，进程内并发安全）：
//   - 仅桌面用户输入（CmdAsk/ask_batch）触发；IM/mesh 消息不算「用户首发 query」。
//   - 已有 AI 标题（Session 内存/恢复注入/jsonl meta）/ 会话历史已有 user 文本（非首条）
//     → 跳过；生成中（in-flight）→ 跳过（防并发双发）。
//   - 异步执行：不阻塞命令响应、不打断会话主 run；LLM 失败静默降级（标题保持现状）。
//
// 标题约束（prompt 侧 + 后处理双侧保证）：≤15 字符；跟随用户输入语言。
func (m *manager) maybeGenSessionTitle(wsPath string, bs *bridgeSession, firstText string) {
	if bs == nil || bs.isCron || strings.TrimSpace(firstText) == "" {
		return
	}
	wsKey := workspaceKey(wsPath)
	// 已生成（内存恢复/jsonl meta/titles 缓存任一命中）→ 不再调用
	if bs.s.AITitle() != "" || sessionTitles.get(wsKey, bs.id) != "" {
		return
	}
	// 非首次输入（历史已有 user 文本：冷恢复会话 / 消息已消费进 Messages）→ 不生成
	if firstUserText(bs.s.Messages()) != "" {
		return
	}
	// 会话绑定的 provider/model（异步期间快照）
	provName, model, effort := bs.provider, bs.model, bs.effort
	if provName == "" || model == "" {
		return
	}
	// 在途去重：同一会话只允许一个生成任务（首次输入并发双发 / 重连重放）
	m.titleGenMu.Lock()
	if m.titleInFlight == nil {
		m.titleInFlight = map[string]bool{}
	}
	if m.titleInFlight[bs.id] {
		m.titleGenMu.Unlock()
		return
	}
	m.titleInFlight[bs.id] = true
	m.titleGenMu.Unlock()
	go func() {
		defer func() {
			m.titleGenMu.Lock()
			delete(m.titleInFlight, bs.id)
			m.titleGenMu.Unlock()
		}()
		m.genSessionTitle(wsPath, bs.id, wsKey, provName, model, effort, firstText)
	}()
}

// genSessionTitle 异步执行标题生成（goroutine；调用方已确认首次输入、无既有标题、
// 无在途任务）。
func (m *manager) genSessionTitle(wsPath, sid, wsKey, provName, model, effort, firstText string) {
	// 输入过长截断（只取前 200 字符作标题依据，省 token + 防异常输入）
	text := firstText
	if r := []rune(text); len(r) > 200 {
		text = string(r[:200])
	}
	sys := "You are a session title generator. Based on the user's first message, produce a concise title for this chat session. " +
		"Rules: at most 15 characters; use the SAME language as the user's message; output only the title — no quotes, punctuation, or explanation."
	req := &provider.StreamRequest{
		Model:    model,
		Provider: provName,
		// 同会话 id：标题生成属辅助请求，OpenCode Go 网关要求同样带
		// x-opencode-session（缺失 400；同 id 复用会话路由）。
		SessionID: sid,
		Config: &provider.RequestConfig{
			MaxTokens: 32,
			Thinking:  boolPtr(false), // 标题生成无需思考：省时省 token，非推理厂商亦兼容
			Effort:    provider.ReasoningEffortLevelLow,
		},
		Messages: []core.Message{
			core.NewSystemMessage(sys),
			core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: text}),
		},
	}
	// 独立超时：标题生成尽力而为，不能长期占用连接/资源
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := m.provCfg.newProvider(provName)
	if p == nil {
		return
	}
	var out string
	if err := p.Stream(ctx, req, func(e provider.StreamEvent) error {
		if ev, ok := e.(provider.LLMEndEvent); ok {
			out = ev.Content
		}
		return nil
	}); err != nil || strings.TrimSpace(out) == "" {
		return // 失败/空 → 静默降级（下次不会再触发，标题维持 New Chat/首条文本）
	}
	title := normalizeGeneratedTitle(out)
	if title == "" {
		return
	}
	bs := m.sessionByID(sid)
	if bs == nil {
		return // 会话已删除：不写
	}
	// 1) 持久化到会话 jsonl meta（权威源，跨重启恢复）+ 内存
	wrote := bs.s.SetAITitle(context.Background(), title)
	// 2) titles.json 查询缓存同步（无论 SetAITitle 是否幂等跳过，缓存都要有值）
	sessionTitles.set(wsKey, sid, title)
	// 3) 广播 → 前端左栏即时更新（新标题才广播；恢复同值/重复生成不打扰）
	if wrote {
		m.broadcastSessionTitle(wsPath, sid, title)
	}
}

// normalizeGeneratedTitle 清洗 LLM 输出的标题：去引号/首尾空白/换行，限 15 字符。
func normalizeGeneratedTitle(raw string) string {
	t := strings.TrimSpace(raw)
	t = strings.Trim(t, "\"'「」『』“”‘’`（）()【】[]<>《》")
	t = strings.TrimSpace(t)
	// 折叠内部换行为空格
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > maxGeneratedTitleRunes {
		t = string(r[:maxGeneratedTitleRunes])
	}
	return strings.TrimSpace(t)
}

// pushMeshSessions 本机会话集合变化（注册/注销/删除）→ 立即同步对端 session_list。
// 幂等：meshGW nil（remote 未开）时 no-op；对端会在握手/周期广播时自然补齐。
func (m *manager) pushMeshSessions() {
	if m.meshGW != nil {
		m.meshGW.PushSessions()
	}
}

// broadcastSessionTitle 标题生成完成 → 前端左栏即时更新（不必等下一次 list）；
// 远程 mesh 已连时立即 PushSessions 同步对端 session_list（不等 30s 周期广播）。
func (m *manager) broadcastSessionTitle(wsPath, sid, title string) {
	m.out.writeLine(map[string]any{
		"event_type": "session_title",
		"workspace":  wsPath,
		"session_id": sid,
		"title":      title,
	})
	m.pushMeshSessions() // 标题/摘要变化 → 即时推给对端（对端 session_list 秒级可见）
}

// meshStatus 本机 Session Mesh 状态（mesh_status 命令响应；设置页展示 + 排障）。
// 含：开关状态、node_id、监听地址、可连接地址（本机网卡非回环 IP，best-effort）、
// TLS 模式、已连节点、网关错误。
func (m *manager) meshStatus() map[string]any {
	st := map[string]any{
		"session_enabled": false,
		"remote_enabled":  false,
	}
	if m.hub == nil {
		return st
	}
	ms := loadMeshSettings()
	st["session_enabled"] = ms.SessionEnabled
	st["remote_enabled"] = ms.RemoteEnabled && m.meshGW != nil
	st["node_id"] = m.hub.NodeID()

	if m.meshGW != nil {
		addr := ms.Remote.ListenAddr
		st["listen_addr"] = addr
		// TLS 模式（cert+key 齐全 = wss；否则 ws）
		tlsOn := ms.Remote.TLSCert != "" && ms.Remote.TLSKey != ""
		st["tls"] = tlsOn
		scheme := "ws"
		if tlsOn {
			scheme = "wss"
		}
		// 可连接地址：监听是回环 → 补局域网/外网候选（网卡探测 best-effort）
		host, _, _ := net.SplitHostPort(addr)
		urls := []string{fmt.Sprintf("%s://%s", scheme, addr)}
		if host == "127.0.0.1" || host == "::1" || host == "" || host == "localhost" {
			for _, ip := range lanIPs() {
				urls = append(urls, fmt.Sprintf("%s://%s:%s", scheme, ip, portOf(addr)))
			}
		}
		st["connect_urls"] = urls
		// 已连节点
		links := m.meshGW.DirectLinks(context.Background())
		nodes := make([]map[string]any, 0, len(links))
		for _, l := range links {
			nodes = append(nodes, map[string]any{
				"node_id":      l.NodeID,
				"inbound":      l.Inbound,
				"connected_at": l.ConnectedAt,
				"last_seen":    l.LastSeen,
			})
		}
		st["links"] = nodes
		// 出站节点拨号状态（排障：为什么没连上 / ping RTT）
		peers := m.meshGW.PeerStatuses(context.Background())
		peerOut := make([]map[string]any, 0, len(peers))
		for _, p := range peers {
			peerOut = append(peerOut, map[string]any{
				"node_id":      p.ID,
				"addr":         p.Addr,
				"connected":    p.Connected,
				"last_error":   p.LastError,
				"last_attempt": p.LastAttempt,
				"last_success": p.LastSuccess,
				"rtt_ms":       p.RTTMs,
			})
		}
		st["peers"] = peerOut
		// 对端节点在线会话摘要（session_list 的远程部分；设置页「对端在线会话」展示）
		remoteSessions := make([]map[string]any, 0)
		for _, n := range m.meshGW.KnownNodes(context.Background()) {
			for _, si := range n.Sessions {
				remoteSessions = append(remoteSessions, map[string]any{
					"node":      si.Node,
					"id":        si.ID,
					"name":      si.Name,
					"status":    si.Status,
					"last_seen": si.LastSeen,
				})
			}
		}
		st["remote_sessions"] = remoteSessions
	} else {
		st["reason"] = "gateway not running (remote disabled or start failed)"
	}
	return st
}

// lanIPs 本机非回环 IPv4 地址（best-effort；供「别人怎么连我」展示）。
func lanIPs() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}

// portOf 取 addr 的端口（缺省 17890）。
func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "17890"
	}
	return port
}

// rebuildAll 热切换后重建全部会话 loop（set_provider）；模型在活跃 provider 缺失时回退默认。
func (m *manager) rebuildAll() {
	m.mu.Lock()
	wsList := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wsList = append(wsList, ws)
	}
	m.mu.Unlock()
	for _, ws := range wsList {
		ws.mu.Lock()
		sessions := make([]*bridgeSession, 0, len(ws.sessions))
		for _, bs := range ws.sessions {
			sessions = append(sessions, bs)
		}
		ws.mu.Unlock()
		for _, bs := range sessions {
			// 模型归属解析（问题六 + 2026-09）：会话模型属于哪个 Provider 以用户选择为准。
			// 1) providerExplicit（switch_model 显式 provider / UI 指定）→ 保留 bs.provider，
			//    仅当原 Provider 已删除/不再含该模型才回退全局解析（防字母序改写：glm-5.3-flash
			//    同时存在于 opencode+zhipu 时曾因 o<z 被改到 opencode，请求打到错误网关）。
			// 2) 否则（旧会话无显式归属）按模型名解析一次并记录归属，后续不再漂移。
			keep := bs.providerExplicit && m.provCfg.hasModelIn(bs.provider, bs.model)
			if !keep && bs.provider != "" && m.provCfg.hasModelIn(bs.provider, bs.model) {
				keep = true // 会话已有归属且模型仍在该 Provider → 保留（不因 rebuildAll 漂移）
			}
			if !keep {
				if prov := m.provCfg.providerForModel(bs.model); prov != "" {
					bs.provider = prov
				} else {
					bs.provider = m.provCfg.providerName()
					if !m.provCfg.hasModel(bs.model) {
						bs.model = m.provCfg.defaultModel()
					}
				}
			}
			// 旧 loop 保活：in-flight Run 持旧 loop 引用直到本轮结束，就地推送运行时值，
			// 其后续轮次即按新窗口判定压缩（rebuildLoop 只换引用，覆盖不到在跑的 Run）。
			if bs.loop != nil {
				bs.loop.SetModel(bs.model)
				bs.loop.SetMaxTokens(m.provCfg.maxTokensForIn(bs.provider, bs.model))
				bs.loop.SetContextWindow(m.provCfg.windowForIn(bs.provider, bs.model))
			}
			m.rebuildLoop(bs)
		}
	}
}

// autoEnablePlugins 自动恢复「上次已启用」的插件（启用状态持久化到组件目录标记）。
// 本进程每工作区只执行一次（pluginsAuto 保护）：用户显式禁用后不会再被恢复；
// 未安装/启用失败仅记日志，不阻断。必须在持 ws.mu 之外调用（Enable 会建子进程连接）。
// 阻塞预算：embedded 的 ensureBrowserEmbed 已去 8s 同步等待（§7.2），此处仅剩 Enable 建连
// （≤15s connectTimeout）；启动路径的 runtime() 内调用保持同步（测试契约 + 避免锁内竞态）。
func (m *manager) autoEnablePlugins(ws *workspaceRuntime) {
	ws.mu.Lock()
	if ws.pluginsAuto {
		ws.mu.Unlock()
		return
	}
	ws.pluginsAuto = true
	ws.mu.Unlock()

	// §启动优化：异步执行（原同步阻塞 create()/会话创建——Enable ≤15s + embed 2s 是
	// 启动头号嫌疑）。异步后 create() 立即返回；Enable 完成时补 rebuildWorkspace 注册
	// 工具 + emitPluginStatus 通知 UI。竞态安全：pluginsAuto 提前置位防重入；goroutine
	// 在 Unlock 后启动，不持 m.mu（无死锁）。
	go m.autoEnablePluginsAsync(ws.path)
}

// autoEnablePluginsAsync 后台执行插件自动恢复（不阻塞会话创建/启动路径）。
func (m *manager) autoEnablePluginsAsync(wsPath string) {
	ctx := context.Background()
	ws := m.runtime(wsPath)
	restored := false
	for _, cap := range ws.plugins.List() {
		we, ok := cap.(interface{ WasEnabled() bool })
		if !ok || !we.WasEnabled() {
			continue
		}
		// Browser 引擎开关（2026-09）：引擎=ego 时跳过 browser（MCP）插件自动启用——
		// 工具不注册进 Agent（互斥：ego 引擎下 agent 走 ego-browser skill）。
		// 用户显式切回 mcp 引擎后（browser_engine_set 或 settings 落盘 + 重启）恢复。
		if cap.ID() == browser.ID && m.getBrowserEngine() == EngineEgo {
			fmt.Fprintf(os.Stderr, "· 引擎=ego：跳过 browser 插件自动启用（browser_engine_set 切回 mcp 恢复）\n")
			continue
		}
		// Browser Use presentation=embedded：先起内嵌端点并注入插件，再 Enable，使
		// chrome-devtools-mcp 连接内嵌视图而非自启系统 Chrome 窗口（否则启动恢复会双开浏览器、
		// 内嵌视图白屏——enable 时没有端点导致 serverArgs 不发 --ws-endpoint 的根因）。
		// TEMP-STARTUP A6 异步子段：ensureBrowserEmbed（事件驱动等 attach，≤2s 超时）
		embStart := time.Now()
		if bp, isB := cap.(*browser.Plugin); isB && bp.Config().Presentation == browser.PresentationEmbedded {
			if _, err := m.ensureBrowserEmbed(); err != nil {
				fmt.Fprintf(os.Stderr, "✗ 自动启用 browser 前起内嵌端点失败: %v\n", err)
			}
		}
		startupLog("A6.async.ensureBrowserEmbed", embStart)
		// TEMP-STARTUP A6 异步子段：Enable 建连（≤15s）
		enStart := time.Now()
		if _, err := ws.plugins.Enable(ctx, cap.ID()); err != nil {
			fmt.Fprintf(os.Stderr, "✗ 自动启用插件 %s: %v\n", cap.ID(), err)
			continue
		}
		startupLog("A6.async.plugins.Enable("+cap.ID()+")", enStart)
		// IM 插件自动恢复后装配全局事件桥（与 enablePlugin 路径一致——否则消息/审批不路由）
		if cap.ID() == implugin.ID {
			m.attachIMBridge(wsPath)
		}
		restored = true
	}
	// 确有插件被自动恢复：重建工作区（注册工具进引擎）+ 推 plugin_status（UI 反映已启用）
	if restored {
		m.rebuildWorkspace(wsPath)
		m.emitPluginStatus(wsPath)
	}
}

// rebuildWorkspace 重建单个工作区的全部会话 loop（mcp_refresh 后工具集/连接状态更新）。
func (m *manager) rebuildWorkspace(wsPath string) {
	ws := m.runtime(wsPath)
	ws.mu.Lock()
	sessions := make([]*bridgeSession, 0, len(ws.sessions))
	for _, bs := range ws.sessions {
		if bs.isCron {
			continue // 定时任务派生会话保持原 loop（触发即跑、跑完归档）
		}
		sessions = append(sessions, bs)
	}
	ws.mu.Unlock()
	for _, bs := range sessions {
		m.rebuildLoop(bs)
	}
}

// parseMode 权限模式映射（客户端三模式 + 兼容旧名）：
// hitl=用户确认（write/edit/bash 执行前需审批）、auto=全自动执行（write/edit/bash
// 均不审批；bash 工具层静态拦截/沙箱仍生效）、full-access=完全访问（全部自动放行）。
// 旧名（plan/all-pass/manual/accept-edits）保留：兼容既有会话与测试。
// 只读工具（read_file/grep/glob/todo_*）恒 Auto —— 审批只门控变更类（write/edit/bash）。
func parseMode(name string) *agents.Mode {
	switch name {
	// plan_submit 已移除；plan 模式也不再作为客户端能力暴露。
	case "all-pass", "full-access":
		return &agents.Mode{Name: name, Approve: func(core.ToolCall) agents.ApprovalDecision { return agents.ApprovalAuto }}
	case "manual", "hitl":
		return &agents.Mode{Name: name, Approve: func(c core.ToolCall) agents.ApprovalDecision {
			switch c.Name {
			// run_python 与 bash 同类：能写文件、可借 subprocess 执行命令（办公执行通道）。
			// 漏列 = 模型可用它绕过审批写任意文件。
			case "write_file", "edit_file", "bash", "run_python":
				return agents.ApprovalRequired
			default:
				return agents.ApprovalAuto
			}
		}}
	case "accept-edits":
		return &agents.Mode{Name: "accept-edits", Approve: func(c core.ToolCall) agents.ApprovalDecision {
			switch c.Name {
			case "write_file", "edit_file":
				return agents.ApprovalAuto
			case "bash", "run_python":
				return agents.ApprovalRequired
			default:
				return agents.ApprovalAuto
			}
		}}
	case "auto":
		// 全自动放行：bash/write/edit 均不弹审批（此前"自动判断"下 bash 自声明
		// 需审批，每次执行都弹窗等 60s，用户无感知即表现为"卡在工具调用"）。
		// 安全底线不变：bash 静态高危命令拦截 + 沙箱 + open 白名单在工具层仍生效。
		// 需要逐命令确认用 hitl/manual。
		return &agents.Mode{Name: "auto", Approve: func(core.ToolCall) agents.ApprovalDecision { return agents.ApprovalAuto }}
	default:
		return agents.NormalMode()
	}
}

func modeName(m *agents.Mode) string {
	if m == nil {
		return "normal"
	}
	return m.Name
}

// startupLog 启动耗时打点（TEMP-STARTUP：测量后移除）。
// **已停用**：不再写 ~/.go-code/startup.log、不写 stderr（启动诊断已完成，避免日志占用磁盘）。
// 保留 API 签名兼容调用点；如需重新诊断，恢复 msg 构造 + 双写逻辑即可。
func startupLog(_ string, _ time.Time) {}

// serve：stdio 事件循环（单进程承载所有工作区，workspace 随命令 payload 传入）
func serve() error {
	// TEMP-STARTUP A1：Go 二进制自身启动（main → serve 间隔）
	startupLog("A1.二进制启动(main→serve)", mainStart)
	serveStart := time.Now()
	// 不再要求启动即有 token：多 provider 配置（settings.json + env）缺 key 时
	// 首个 LLM 请求报错，前端设置里可填 key 热切换（set_provider）。
	pcStart := time.Now()
	pc := loadProviderConfig()
	startupLog("A2.loadProviderConfig", pcStart)
	m := &manager{
		workspaces: map[string]*workspaceRuntime{},
		provCfg:    pc,
		out:        newOut(os.Stdout),
		metrics:    NewFileMetricsStore(filepath.Join(metricsHomeDir(), "metrics.jsonl")),
		imPlugin:   implugin.New(implugin.DefaultConfigPath()), // 全局 IM 单例（跨 workspace 共享）
		hub:        session.NewHub(meshNodeID()),               // Session Mesh Hub（node id 持久化解析）
	}
	// Browser 引擎开关：启动时读 settings.json 顶层 browser_engine（mcp|ego）。
	m.refreshBrowserEngine()
	startupLog("A2.5.browserEngine("+string(m.getBrowserEngine())+")", serveStart)
	// Session Mesh 启动自愈：settings 已持久化的 mesh 配置（开关/peers/监听）在 bridge
	// 重启后立即生效——不再依赖前端再触发一次 reload_settings（原缺陷：重启后网关静默停，
	// 保存的节点列表不拨号，跨机 session_list 为空）。
	m.applyMeshSettings()
	cronStart := time.Now()
	m.startCron()
	startupLog("A3.startCron", cronStart)

	// P0/P1（§4.4/§4.6）：bridge 启动完成 → 发一次 ready 事件。
	// 前端 embed 自举以此为准（不再靠任意 command response 猜测 bridge 就绪）。
	startupLog("A4.bridge_ready(总启动)", serveStart)
	m.out.writeLine(map[string]any{"event_type": "bridge_ready"})

	// stdin 逐行读取：不用 Scanner（4MiB 行上限遇超长行会 ErrTooLong 终止整个服务）。
	// 用 Reader 逐行读 + 单行长度上限：超长行报错拒绝、继续服务（V2 资源边界观察项）。
	reader := bufio.NewReaderSize(os.Stdin, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			if line == "" {
				break
			}
			// 最后一行无换行：照常处理
		} else if err != nil {
			m.out.writeLine(map[string]any{"event_type": "command_response", "id": 0, "ok": false, "error": "stdin read: " + err.Error()})
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if err == io.EOF {
				break
			}
			continue
		}
		if len(line) > maxCommandLineBytes {
			m.out.writeLine(map[string]any{"event_type": "command_response", "id": 0, "ok": false, "error": fmt.Sprintf("command line too long (%d bytes, max %d)", len(line), maxCommandLineBytes)})
			if err == io.EOF {
				break
			}
			continue
		}
		var c command
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			m.out.writeLine(map[string]any{"event_type": "command_response", "id": 0, "ok": false, "error": "bad json: " + err.Error(), "code": "bad_json"})
			if err == io.EOF {
				break
			}
			continue
		}
		m.dispatch(c)
		if err == io.EOF {
			break
		}
	}
	// stdin EOF → 优雅 Shutdown 全部会话（状态落盘）+ 停定时调度器
	m.stopCron()
	m.shutdownAll()
	return nil
}

// maxCommandLineBytes stdin 单条命令上限（4 MiB，与原 Scanner 上限一致；超限拒绝不终止服务）。
const maxCommandLineBytes = 4 * 1024 * 1024

// maxImageBlockBytes 单条 ask_batch 消息的图片 data URL 总量上限。
// 取值依据（P0 核心约束）：整条命令序列化成一行经 stdin 传输，受
// maxCommandLineBytes（4MiB）硬限 —— 图片 base64 膨胀 4/3，故单条消息的图片
// 必须留在「4MiB − JSON 转义与文本开销」以内。取 3MiB 留 ~1MiB 余量给文本块、
// JSON 转义（base64 无特殊字符，开销小）与命令封装。
// 注意：这是**传输层**闸门，与前端单图上限（压缩后 ≤2.5MiB）、工具结果内嵌图
// 上限（tools.defaultMaxImageBytes=3MiB）同口径收敛 —— 三者取同一个 3MiB 量级，
// 任何一层放行都不该让下一层静默失败。
const maxImageBlockBytes = 3 * 1024 * 1024

// imageBudgetExceeded 判断一组内容块的图片总量是否超限，返回 (超限, 实际字节数)。
// 只统计 image 块（text 体积由 maxCommandLineBytes 兜底；此处专盯图片——
// 它是唯一会因 base64 膨胀而意外撞线的内容）。
func imageBudgetExceeded(blocks []core.Content, limit int) (bool, int) {
	total := 0
	for _, b := range blocks {
		if b.Type == core.ContentTypeImage {
			total += len(b.Content)
		}
	}
	return total > limit, total
}

// humanBytes 人类可读字节数（错误信息用，与 tools.builtin 同款措辞）。
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

type command struct {
	Id      int            `json:"id"`
	Type    Command        `json:"type"` // 类型化命令名（V2 P2-PROTOCOL-03：编译期拼写检查）
	Payload map[string]any `json:"payload"`
}

func strSlice(m map[string]any, k string) []string {
	v, _ := m[k].([]any)
	out := make([]string, 0, len(v))
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intNum(m map[string]any, k string, fallback int) int {
	if m == nil {
		return fallback
	}
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return fallback
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func boolVal(v any) bool {
	b, _ := v.(bool)
	return b
}

// boolPtr 返回 bool 指针（RequestConfig.Thinking 等可选字段用）。
func boolPtr(b bool) *bool {
	return &b
}

// parseCronIMChat 解析 cron 任务的 IM 推送目标（payload.im_chat）。
// 结构：{"gateway":"feishu","chat_id":"oc_xxx"}；nil/空对象 → nil（清除绑定）。
func parseCronIMChat(v any) *cron.IMChat {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	gw := str(m, "gateway")
	cid := str(m, "chat_id")
	if gw == "" || cid == "" {
		return nil
	}
	return &cron.IMChat{Gateway: gw, ChatID: cid}
}

// isEmptyContent 判断一组内容块是否为空：全部为空白文本、且无任何非文本块（如图片附件）。
// 用于 ask_batch 跳过「发了但没内容」的占位消息。
func isEmptyContent(blocks []core.Content) bool {
	hasMeaning := false
	for _, c := range blocks {
		if c.Type != core.ContentTypeText {
			return false // 有非文本块（图片等）→ 非空
		}
		if strings.TrimSpace(c.Content) != "" {
			hasMeaning = true
		}
	}
	return !hasMeaning
}

// int64Val payload 数字统一解析：JSON 解码 → float64；Go 测试/直传 → int / json.Number。
// 返回 (值, 是否为正数)。模型窗口/输出上限只接受正数。
func int64Val(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int64(n), true
		}
	case int:
		if n > 0 {
			return int64(n), true
		}
	case json.Number:
		if f, err := n.Float64(); err == nil && f > 0 {
			return int64(f), true
		}
	}
	return 0, false
}

// resolveToken：环境变量优先，否则解析仓库根 .env
func resolveToken() string {
	if v := os.Getenv("DEEPSEEK_API_TOKEN"); v != "" {
		return strings.TrimSpace(v)
	}
	for _, p := range []string{".env", "../.env"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "DEEPSEEK_API_TOKEN=") {
				_ = f.Close()
				return strings.TrimSpace(strings.TrimPrefix(line, "DEEPSEEK_API_TOKEN="))
			}
		}
		_ = f.Close()
	}
	return ""
}

// mainStart 进程启动时刻（TEMP-STARTUP A1：二进制加载 → serve 间隔）。
var mainStart = time.Now()

func main() {
	flag.String("workspace", "", "（已废弃）工作区根目录：现在随命令 payload 传入")
	flag.Parse()
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
}
