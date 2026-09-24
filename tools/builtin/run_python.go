package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RunPythonTool 用**受管 Python 运行时**执行一段脚本（办公文档/数据处理的主执行通道）。
//
// 与 bash 的关系：同为「执行类工具」，但语义收窄 —— 模型不能指定解释器、不能加命令行开关、
// 不能借它执行任意二进制。装配方式与 bash 一致：cwd = 工作区根（与文件工具同锚）、执行经
// 调用方注入的 sandbox.Sandbox（与 bash 同一个后端）、超时语义照搬 bash.go。
//
// 三个实现决策（改本文件前先读）：
//
//  1. **脚本传递：写临时文件再执行，绝不把源码拼进命令行。**
//     命令行要经 sh -c 走 shell（sandbox.ExecSpec.Command 是 shell 文本），把源码内联进去要过
//     两层转义（shell 的引号/反引号/$，Python 的引号/转义），引号、换行、$、中文任一组合都能
//     把脚本改坏；sandbox.ExecSpec 也没有 stdin 字段（进程式接口：Command/Cwd/Timeout），
//     故「stdin 传脚本（python -）」当前不可行。落临时文件后命令行只剩两个路径参数，
//     用单引号 shell 转义即可，且 traceback 里带真实文件路径与行号（调试友好）。
//     临时目录：优先受管运行时目录（<Root>/tmp，P1 已确认沙箱对该子树 carve-out 读写两开 ——
//     这是 venv 能跑起来的前提，故一定可读），失败兜底系统 tmp（macOS 实测 /tmp、
//     /private/tmp、/private/var 均在沙箱读放行内）。执行后**一律清理**（defer RemoveAll）。
//     注意副作用：脚本文件所在目录不是工作区 —— 模型要用「相对于工作区」的路径写产物
//     （cwd 已设为工作区根），不要用 __file__ 推同目录（工具描述里已如实写明）。
//
//  2. **沙箱文件边界 + 审批是本工具的护栏**（不是 Python 层的语法限制）。
//     脚本内可 `import subprocess`，也能自己算字符串再删文件 —— 语义层面拦不住，真正的边界是
//     （a）沙箱文件策略：**默认装配下工作区、$HOME、/tmp 可写**（生产 opts 默认开
//     sandbox.WithHomeWrite，见 desktop/bridge/main.go 的装配点），其余宿主路径写被内核拒
//     （EPERM，与 bash 同语义）；注意凭据目录（~/.ssh/.aws 等）**不在默认 deny 清单里**
//     （sandbox/defaultSensitivePaths 只含 .go-code），只有用户经 sandbox.sensitive_paths
//     显式追加才 deny —— 工具描述里如实写明，不得宣称「工作区外一律拒绝」；
//     （b）工具层 HITL 审批（RequiresApproval + desktop/bridge parseMode 名单）。
//
//  3. **另加一条窄口径静态 tripwire**（checkPythonHighRisk，见其注释）：只拦「递归删除
//     根/家/工作区根**本身**」的字面量目标与经 os.system/subprocess 转 shell 的 `rm -rf 危险目标`。
//     不照搬 bash 的 shell 正则（Python 源码里字符串/注释/模板文本与真实调用不同义，照搬既漏又伤），
//     也不做语义分析（变量拼接一律漏过 —— 有意为之，见函数注释）。理由与代价写在函数注释里。
type RunPythonTool struct {
	tools.BaseTool
	Timeout time.Duration // 单次脚本默认超时（模型未传 timeout 时兜底；<=0 → 2 分钟）
	Cwd     string        // 脚本工作目录（= 工作区根，见 NewRunPythonTool）
	py      PythonRuntime // 受管运行时（nil = 未装配 → 调用时报明确错误）
	ft      *FileTools    // 仅用于 script_path 的路径解析（与文件工具同口径：工作区锚定 + ~/.go-code 保护）
	sbx     sandbox.Sandbox

	// diff 本次调用对**声明的产物**的变更（engine 经 ToolDiffProvider 读走，见 Diff）。
	// 暂存字段的并发前提：本工具 CanParallel=false（执行类工具，同 bash），同一实例不会
	// 被并行批共享 —— 与 writeFileTool.diff 同一先例；若将来放开并行，必须改成 ctx 槽
	// （见 tools.ImageSink 注释里那个张冠李戴的窗口）。
	diff *core.FileDiff
}

// PythonRuntime run_python 需要的受管运行时能力面（*runtime.Runtime 实现；测试注入假实现）。
// 只依赖 Ensure 一个方法：工具不关心 venv/依赖清单怎么建，只关心「给我一个可执行解释器」。
type PythonRuntime interface {
	Ensure(ctx context.Context) (runtime.Python, error)
}

// 脚本时限策略（对齐 bash 的 default/max 口径：模型可经 timeout 参数覆盖，钳制 1s–1h）。
const (
	defaultRunPythonTimeout = 2 * time.Minute
	maxRunPythonTimeout     = time.Hour

	// bootstrapHeadroom 引擎级时限的额外余量：工具级/Percall 时限覆盖**整个 Call**，
	// 而首次调用要先 Ensure（建 venv + pip 装约 42MB 依赖，慢网到分钟级）。若引擎按
	// 「脚本时限」掐整个 Call，首次调用会在 bootstrap 中途被 ctx 取消、报成看不懂的错误。
	// 故引擎看到的时限 = 脚本时限 + 本余量；真正传给 sandbox 的 spec.Timeout 仍是脚本时限
	// （脚本本身不多拿一秒）。Runtime 自身对每条 bootstrap 子命令另有 10 分钟上限。
	runPythonBootstrapHeadroom = 10 * time.Minute

	// maxPythonCheckBytes script_path 静态检查的读取上限（.py 源码，1 MiB 足够；
	// 超长脚本只检查前 1 MiB —— 检查本身是尽力而为的 tripwire，见 checkPythonHighRisk）。
	maxPythonCheckBytes = 1 << 20

	// maxRunPythonOutputs outputs 声明条数上限（= schema maxItems）。
	// 产出卡是「本轮成果」摘要而非文件清单：办公场景的批量产物（十来个 xlsx/pptx）用它足够，
	// 同时给「声明」这一动作一个硬边界 —— 让模型按重要性排序、只列真正的产物。
	// 超限是**参数错误**（Call 在执行前就拒绝）：静默丢掉多余声明 = 用户拿到残缺的产出卡。
	maxRunPythonOutputs = 10
)

// NewRunPythonTool 创建 run_python 工具。
// workspace = 工作区根（cwd + script_path 相对路径锚点，与 FileTools 同口径）。
// py = 受管运行时（nil = 未装配，调用时返回可恢复的结构化错误，不 panic）。
// sbx = 执行后端（nil = NoSandbox 进程隔离直通；通常与 bash 注入同一个实例）。
func NewRunPythonTool(workspace string, py PythonRuntime, sbx sandbox.Sandbox) *RunPythonTool {
	return &RunPythonTool{
		BaseTool: tools.BaseTool{
			Name_: "run_python",
			Description_: "Execute a Python script and return its output (stdout+stderr merged). This is the execution channel for office documents and data work: python-docx, openpyxl, python-pptx, pypdf, pypdfium2 and mammoth are preinstalled. " +
				"The interpreter is fixed (you cannot choose a binary or command-line switches) and the script runs inside the OS sandbox with the working directory set to the workspace root; under the default policy the workspace, $HOME and /tmp are writable, go-code's own config directory (~/.go-code, except its managed plugins/ and runtime/ subtrees) is denied for read and write, and writes to any other host path are rejected — but that policy does not cover credential files under $HOME (e.g. ~/.ssh/.aws) unless the user listed them in sandbox.sensitive_paths, so never read or copy them. Obviously destructive literals (wiping the workspace, home or root itself) are rejected before execution. " +
				"Network access must not be assumed: outbound connections follow the same host policy as bash (currently allowed), but nothing guarantees reachability — do not design a task around downloads, and do not run pip install on your own. Python can still call subprocess internally, so treat this as a policy boundary rather than a hard isolation. " +
				"Provide exactly one of script (source) or script_path (a .py file in the workspace). script is written to a temporary file that is deleted after the run, so write outputs with paths relative to the working directory (the workspace), not next to the script. " +
				"Declare what the script writes in outputs (up to 10 workspace-relative or absolute paths, most important first): the declared files that exist after the run are reported to the user as this run's artifacts, and the result tells you which declared paths did not materialize. List only files this call writes — not inputs, not files you merely read. " +
				"The first call can take minutes while the managed runtime installs itself (tens of MB, one time only; the venv measures about 70 MB).",
			Params_: tools.Obj(map[string]any{
				"script":      tools.Str("Python source code to execute (provide exactly one of script / script_path)"),
				"script_path": tools.Str("Path to a Python script: relative to the workspace root, or absolute (provide exactly one of script / script_path)"),
				"timeout":     tools.Map{"type": "number", "minimum": 1, "maximum": 3600, "default": 120, "description": "Script timeout in seconds (optional); default 120, range 1-3600"},
				"outputs": tools.Map{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxRunPythonOutputs,
					"description": "Files this script writes, most important first (up to 10; workspace-relative like out/report.xlsx, or absolute). Declared paths that exist after the run show up as this run's artifacts for the user; a line in the result reports the ones that did not materialize. List only files this call writes."},
			}),
			CanParallel_: false, // 执行类工具：与 bash 一致不并行
			ReadOnly_:    false, // 能写文件（脚本可落盘产物）
		},
		Timeout: 0,
		Cwd:     workspace,
		py:      py,
		ft:      NewFileTools(workspace),
		sbx:     sbx,
	}
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：脚本默认时限 + bootstrap 余量（见常量注释）。
func (t *RunPythonTool) ToolTimeout() time.Duration {
	def := t.Timeout
	if def <= 0 {
		def = defaultRunPythonTimeout
	}
	return def + runPythonBootstrapHeadroom
}

// Diff 实现 tools.ToolDiffProvider：本次调用声明的产物里**第一个存在**的文件
// （engine 在 Call 成功后读走 → ToolResponse.Diff → 产出卡；见 summarizeOutputs）。
// 无 outputs 声明 / 声明的路径都没落盘 → nil（不产生产出，绝不猜）。
func (t *RunPythonTool) Diff() *core.FileDiff { return t.diff }

// TimeoutForArgs 实现 tools.PerCallTimeout：模型经 timeout 参数（秒）自设脚本时限；
// 缺省/非法 → 0（用工具级默认）。返回给引擎的时限额外加 bootstrap 余量（spec 时限不含余量）。
func (t *RunPythonTool) TimeoutForArgs(arguments string) time.Duration {
	var a runPythonArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return 0
	}
	if v := parseRunPythonTimeout(a); v > 0 {
		return v + runPythonBootstrapHeadroom
	}
	return 0
}

// RequiresApproval 实现 tools.ApprovalRequired：与 bash 同理 —— run_python 能写文件、
// 能借 subprocess 执行任意命令，属执行类高权限入口，启用审批时需用户确认。
// （模式策略可覆盖：auto/full-access 下不弹；manual/hitl/accept-edits 见 parseMode 名单。）
func (t *RunPythonTool) RequiresApproval(context.Context, core.ToolCall) bool { return true }

type runPythonArgs struct {
	Script     string   `json:"script"`
	ScriptPath string   `json:"script_path"`
	Timeout    *float64 `json:"timeout"` // 秒；缺省 nil → 用默认（NewRunPythonTool.Timeout / 2 分钟）
	Outputs    []string `json:"outputs"` // 脚本产出的文件声明（可选；见 summarizeOutputs）
}

// outputsArgError 校验 outputs 声明条数上限（0–maxRunPythonOutputs）；返回错误消息，合法返回 ""。
// 不做目录快照/时间窗推断：脚本的 args 是源码、result 是 stdout，都不含可靠路径 ——
// 产物必须由模型显式声明（启发式在用户机器上必然误报/漏报）。
func outputsArgError(a runPythonArgs) string {
	if len(a.Outputs) > maxRunPythonOutputs {
		return fmt.Sprintf("outputs accepts at most %d entries (got %d)", maxRunPythonOutputs, len(a.Outputs))
	}
	return ""
}

// parseRunPythonTimeout 解析模型传入的 timeout（秒）：缺省/非法 → 0（= 用工具级默认）；
// 合法值钳制到 [1s, 1h]（与 bash 同口径）。
func parseRunPythonTimeout(a runPythonArgs) time.Duration {
	if a.Timeout == nil || *a.Timeout <= 0 {
		return 0
	}
	d := time.Duration(*a.Timeout * float64(time.Second))
	if d < time.Second {
		return time.Second
	}
	if d > maxRunPythonTimeout {
		return maxRunPythonTimeout
	}
	return d
}

// effectiveTimeout 解析模型传入的 timeout（秒）：非法/缺省 → 工具默认（直接调用兜底用）。
func (t *RunPythonTool) effectiveTimeout(a runPythonArgs) time.Duration {
	def := t.Timeout
	if def <= 0 {
		def = defaultRunPythonTimeout
	}
	if v := parseRunPythonTimeout(a); v > 0 {
		return v
	}
	return def
}

// scriptArgError 校验 script / script_path 二选一；返回（消息, 修正建议），无错返回 ""。
func scriptArgError(a runPythonArgs) (string, string) {
	hasScript := strings.TrimSpace(a.Script) != ""
	hasPath := strings.TrimSpace(a.ScriptPath) != ""
	switch {
	case hasScript && hasPath:
		return "script and script_path are mutually exclusive (both were provided)",
			"Send exactly one of them: script for inline source, or script_path for a .py file in the workspace."
	case !hasScript && !hasPath:
		return "one of script or script_path is required (neither was provided)",
			"Send the Python source as script (or the path of an existing .py file as script_path)."
	}
	return "", ""
}

func (t *RunPythonTool) ValidParams(_ context.Context, _, arguments string) error {
	var a runPythonArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("run_python: %w", err)
	}
	if msg, _ := scriptArgError(a); msg != "" {
		return errors.New("run_python: " + msg)
	}
	if msg := outputsArgError(a); msg != "" {
		return errors.New("run_python: " + msg)
	}
	return nil
}

func (t *RunPythonTool) Call(ctx context.Context, _, arguments string) (string, error) {
	t.diff = nil // 上次调用的产物不得泄漏到本次（engine 只在 Call 成功后读取）
	var a runPythonArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	if msg, fix := scriptArgError(a); msg != "" {
		return "", recoveryError("invalid_arguments", "correct_arguments", msg, fix)
	}
	if msg := outputsArgError(a); msg != "" {
		return "", recoveryError("invalid_arguments", "correct_arguments", msg,
			"Declare at most "+strconv.Itoa(maxRunPythonOutputs)+" files, most important first — the first declared file that exists becomes the one shown to the user.")
	}
	// 时限三层（骨架同 bash.Call，多一层 bootstrap 余量，见常量注释）：
	//   - 引擎可见时限（ToolTimeout/TimeoutForArgs）= 脚本时限 + bootstrap 余量 → 首次调用的
	//     Ensure（建 venv + 装依赖，慢网到分钟级）不会在安装中途被引擎掐掉；
	//   - 传给沙箱的 spec.Timeout = **脚本时限本身**（不含余量）。sandbox.runExec 在
	//     timeout>0 时用 min(ctx deadline, timeout)，故引擎那层更宽的 ctx 不会放宽脚本上限 ——
	//     余量只可能被 bootstrap 用掉，脚本不多拿一秒；
	//   - promote 为后台任务（IsUnlimited）→ -1 = 不限时（与 bash 及工具族惯例一致：
	//     后台任务不受时长限制，改由显式中断控制）。
	scriptTimeout := time.Duration(0)
	if tools.IsUnlimited(ctx) {
		scriptTimeout = -1
	} else {
		scriptTimeout = t.effectiveTimeout(a)
	}

	// ① 脚本来源（先解析，不执行）：
	//   - script → 源码字符串（稍后落临时文件）；
	//   - script_path → 解析为工作区内绝对路径（与文件工具同锚 + ~/.go-code 保护），
	//     直接执行该文件以保留 __file__/相对导入语义、traceback 指向用户真实文件。
	// 先解析的意义：参数错/文件不存在/高危脚本都在**触发 bootstrap（下载 ~42MB）之前**失败。
	src, scriptPath, err := t.resolveScript(a)
	if err != nil {
		return "", err
	}

	// ② 静态 tripwire（执行前，见 checkPythonHighRisk）
	if reason := checkPythonHighRisk(src); reason != "" {
		where := "script"
		if scriptPath != "" {
			where = "script_path " + scriptPath
		}
		return "", recoveryError("high_risk_script", "rewrite_script",
			"high-risk script rejected: "+reason+" ("+where+"); the script was not executed",
			"Target a specific subdirectory instead of the workspace/home/root itself (e.g. shutil.rmtree(\"build\")), or delete nothing and write the result to a new file. Do not repeat the same script.")
	}

	// ③ 受管运行时：首次调用会 bootstrap（建 venv + 装依赖）。失败 → 可恢复的结构化错误，
	//    引导模型告知用户，而不是 panic/静默失败（环境不可用是用户侧问题，模型重试无用）。
	if t.py == nil {
		return "", runPythonRuntimeError(errors.New("no managed Python runtime is configured for this session"))
	}
	py, err := t.py.Ensure(ctx)
	if err != nil {
		return "", runPythonRuntimeError(err)
	}

	// ④ script 语法糖：落临时文件（执行后清理）；script_path 已在①就位。
	if scriptPath == "" {
		p, cleanup, werr := writeTempScript(py, src)
		if werr != nil {
			return "", recoveryError("temp_script_failed", "report_to_user", singleLine(werr.Error()),
				"Retry once; if it keeps failing the tmp directory is not writable (disk full or restricted TMPDIR). Tell the user the failure detail.")
		}
		defer cleanup()
		scriptPath = p
	}

	// ⑤ 固定解释器 + 脚本路径（单引号转义：路径可含空格/引号/中文）。
	//   模型不可指定解释器与开关 —— 命令行里只有这两个我们自己的参数。
	cmdline := shQuote(py.VenvPython) + " " + shQuote(scriptPath)
	sbx := t.sbx
	if sbx == nil {
		sbx = sandbox.NoSandbox{}
	}
	// ⑥ 产物声明：**执行前**快照「此前是否存在」（Created 判定源 —— 覆盖与新建只能在这一刻
	//   区分，脚本跑完就无从得知；同 write_file 的 lock==nil）。失败声明不阻塞执行，见
	//   declaredOutputs 注释；无 outputs 声明时这里零 IO。
	outs := t.declaredOutputs(a.Outputs)
	out, err := sbx.Run(ctx, sandbox.ExecSpec{
		Command: cmdline,
		Cwd:     t.Cwd,
		Timeout: scriptTimeout,
	})
	if err != nil {
		return out, err // 执行后端本身失败（非脚本非零退出）：不产生产出（engine 也不会读 diff）
	}
	// ⑦ 补齐产物事实（存在性/尺寸/指纹 → FileDiff）+ 结果文本里的显式告知。
	return t.summarizeOutputs(out, outs), nil
}

// ---------- 产物声明（outputs → ToolDiffProvider → 产出卡） ----------
//
// 为什么必须**显式声明**而不是自动发现：脚本的 args 是源码、result 是 stdout，两者都不含
// 可靠路径（模型可能用变量拼接/f-string 拼路径）；按目录快照或时间窗推断会在用户机器上
// 误报（把日志/缓存当产物）与漏报（产物写在子目录）。声明式契约把「哪个文件是本轮成果」
// 交回唯一知道答案的一方（模型），工具侧只做**事实核对**：声明的路径到底落盘没有。
//
// 与产出卡的关系（agents/artifacts.go）：本工具实现 tools.ToolDiffProvider，engine 把
// Diff 填进 events.ToolResponse.Diff（仅在 Call 成功时）；采集器据此把这份 diff 计入
// AgentEnd.Artifacts。Diff 只给宿主 UI（不进入 LLM 上下文），模型侧的信息渠道是结果文本
// 里那行显式告知（见 summarizeOutputs）——两条渠道互不替代。

// runPythonOutput 一条 outputs 声明的运行前快照。
type runPythonOutput struct {
	declared string // 模型声明的原始写法（缺失告知里回显它，模型才能对上自己的参数）
	abs      string // 解析后的绝对路径（resolve 失败 → 空：相对路径越出工作区 / ~/.go-code 保护区）
	existed  bool   // 运行前是否已存在（Created 判定源）
}

// declaredOutputs 解析 outputs 声明并做运行前存在性快照（≤10 次 resolve+stat，元数据级 IO）。
//   - 空白项跳过：声明里没有路径 = 没有声明（不当错误）；
//   - 解析失败不报错：resolve 只管路径合法性（越出工作区/~/.go-code），这条路径只是
//     「不会成为产物」—— 由 summarizeOutputs 的显式告知让模型自我纠正，而不是让整次
//     执行失败（脚本本身可能完全成功，产物也确实写到了别处）。
func (t *RunPythonTool) declaredOutputs(outputs []string) []runPythonOutput {
	if len(outputs) == 0 {
		return nil
	}
	out := make([]runPythonOutput, 0, len(outputs))
	for _, raw := range outputs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		o := runPythonOutput{declared: raw}
		if abs, err := t.ft.resolve(raw); err == nil {
			o.abs = abs
			if _, err := os.Stat(abs); err == nil {
				o.existed = true
			}
		}
		out = append(out, o)
	}
	return out
}

// summarizeOutputs 执行后核对声明并组织结果文本：
//   - 产出（diff）：取**第一个存在**的声明路径（模型按重要性排序，第一个即主产物）→
//     构造 FileDiff 并暂存给 engine（Diff()）；
//   - 显式告知：没落盘的声明写进结果首行（outputs: N/M declared file(s) exist (missing: ...)），
//     模型据此自我纠正 —— 这**不是错误**（IsError=false：脚本可能完全成功，只是产物在别处）。
//
// 告知放在**首行而非尾部**：引擎截断只保留头部（tools/engine.go truncate 取前 20000 字节），
// 长输出的尾部追加会被静默吃掉 —— 而那正是最需要「声明的产物没落盘」提示的场景。
func (t *RunPythonTool) summarizeOutputs(out string, outs []runPythonOutput) string {
	if len(outs) == 0 {
		return out // 未声明产物：结果文本原样（含空声明，零额外 IO、零额外文本）
	}
	var missing []string
	found := 0
	for _, o := range outs {
		if !o.exists() {
			missing = append(missing, o.declared)
			continue
		}
		found++
		if t.diff == nil {
			// 只给**主产物**（第一个存在的声明）读文件算事实：一次调用只有一份 diff 会被采集
			// （engine 只读一次 Diff()），其余声明判存在即可 —— 10 条声明最多读 1 个文件。
			t.diff = t.outputDiff(o)
		}
	}
	if len(missing) == 0 {
		return out
	}
	return fmt.Sprintf("outputs: %d/%d declared file(s) exist (missing: %s)\n", found, len(outs), strings.Join(missing, ", ")) + out
}

// exists 运行后该声明是否已落盘为普通文件（目录/空声明不算产物）。
func (o runPythonOutput) exists() bool {
	if o.abs == "" {
		return false
	}
	info, err := os.Stat(o.abs)
	return err == nil && !info.IsDir()
}

// outputDiff 主产物的产物事实（仅对第一个存在的声明调用，见 summarizeOutputs）；
// 不存在/是目录 → nil（不产生产出）。
//   - Path：工作区内 → 工作区相对（与 write/edit 同口径，宿主直接可读）；工作区外 → 绝对；
//   - Created：运行前快照（declaredOutputs）；
//   - Size/Lines/SHA256：读文件算（同 finalizeDiff 的信息源头，只是这里源头是磁盘而不是内存——
//     run_python 不持有脚本写下的内容，只能读回来；上限与 finalizeDiff 一致：超
//     maxArtifactHashBytes 的大文件只取 Stat 的 Size，不读进内存）。
func (t *RunPythonTool) outputDiff(o runPythonOutput) *core.FileDiff {
	if o.abs == "" {
		return nil
	}
	info, err := os.Stat(o.abs)
	if err != nil || info.IsDir() {
		return nil
	}
	d := &core.FileDiff{Path: t.outputPath(o.abs), Created: !o.existed, Size: info.Size()}
	if info.Size() > maxArtifactHashBytes {
		return d // 大文件：只给 Size（读进内存的代价与收益不成比例，同 finalizeDiff 的哈希上限）
	}
	if b, err := os.ReadFile(o.abs); err == nil {
		if looksBinary(b) {
			// 二进制产物（xlsx/docx/pptx/pdf —— 办公主路径的大多数）：没有行语义 → Lines 留空
			// （诚实优于错数，同 write_file 的 append 分支），指纹照算（去重/变更判定仍有效）。
			d.SHA256 = contentFingerprint(string(b))
		} else {
			d = finalizeDiff(d, string(b)) // 文本：size/lines/指纹一次补齐
		}
	} // 读失败（权限/竞态）：Size 仍是事实，其余维度留空
	if d.Created {
		// 新建文件的每一行都是本次新增（同 write_file 新建分支的 Added 口径）；
		// 覆盖既有文件时**不给 ± 数字**：旧内容没读过，增量无从诚实计算（0 = 不声明）。
		d.Added = int(d.Lines)
	}
	return d
}

// outputPath 产物路径口径：工作区内 → 工作区相对；工作区外 → 绝对（宿主可直接打开）。
// 与 agents/artifacts.go 的 displayPath 同一判据（那边对绝对路径再做一次相对化，幂等）。
func (t *RunPythonTool) outputPath(abs string) string {
	root := t.ft.root
	if root == "" || !filepath.IsAbs(abs) {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs // 工作区外：保留绝对路径
	}
	return rel
}

// resolveScript 解析脚本来源：返回（源码, 已就位的绝对路径（script 形态为空）, error）。
// script_path 形态会顺带完成路径解析/存在性/类型校验——错误都是可恢复的（引导 glob/read_file）。
func (t *RunPythonTool) resolveScript(a runPythonArgs) (string, string, error) {
	if strings.TrimSpace(a.ScriptPath) == "" {
		return a.Script, "", nil
	}
	p, err := t.ft.resolve(a.ScriptPath)
	if err != nil {
		return "", "", recoveryError("invalid_arguments", "correct_arguments", err.Error(),
			"Pass a path inside the workspace (or an absolute path outside go-code's own config directory), then retry.")
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", "", recoveryError("script_not_found", "glob", "script_path not found: "+p,
			"Locate the file first (glob/read_file), then pass a path that exists.")
	}
	if info.IsDir() {
		return "", "", recoveryError("invalid_arguments", "correct_arguments", "script_path is a directory: "+p,
			"Pass the path of a .py file instead.")
	}
	src, err := readHeadText(p, maxPythonCheckBytes)
	if err != nil {
		return "", "", recoveryError("script_unreadable", "read_file", "cannot read script_path "+p+": "+singleLine(err.Error()),
			"Check the file permissions, or pass the source inline via script instead.")
	}
	return src, p, nil
}

// runPythonRuntimeError 受管运行时不可用（bootstrap 失败/未装配）：结构化、可恢复的错误。
// 这类失败是**用户环境问题**（网络/磁盘/权限），模型重试同一个脚本没有意义 —— next_action
// 指向「告知用户」，detail 压缩成单行（多行会破坏 TOOL_ERROR 的 `key: value` 解析）。
func runPythonRuntimeError(err error) error {
	return recoveryError("python_runtime_unavailable", "report_to_user",
		"the managed Python runtime is not available: "+singleLine(err.Error()),
		"Tell the user that processing docx/xlsx/pptx/pdf needs the managed Python runtime and that its setup failed (the detail above names the failing step: network/proxy, disk space, or permissions). Do not retry the same script; running it without the runtime is impossible.")
}

// tempDirBases 临时目录基址清单（按沙箱可读性排序，run_python 的脚本与 xlsx 转换的
// 脚本/产物共用同一份选择理由）：
//  1. <运行时根>/tmp —— 运行时目录是沙箱 defaultOpenSubpaths 的 carve-out（读写两开），
//     只要 venv python 能跑，这个目录就一定可读；不污染用户工作区（git status / 文件监视器）。
//  2. 系统 tmp —— 兜底（运行时目录不可写时）。macOS 实测 /tmp、/private/tmp、/private/var
//     均在沙箱读放行内（systemReadPaths），故 TMPDIR 指向这两处都能被沙箱内解释器读到。
func tempDirBases(py runtime.Python) []string {
	var bases []string
	if py.Root != "" {
		bases = append(bases, filepath.Join(py.Root, "tmp"))
	}
	return append(bases, os.TempDir())
}

// writeTempFile 在临时目录里落一个文件，返回（文件路径, 清理函数, error）。
// prefix 区分用途（run-python- / xlsx-preview-），便于残留目录排查。
// 目录 0700 + 文件 0600：内容可能含用户数据，不与同机其他用户共享。
//
// 导出为 WriteTempFile：宿主层的同类固定脚本执行（bridge 的 PDF 抽取器）复用同一份
// 选址与权限策略 —— 临时目录选择理由见 tempDirBases，重写一份必然漂移。
func WriteTempFile(py runtime.Python, prefix, name string, data []byte) (string, func(), error) {
	return writeTempFile(py, prefix, name, data)
}

func writeTempFile(py runtime.Python, prefix, name string, data []byte) (string, func(), error) {
	var lastErr error
	for _, base := range tempDirBases(py) {
		if base == "" {
			continue
		}
		// 基目录必须存在（MkdirTemp 不建父目录）：<运行时根>/tmp 首次调用时通常还没有。
		if err := os.MkdirAll(base, 0o700); err != nil {
			lastErr = err
			continue
		}
		dir, err := os.MkdirTemp(base, prefix)
		if err != nil {
			lastErr = err
			continue
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			lastErr = err
			continue
		}
		return p, func() { _ = os.RemoveAll(dir) }, nil
	}
	return "", nil, fmt.Errorf("创建临时文件失败: %w", lastErr)
}

// writeTempScript 把源码写到临时文件（位置选择见 tempDirBases）。
func writeTempScript(py runtime.Python, src string) (string, func(), error) {
	return writeTempFile(py, "run-python-", "script.py", []byte(src))
}

// shQuote 单引号 shell 转义（命令经 sh -c 执行；引号内除单引号外无特殊字符）。
// 导出为 ShQuote：宿主层的固定脚本执行（bridge 的 PDF 抽取器）拼命令行时复用同一份
// 转义 —— 这层语义必须只有一份实现。
func ShQuote(s string) string { return shQuote(s) }

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// singleLine 把多行文本压成单行（TOOL_ERROR 的 `key: value` 行解析依赖单行；工作区路径
// 换行/错误堆栈换行都会破坏后续字段）。
func singleLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\r", " ")), " ")
}

// readHeadText 读文件前 n 字节（静态检查用；超长脚本只检查开头，见 maxPythonCheckBytes）。
func readHeadText(p string, n int) (string, error) {
	b, err := readHead(p, n)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---------- 高危脚本静态拦截（窄口径 tripwire） ----------
//
// 判断与理由（2026-09 实现决策，务必与工具描述保持一致）：
//
//   - **不照搬 bash 的 shell 正则**：语义不同 —— bash 检查的是「即将被 shell 执行的一行文本」，
//     而这里的输入是 Python 源码：`shutil.rmtree("/")` 出现在字符串/注释/模板里与真实调用
//     完全不同义，照搬必然既漏（os.system/变量拼接）又误伤（文档字符串、生成代码的脚本）。
//   - **做，但只做窄口径 tripwire**：递归删除「根/家/工作区根**本身**」的字面量目标，是 LLM
//     写清理代码时最常见、后果最不可逆的一类事故；字面量目标误伤率极低（明确到具体子目录
//     一律放行，与 bash 的 isDangerousPath 同口径），且能给出可操作提示让模型自我修正。
//   - **不做语义分析**：变量拼接、f-string 动态目标、getattr 一律漏过 —— 这是**有意的**。
//     本检查**不是安全边界**，真正的边界是（a）沙箱文件策略（工作区外写被内核拒）与
//     （b）工具层 HITL 审批；tripwire 只降低「手滑」概率，不承诺隔离。
//   - 代价：一个「先删后重建」的合法脚本若真以 / 或家目录为目标会被误拦。实测没有这类合法
//     场景（要清理的是具体子目录），判断为可接受的代价。

// pythonSkeleton 把 Python 源码转成「结构串 + 字面量表」：注释删除、字符串字面量替换为
// \x00<下标>\x00 占位符。于是正则只在**代码位**上匹配 —— `print("shutil.rmtree('/')")`
// 这类「文本里出现危险调用」不误报，而真实调用（参数是字面量）仍可判定。
// 词法启发、非 Python 解析器；未闭合的引号按「其余全是字面量」处理（宁漏勿误）。
func pythonSkeleton(src string) (string, []string) {
	const mark = '\x00'
	var b strings.Builder
	var lits []string
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		switch {
		case c == '#': // 注释：到行尾
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '\'' || c == '"' || isPythonStringPrefix(c):
			end := pythonStringEnd(src, i)
			if end < 0 {
				b.WriteByte(c)
				i++
				continue
			}
			lits = append(lits, src[i:end])
			fmt.Fprintf(&b, "%c%d%c", mark, len(lits)-1, mark)
			i = end
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), lits
}

// isPythonStringPrefix 字符串前缀字母（r/b/f/u，可组合如 rb/f）。
func isPythonStringPrefix(c byte) bool {
	return strings.IndexByte("rbufRBUF", c) >= 0
}

// pythonStringEnd 若 s[i] 处是字符串字面量起点（可带前缀），返回字面量结束后的下标；
// 不是字面量返回 -1（如 `r = 1` 里的标识符 r）；未闭合返回 len(s)（其余按字面量吞掉）。
func pythonStringEnd(s string, i int) int {
	n := len(s)
	j := i
	raw := false
	for j < n && j-i < 3 {
		switch s[j] {
		case 'r', 'R':
			raw = true
			j++
		case 'b', 'B', 'f', 'F', 'u', 'U':
			j++
		default:
			goto quotes
		}
	}
quotes:
	if j >= n || (s[j] != '\'' && s[j] != '"') {
		return -1
	}
	q := s[j]
	triple := j+2 < n && s[j+1] == q && s[j+2] == q
	k := j + 1
	if triple {
		k = j + 3
	}
	for k < n {
		c := s[k]
		if !raw && c == '\\' {
			k += 2 // 转义对跳过
			continue
		}
		if triple {
			if c == q && k+2 < n && s[k+1] == q && s[k+2] == q {
				return k + 3
			}
			k++
			continue
		}
		if c == q {
			return k + 1
		}
		k++
	}
	return n // 未闭合：其余全部按字面量处理
}

// pyTokenRe 结构串里的字面量占位符 \x00<下标>\x00。
var pyTokenRe = regexp.MustCompile("\x00(\\d+)\x00")

// pyTokenValues 取结构串片段里引用的全部字面量**值**（去前缀与引号；未解转义 —— 检查
// 只看形状是否等于 / 或 ~，不做语义解码）。
func pyTokenValues(frag string, lits []string) []string {
	var out []string
	for _, m := range pyTokenRe.FindAllStringSubmatch(frag, -1) {
		if idx, err := strconv.Atoi(m[1]); err == nil && idx >= 0 && idx < len(lits) {
			out = append(out, pythonLiteralValue(lits[idx]))
		}
	}
	return out
}

// pythonLiteralValue 去字符串前缀与引号，返回字面量内容（不做转义解码）。
func pythonLiteralValue(lit string) string {
	s := lit
	for len(s) > 0 && isPythonStringPrefix(s[0]) {
		s = s[1:]
	}
	for _, q := range []string{`"""`, `'''`} {
		if strings.HasPrefix(s, q) && strings.HasSuffix(s, q) && len(s) >= 3 {
			return s[3 : len(s)-3]
		}
	}
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') {
		return s[1 : len(s)-1]
	}
	return s
}

// pyCallArgs 从结构串的 '(' 下标取该调用括号内的文本（结构串已无字符串/注释，直接数括号；
// 未闭合时取到末尾）。
func pyCallArgs(code string, open int) string {
	if open < 0 || open >= len(code) || code[open] != '(' {
		return ""
	}
	depth := 0
	for i := open; i < len(code); i++ {
		switch code[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return code[open+1 : i]
			}
		}
	}
	return code[open+1:]
}

// pyFirstArg 取逗号分隔的首个顶层参数（关键字参数形态 `path=x` 保留原文，由 pySingleLiteral 剥）。
func pyFirstArg(args string) string {
	depth := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				return args[:i]
			}
		}
	}
	return args
}

// pySingleLiteral 片段是否「恰好是一个字面量」（可带 `name=` 关键字前缀）→ 返回其值。
func pySingleLiteral(frag string, lits []string) (string, bool) {
	f := strings.TrimSpace(frag)
	if i := strings.IndexByte(f, '='); i > 0 && isPythonIdent(f[:i]) {
		f = strings.TrimSpace(f[i+1:]) // 关键字参数：path="/"
	}
	if m := pyTokenRe.FindStringSubmatch(f); m != nil && m[0] == f {
		if idx, err := strconv.Atoi(m[1]); err == nil && idx >= 0 && idx < len(lits) {
			return pythonLiteralValue(lits[idx]), true
		}
	}
	return "", false
}

// isPythonIdent 是否是合法 Python 标识符（用于剥关键字参数前缀）。
func isPythonIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

var (
	// pyDeleteCallRe 递归删除调用（shutil.rmtree / os.removedirs）。os.remove/rmdir 只管单个
	// 路径，爆炸半径小，不纳入（否则误伤面变大而收益很低）。
	pyDeleteCallRe = regexp.MustCompile(`\b(?:shutil\s*\.\s*rmtree|os\s*\.\s*removedirs)\s*\(`)
	// pyShellCallRe 转 shell 的调用（os.system/os.popen/subprocess.*）——其字面量参数拼起来
	// 交给 bash 的 rm 分析复用。
	pyShellCallRe = regexp.MustCompile(`\b(?:os\s*\.\s*system|os\s*\.\s*popen|subprocess\s*\.\s*[A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	// pyHomeFuncRe 家目录相关函数（配合 HOME/~ 字面量判定「目标就是家目录本身」）。
	pyHomeFuncRe = regexp.MustCompile(`\b(?:expanduser|environ|getenv)\b`)
)

// pyRootTarget 目标是否「就是根/家/工作区根本身」（明确到子目录 → false）。
// 判定分两种形态：
//   - 纯字面量 → 复用 bash 的 isDangerousPath 口径（/ ~ . .. ../* ~/* $HOME/* 等）；
//   - 无参调用本身（Path.home()/Path.cwd()/os.getcwd()）或 expanduser("~")/environ["HOME"]
//     这类家目录表达式（**整段就是该表达式**，后面没有再接路径分量）。
func pyRootTarget(arg string, lits []string) bool {
	code := strings.Join(strings.Fields(arg), "") // 去空白：Path . home ( ) → Path.home()
	switch code {
	case "Path.home()", "pathlib.Path.home()", "Path.cwd()", "pathlib.Path.cwd()", "os.getcwd()", "os.getcwdb()":
		return true
	}
	if pyHomeFuncRe.MatchString(code) {
		for _, v := range pyTokenValues(code, lits) {
			switch v {
			case "~", "~/", "HOME", "$HOME", "$HOME/":
				return true
			}
		}
	}
	return false
}

// checkPythonHighRisk 高危脚本静态检查：命中返回原因（非空即拦截），未命中返回 ""。
// 两类命中：
//  1. 递归删除「根/家/工作区根本身」的字面量或家目录表达式目标；
//  2. os.system/subprocess 里字面量拼出的 `rm [-r -f] 危险目标`（复用 bash 的 dangerousRmTarget，
//     与 `bash` 工具同口径：明确子目录如 rm -rf dist/ 放行）。
func checkPythonHighRisk(script string) string {
	if strings.TrimSpace(script) == "" {
		return ""
	}
	code, lits := pythonSkeleton(script)
	for _, m := range pyDeleteCallRe.FindAllStringIndex(code, -1) {
		target := pyFirstArg(pyCallArgs(code, m[1]-1))
		if v, ok := pySingleLiteral(target, lits); ok {
			if isDangerousPath(v) {
				return "recursive delete of " + strconv.Quote(v) + " (root/home/workspace itself or a whole-glob target)"
			}
			continue
		}
		if pyRootTarget(target, lits) {
			return "recursive delete of the home/working directory itself (" + strings.TrimSpace(target) + ")"
		}
	}
	for _, m := range pyShellCallRe.FindAllStringIndex(code, -1) {
		joined := strings.Join(pyTokenValues(pyCallArgs(code, m[1]-1), lits), " ")
		if t := dangerousRmTarget(joined); t != "" {
			return "shell delete of " + strconv.Quote(t) + " via os.system/subprocess (rm -rf would wipe it)"
		}
	}
	return ""
}
