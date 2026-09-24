// Package runtime 受管运行时 bootstrap（首例：Python）。
//
// 办公文档能力（docx/xlsx/pptx/pdf）需要 Python 侧依赖，但用户机器上的 Python 环境
// 不可依赖：系统自带 / Homebrew / Anaconda 各不相同，预装了哪些库完全未知。因此自管
// 一份隔离运行时 —— ~/.go-code/runtime/python/{venv,requirements.lock} —— 由本包
// bootstrap（建 venv + 按锁文件装依赖），后续 run_python 工具直接用它。
//
// 两个实测前提（2026-09，改动本包前先读）：
//
//  1. 运行时目录是 ~/.go-code 的子树，而 ~/.go-code 被沙箱与文件工具「双面保护」。
//     只建目录不放行 → venv python 在沙箱内直接崩：
//     `Fatal Python error: init_import_site: Failed to import the site module`。
//     放行有两处（缺一不可）：sandbox.defaultOpenSubpaths 与
//     tools/builtin.denyHarnessConfigPath（desktop/bridge/main_helpers.go 同语义副本）。
//
//  2. venv 的基座解释器在 macOS 上是符号链接指向
//     /Library/Developer/CommandLineTools/.../Python3.framework（不在 /usr 下）。
//     沙箱已放行 /Library（systemReadPaths），故沙箱内执行 venv python 可用。
package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxOutputLog 子命令输出截断上限。pip 下载/解析的日志动辄数百行，全量灌进错误文本
// 或日志会淹没有效信息（与 plugin/browser/install.go 同口径）。
const maxOutputLog = 1024

// DefaultTimeout 单条子命令超时。pip install 要下载约 42MB（4 个库 + lxml/pillow
// 等传递依赖），实测首次安装 30s 级；但用户网络慢时可能到数分钟 —— 给足 10 分钟，
// 调用方可用 Config.Timeout 覆盖。
const DefaultTimeout = 10 * time.Minute

// probeTimeout 探测类子命令超时（解释器自检 / import 单个模块）。venv python 冷启动
// 正常 <100ms，慢到 30s 说明环境已坏，报错比挂住好。
const probeTimeout = 30 * time.Second

// pyWaitDelay 子命令**成功退出后**等待 I/O 管道关闭的上限（C10，2026-09-18）。
//
// **为什么必需**：`cmd.Stdout = &buf`（io.Writer）⇒ Go 建 `os.Pipe` + 拷贝协程 ⇒
// `Wait()` **依赖管道关闭**。而 `exec.CommandContext` 在 ctx 到期时**只杀直接子进程**，
// 脚本 fork 出的孙进程会存活并握住写端 ⇒ `Wait()` 永不返回 —— **超时在这里是失效的**
// （超时管不住一个「自己已退出、但管道被孙进程持有」的等待）。
//
// 这正是 2026-09-18 **C8** 在 `sandbox.runExec` 上修掉的同一缺陷（生产实测挂起 602s）；
// 本处是同型站点，故采用同一处方（C10 清单第 1 项，形态与 C8 逐字相同）。
//
// 语义：进程退出后最多再等 `pyWaitDelay`，到点由 `os/exec` 强制关管道并使 `Wait` 返回
// （返回 `exec.ErrWaitDelay`）。**代价**：该情形下输出可能被进程组外的后代截断 ——
// 由调用点如实注明，而**不是**把成功印成失败。
const pyWaitDelay = 3 * time.Second

// pyWaitDelayNote ErrWaitDelay 情形下追加在**成功**输出尾部的说明（模型可见）。
const pyWaitDelayNote = "[exec: WaitDelay 到期 —— 输出可能被进程组外的后代截断]"

// lockedBuffer 并发安全的输出缓冲（C10）。
//
// **为什么不是裸 `bytes.Buffer`**：`WaitDelay` 到期时 `Wait` 会在**拷贝协程仍可能写入**的
// 情况下返回，此时主流程读缓冲是**真实数据竞争** —— C8 在 `sandbox` 侧用 `-race` 实测到
// `WARNING: DATA RACE`（读：极端分支取部分输出；写：`io.copyBuffer` 的拷贝协程）。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// minPythonMajor/minPythonMinor 基座解释器最低版本。
// 3.9 是硬底：python-docx 1.2 与 pypdf 6.x 的 requires-python 都是 >=3.9；
// 更低版本装不上（pip 直接报 no matching distribution）。macOS 自带 3.9.6 正好达标。
const (
	minPythonMajor = 3
	minPythonMinor = 9
)

// requirementsLock 依赖清单（Python 侧唯一事实源，Ensure 时落盘到运行时目录）。
//
// 只钉办公文档四件套 —— 刻意不含 pandas（体积 +105MB，绝大多数文档任务用不上，
// 需要时再按需加装）。版本钉死而非浮动：浮动版本会在不同用户机器上解析出不同组合，
// 出问题无法复现。
//
// 末端传递依赖（lxml/pillow/xlsxwriter/typing_extensions/et_xmlfile）不钉版本：
// 它们由上面四者的 requires 决定，钉了反而在同版本 Python 上解不出解。
const requirementsLock = `# go-code 受管 Python 运行时依赖（版本钉死；本文件由 Go 侧生成，勿手改）
# 占用约 53MB。不含 pandas（+105MB，按需再加）。
python-docx==1.2.0
openpyxl==3.1.5
python-pptx==1.0.2
pypdf==6.19.0
# pypdfium2：扫描件/纯图 PDF 的**渲染**能力（pypdf 只能抽文本层，无文本层时它无能为力）。
# 版本钉 5.9.0 而非最新：5.10.0 起 wheel 的平台标签升到 macosx_13_0，会把 macOS 11/12
# 用户挡在门外；5.9.0 是仍带 macosx_11_0 wheel 的最高版。
pypdfium2==5.9.0
# mammoth：docx → 语义 HTML（标题/列表/表格/图片 data URL），纯 Python 无平台 wheel，
# 仅 50KB。为什么不用 python-docx 自绘：docx 的样式语义（Heading/List/表格网格）在
# python-docx 里是裸 XML，自绘等于重实现一个转换器；mammoth 专做这件事。
mammoth==1.12.2
`

// requiredModules 依赖清单对应的 import 名（包名 ≠ import 名：python-docx → docx，
// python-pptx → pptx）。装完用它整体自检 —— pip 退出码 0 不代表模块真能 import。
var requiredModules = []string{"docx", "openpyxl", "pptx", "pypdf", "pypdfium2", "mammoth"}

// readyMarker 就绪标记文件名（运行时根目录下）。用标记而非「venv 目录存在」判就绪：
// venv 建到一半失败（pip 断网/磁盘满）会留下一个看似完整、实则缺库的目录树，只信目录
// 存在会导致后续每次调用都在坏环境里跑。标记只在全部步骤成功后原子落盘。
const readyMarker = ".ready.json"

// Config 运行时 bootstrap 配置（零值即生产默认）。
type Config struct {
	// Root 运行时根目录，默认 ~/.go-code/runtime/python（含 venv/ 与 requirements.lock）。
	Root string
	// Timeout 单条子命令超时，默认 DefaultTimeout（10min）。
	Timeout time.Duration
	// Python 基座解释器绝对路径；空 = PATH → 已知安装位置兜底。
	Python string
	// RunCmd 执行固定命令行并返回合并输出（测试注入；nil = 进程执行 + 超时）。
	// 注入它可让 bootstrap 全流程离线可测（不真装依赖）。
	RunCmd func(ctx context.Context, name string, args ...string) (string, error)
}

// Runtime 受管 Python 运行时（可并发使用）。
type Runtime struct {
	cfg Config
}

// Python 已就绪的运行时句柄。值类型（只含路径，可随意传递/缓存）。
type Python struct {
	Root       string // 运行时根（~/.go-code/runtime/python）
	VenvDir    string // venv 目录（Root/venv）
	VenvPython string // venv 解释器绝对路径（run_python 工具该用的就是它）
	Base       string // 基座解释器（建 venv 时所用；排查版本问题看它）
	Created    bool   // 本次 Ensure 是否真的执行了创建（false = 命中已有环境）
}

// Status 运行时状态探查结果。
type Status struct {
	Ready      bool   // venv 可执行 + 就绪标记与当前依赖清单一致
	VenvPython string // 探测到的 venv 解释器（未就绪时可能为空）
	Reason     string // 未就绪原因（供日志/诊断，就绪时为空）
	// Stale 运行时**已存在**，但就绪标记记的依赖清单与当前源码不一致（需重装才能用）。
	//
	// 为什么单列一个字段而不是让调用方去匹配 Reason 文案：这是真链路测试的**分支依据** ——
	// 依赖运行时的慢测试在未就绪时会 t.Skip，而「指纹不匹配」与「本机从没 bootstrap 过」
	// 是两件完全不同的事：前者是**可修复的破损状态**（旧二进制写过共享运行时 / 源码刚改了
	// 依赖清单），此时整套真链路测试会**全部静默跳过、套件仍显示全绿**，而实际一条都没跑
	// （2026-09 实测踩过两次：16:32 与 18:29，都是旧构建的进程调 Ensure 把共享运行时写回旧指纹）。
	// 有了这个字段，测试就能把「静默跳过」升级为「响亮失败」，不再假绿。
	Stale bool
}

// New 构造运行时（仅解析配置，不做任何 I/O —— 建 venv 是 Ensure 的职责）。
func New(cfg Config) *Runtime { return &Runtime{cfg: cfg} }

// DefaultRoot 默认运行时根目录 ~/.go-code/runtime/python。
//
// 与 desktop/bridge 的 appDataDir() 同一约定（APP 级数据都在 ~/.go-code 下），但
// 本包是 SDK 层，不能 import desktop/bridge —— 自行解析 home。同时必须与
// sandbox.defaultOpenSubpaths 的 ".go-code/runtime" 对齐，改名会再次触发上文
// 「site module 崩」问题。
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".go-code", "runtime", "python")
	}
	return filepath.Join(home, ".go-code", "runtime", "python")
}

// root 运行时根目录（配置优先）。
func (r *Runtime) root() string {
	if r.cfg.Root != "" {
		return r.cfg.Root
	}
	return DefaultRoot()
}

// timeout 单条子命令超时（<=0 用默认）。
func (r *Runtime) timeout() time.Duration {
	if r.cfg.Timeout > 0 {
		return r.cfg.Timeout
	}
	return DefaultTimeout
}

// Ensure 幂等/可重入地保证受管 Python 运行时可用，返回就绪句柄。
//
// 状态机（任一环失败都返回错误而非 panic —— 调用方据此降级，比如 run_python
// 工具回报「环境不可用」而不是让整个会话崩）：
//
//	① 命中就绪标记（指纹与当前依赖清单一致 + venv 解释器能跑通）
//	   → 直接返回（Created=false），不碰磁盘、不装依赖 —— 幂等快路径
//	② 找基座解释器（Config.Python → PATH → 已知安装位置），版本须 >= 3.9
//	③ 落盘 requirements.lock（内容变化会使 ① 的指纹失配 → 走安装升级）
//	④ venv 缺失才 `python3 -m venv venv`（已存在则复用，不重建 —— 重建会丢掉
//	   已装依赖，白等一次 42MB 下载）
//	⑤ `venv/bin/python -m pip install -r requirements.lock`（pip 自身幂等：
//	   已满足的依赖秒过）
//	⑥ 自检：整体 import 全部必需模块（pip 退出码 0 ≠ 模块可用）
//	⑦ 原子写就绪标记
//
// 第 ④⑤ 步失败会留下「有 venv 但无标记」的状态：下次 Ensure 重跑 ⑤，自愈。
//
// 并发：进程内按 Root 串行（mutex），跨进程用运行时目录下的 flock 兜底（两个
// go-code 实例同时首启）。等锁期间响应 ctx 取消，不会永久挂住。
func (r *Runtime) Ensure(ctx context.Context) (Python, error) {
	root := r.root()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Python{}, fmt.Errorf("创建运行时目录 %s: %w", root, err)
	}

	// 进程内串行（同 Root 的多 goroutine/多会话）
	mu := inProcLock(root)
	mu.Lock()
	defer mu.Unlock()

	// 跨进程串行（多实例共用一个 home）
	unlock, err := r.lockFile(ctx, root)
	if err != nil {
		return Python{}, err
	}
	defer unlock()

	// ① 就绪快路径
	if st, err := r.Status(ctx); err == nil && st.Ready {
		return Python{
			Root:       root,
			VenvDir:    filepath.Join(root, "venv"),
			VenvPython: st.VenvPython,
			Base:       baseFromMarker(root),
		}, nil
	}

	// ② 基座解释器
	base, err := r.findBase(ctx)
	if err != nil {
		return Python{}, err
	}

	// ③ 依赖清单落盘（Ensure 是唯一写入方；指纹变化驱动 ① 失配 → 重新安装）
	lockPath := filepath.Join(root, "requirements.lock")
	if err := writeFileIfChanged(lockPath, requirementsLock); err != nil {
		return Python{}, fmt.Errorf("写依赖清单 %s: %w", lockPath, err)
	}

	// ④ venv（缺失才建）
	venvDir := filepath.Join(root, "venv")
	venvPython, _ := venvPythonPath(venvDir)
	if venvPython == "" {
		if _, err := r.run(ctx, base, "-m", "venv", venvDir); err != nil {
			return Python{}, fmt.Errorf("创建 venv 失败（基座 %s）：%w\n提示：确认磁盘空间，或删除 %s 后重试", base, err, venvDir)
		}
		venvPython, err = venvPythonPath(venvDir)
		if err != nil {
			return Python{}, fmt.Errorf("venv 创建后未找到解释器（%s）：%w", venvDir, err)
		}
	}

	// ⑤ 安装依赖（pip 幂等；--no-input 防无 TTY 时挂起在交互提示）
	if _, err := r.run(ctx, venvPython, "-m", "pip", "install",
		"--no-input", "--disable-pip-version-check", "-r", lockPath); err != nil {
		return Python{}, fmt.Errorf("安装 Python 依赖失败（网络不通/代理/磁盘空间？）：%w\n"+
			"提示：依赖清单在 %s，可手动执行 %s -m pip install -r %s 排查",
			err, lockPath, venvPython, lockPath)
	}

	// ⑥ 自检：真跑一遍 import（pip 成功 ≠ 库可用；也顺带产出 __pycache__，
	// 验证运行时目录可写 —— 这正是沙箱必须放行读写而非只读的原因）
	if _, err := r.run(ctx, venvPython, "-c", importCheckScript(requiredModules)); err != nil {
		return Python{}, fmt.Errorf("依赖自检失败（venv 已建但模块不可用）：%w", err)
	}

	// ⑦ 原子写就绪标记
	m := readyInfo{
		Version:      1,
		Requirements: fingerprint(requirementsLock),
		Base:         base,
		VenvPython:   venvPython,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeMarker(filepath.Join(root, readyMarker), m); err != nil {
		return Python{}, fmt.Errorf("写就绪标记失败: %w", err)
	}

	return Python{Root: root, VenvDir: venvDir, VenvPython: venvPython, Base: base, Created: true}, nil
}

// Status 探查运行时是否就绪（不产生副作用、不装依赖）。
//
// 就绪 = 三件事同时成立：
//  1. 就绪标记存在，且依赖清单指纹 == 当前 requirementsLock（清单升级后自动失配）
//  2. 标记记录的 venv 解释器存在且可执行（用户手工删过文件则不算就绪）
//  3. 该解释器真能跑通一句自检（防空壳/坏链接 —— 「目录存在」不算数）
//
// 代价是一次进程 spawn（~50ms），换来的是「绝不进坏环境」。
func (r *Runtime) Status(ctx context.Context) (Status, error) {
	root := r.root()
	raw, err := os.ReadFile(filepath.Join(root, readyMarker))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{Reason: "尚未 bootstrap（无就绪标记）"}, nil
		}
		return Status{}, fmt.Errorf("读就绪标记: %w", err)
	}
	var m readyInfo
	if err := json.Unmarshal(raw, &m); err != nil {
		return Status{Reason: "就绪标记损坏，需重建"}, nil
	}
	if m.Requirements != fingerprint(requirementsLock) {
		return Status{Reason: "依赖清单已变更，需重新安装", Stale: true}, nil
	}
	if m.VenvPython == "" || !isExecutable(m.VenvPython) {
		return Status{VenvPython: m.VenvPython, Reason: "venv 解释器缺失或不可执行"}, nil
	}
	if _, err := r.run(ctx, m.VenvPython, "-c", "import sys"); err != nil {
		return Status{VenvPython: m.VenvPython, Reason: "venv 解释器无法运行"}, nil
	}
	return Status{Ready: true, VenvPython: m.VenvPython}, nil
}

// HasModule 探测某模块在该运行时中能否 import（后续技能据此判断 pandas 之类
// 按需依赖装没装）。返回 (false, nil) = 明确「没装」；(_, err) = 探测本身失败
// （环境不可用），调用方应区别对待：前者可提示装、后者要修环境。
//
// 判定依据是解释器的报错文本而非退出码：both「模块不存在」（ModuleNotFoundError）
// 与「环境坏了」（Fatal Python error: init_import_site: Failed to import the site
// module —— 沙箱没放行运行时目录时的典型症状）都返回退出码 1，只看码会把
// 环境损坏误报成「库没装」，把用户引向错误的自救方向。
func (p Python) HasModule(ctx context.Context, module string) (bool, error) {
	if p.VenvPython == "" {
		return false, errors.New("受管 Python 不可用：venv 解释器路径为空（先调用 Ensure）")
	}
	if !isExecutable(p.VenvPython) {
		return false, fmt.Errorf("受管 Python 不可用：%s 不存在或不可执行", p.VenvPython)
	}
	// 模块名直接进 -c 脚本：调用方只该传合法标识符（技能侧是硬编码常量，非用户输入）
	out, err := defaultRunCmd(ctx, probeTimeout, p.VenvPython, "-c", "import "+module)
	if err == nil {
		return true, nil
	}
	if strings.Contains(out, "ModuleNotFoundError") || strings.Contains(out, "No module named") {
		return false, nil
	}
	return false, fmt.Errorf("探测模块 %s 失败：%w", module, err)
}

// findBase 定位基座解释器：Config.Python → PATH → 已知安装位置兜底 → 逐版本校验。
//
// 为什么要有「已知安装位置兜底」：GUI 启动的桌面 App（Electron spawn 的 bridge）
// 继承的是 launchd 最小 PATH（/usr/bin:/bin:/usr/sbin:/sbin），不含用户 shell 配置。
// macOS 自带解释器恰好就在 /usr/bin/python3，所以最常见情况 PATH 能命中；但用户
// 若只装了 Homebrew python 而 PATH 残缺，LookPath 找不到 ≠ 没装 —— 与
// plugin/browser 的 nodeCandidates 是同一类「明明装了却报没装」问题。
//
// 候选逐个试跑而不是取第一个存在的：PATH 上可能有 pyenv/conda shim 指向一个
// 版本过低（<3.9）或已损坏的解释器，试跑过后才能确认。
func (r *Runtime) findBase(ctx context.Context) (string, error) {
	var tried []string
	var lastErr error
	for _, c := range candidatePythons(r.cfg.Python) {
		if c == "" || !isExecutable(c) {
			continue
		}
		tried = append(tried, c)
		ver, err := r.pythonVersion(ctx, c)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", c, err)
			continue
		}
		if ver[0] < minPythonMajor || (ver[0] == minPythonMajor && ver[1] < minPythonMinor) {
			lastErr = fmt.Errorf("%s: 版本 %d.%d 过低（需 >= %d.%d）", c, ver[0], ver[1], minPythonMajor, minPythonMinor)
			continue
		}
		return c, nil
	}
	detail := ""
	if len(tried) > 0 {
		detail = fmt.Sprintf("；已尝试 %s", strings.Join(tried, ", "))
	}
	if lastErr != nil {
		detail += fmt.Sprintf("；最后一条错误：%v", lastErr)
	}
	return "", fmt.Errorf("未找到可用的 python3（需 >= %d.%d）%s\n"+
		"提示：macOS 可执行 `xcode-select --install` 装命令行工具（自带 3.9），或用 Homebrew 装 python",
		minPythonMajor, minPythonMinor, detail)
}

// pythonVersion 问解释器要 (major, minor)。
func (r *Runtime) pythonVersion(ctx context.Context, python string) ([2]int, error) {
	out, err := r.run(ctx, python, "-c", "import sys; print('%d.%d' % sys.version_info[:2])")
	if err != nil {
		return [2]int{}, err
	}
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d.%d", &major, &minor); err != nil {
		return [2]int{}, fmt.Errorf("解析版本号失败（输出 %q）", truncate(strings.TrimSpace(out), 64))
	}
	return [2]int{major, minor}, nil
}

// run 执行固定命令行并返回合并输出。
//
// 语义对齐 sandbox.runExec：非零退出/超时都不 panic，原因与输出一并进 error 文本
// （截断后），调用方据此给可操作提示 —— 基础设施层不该把「网络不通」升级成进程崩溃。
//
// 与 runExec 的差异及原因：不做 Setpgid/杀进程组（venv/pip 不派生长期孤儿进程，
// 且 syscall 字段会破坏非 darwin 构建）。超时走 exec.CommandContext（杀直接子进程）。
func (r *Runtime) run(ctx context.Context, name string, args ...string) (string, error) {
	timeout := r.timeout()
	// 探测类短命令用 probeTimeout，避免 dev 误配的超长超时拖住状态检查
	if isProbe(name, args) {
		timeout = probeTimeout
	}
	if r.cfg.RunCmd != nil {
		// 注入路径：时限交给注入实现（测试可控），但保留 ctx 语义
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		return r.cfg.RunCmd(ctx, name, args...)
	}
	return defaultRunCmd(ctx, timeout, name, args...)
}

// isProbe 判定是否为探测类命令（解释器自检 / import 检测），用短超时。
// venv 创建与 pip install 是真正的重活，保留完整超时。
func isProbe(name string, args []string) bool {
	if len(args) == 0 {
		return false
	}
	if name == "" || !strings.Contains(filepath.Base(name), "python") {
		return false
	}
	// `-c ...` 自检；`-m venv` / `-m pip` 属重活
	return args[0] == "-c"
}

// defaultRunCmd 进程执行 + 超时 + 输出截断（runExec 语义的轻量版）。
func defaultRunCmd(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var buf lockedBuffer // 并发安全：WaitDelay 到期时拷贝协程仍可能写入（见 lockedBuffer 注释）
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// C10：进程退出后最多再等 pyWaitDelay 即强制关管道并返回 —— 根治「孙进程持管道
	// ⇒ Wait 永不返回」（与 C8 在 sandbox.runExec 的处方一致）。
	cmd.WaitDelay = pyWaitDelay
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		// C10：ErrWaitDelay = 进程**已成功退出**，只是管道被进程组外的后代占着。
		// 这不判失败（Go 仅在进程成功退出时返回该错误）；旧行为在此**会一直挂住**
		// —— 超时管不住「进程已退出、管道被孙进程持有」的等待。
		// 如实返回输出并注明可能被截断：有界返回 + 明说截断 > 无限期等待。
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return out + "\n" + pyWaitDelayNote, nil
		}
		// 非零退出/超时：输出与原因并入错误文本（调用方可见后自行判断）
		if ctx.Err() != nil {
			return out, fmt.Errorf("%s: 超时(%s): %w\n%s", cmdline(name, args), timeout, ctx.Err(), truncate(out, maxOutputLog))
		}
		return out, fmt.Errorf("%s: %w\n%s", cmdline(name, args), err, truncate(out, maxOutputLog))
	}
	return out, nil
}

// candidatePythons 基座解释器候选（按确定性/常见度排序）。
// GO_CODE_PYTHON_CANDIDATES（':' 分隔）整体覆盖 —— 测试隔离用（屏蔽本机真实安装）。
func candidatePythons(explicit string) []string {
	if ov := os.Getenv("GO_CODE_PYTHON_CANDIDATES"); ov != "" {
		return strings.Split(ov, string(os.PathListSeparator))
	}
	var cs []string
	if explicit != "" {
		cs = append(cs, explicit)
	}
	// PATH 优先（尊重用户的 python 选择：pyenv/conda 切换在这里生效）
	if p, err := exec.LookPath("python3"); err == nil {
		cs = append(cs, p)
	}
	cs = append(cs,
		"/usr/bin/python3",          // macOS 自带（Command Line Tools）
		"/usr/local/bin/python3",    // 官网 pkg / Intel Homebrew
		"/opt/homebrew/bin/python3", // Apple Silicon Homebrew
		"/opt/local/bin/python3",    // MacPorts
	)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cs = append(cs,
			filepath.Join(home, ".local", "bin", "python3"), // pipx/uv 类布局
			filepath.Join(home, "miniconda3", "bin", "python3"),
			filepath.Join(home, "anaconda3", "bin", "python3"),
			filepath.Join(home, "miniforge3", "bin", "python3"),
			filepath.Join(home, ".pyenv", "shims", "python3"),
		)
	}
	return cs
}

// readyInfo 就绪标记内容（JSON，落盘于运行时根目录）。
type readyInfo struct {
	Version      int    `json:"version"`      // 标记格式版本（未来结构变更时判定）
	Requirements string `json:"requirements"` // 依赖清单指纹（sha256 前 16 字节 hex）
	Base         string `json:"base"`         // 基座解释器（排查用）
	VenvPython   string `json:"venv_python"`  // venv 解释器
	CreatedAt    string `json:"created_at"`
}

// writeMarker 原子写就绪标记（temp + rename）：并发读者永远看到完整 JSON，
// 也不会出现「写到一半被读到 → 标记损坏 → 判定未就绪 → 重复安装」。
func writeMarker(path string, m readyInfo) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// baseFromMarker 从就绪标记读回基座解释器（仅为句柄信息完整；读不到留空）。
func baseFromMarker(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, readyMarker))
	if err != nil {
		return ""
	}
	var m readyInfo
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m.Base
}

// venvPythonPath 定位 venv 解释器：优先 bin/python，回退 bin/python3。
// 两个都试是因为不同平台/版本的 venv 布局略有差异（Windows 走 Scripts/）。
func venvPythonPath(venvDir string) (string, error) {
	for _, rel := range []string{
		filepath.Join(venvDir, "bin", "python"),
		filepath.Join(venvDir, "bin", "python3"),
	} {
		if isExecutable(rel) {
			return rel, nil
		}
	}
	return "", fmt.Errorf("%s 下没有可执行的 python", filepath.Join(venvDir, "bin"))
}

// importCheckScript 生成整体 import 自检脚本（一条进程搞定所有模块，省冷启动开销）。
func importCheckScript(modules []string) string {
	return "import " + strings.Join(modules, ", ")
}

// fingerprint 依赖清单指纹（内容变化 → 就绪标记失配 → 触发重新安装）。
func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// isExecutable 判定 path 为可执行文件（存在 + 非目录 + 有执行位）。
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// truncate 截断长输出（pip 日志摘要）。按 UTF-8 边界回退，不劈裂多字节字符。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// isRuneStart 判定 b 是否为 UTF-8 起始字节。
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// cmdline 命令行文本（错误信息里给可复现的命令，用户能直接粘贴排查）。
func cmdline(name string, args []string) string {
	parts := append([]string{name}, args...)
	return strings.Join(parts, " ")
}

// writeFileIfChanged 内容有变化才写（Ensure 幂等时不产生无谓 diff）。
func writeFileIfChanged(path, content string) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return nil
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// ---------- 并发串行 ----------

// inProcLocks 进程内按 Root 串行（Ensure 会建 venv，不能并发跑两份）。
var (
	inProcMu    sync.Mutex
	inProcLocks = map[string]*sync.Mutex{}
)

// inProcLock 取某 Root 的进程内互斥锁。
func inProcLock(root string) *sync.Mutex {
	inProcMu.Lock()
	defer inProcMu.Unlock()
	mu := inProcLocks[root]
	if mu == nil {
		mu = &sync.Mutex{}
		inProcLocks[root] = mu
	}
	return mu
}

// lockFile 跨进程串行（两个 go-code 实例共用同一 home）：运行时目录下 flock。
// 用非阻塞重试而非阻塞 flock —— 阻塞式锁无法响应 ctx 取消，pip 装 10 分钟时
// 另一调用方应能按时超时退出，而不是永久挂住。
// 返回的释放函数在 Close 时一并解锁（fd 关闭即释放 flock）。
func (r *Runtime) lockFile(ctx context.Context, root string) (func(), error) {
	path := filepath.Join(root, ".bootstrap.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开运行时锁 %s: %w", path, err)
	}
	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("锁 %s: %w", path, lockErr)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("等待运行时锁超时（另一实例正在 bootstrap）: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
