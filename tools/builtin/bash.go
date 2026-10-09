package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// BashTool 执行 bash/shell 命令。危险工具：默认注册（进程隔离执行），
// 仍建议评估信任边界后使用。
//
// 工具名为 "bash"（对齐业界惯例，模型认知度高）；命令经 sh -c 执行
// （POSIX，macOS/Linux 最稳，bash 语法子集兼容执行）。
//
// 可选参数 timeout（秒）：模型按任务难易自设命令时限（如长编译/测试给更大值）；
// 缺省/非法 → 默认 5 分钟。范围钳制 [1s, 1h]，超长任务建议拆步骤或换后台。
//
// 安全双保险（执行前静态检查，所有沙箱后端共享）：
//   - 高风险命令拦截：rm 递归强删工作区/根/家目录、磁盘操作（mkfs/fdisk/dd）、
//     shutdown/reboot/sudo/kill -9 1、fork 炸弹、curl|sh 等 —— 拒绝执行并返回
//     可操作的修正提示（明确到具体子目录的删除放行，如 rm -rf dist/）
//   - 执行后端可注入沙箱（NewBashTool 末参）：nil = NoSandbox 进程隔离
//     （独立进程组 + 最小环境 + 超时杀整组）；注入 sandbox.Sandbox
//     （如 bwrap/Docker 实现）后命令在沙箱内执行
type BashTool struct {
	tools.BaseTool
	Timeout time.Duration // 单条命令默认超时（模型未传 timeout 时兜底；<=0 → 5 分钟）
	Cwd     string        // 命令初始工作目录（FileTools.Tools() 默认绑工作区根；空 = agent 进程默认目录）
	sbx     sandbox.Sandbox

	// appAllow 已弃用（2026-09 决策：open 命令全放行，不再 per-app 白名单拦截）。
	// 保留字段与 NewBashTool 第 4 参仅为兼容既有装配链/测试；不参与任何逻辑。
	appAllow []string

	// cd 跨调用记忆（轻量版 Claude Code shell 语义）：每次命令仍是独立进程，
	// 但 cd 到的目录在**同一 BashTool 实例**（同一会话 loop）内跨调用保持——
	// 模型 `cd /some/dir && make` 后，下一次命令自动从 /some/dir 开始，
	// 无需重复 cd。实例 per-session（newSessionEngine 每会话新建 FileTools），
	// 不跨会话串扰；子 agent 共享主 loop engine 的 bash → cd 也共享（同 shell 语义）。
	mu     sync.Mutex
	curCwd string // 当前工作目录（初始 = Cwd；cd 命令成功后更新）

	// spill 落盘目录与序号（惰性建目录，见 ensureSpillDir）。
	//
	// 注意**没有**「结构化结果暂存」字段：结构化结果写进引擎注入的**每次调用独立**的
	// ctx 槽（tools.StructuredSinkFrom(ctx)）。用实例字段会在「主 agent 与后台子 agent
	// 共享同一 ToolEngine」的并发下互相清空/串味（评审实测：脚本路径 3/300 拿到 nil，
	// 退化成 20KB 文本）。smu 保护下面两个字段。
	smu      sync.Mutex
	spillDir string
	spillSeq uint64

	// BeforeExec 可选钩子：每条命令真正执行前调用（参数 = 最终命令文本，cd 前缀
	// 已剥离）。宿主注入产品级副作用（如检测 ego-browser 命令激活窗口），工具层
	// 不关心具体逻辑；nil = 无钩子。执行不应阻塞命令主路径（实现应快速返回）。
	BeforeExec func(cmd string)
}

// 命令时限策略：模型可经 timeout 参数覆盖（默认 5 分钟，钳制 1s–1h）。
const (
	defaultBashTimeout = 5 * time.Minute
	maxBashTimeout     = time.Hour
)

// maxBashStructuredOutput 脚本路径（OutputSchemaProvider 的 output 字段）上限：1 MiB。
// 两条路径的预算**刻意不同**，不要合并：
//   - 模型路径：引擎 defaultMaxResponseSize（20 KB）头尾截断 —— 结果每轮都进上下文，
//     放开会把上下文炸掉（tools/exposure.go 的 SkipTruncateProvider 注释同理）。
//   - 脚本路径：结果只在脚本里活着（不进模型上下文），是「脚本内过滤/聚合」的原料 ——
//     砍到 20 KB 的话 `await tools.bash(...)` 拿到的就是残片，过滤无从谈起
//     （设计文档 §15.4 / pi CHANGELOG.md:106 同口径）。
//
// 超上限的部分不丢：全文落盘并把路径写进 full_output_path（见 spillFile）。
const maxBashStructuredOutput = 1 << 20

// bashSpillDirPrefix 落盘目录前缀（os.MkdirTemp 在系统临时目录下建，见 ensureSpillDir）。
const bashSpillDirPrefix = "hai-harness-bash-spill-"

// NewBashTool 创建 bash 工具（FileTools.Tools() 默认注册）。
// cwd 命令初始工作目录：推荐传工作区根 —— bash 相对路径与文件工具同锚（Current
// Workspace 即命令真实起点）；绑定仅决定起点不锁访问面（命令内可 `cd 任意目录`、
// 绝对路径照常可用）。传空 = 沿用 agent 进程默认目录。
// sbx 沙箱执行后端（nil = NoSandbox 进程隔离直通）。
// appAllow 已弃用（open 命令自 2026-09 起全放行，不再 per-app 白名单拦截）；
// 保留第 4 参仅为兼容既有装配链/测试，传 nil 即可。
func NewBashTool(cwd string, timeout time.Duration, sbx sandbox.Sandbox, appAllow []string) *BashTool {
	t := &BashTool{
		BaseTool: tools.BaseTool{
			Name_:        "bash",
			Description_: "Execute a bash/shell command and return its output (including stderr). For file operations (read/search/edit/find files), prefer the dedicated tools (read_file/grep/edit_file/glob) — they return more refined results; use bash for command execution, builds, tests, git, and scripts. The working directory persists across calls within this session: after a command like `cd /some/dir && make`, subsequent commands start in /some/dir (no need to re-cd); use `cd` to change it again, or use absolute paths to escape. Each command runs as an isolated process (env/state other than the directory does not persist). Access outside the workspace via absolute paths still works (installed Python/Node/Go packages, desktop apps). High-risk commands (rm clearing, sudo, disk operations, curl|sh, etc.) are statically blocked — be explicit about the exact subdirectory when deleting; when approval is enabled, commands require user confirmation before running.",
			Params_: tools.Obj(map[string]any{
				"command": tools.Str("Command to execute (bash syntax)"),
				"timeout": tools.Map{"type": "number", "minimum": 1, "maximum": 3600, "default": 300, "description": "Command timeout in seconds (optional); default 300, range 1-3600"},
			}, "command"),
			CanParallel_: false,
		},
		Cwd:      cwd,
		Timeout:  timeout,
		sbx:      sbx,
		appAllow: appAllow,
		curCwd:   cwd,
	}
	return t
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明工具级默认时限
// （NewBashTool 传入；<=0 → 5 分钟）。引擎据 TimeoutForArgs 可再按本次参数覆盖。
func (t *BashTool) ToolTimeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return defaultBashTimeout
}

// TimeoutForArgs 实现 tools.PerCallTimeout：模型可经 timeout 参数（秒）按任务难易
// 自设本次命令时限；缺省/非法 → 0（用工具级默认）。promote 后台时由引擎 liftable 解除。
func (t *BashTool) TimeoutForArgs(arguments string) time.Duration {
	var a bashArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return 0
	}
	return parseTimeoutArg(a)
}

// RequiresApproval 实现 tools.ApprovalRequired：bash 是最高权限入口，
// 启用审批（Session WithApprovalTimeout）时所有命令执行前需用户确认。
func (t *BashTool) RequiresApproval(context.Context, core.ToolCall) bool { return true }

type bashArgs struct {
	Command string   `json:"command"`
	Timeout *float64 `json:"timeout"` // 秒；缺省 nil → 用默认（NewBashTool.Timeout / 5 分钟）
}

// parseTimeoutArg 解析模型传入的 timeout（秒）：缺省/非法 → 0（= 用工具级默认）；
// 合法值钳制到 [1s, 1h]。
func parseTimeoutArg(a bashArgs) time.Duration {
	if a.Timeout == nil || *a.Timeout <= 0 {
		return 0
	}
	d := time.Duration(*a.Timeout * float64(time.Second))
	if d < time.Second {
		return time.Second
	}
	if d > maxBashTimeout {
		return maxBashTimeout
	}
	return d
}

// effectiveTimeout 解析模型传入的 timeout（秒）：非法/缺省 → 工具默认；（直接调用兜底用）
func (t *BashTool) effectiveTimeout(a bashArgs) time.Duration {
	def := t.Timeout
	if def <= 0 {
		def = defaultBashTimeout
	}
	if v := parseTimeoutArg(a); v > 0 {
		return v
	}
	return def
}

func (t *BashTool) ValidParams(_ context.Context, _, arguments string) error {
	var a bashArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("bash: %w", err)
	}
	if a.Command == "" {
		return errors.New("bash: command is required")
	}
	return nil
}

func (t *BashTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a bashArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	// 失败/早退的调用不写结构化槽（命令未执行 = 没有 exit_code 可言）。槽是**本次调用
	// 独立**的（引擎注入），故这里无需、也无法"清空上一次" —— 上一次的槽已随那次调用回收。
	// 时限交给执行层（sandbox.runExec）动态处理，不在本层自叠 WithTimeout——
	// 若任务被 promote 为后台，引擎 liftableTimeout 已解除时限（ctx 标记 Unlimited），
	// 本层的快照定时器反而会挡住「后台不限时」的语义。
	//   - 后台（promote 后）：-1 = 不限时（仅随显式取消/会话 ctx）。
	//   - 引擎已设时限（ctx 有 deadline）：0 = 跟随 ctx（lift 后自动解除）。
	//   - 直接调用（无 ctx deadline，如测试）：工具默认/模型 timeout 兜底。
	toolTimeout := time.Duration(0)
	switch {
	case tools.IsUnlimited(ctx):
		toolTimeout = -1
	case func() bool { _, ok := ctx.Deadline(); return !ok }():
		toolTimeout = t.effectiveTimeout(a)
	}
	// 高风险拦截：静态检查（命令不执行）—— 提示要可操作（模型可见后可自我修正）
	if reason := checkHighRisk(a.Command); reason != "" {
		return "", fmt.Errorf("bash: high-risk command rejected: %s. Command not executed. For deletion, target a specific subdirectory (e.g. rm -rf dist/) or rewrite the content with write_file.", reason)
	}
	sbx := t.sbx
	if sbx == nil {
		sbx = sandbox.NoSandbox{}
	}
	// cd 跨调用记忆：命令以 `cd <目标>` 开头时解析目标并更新会话 cwd（后续调用
	// 自动从该目录开始）。执行策略：
	//   - 目标存在（目录）→ 剥离 cd 前缀只执行剩余命令（spec.Cwd 已指向目标；
	//     保留前缀在相对目标下会二次 cd 失败：cwd=X 里再 `cd sub` 找 X/sub）。
	//     纯 `cd X`（无剩余）→ 执行原命令（shell 里 cd 成功无输出，语义正确）。
	//   - 目标不存在/非目录 → 不更新记忆，原样执行让 shell 报错（模型可见后修正）。
	cmd := a.Command
	cwd := t.currentCwd()
	if target, rest, ok := t.splitCd(a.Command, cwd); ok {
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			t.setCurrentCwd(target)
			cwd = target
			if rest != "" {
				cmd = rest // cd 前缀已由 spec.Cwd 兑现，只跑剩余命令
			}
		}
	}
	// 宿主钩子（BeforeExec）：真实执行前通知（命令 = 剥离 cd 后的最终文本）。
	// 快速返回约定；慢实现阻塞命令本身（宿主应自行异步/限时）。
	if t.BeforeExec != nil {
		t.BeforeExec(cmd)
	}
	var exitCode int
	var timedOut, canceled bool
	started := time.Now() // 墙钟只包住真实执行（cd 解析/高危拦截不算命令耗时）
	out, err := sbx.Run(ctx, sandbox.ExecSpec{
		Command:  cmd,
		Cwd:      cwd,
		Timeout:  toolTimeout,
		ExitCode: &exitCode,
		TimedOut: &timedOut,
		Canceled: &canceled,
	})
	wall := time.Since(started)
	// 上报结构化结局（旁路：Call 必须返回 nil error，否则引擎会丢弃 out，见非目标 1）
	if sink := tools.ExecSinkFrom(ctx); sink != nil {
		sink.SetExit(cmd, exitCode, timedOut, canceled)
	}
	if err != nil {
		// 真正的执行层故障（如 sh 无法启动）：不写结构化结果（无真实结局可报），
		// 引擎在此分支也不会读它（err != nil 直接丢弃 out）。
		return out, err
	}
	switch {
	case canceled:
		// 取消（用户中断 / 会话关闭 / 后台任务被中断）：与超时共用一个标志会让用户中断
		// 被印成 [TIMEOUT after …]（错的事实 —— 不是命令慢，也不该建议调大 timeout）。
		// 同样返回部分输出，故同样必须在首行声明不得当完整结果用。
		out = fmt.Sprintf("[CANCELLED — the command was interrupted before finishing; the output below is PARTIAL]\n%s", out)
	case timedOut:
		// 超时返回的是**部分输出**且命令未完成——必须在首行显式声明，
		// 否则模型会把被截断的中间结果当成完整结果继续推理（实测 130 次）。
		// 用 toolTimeout 说明实际时限（<=0/-1 表示不限时，此时只能是 ctx 取消）。
		limit := "the configured limit"
		if toolTimeout > 0 {
			limit = toolTimeout.String()
		}
		out = fmt.Sprintf("[TIMEOUT after %s — the command was killed and the output below is PARTIAL]\n%s", limit, out)
	}
	// 结构化结果（**仅脚本路径**）：与文本同源（同一份 out，含超时/取消首行声明），
	// 只是上限放宽到 1 MiB 且带真实结局。引擎在 Call 成功后读走它 → 脚本里
	// tools.bash(...) 拿到对象而不是被 20 KB 砍过的文本（设计文档 §15.4 的单点改造）。
	//
	// 模型路径**不产出**（设计文档 §15.4 原文：「仅在编排/SDK 调用路径返回完整输出，
	// 模型路径维持现有限额」）：模型看的是被引擎截断到 20 KB 的文本，结构化原文没有
	// 消费者，而 >1 MiB 时它还会顺带落盘一个没人引用、永不删除的文件 —— 纯成本
	//（磁盘 + 一份命令输出的额外副本），故按 tools.IsScriptCall 收口。
	// 槽是每次调用独立的（引擎在 Call 前注入），故不需要"清空上一次"——
	// 上一次的槽已随那次调用回收。
	// 写进**本次调用**的结构化槽（引擎注入；直接调用本工具时可能为 nil —— 那就没人收）。
	// 槽随调用生命周期回收，故不需要（也不能）用实例字段：主 agent 与后台子 agent 共享
	// 同一个 ToolEngine，同一实例会被并发调用，实例字段会互相清空/串味（评审实测复现）。
	if tools.IsScriptCall(ctx) {
		if sink := tools.StructuredSinkFrom(ctx); sink != nil {
			sink.Set(t.buildStructured(ctx, out, exitCode, wall))
		}
	}
	return out, nil
}

// OutputSchema 实现 tools.OutputSchemaProvider：bash 结构化返回契约（脚本路径）。
// 字段与语义见 bashStructuredOutput / buildStructured。
func (t *BashTool) OutputSchema() any {
	return tools.Obj(map[string]any{
		"output":            tools.Str("命令的合并输出（stdout+stderr）。超时/取消时首行是 [TIMEOUT …]/[CANCELLED …] 声明（说明下文为部分输出）。上限 1 MiB：被截断时这里是前 1 MiB，全文见 full_output_path"),
		"truncated":         tools.Bool("output 是否被 1 MiB 上限截断。true 时通常能在 full_output_path 找到全文；落盘失败（磁盘满/目录不可写）时该路径为空 —— 此时 output 就是全部可得内容"),
		"full_output_path":  tools.Str("未截断全文的落盘路径（truncated=true 且落盘成功时有值；落盘失败时为空串，不要当成有效路径）。文件权限 0600 且不在工作区内，脚本可用 read 工具或 fs 读取"),
		"exit_code":         tools.Int("命令真实退出码（被信号杀死 / 超时 / 取消为 -1）"),
		"wall_time_seconds": tools.Number("命令实际墙钟耗时（秒，只计真实执行，不含 cd 解析与高危检查）"),
	}, "output", "truncated", "exit_code", "wall_time_seconds")
}

// bashStructuredOutput bash 的结构化结果（脚本路径消费；模型路径仍看文本）。
// 字段名与 pi 一致（CHANGELOG.md:106 / 设计文档 §15.4）。
type bashStructuredOutput struct {
	Output         string  `json:"output"`
	Truncated      bool    `json:"truncated"`
	FullOutputPath string  `json:"full_output_path,omitempty"` // 空 = 未截断（无落盘）
	ExitCode       int     `json:"exit_code"`
	WallTimeSec    float64 `json:"wall_time_seconds"`
}

// buildStructured 组装本次调用的结构化结果。语义（三条都是硬要求）：
//  1. output 与文本路径**同源**：文本是 output 的投影（同一份 out + 引擎的 20 KB 截断），
//     连超时/取消的首行声明也一样 —— 脚本与模型看到的是同一件事实的两个粒度。
//  2. 超 1 MiB 只保留前 1 MiB，且**不注入任何提示行**：结构化字段要能被脚本直接解析，
//     「被砍了」由 truncated + full_output_path 两个字段承载（写注释进 output 会污染数据）。
//  3. exit_code / wall_time_seconds 取真实结局（与 ExecSink 同一组变量算出，不二次判定）。
//
// 落盘失败（磁盘满 / 目录不可写）不改变文本路径：truncated=true 但 full_output_path 为空
// —— spill 是优化不是正确性（同 agents/slim.go 的取舍）。
func (t *BashTool) buildStructured(ctx context.Context, out string, exitCode int, wall time.Duration) []byte {
	so := bashStructuredOutput{Output: out, ExitCode: exitCode, WallTimeSec: wall.Seconds()}
	if len(out) > maxBashStructuredOutput {
		so.Truncated = true
		so.Output = truncateUTF8(out, maxBashStructuredOutput) // 不劈裂多字节字符
		so.FullOutputPath = t.spillFile(ctx, out)
	}
	raw, err := json.Marshal(so)
	if err != nil {
		return nil // 不可达（字段全可序列化）；返回 nil = 退回文本路径
	}
	return raw
}

// spillFile 把未截断全文落盘并返回路径（失败 = ""）。
//
// 落盘约定（设计文档 §15.4 的硬要求：0600 且在工作区之外，pi 的裸 writeFile 是缺陷不要照抄）：
//   - 目录：os.MkdirTemp 在系统临时目录（$TMPDIR；macOS = /var/folders/…）下建 0700 目录，
//     同一 BashTool 实例复用（惰性、不 per-call 造目录）。TMPDIR 天然在工作区之外 ——
//     本仓从不把 TMPDIR 指向工作区（沙箱层反而显式把沙箱内 TMPDIR 钉到 /tmp，见 sandbox.isolatedEnv）。
//   - 文件名：沿用本仓 spill 命名约定 `spill-<runId>-<n>.txt`（agents/slim.go 先例；bash 没有
//     callId，用实例内自增序号保证唯一 —— 同一次 run 内可直接按名字排序定位）。
//   - 权限 0600：命令输出常含凭据/私有数据（pi 的 MCP 侧也这么注释），只有用户本人可读。
//
// 不删文件：路径已经交给脚本/宿主（full_output_path），删掉等于给死链；留在系统临时目录由
// 操作系统回收（与 slim 的 spill 同性质）。
func (t *BashTool) spillFile(ctx context.Context, full string) string {
	dir, err := t.ensureSpillDir()
	if err != nil {
		return ""
	}
	runID := ""
	if tc := events.ToolContextFrom(ctx); tc != nil {
		runID = tc.RunId
	}
	path := filepath.Join(dir, t.nextSpillName(runID))
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		return ""
	}
	return path
}

// ensureSpillDir 惰性建落盘目录（0700，见 spillFile 的约定）；同实例复用同一个目录。
func (t *BashTool) ensureSpillDir() (string, error) {
	t.smu.Lock()
	defer t.smu.Unlock()
	if t.spillDir != "" {
		return t.spillDir, nil
	}
	dir, err := os.MkdirTemp("", bashSpillDirPrefix)
	if err != nil {
		return "", err
	}
	t.spillDir = dir
	return dir, nil
}

// nextSpillName 生成落盘文件名（spill-<runId>-<n>.txt；无 runId 时省略该段）。
// runId 经 sanitize 处理：它来自事件层（宿主/模型可影响），不能带路径分隔符。
func (t *BashTool) nextSpillName(runID string) string {
	t.smu.Lock()
	defer t.smu.Unlock()
	t.spillSeq++
	if runID = sanitizeSpillID(runID); runID == "" {
		return fmt.Sprintf("spill-%d.txt", t.spillSeq)
	}
	return fmt.Sprintf("spill-%s-%d.txt", runID, t.spillSeq)
}

// sanitizeSpillID 只保留文件名安全字符（字母/数字/.-_），其余替换为 "_"，并限长。
func sanitizeSpillID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "._-")
}

// currentCwd 返回当前工作目录（会话内跨调用；初始 = 构造 cwd）。
func (t *BashTool) currentCwd() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.curCwd != "" {
		return t.curCwd
	}
	return t.Cwd
}

// setCurrentCwd 更新会话 cwd（cd 目标存在且为目录时调用）。
func (t *BashTool) setCurrentCwd(dir string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.curCwd = dir
}

// cdPrefixRe 匹配命令开头的 cd：`cd <target>`（target 可带引号/空格）。
var cdPrefixRe = regexp.MustCompile(`^cd\s+(.+)$`)

// splitCd 解析命令开头的 `cd <目标>`：返回 (目标绝对路径, 剩余命令, true)。
// 剩余命令 = `cd X && cmd` 的 cmd（无 && 则为 ""）。`cd` 无参/非 cd 开头 → ("","",false)。
// 目标支持：绝对路径 / 相对路径（相对 cwd）/ ~ / ~/xxx（展开 home）。
func (t *BashTool) splitCd(command, cwd string) (target, rest string, ok bool) {
	m := cdPrefixRe.FindStringSubmatch(strings.TrimSpace(command))
	if m == nil {
		return "", "", false
	}
	after := strings.TrimSpace(m[1])
	// 分离目标与剩余命令（`cd X && cmd` 或 `cd X; cmd` 均支持 && 形态；; 不特殊处理）
	if i := strings.Index(after, "&&"); i >= 0 {
		rest = strings.TrimSpace(after[i+2:])
		after = strings.TrimSpace(after[:i])
	}
	// 只取目标首个 token；引号包裹的含空格路径保留
	if strings.HasPrefix(after, `"`) || strings.HasPrefix(after, `'`) {
		q := after[0]
		if j := strings.IndexByte(after[1:], q); j >= 0 {
			after = after[1 : 1+j]
		} else {
			return "", "", false // 未闭合引号：交给 shell 报错
		}
	} else if i := strings.IndexAny(after, " \t"); i >= 0 {
		after = after[:i]
	}
	if after == "" {
		return "", "", false
	}
	switch {
	case after == "~":
		if h, err := os.UserHomeDir(); err == nil {
			return h, rest, true
		}
		return "", "", false
	case strings.HasPrefix(after, "~/"):
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, after[2:]), rest, true
		}
		return "", "", false
	case filepath.IsAbs(after):
		return filepath.Clean(after), rest, true
	default:
		return filepath.Clean(filepath.Join(cwd, after)), rest, true
	}
}

// checkHighRisk 静态高风险命令检查：先识别危险命令/可执行词，再按其类型做深度二次判断，
// 命中返回原因（非空即拦截）。目的：避免「整串盲扫一命中就拒绝」——纯文本描述、文件名、
// 参数里的普通词（如 echo shutdown、python stop.py）不应误伤，真正的命令调用才拦。
func checkHighRisk(command string) string {
	// 0. 字面量剥离：单/双引号内容、heredoc 体剔除；
	//    仅 $(…) / `…` 命令替换子表达式保留（它们会真实执行）。命令位检查在
	//    剥离后的结构串上做 —— 否则 `python3 "open(shutdown.py)"`、`echo "(shutdown)"`
	//    里的引号内文本会被 `(` 前导误判成真实命令（问题：引号内词法命中）。
	//    rm 检查不用剥离串（它按 token 流解析，引号目标如 "dist dir" 需保留原文）。
	structural := stripShellLiterals(command)

	// 1. rm 递归强制删除：识别到 rm -rf 后逐个检查目标，仅危险目标拒绝
	//   （. .. / ~ $HOME *.* 全局/根/家/上级）；显式安全子目录（/tmp/foo、dist/、./build）放行。
	if rmTarget := dangerousRmTarget(command); rmTarget != "" {
		return "rm -rf target is " + rmTarget + " (would wipe the workspace/parent/root/home directory)"
	}
	// 2. 命令位危险可执行词（shutdown/reboot/sudo/mkfs 等）：须以「命令身份」出现
	//   （前导为命令分隔符），后随单词边界 —— echo shutdown、python x.sh 这类文本/文件名不命中。
	//   规则带 fn 时二次精确判定（函数定义 / fdisk -l 只读 / dd 非块设备 放行）。
	for _, q := range commandWordDangers {
		if q.re.MatchString(structural) {
			if q.fn == nil || q.fn(structural) {
				return q.name
			}
		}
	}
	// 3. 结构特征类危险（fork 炸弹 / chmod 全放开 / curl|sh 远程管道执行）：模式独特，几乎不误报。
	for _, q := range structuralDangers {
		if q.re.MatchString(structural) {
			if q.fn == nil || q.fn(structural) {
				return q.name
			}
		}
	}
	return ""
}

// dangerousRmTarget 深度识别 rm 递归强制删除（-r 与 -f 组合，短 flag 或 --recursive/--force）。
// 先定位到 rm 调用（命令位置），再逐个检查其后目标：仅当同时具备递归+强制 且 目标为危险路径
// （. .. / ~ $HOME 与 `*`/`.*` 全局、根/家/上级前缀）才返回该目标；显式安全子目录（/tmp/foo、
// dist/、./build、.git/）放行。命令分隔符处重置 rm 状态（避免把 echo 的 rm 文本当目标误判）。
func dangerousRmTarget(command string) string {
	tokens := strings.Fields(command)
	inRm, hasR, hasF := false, false, false
	for _, tok := range tokens {
		switch {
		case isCommandSeparator(tok):
			// 进入新的命令片段：rm/flag 状态作废
			inRm, hasR, hasF = false, false, false
		case tok == "rm":
			inRm, hasR, hasF = true, false, false
		case inRm && isRmFlag(tok):
			applyRmFlags(tok, &hasR, &hasF)
		case inRm:
			// 目标候选：具备递归+强制 且 危险路径 → 拦截
			if hasR && hasF && isDangerousPath(tok) {
				return tok
			}
		}
	}
	return ""
}

// isCommandSeparator 命令分隔符（重置 rm 判断的独立命令片段边界）。
func isCommandSeparator(tok string) bool {
	switch tok {
	case ";", "&&", "||", "|", "(", ")", "`":
		return true
	}
	return false
}

// isRmFlag rm 的 flag token（递归/强制；空逗号号如 "-" 不算）。
func isRmFlag(tok string) bool {
	if tok == "--" || tok == "-" {
		return false
	}
	return strings.HasPrefix(tok, "-")
}

// applyRmFlags 从 rm flag token 提取递归(r)与强制(f)（--recursive/--force 或短 flag 组合 -rf/-r -f 等）。
func applyRmFlags(tok string, hasR, hasF *bool) {
	switch {
	case strings.HasPrefix(tok, "--recursive"):
		*hasR = true
	case strings.HasPrefix(tok, "--force"):
		*hasF = true
	case strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "--"):
		body := strings.TrimLeft(tok, "-")
		if strings.Contains(body, "r") {
			*hasR = true
		}
		if strings.Contains(body, "f") {
			*hasF = true
		}
	}
}

// isDangerousPath rm 危险目标判定：当前目录/根/家本身/上级/全通配（含 .* 隐藏全局）。
// 明确到具体子目录（/tmp/foo、dist/、./build、.git/、~/Downloads/old-backup、
// $HOME/.cache/tmp）不算危险（用户决策 2026-09：家目录子路径放行，与工作区子目录一致）。
// 通配形态（~/Downloads/*、$HOME/*、*/…）仍拦——范围无法静态确认。
func isDangerousPath(tok string) bool {
	q := strings.Trim(tok, `"'`)
	switch q {
	case ".", "./", "..", "../", "/", "~", "~/", "*", ".*", "$HOME", "$HOME/", "~/*", "$HOME/*":
		return true
	}
	return strings.HasPrefix(q, "../") || // 上级目录
		strings.HasPrefix(q, "*/") || // 通配引导的跨目录删除
		// 家目录通配（~/*、$HOME/* 及更深的通配段）仍危险
		hasHomeGlob(q)
}

// hasHomeGlob 家目录路径里是否含通配段（~/*、~/foo/*、$HOME/* 等）。
// 具体子路径（~/Downloads/old-backup、$HOME/.cache/tmp）不含通配 → false。
func hasHomeGlob(q string) bool {
	for _, prefix := range []string{"~/", "$HOME/"} {
		if strings.HasPrefix(q, prefix) {
			rest := q[len(prefix):]
			return strings.Contains(rest, "*")
		}
	}
	return false
}

// dangerRule 一条高危规则：name = 用于错误提示的规则标签；re = 深度命中正则，
// fn = 可选精确判定（有 fn 时在 re 命中后二次确认；无 fn 直接用 re 结果）。
type dangerRule struct {
	name string
	re   *regexp.Regexp
	fn   func(cmd string) bool
}

// commandWordRe 构造「以命令身份出现 word」的深度正则：前导须为命令分隔符（行首/;&&|||()反引号），
// 后随单词边界。这样仅拦真实命令调用（shutdown -h now），放行纯文本/文件名/参数
// （echo shutdown、python shutdown.py、print("reboot")）。
func commandWordRe(word string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[\n;&|()\x60])\s*` + regexp.QuoteMeta(word) + `\b`)
}

// notFunctionDef 排除 shell 函数定义形态（`shutdown() { ... }`）：命令词后紧跟 `()`
// 是定义不是调用（定义本身不执行危险动作）。注意命令位已保证 word 以命令身份出现。
func notFunctionDef(word string) func(cmd string) bool {
	return func(cmd string) bool {
		// 在 cmd 中找命令位的 word，其后若紧跟 () 则为函数定义 → 放行
		re := regexp.MustCompile(`(^|[\n;&|()\x60])\s*` + regexp.QuoteMeta(word) + `\s*\(\)`)
		return !re.MatchString(cmd)
	}
}

// commandWordDangers 命令位危险可执行词：需以命令身份出现（深度判定，见 commandWordRe）。
// 顺序即错误提示顺序。覆盖：关机/重启类、越权、磁盘格式化/分区（均为可执行命令词）。
// 函数定义形态（shutdown() {…}）经 fn 排除；fdisk 只读列出（-l）经 fn 放行。
var commandWordDangers = []dangerRule{
	{name: `\bshutdown\b`, re: commandWordRe("shutdown"), fn: notFunctionDef("shutdown")},                   // 关机
	{name: `\breboot\b`, re: commandWordRe("reboot"), fn: notFunctionDef("reboot")},                         // 重启
	{name: `\bpoweroff\b`, re: commandWordRe("poweroff"), fn: notFunctionDef("poweroff")},                   // 断电
	{name: `\bhalt\b`, re: commandWordRe("halt"), fn: notFunctionDef("halt")},                               // 停机
	{name: `\bsudo\b`, re: commandWordRe("sudo"), fn: notFunctionDef("sudo")},                               // 越权提权
	{name: `\bmkfs\b`, re: commandWordRe("mkfs"), fn: notFunctionDef("mkfs")},                               // 格式化磁盘
	{name: `\bfdisk\b`, re: commandWordRe("fdisk"), fn: fdiskDanger},                                        // 磁盘分区（-l 只读放行）
	{name: `\bdd\s+if=\b`, re: ddIfRe, fn: ddDanger},                                                        // 块设备直写（of= 块设备才拦）
	{name: `\bkill\s+-9\s+1\b`, re: regexp.MustCompile(`(^|[\n;&|()\x60])\s*kill\b[^\n;&|()]*\s+-9\s+1\b`)}, // 杀 init/系统进程
}

// ddIfRe dd 命令位 + if= 初筛（fn 二次确认 of= 目标）。
var ddIfRe = regexp.MustCompile(`(^|[\n;&|()\x60])\s*dd\b[^\n;&|()]*\bif=`)

// ddDanger dd 精确判定：仅当 of= 指向块设备（/dev/ 下非 null/zero/random/urandom/tty）
// 才算块设备直写；of= 普通文件 / 无 of=（输出 stdout）放行（benchmark、读文件等常见用法）。
func ddDanger(cmd string) bool {
	// 取 dd 命令片段（到分隔符为止）
	for _, seg := range splitCommands(cmd) {
		if !strings.HasPrefix(seg, "dd") && !strings.HasPrefix(seg, "dd ") {
			continue
		}
		// 找 of= 参数
		for _, f := range strings.Fields(seg) {
			if strings.HasPrefix(f, "of=") {
				target := strings.TrimPrefix(f, "of=")
				target = strings.Trim(target, `"'`)
				return isBlockDeviceTarget(target)
			}
		}
		// 无 of=：输出到 stdout，不是块设备直写
		return false
	}
	return false
}

// isBlockDeviceTarget of= 目标是否为危险块设备：/dev/ 下且非安全设备白名单。
func isBlockDeviceTarget(t string) bool {
	if !strings.HasPrefix(t, "/dev/") {
		return false // 普通文件路径
	}
	base := strings.TrimPrefix(t, "/dev/")
	switch base {
	case "null", "zero", "random", "urandom", "tty", "stdin", "stdout", "stderr", "fd":
		return false // 安全设备
	}
	return true // /dev/disk* /dev/rdisk* /dev/sda 等块设备
}

// fdiskDanger fdisk 精确判定：无 -l/--list（只读列出）时才算危险（交互分区/写盘）。
func fdiskDanger(cmd string) bool {
	for _, seg := range splitCommands(cmd) {
		if !strings.HasPrefix(seg, "fdisk") {
			continue
		}
		fields := strings.Fields(seg)
		for _, f := range fields[1:] {
			if f == "-l" || f == "--list" {
				return false // 只读列出
			}
		}
		return true // 无 -l：进入交互/写
	}
	return false
}

// splitCommands 把命令按分隔符切成独立命令片段（去空白；含引号内分隔符的误切
// 可接受——词法启发式，见 stripShellLiterals 威胁模型注释）。
func splitCommands(cmd string) []string {
	var out []string
	for _, seg := range regexp.MustCompile(`[\n;&|()\x60]+`).Split(cmd, -1) {
		if s := strings.TrimSpace(seg); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// structuralDangers 结构特征类危险：模式足够独特、几乎不会作为普通文本出现，整串匹配即可。
var structuralDangers = []dangerRule{
	{name: `:\(\)\{`, re: regexp.MustCompile(`:\(\)\{`)}, // fork 炸弹
	// chmod 根目录全放开：目标恰为 /（后随空白/结束/其它 flag），具体子目录不算。
	// 用 fn 精确判定，避免 `/\b` 误伤 /tmp/foo（/ 与 t 之间也有单词边界）。
	{name: `chmod -R 777 根目录`, re: regexp.MustCompile(`\bchmod\s+-R\s+777\b`), fn: chmodRootDanger},
	{name: `\b(curl|wget)\b.*\|\s*sh\b`, re: regexp.MustCompile(`\b(curl|wget)\b.*\|\s*sh\b`)}, // 远程脚本管道执行
}

// chmodRootDanger chmod -R 777 的目标是否为根目录（/ 后随空白/结束）。
func chmodRootDanger(cmd string) bool {
	re := regexp.MustCompile(`\bchmod\s+-R\s+777\s+/\s*([;&|\x60\n]|$)`)
	return re.MatchString(cmd)
}

// ---------- 字面量剥离（问题八：内嵌脚本里的 open() 等同形词误报） ----------

// stripShellLiterals 返回去除「字面内容」后的命令结构串，供命令位启发式匹配使用：
//   - 单引号区（含 ANSI-C $'…'）整体剔除；
//   - 双引号区剔除，但其中的 $(…) / 反引号子表达式会真实执行，原样保留供再扫描；
//   - heredoc 体（<<[-]TAG … 独行 TAG）与 here-string（<<< 行）剔除。
//
// 词法级启发、非 shell 解析器；未闭合引号/定界符时其余部分按字面剔除
// （宁漏勿误——与命令位检查同一威胁模型：故意混淆不在防护承诺内）。
func stripShellLiterals(cmd string) string {
	var b strings.Builder
	i, n := 0, len(cmd)
	for i < n {
		c := cmd[i]
		switch {
		case c == '\'': // 单引号区：到下一个单引号
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return b.String() // 未闭合：其余全为字面
			}
			i += j + 2
		case c == '$' && i+1 < n && cmd[i+1] == '\'': // ANSI-C $'…'
			j := strings.IndexByte(cmd[i+2:], '\'')
			if j < 0 {
				return b.String()
			}
			i += j + 3
		case c == '"': // 双引号区：保留其中的 $(…) / `…` 子表达式
			j := i + 1
			closed := false
			for j < n {
				if cmd[j] == '\\' { // 转义对跳过
					j += 2
					continue
				}
				if cmd[j] == '"' {
					closed = true
					break
				}
				if cmd[j] == '`' { // 反引号替换原样保留
					k := strings.IndexByte(cmd[j+1:], '`')
					if k < 0 {
						return b.String()
					}
					b.WriteString(cmd[j : j+k+2])
					j += k + 2
					continue
				}
				if cmd[j] == '$' && j+1 < n && cmd[j+1] == '(' { // 命令替换 $(…) 原样保留
					k := matchParen(cmd, j+1)
					if k < 0 {
						return b.String()
					}
					b.WriteString(cmd[j : k+1])
					j = k + 1
					continue
				}
				j++
			}
			if !closed {
				return b.String()
			}
			i = j + 1
		case strings.HasPrefix(cmd[i:], "<<<"): // here-string：整行为字面
			nl := strings.IndexByte(cmd[i:], '\n')
			if nl < 0 {
				return b.String()
			}
			i += nl + 1
		case c == '<' && i+1 < n && cmd[i+1] == '<': // heredoc：头保留、体剔除
			tag, bodyStart := heredocTag(cmd, i+2)
			if tag == "" {
				b.WriteByte(c)
				i++
				continue
			}
			end := heredocEnd(cmd, bodyStart, tag)
			if end < 0 {
				return b.String() // 未闭合：体全部按字面剔除
			}
			b.WriteString(cmd[i:bodyStart]) // 保留 `<<[-]'TAG'` 头与其换行
			i = end
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// matchParen 从 open 下标（指向 '('）找配对的 ')' 下标；引号段内的括号不计数；
// 找不到返回 -1。
func matchParen(s string, open int) int {
	depth := 0
	for j := open; j < len(s); j++ {
		switch s[j] {
		case '\'':
			k := strings.IndexByte(s[j+1:], '\'')
			if k < 0 {
				return -1
			}
			j += k + 1
		case '"':
			k := j + 1
			for k < len(s) && s[k] != '"' {
				if s[k] == '\\' {
					k++
				}
				k++
			}
			if k >= len(s) {
				return -1
			}
			j = k
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// heredocTag 解析 << 之后（i 指向第二个 '<' 的下一位置）的定界符：支持 <<-、
// 引号包裹与裸词（引号模式下仅闭合引号终止 tag）。返回 tag 与体起始下标
// （定界符行的下一行首）；非 heredoc 形态返回 ("", -1)，调用方把 '<' 当普通字符写回。
func heredocTag(cmd string, i int) (string, int) {
	n := len(cmd)
	for i < n && cmd[i] == '-' {
		i++
	}
	// bash 语法允许 << 与定界符之间有空白（<< word / <<- word）——
	// 不跳过会把 `<< 'TAG'` 当成非 heredoc，导致体未剥离、体内行首的
	// 危险词等同形词被命令位检查误拦（问题八的带空格变体）。
	for i < n && (cmd[i] == ' ' || cmd[i] == '\t') {
		i++
	}
	var quote byte
	if i < n && (cmd[i] == '\'' || cmd[i] == '"') {
		quote = cmd[i]
		i++
	}
	start := i
	if quote != 0 {
		for i < n && cmd[i] != quote {
			i++
		}
		if i >= n {
			return "", -1 // 引号未闭合：按非 heredoc 处理
		}
	} else {
		for i < n && isHeredocTagByte(cmd[i]) {
			i++
		}
	}
	tag := cmd[start:i]
	if tag == "" {
		return "", -1 // 无 tag（如 "<< "）
	}
	if quote != 0 {
		i++ // 跳过闭合引号
	}
	nl := strings.IndexByte(cmd[i:], '\n') // 体从下一行开始
	if nl < 0 {
		return "", -1
	}
	return tag, i + nl + 1
}

func isHeredocTagByte(c byte) bool {
	return c != ' ' && c != '\t' && c != '\n' && c != ';' && c != '&' && c != '|' && c != '<' && c != '>'
}

// heredocEnd 从 body 起始扫描到「独行 TAG」行（容忍 \r），返回该行之后的下标；找不到 -1。
func heredocEnd(cmd string, from int, tag string) int {
	line := from
	for line <= len(cmd) {
		nl := strings.IndexByte(cmd[line:], '\n')
		content := cmd[line:]
		if nl >= 0 {
			content = cmd[line : line+nl]
		}
		if strings.TrimSuffix(content, "\r") == tag {
			if nl < 0 {
				return len(cmd)
			}
			return line + nl + 1
		}
		if nl < 0 {
			return -1
		}
		line += nl + 1
	}
	return -1
}
