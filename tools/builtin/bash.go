package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"
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
	curCwd map[string]string // 当前工作目录，**按调用方分区**（见 cwdKey）；cd 命令成功后更新

	// resetNotified 记录「该分区上一次的越界提示已经给过」。
	//
	// 为什么需要：越界会**重置**回根 ⇒ 分区 cwd 变回根，于是"再越界一次"看起来
	// 与"从根第一次越界"完全一样。但两者对模型的信息量不同：
	//   · 第一次越界 ⇒ 必须提示（模型以为自己在外面，其实被拉回来了）；
	//   · 连续越界（中间没成功 cd 到根内任何地方）⇒ **再提示一遍是纯噪音**
	//     （claude_s17 §17.5 的不变式：连续越界只提示一次）。
	// 成功 cd 到根内目录 ⇒ 清标记（"真变化后再提示"）。
	resetNotified map[string]bool

	// wd 会话级 cwd 状态（nil = 用上面的实例内 curCwd，见 NewBashToolWithWorkdir）。
	//
	// 为什么两套并存：wd != nil（生产装配）时 cwd 记忆放在**共享**的状态里 ⇒
	// bash / 文件工具 / run_python 同一份真相；wd == nil（库默认 / 独立使用）时
	// 保持实例内记忆，行为一字不变。
	wd *workdir.State

	// spill 落盘目录与序号（惰性建目录，见 ensureSpillDir）。
	//
	// 注意**没有**「结构化结果暂存」字段：结构化结果写进引擎注入的**每次调用独立**的
	// ctx 槽（tools.StructuredSinkFrom(ctx)）。用实例字段会在「主 agent 与后台子 agent
	// 共享同一 ToolEngine」的并发下互相清空/串味（评审实测：脚本路径 3/300 拿到 nil，
	// 退化成 20KB 文本）。
	//
	// spill 落盘仓库（目录 + 序号；见 spill.go：与 run_python 共用同一套约定）。
	spill *spillStore

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

// bashModelOutputLimit 模型路径的输出**内存窗口**（头 + 尾合计）。
//
// 改造前命令输出走无界缓冲（sandbox 的 bytes.Buffer）：实测 32 MB 的输出要
// 96 MB 分配（扩容 ~2× + String() 拷贝 1×），一条 1 GB 的命令能把 bridge 撑爆。
// 而模型侧真正能看到的只有引擎截断后的 20 KB —— 中间那一大段从来没人看。
//
// 16 KB = 头 8 KB + 尾 8 KB。**为什么不是 64 KB**：引擎的截断阈值是 20 KB
// （tools/engine.go:22 defaultMaxResponseSize），返回文本一旦超过它，引擎会
// **再截一次并再落一次盘**，且它那句「full output is written to <path>」指的
// 是**已经被窗口截过的文本**——对模型就是假话。
// 压在阈值以下 ⇒ 引擎不介入 ⇒ 只有我们这一份落盘路径（指向**完整**输出），
// 且模型看到的是「开头 8 KB + 结尾 8 KB」——**错误信息通常在结尾**
// （"error: cannot find package X"），只留头会把最关键的信息丢掉。
const bashModelOutputLimit = 16 << 10

// bashModelSpillThreshold 超过这个量就把**完整**输出落盘，模型拿路径分段读。
//
// 取值 = 模型路径的窗口上限：**凡是模型看不到全貌的输出都落盘** ⇒
// 模型永远拿得到全文（对齐 Claude Code 的 <persisted-output> / OpenCode /
// goose 的做法：落盘 + 预览，而不是硬砍）。
const bashModelSpillThreshold = bashModelOutputLimit

// bashSpillDirPrefix 落盘目录前缀（os.MkdirTemp 在系统临时目录下建，见 ensureSpillDir）。
const bashSpillDirPrefix = "hai-harness-bash-spill-"

// bashDescription 是 bash 工具的描述正文（**不含**工作目录那一行）。
//
// ⚠️ 别在这里写"当前目录"：那一行是**每请求重算**的（见 DescribeFor），
// 写死就等于告诉模型一个过期的目录。
const bashDescription = "Execute a bash/shell command and return its output (including stderr). For file operations (read/search/edit/find files), prefer the dedicated tools (read_file/grep/edit_file/glob) — they return more refined results; use bash for command execution, builds, tests, git, and scripts. Keep a whole shell step in ONE call (commands joined with &&, loops, heredocs) rather than one command per call — and don't wrap a single command in a codemode script: a single-call script only adds a process spawn and hands the output back as JSON instead of text. The working directory persists across calls within this session: after a command like `cd /some/dir && make`, subsequent commands start in /some/dir (no need to re-cd); use `cd` to change it again, or use absolute paths to escape. Each command runs as an isolated process (env/state other than the directory does not persist). PROCESS LIFETIME: processes a command leaves behind are killed when it finishes. A trailing `&` or run_in_background: true starts a background process instead (it returns a pid and a log file, and survives later commands). Access outside the workspace via absolute paths still works (installed Python/Node/Go packages, desktop apps). High-risk commands (rm clearing, sudo, disk operations, curl|sh, etc.) are statically blocked — be explicit about the exact subdirectory when deleting; when approval is enabled, commands require user confirmation before running."

// bashDescribe 拼工具描述：固定格式 + 该调用方当前的工作目录。
//
// 为什么 cwd 要放进**工具描述**（而不是靠一条额外消息）：
//
//	· 模型每个请求都看得到工具 schema ⇒ 这是**最稳的 cwd 载体**
//	· 上下文压缩不动 schema ⇒ 压缩后模型仍然知道自己在哪
//	· 会话恢复后每请求重算 ⇒ 恢复后不会拿着历史里的旧目录
//	· 首轮就有 ⇒ 模型第一次 cd 之前就知道起点（不必先 cd 一次探路）
func bashDescribe(cwd string) string {
	return "Working directory: " + cwd + "\n" + bashDescription
}

// describe 本实例的描述（静态版：按构造 cwd = 工作区根）。
func (t *BashTool) describe(cwd string) string { return bashDescribe(cwd) }

// DescribeFor 按调用方给出描述（含**该调用方当前的工作目录**，实现 tools.ContextDescriber）。
//
// 必须按 ctx 取分区键：主 agent 与子 agent 各有自己的 cwd
// （语义对齐 Claude Code：子 agent 不继承父级的 cd）。
func (t *BashTool) DescribeFor(ctx context.Context) string {
	cwd := t.Cwd
	if t.wd != nil {
		cwd = t.wd.Current(CwdKey(ctx))
	}
	return t.describe(cwd)
}

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
			Description_: bashDescribe(cwd),
			Params_: tools.Obj(map[string]any{
				"command":           tools.Str("Command to execute (bash syntax)"),
				"timeout":           tools.Map{"type": "number", "minimum": 1, "maximum": 3600, "default": 300, "description": "Command timeout in seconds (optional); default 300, range 1-3600"},
				"run_in_background": tools.Bool("Run the command as a background process and return immediately (optional; default false). Use it for anything that must keep running (dev server, watcher); it returns a pid and a log file, and survives later commands."),
			}, "command"),
			CanParallel_: false,
		},
		Cwd:           cwd,
		Timeout:       timeout,
		sbx:           sbx,
		appAllow:      appAllow,
		curCwd:        map[string]string{},
		resetNotified: map[string]bool{},
		spill:         newSpillStore(bashSpillDirPrefix),
	}
	return t
}

// NewBashToolWithWorkdir 创建**接会话级 cwd 状态**的 bash 工具。
//
// wd = 会话级 cwd 状态（workdir.State）：命令的**起始目录**取调用方分区的当前目录，
// cd 成功时也写回同一份状态 ⇒ 文件工具 / run_python 立刻看到同一个目录。
//
// 为什么不是"bash 记一份、别的工具再抄一份"：cwd 是运行期状态（模型随时 cd），
// 两份各记的必然漂移 —— 改造前 bash 与文件工具对不上账正是这么来的。
//
// wd 为 nil ⇒ 退化成 NewBashTool（工作区根），不 panic。
func NewBashToolWithWorkdir(wd *workdir.State, timeout time.Duration, sbx sandbox.Sandbox, appAllow []string) *BashTool {
	if wd == nil {
		return NewBashTool("", timeout, sbx, appAllow)
	}
	t := NewBashTool(wd.Root(), timeout, sbx, appAllow)
	t.wd = wd
	return t
}

// Description 静态版（无调用方上下文时用）：带构造时的 cwd（工作区根）。
//
// 引擎在工具实现 tools.ContextDescriber 时会优先用 DescribeFor(ctx)；
// 这里的返回值给「没有调用方」的场景（工具清单展示、审批文案等）。
func (t *BashTool) Description() string { return t.describe(t.Cwd) }

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
	// RunInBackground：把命令起成**常驻后台进程**并立即返回（见工具描述里的
	// PROCESS LIFETIME 契约）。
	//
	// 为什么需要这个显式参数（而不是让模型自己写 `cmd &`）：`&` 让模型**无法**
	// 表达「我要一个后台进程」与「脚本自己 fork 了浏览器」的区别 ⇒ 宿主只能
	// 一刀切地杀（误杀用户要的服务）或一刀切地不杀（留孤儿烧 CPU）。显式参数
	// 把意图交回唯一知道答案的一方（模型）⇒ 豁免可以精确。
	RunInBackground bool `json:"run_in_background"`
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
	cwd := t.currentCwd(ctx)
	// 后台意图的两种表达，统一收敛到后台模式：
	//   - 显式参数 run_in_background=true（模型主动声明）；
	//   - **命令末尾的 `&`**（shell 惯例）⇒ 自动改写。
	//     不改写的话，那条后台进程会被「命令结束即收组」杀掉 ⇒ 模型以为服务
	//     起来了、其实已经死了（命令返回 0、输出为空 ⇒ 静默失败，最难查）。
	//     改写后语义更好：独立进程组 + 立即返回 pid 与日志路径。
	// 显式参数优先（模型明确说了要后台 ⇒ 别动它的命令文本）。
	//
	// 2026-11-19（R5）：后台识别**先于** cd 拆分。顺序反了的话 `cd X && job &`
	// 会把**会话 cwd** 改到 X —— 而后台进程在自己的进程组里跑，那个 cd 只是它自己
	// 那个 shell 的事（Claude Code 读的是命令的**结束 cwd**，后台命令没有结束 cwd
	// ⇒ 不改会话 cwd）。现实影响极小（实测真·后台命令 3/494,060），但顺序错就是错。
	background := a.RunInBackground
	if !background {
		if stripped, ok := splitTrailingBackground(a.Command); ok {
			cmd = stripped
			background = true
		}
	}
	// 会话 cwd 变更回执（§15.3 B 载体）：cd 生效时在**最终交给模型的那份文本**
	// 末尾追加一行（spill/slim 之后追加 ⇒ 输出被落盘时回执仍可见）。
	cwdNotice := ""
	// cd 生效：更新会话 cwd + 回执。后台命令**不**改会话 cwd（见上）。
	if target, rest, ok := t.splitCd(cmd, cwd); ok && !background {
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			outside := false
			if root := t.projectRoot(); root != "" && !pathWithinRoot(root, target) {
				// 越界（出工作区根）⇒ 记忆重置回根 + 回执（Q2 = 重置而非报错）。
				// 命令本身仍原样交给 shell 跑到那个目录，只是**后续命令的起点**回到根。
				t.resetCurrentCwd(ctx)
				cwd = t.currentCwd(ctx)
				if t.markResetNotified(ctx) {
					cwdNotice = cwdResetNotice(root)
				}
				outside = true
			} else {
				prev := cwd
				t.setCurrentCwd(ctx, target)
				t.clearResetNotified(ctx)
				cwd = target
				cwdNotice = cwdChangeNotice(prev, target)
			}
			if rest != "" && !outside {
				cmd = rest // cd 前缀已由 spec.Cwd 兑现，只跑剩余命令
			}
		}
	}
	// 宿主钩子（BeforeExec）：真实执行前通知（命令 = 剥离 cd 后的最终文本）。
	// 快速返回约定；慢实现阻塞命令本身（宿主应自行异步/限时）。
	if t.BeforeExec != nil {
		t.BeforeExec(cmd)
	}
	// 后台模式：起一个**独立进程组**的常驻进程，立即返回（不等它结束）
	// ⇒ 模型可以继续做别的事。
	//
	// 豁免机制：后台进程在**自己的进程组**里 ⇒ 之后任何命令结束时的「收组」
	// 都不会波及它（豁免靠**边界不同**，不靠标志位 ⇒ 不会漏判、也不会误杀）。
	if background {
		return t.startBackground(ctx, sbx, cmd, cwd)
	}
	var exitCode int
	var timedOut, canceled bool
	started := time.Now() // 墙钟只包住真实执行（cd 解析/高危拦截不算命令耗时）
	// 运行中的输出尾巴（tail -f，**只给人看**）：没有事件出口（嵌套调用/直调）时
	// tailer == nil，OnOutput 保持 nil → 执行层走历史路径，逐字节不变。
	tailer := newOutputTailer(ctx)
	// 收尾：排空最后一帧 + 停发射协程（第 5 轮爬山第 2 步）。defer 挂在创建点 ⇒
	// 任何早退路径（命令出错、上下文取消）都会收。
	defer tailer.close()
	onOutput := spec0OnOutput(tailer)
	// 输出有界化 + 落盘（2026-11-17）：
	//   - 模型路径：内存窗口 16 KB（头 8 + 尾 8，见 bashModelOutputLimit 的注释），
	//     超过 16 KB 把**完整**输出落盘并在结果里报出路径 ⇒ 模型可分段读
	//     （不再只能看被硬砍的 20 KB）。
	//   - 脚本路径：窗口 = 结构化输出上限（1 MiB），超过同样的量落盘。
	// 内存占用与输出大小**无关**（改造前：32 MB 输出 = 96 MB 分配，1 GB 输出能撑爆 bridge）。
	isScript := tools.IsScriptCall(ctx)
	outLimit, spillAfter := bashModelOutputLimit, bashModelSpillThreshold
	headOnly := false
	if isScript {
		// 脚本路径：结构化 output 必须是**逐字节前缀**（脚本按顺序读）⇒ 只留头。
		outLimit, spillAfter = maxBashStructuredOutput, maxBashStructuredOutput
		headOnly = true
	}
	spillPath := t.reserveSpillPath(ctx)
	var spilled bool
	var omitted, totalOut int64
	out, err := sbx.Run(ctx, sandbox.ExecSpec{
		Command:          cmd,
		Cwd:              cwd,
		Timeout:          toolTimeout,
		ExitCode:         &exitCode,
		TimedOut:         &timedOut,
		Canceled:         &canceled,
		OnOutput:         onOutput,
		OutputLimit:      outLimit,
		OutputHeadOnly:   headOnly,
		OutputFile:       spillPath,
		OutputFileAfter:  spillAfter,
		Spilled:          &spilled,
		OmittedBytes:     &omitted,
		TotalOutputBytes: &totalOut,
	})
	// 末帧补发：命令结束 ⇒ 把最后 ≤250ms 的输出补出去（节流窗口内的那段没人看得到，
	// 而错误信息通常就在结尾）。短命令不补（防抖，见 flushFinal 的注释）。
	tailer.flushFinal()
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
	// 落盘提示（**只加在模型路径的文本上**）：
	//   - 模型拿到路径就能分段读完整输出，而不是只拿到被引擎硬砍的 20 KB；
	//   - **不进结构化 output** —— 那是脚本按顺序读的**逐字节前缀**，
	//     插任何提示行都会污染数据（bash_structured_test.go 钉住）。
	//
	// 位置：在超时/取消声明**之后**加 —— 那两条声明必须留在首行。
	modelOut := out
	if spilled && spillPath != "" {
		modelOut = fmt.Sprintf("[OUTPUT TOO LARGE (%s) — full output saved to %s (this path is valid for %d hours)\n"+
			"Read it with read_file (start_line/end_line) or bash (head/grep/sed) to get the parts you need.]\n%s",
			humanBytes(totalOut), spillPath, int(spillFileTTL.Hours()), out)
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
	if isScript {
		if sink := tools.StructuredSinkFrom(ctx); sink != nil {
			spill := ""
			if spilled {
				spill = spillPath
			}
			sink.Set(t.buildStructured(ctx, out, exitCode, wall, spill))
		}
	}
	// cwd 回执追加在**模型路径**上（结构化 output 必须保持逐字节纯净，见上）。
	if cwdNotice != "" {
		modelOut += cwdNotice
	}
	return modelOut, nil
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
func (t *BashTool) buildStructured(ctx context.Context, out string, exitCode int, wall time.Duration, spillPath string) []byte {
	so := bashStructuredOutput{Output: out, ExitCode: exitCode, WallTimeSec: wall.Seconds()}
	if len(out) > maxBashStructuredOutput || spillPath != "" {
		so.Truncated = true
		so.Output = truncateUTF8(out, maxBashStructuredOutput) // 不劈裂多字节字符
		// 全文路径优先用**执行层**已经落盘的那个（它拿到的是命令的完整输出，
		// 不是被内存窗口截过的文本）；没有才自己再落一份（兜底）。
		so.FullOutputPath = spillPath
		if so.FullOutputPath == "" {
			so.FullOutputPath = t.spillFile(ctx, out)
		}
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
	return t.spill.write(ctx, full)
}

// reserveSpillPath 预留一个落盘路径（**文件由执行层按需创建** —— 输出没超阈值
// 就不会有文件，也就没有磁盘开销）。返回空串 = 拿不到路径（只能靠内存窗口截断）。
//
// 与 spillFile 的分工：spillFile 是「拿到完整文本后再写一遍」；本函数是**提前
// 给出路径**，让执行层边跑边写 ⇒ 命令输出 1 GB 也不会在内存里堆 1 GB。
func (t *BashTool) reserveSpillPath(ctx context.Context) string {
	return t.spill.reserve(ctx)
}

// startBackground 起一个**后台常驻进程**并把句柄（pid + 日志路径）返回给模型。
//
// 契约（与工具描述里的 PROCESS LIFETIME 一致）：
//   - 它在**自己的进程组**里 ⇒ 后续命令结束时的收组不会波及它；
//   - stdout+stderr 合并写进日志文件（模型可 tail / grep / read_file）；
//   - 它活到**自己退出**为止（不设时限）；应用关闭时我们统一收掉（sandbox.Shutdown）。
//
// 为什么后台模式**不**走普通执行路径（runExec）：普通路径等命令结束、且结束即收组
// ⇒ 「立即返回 + 进程继续活」这两件事它做不到。
func (t *BashTool) startBackground(ctx context.Context, sbx sandbox.Sandbox, cmd, cwd string) (string, error) {
	logPath := t.spill.reserveNamed(ctx, "bg")
	if logPath == "" {
		return "", errors.New("bash: cannot create a log path for the background process")
	}
	starter, ok := sbx.(sandbox.BackgroundStarter)
	if !ok {
		return "", errors.New("bash: this execution backend does not support background runs")
	}
	proc, err := starter.StartBackground(ctx, sandbox.ExecSpec{
		Command:       cmd,
		Cwd:           cwd,
		Timeout:       -1, // 后台不限时（活到自己退出）
		BackgroundLog: logPath,
	})
	if err != nil {
		return "", fmt.Errorf("bash: background start failed: %w", err)
	}
	return fmt.Sprintf("[started in background] pid=%d log=%s\n"+
		"It keeps running after this call. Follow it with `tail -f %s` (or read the file); stop it with `kill %d`.",
		proc.Pid(), logPath, logPath, proc.Pid()), nil
}

// cwdKey 调用方的 cwd 记忆分区键（实现与理由见同包的 workdir.go）：
// 分区键 = 子 agent 的 task id（跨轮稳定 ✓），主 agent = workdir.MainKey（跨轮稳定 ✓）。

// currentCwd 返回当前工作目录（本调用方分区内跨调用记忆；初始 = 构造 cwd）。
func (t *BashTool) currentCwd(ctx context.Context) string {
	key := cwdKey(ctx)
	if t.wd != nil {
		// 会话级状态：与文件工具 / run_python 共用同一份真相（见 workdir.go）。
		return t.wd.Current(key)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if dir := t.curCwd[key]; dir != "" {
		return dir
	}
	return t.Cwd
}

// setCurrentCwd 更新本调用方分区的 cwd（cd 目标存在且为目录时调用）。
func (t *BashTool) setCurrentCwd(ctx context.Context, dir string) {
	key := cwdKey(ctx)
	if t.wd != nil {
		t.wd.Set(key, dir)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.curCwd[key] = dir
}

// WorkdirState 暴露本工具使用的会话级 cwd 状态（tools.WorkdirAware）。
func (t *BashTool) WorkdirState() *workdir.State { return t.wd }

// projectRoot 返回工作区根（cd 越界判据 + 重置目标）。
//
// 没接会话级状态时用构造 cwd（改造前的口径：cwd 即工作区根）。
func (t *BashTool) projectRoot() string {
	if t.wd != nil {
		return t.wd.Root()
	}
	return t.Cwd
}

// resetCurrentCwd 把本调用方分区复位回工作区根（cd 越界时用）。
func (t *BashTool) resetCurrentCwd(ctx context.Context) {
	key := cwdKey(ctx)
	if t.wd != nil {
		t.wd.Reset(key)
		return
	}
	t.mu.Lock()
	delete(t.curCwd, key)
	t.mu.Unlock()
}

// markResetNotified 记录本次越界提示并返回**是否该提示**（false = 上一次已是连续越界）。
//
// 只用实例内 map：与 curCwd 同生命周期（同一会话的工具实例）。wd != nil 时也放在
// 实例上 —— 它表达的是「提示过没有」，是**工具行为**的状态，不是 cwd 真值。
func (t *BashTool) markResetNotified(ctx context.Context) bool {
	key := cwdKey(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resetNotified == nil {
		t.resetNotified = map[string]bool{}
	}
	if t.resetNotified[key] {
		return false
	}
	t.resetNotified[key] = true
	return true
}

// clearResetNotified 清掉该分区的「已提示过越界」标记（成功 cd 后调用）。
func (t *BashTool) clearResetNotified(ctx context.Context) {
	key := cwdKey(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.resetNotified, key)
}

// pathWithinRoot 判定 p 是否落在 root 内（含 root 自身）。
func pathWithinRoot(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// cwdChangeNotice cd 生效时的回执（空 = 无变化，不需要追加）。
//
// 措辞对齐 Claude Code 的 "Shell cwd was reset to <dir>"（重置那条），但正常切换
// 不叫 "shell cwd" —— shell 的其它状态确实不保持，只有 cwd 保持，避免误导。
func cwdChangeNotice(prev, cur string) string {
	if cur == "" || cur == prev {
		return ""
	}
	return "\nWorking directory is now " + cur
}

// cwdResetNotice 越界重置的回执。
func cwdResetNotice(root string) string {
	return "\nWorking directory was reset to " + root +
		" (the requested path is outside the workspace root)"
}

// cdPrefixRe 匹配「命令以 cd <目标> 开头」。
//
// 2026-11-19 修正（R1）：**必须能匹配多行命令**（heredoc）。
// 原式 `^cd\s+(.+)$` 在 Go 里 `.` 不匹配 \n、`$` 只在文本末尾 ⇒
// 「cd X && python3 - <<'EOF' … EOF」这类命令**匹配不上** ⇒ cd 记忆对它静默失效
// （实测：真实数据里 23.55% 的 cd 命令是这种形态）。
// `(?s)` + `([^\n]+)` ⇒ 只吃**第一行**的 cd 目标，后续正文原样保留。
var cdPrefixRe = regexp.MustCompile(`(?s)^cd\s+([^\n]+)`)

// splitCd 解析命令开头的 `cd <目标>`：返回 (目标绝对路径, 剩余命令, true)。
// 剩余命令 = `cd X && cmd` 的 cmd（无 && 则为 ""）。`cd` 无参/非 cd 开头 → ("","",false)。
// 目标支持：绝对路径 / 相对路径（相对 cwd）/ ~ / ~/xxx（展开 home）。
func (t *BashTool) splitCd(command, cwd string) (target, rest string, ok bool) {
	trimmed := strings.TrimSpace(command)
	// 正则只吃**第一行**（见 cdPrefixRe），第一行之后的正文必须原样接回来：
	// 否则 heredoc 的正文会被整段丢掉（2026-11-19 R1 配套，§16.3 #2）。
	m := cdPrefixRe.FindStringSubmatch(trimmed)
	if m == nil {
		return "", "", false
	}
	tail := strings.TrimSpace(trimmed[len(m[0]):])
	after := strings.TrimSpace(m[1])
	// 分离目标与剩余命令：`cd X && cmd` 的 cmd。`;` 刻意**不做**分隔符
	//（引号路径里出现 `;` 比换行常见）。
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
		h, err := os.UserHomeDir()
		if err != nil {
			return "", "", false
		}
		target = h
	case strings.HasPrefix(after, "~/"):
		h, err := os.UserHomeDir()
		if err != nil {
			return "", "", false
		}
		target = filepath.Join(h, after[2:])
	case filepath.IsAbs(after):
		target = filepath.Clean(after)
	default:
		target = filepath.Clean(filepath.Join(cwd, after))
	}
	// 第一行之后的正文接回剩余命令（换行分隔 ⇒ 用 \n 拼）。
	if tail != "" {
		if rest != "" {
			rest += "\n"
		}
		rest += tail
	}
	return target, rest, true
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
//
// 只在**包初始化**时调用（dangerRules 的元素构造）⇒ 编译一次。
func commandWordRe(word string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[\n;&|()\x60])\s*` + regexp.QuoteMeta(word) + `\b`)
}

// notFunctionDef 排除 shell 函数定义形态（`shutdown() { ... }`）：命令词后紧跟 `()`
// 是定义不是调用（定义本身不执行危险动作）。注意命令位已保证 word 以命令身份出现。
func notFunctionDef(word string) func(cmd string) bool {
	// 正则在**闭包外**编译一次（改造前在闭包内 ⇒ 每命中一次规则就重编译一次正则）。
	re := regexp.MustCompile(`(^|[\n;&|()\x60])\s*` + regexp.QuoteMeta(word) + `\s*\(\)`)
	return func(cmd string) bool {
		// 在 cmd 中找命令位的 word，其后若紧跟 () 则为函数定义 → 放行
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

// commandSeparatorRe 命令分隔符（包级编译一次 —— 改造前每次调用都重编译）。
var commandSeparatorRe = regexp.MustCompile(`[\n;&|()\x60]+`)

// splitCommands 把命令按分隔符切成独立命令片段（去空白；含引号内分隔符的误切
// 可接受——词法启发式，见 stripShellLiterals 威胁模型注释）。
func splitCommands(cmd string) []string {
	var out []string
	for _, seg := range commandSeparatorRe.Split(cmd, -1) {
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

// chmodRoot777RootRe chmod -R 777 的目标恰为根目录（包级编译一次）。
var chmodRoot777RootRe = regexp.MustCompile(`\bchmod\s+-R\s+777\s+/\s*([;&|\x60\n]|$)`)

// chmodRootDanger chmod -R 777 的目标是否为根目录（/ 后随空白/结束）。
func chmodRootDanger(cmd string) bool {
	return chmodRoot777RootRe.MatchString(cmd)
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
