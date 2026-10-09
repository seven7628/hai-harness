package runtime

// node.go：codemode JS 沙箱的解释器探测（Node）。
//
// # 为什么是「探测系统 Node」而不是像 Python 那样自建受管环境
//
// runtime/python.go 自建 venv 的**根因**很具体：办公文档四件套（python-docx /
// openpyxl / pypdf …）是用户机器上**不可能预装**的第三方库，版本组合也必须钉死
// 才能复现 —— 所以「装依赖」是刚需，「装解释器」只是搭台。
//
// codemode 的情况相反：**它要的是一套 JS 运行时语义，不是任何 npm 包**。
// 我们的脚本只用到 console/JSON/Promise/await 这些语言内置能力，prelude 与
// 桥接协议全是零依赖的宿主注入代码。因此「装 node 本身」在 Phase 1 没有任何
// 产出 —— 却要付出全部代价：
//
//   - 下载 ~100MB 二进制并校验（网断/代理失败即整条能力不可用）；
//   - 与用户自己的 npm/nvm 生态**并存**（两份 node、两个 npm prefix，用户
//     改 PATH 后行为漂移，且 ~/.go-code/runtime/node 会成为第二个事实源）；
//   - 需要给沙箱再加一条放行子树 + 就绪标记 + 跨进程 flock（复刻 python.go
//     300 行的 bootstrap 状态机），而它产出的东西**零新增能力**。
//
// 故 Phase 1 只做**探测 + 明确可读的缺失提示**（与 plugin/browser/install.go
// 的 defaultNodeCheck 同一范式、同一份候选清单口径）。用户真需要「bridge 单文件
// 分发 / 用户无 Node」时，触发条件是 design doc §19 P2-a 那条，届时按
// run_python 的形态补受管环境 —— 那时它有真实消费者，不是预先铺设。
//
// # 与 plugin/browser 的 node 探测的关系
//
// 候选位置刻意与 plugin/browser/install.go:nodeCandidates 同口径（homebrew /
// 官网 pkg / nvm / fnm / volta / MacPorts）。**不**直接复用那个函数：它在
// plugin 包内且与「npm i -g 装 Browser Use」的语义耦合（本包不需要 npm），跨包
// import 会把 plugin 拖进 codemode 的依赖图。两份清单各自演化是本仓既存的代价
// （候选清单只增不改，不会出现「一边支持一边不支持」的语义分歧）。
//
// 关键教训沿用其注释：GUI 启动的桌面 App（Electron spawn 的 bridge）继承的是
// launchd 最小 PATH，**LookPath 找不到 ≠ 用户没装 node**。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// minNodeMajor 最低主版本。
//
// 取 20 而非设计文档 §14 写的 18：prelude 经 `--import <data: URL>` 注入，
// 该标志 Node 20.6 才稳定（18.19 才有实验性版本）；<20 的语义差异（ESM 解析、
// 顶层 await over stdin）不逐一测，只用「≥20 且 CI 断言」兜住 —— 设计文档
// §19 Phase 1 风险 (b) 给的正是 20。§14 那处写 18 是文档内部不一致，取严的一边。
const minNodeMajor = 20

// nodeProbeTimeout 探测期超时。
//
// 与 probeTimeout（python）同口径：解释器自检正常 <100ms，慢到 30s 说明环境
// 已坏（PATH 上的 shim 指向了已卸载的 nvm 版本是典型），报错比挂住好。
const nodeProbeTimeout = 30 * time.Second

// NodeConfig 解释器探测配置（零值即生产默认）。
type NodeConfig struct {
	// Node 解释器绝对路径（优先用它；空 = PATH → 已知安装位置兜底）。
	Node string
	// Home 用户主目录（候选清单用）；空 = 从环境/os.UserHomeDir 解析。
	Home string
	// RunCmd 执行版本探测（测试注入；nil = 进程执行 + 超时）。
	RunCmd func(ctx context.Context, name string, args ...string) (string, error)
}

// Node 已就绪的解释器句柄。值类型（只含路径与版本，可随意传递/缓存）。
type Node struct {
	// Path 解释器绝对路径（执行器该用的就是它）。
	Path string
	// Version 原始版本串（如 "v25.8.1"），进错误文本用。
	Version string
	// Major 主版本号。
	Major int
}

// String 便于日志/错误里指名道姓（而不是甩一串路径）。
func (n Node) String() string {
	if n.Version == "" {
		return n.Path
	}
	return n.Path + " (" + n.Version + ")"
}

// NodeRuntime JS 沙箱解释器探测器（可并发使用）。
type NodeRuntime struct {
	cfg NodeConfig

	// mu 保护 mem（探测结果缓存）与惰性探测的并发。
	//
	// 惰性探测必须在锁内完成：编排脚本会 Promise.all 并发发起调用，同时冷启
	// 一次探测会 spawn 多个 node 进程，且各自输出一遍错误。无锁的 check-then-set
	// 是 Phase 0 教训 2 的原样复发。
	mu  sync.Mutex
	mem Node
}

// NewNodeRuntime 构造探测器（仅解析配置，不做任何 I/O —— 探测是 Ensure 的职责）。
func NewNodeRuntime(cfg NodeConfig) *NodeRuntime { return &NodeRuntime{cfg: cfg} }

// Ensure 幂等地返回一个可用的 Node 解释器（>= 20），供 execproc 经 sandbox 拉起。
//
// 语义与 runtime.Python.Ensure 同形（返回就绪句柄而非裸路径），但**没有**
// bootstrap 阶段：探测成功即就绪，不写任何文件、不建任何目录。
//
// 失败时的错误文本是**给模型看的**（模型会看到它并自我修正），故必须说清
// 「缺什么、装什么、装完怎么确认」，而不是甩一个 exec.ErrNotFound。
func (r *NodeRuntime) Ensure(ctx context.Context) (Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mem.Path != "" {
		return r.mem, nil
	}
	// 显式配置优先（测试隔离与「我就是要用某个 node」的逃生阀）。
	if p := strings.TrimSpace(r.cfg.Node); p != "" {
		n, err := r.probe(ctx, p)
		if err != nil {
			return Node{}, fmt.Errorf("配置的 Node 解释器 %s 不可用：%w\n"+
				"提示：清空配置改用自动探测，或指向一个 Node >= %d 的可执行文件", p, err, minNodeMajor)
		}
		r.mem = n
		return n, nil
	}
	// 候选逐个试跑而不是取第一个存在的：PATH 上可能有 nvm/volta shim 指向一个
	// 版本过低或已损坏的解释器，试跑过后才能确认（与 python.go findBase 同理）。
	var tried []string
	var lastErr error
	for _, c := range r.candidates() {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err != nil {
			continue
		}
		tried = append(tried, c)
		n, err := r.probe(ctx, c)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", c, err)
			continue
		}
		r.mem = n
		return n, nil
	}
	detail := ""
	if len(tried) > 0 {
		detail = fmt.Sprintf("；已尝试 %s", strings.Join(tried, ", "))
	}
	if lastErr != nil {
		detail += fmt.Sprintf("；最后一条错误：%v", lastErr)
	}
	return Node{}, fmt.Errorf(
		"未找到可用的 Node.js（需 >= %d）%s。\n"+
			"提示：安装 Node.js（macOS 推荐 `brew install node`，或从 https://nodejs.org 下载），"+
			"装完确认 `node --version` 能跑通即可；本工具需要 node 来执行 JavaScript，"+
			"没有它本工具整体不可用（其它工具不受影响）。",
		minNodeMajor, detail)
}

// candidates 候选解释器列表（PATH 优先，再按确定性排序的已知安装位置）。
//
// GO_CODE_NODE_CANDIDATES（PathListSeparator 分隔）整体覆盖候选列表 —— 测试隔离
// 用（屏蔽本机真实安装）；正常不设。与 plugin/browser 同名同义（见 install.go）。
func (r *NodeRuntime) candidates() []string {
	if ov := os.Getenv("GO_CODE_NODE_CANDIDATES"); ov != "" {
		return strings.Split(ov, string(os.PathListSeparator))
	}
	var cs []string
	if p, err := exec.LookPath("node"); err == nil {
		cs = append(cs, p)
	}
	home := r.cfg.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	cs = append(cs,
		"/opt/homebrew/bin/node", // Apple Silicon Homebrew
		"/usr/local/bin/node",    // 官网 pkg / Intel Homebrew
		"/opt/local/bin/node",    // MacPorts
	)
	if home != "" {
		cs = append(cs,
			home+"/.volta/bin/node",                               // volta shim
			home+"/.local/bin/node",                               // 用户级 ~/.local/bin
			filepath.Join(home, ".nvm/versions/node"),             // nvm 多版本根（子目录展开）
			filepath.Join(home, ".fnm/node-versions"),             // fnm legacy 安装根
			filepath.Join(home, ".local/share/fnm/node-versions"), // fnm 安装根
		)
	}
	return cs
}

// probe 试跑一个候选，确认它可执行且主版本达标。
func (r *NodeRuntime) probe(ctx context.Context, path string) (Node, error) {
	if isDir(path) {
		// 多版本根（nvm/fnm）：子目录名 = 版本号，取最大者的 bin/node。
		p, ok := newestInVersionsRoot(path)
		if !ok {
			return Node{}, fmt.Errorf("版本根下没有可用的 bin/node")
		}
		path = p
	}
	out, err := r.run(ctx, path, "--version")
	if err != nil {
		return Node{}, err
	}
	// `node --version` 恒回单行 `v25.8.1`；但 nvm shim 可能先打一行噪声。
	ver, major, err := parseNodeVersion(out)
	if err != nil {
		return Node{}, err
	}
	if major < minNodeMajor {
		return Node{}, fmt.Errorf("版本 %s 过低（需 >= %d）", ver, minNodeMajor)
	}
	return Node{Path: path, Version: ver, Major: major}, nil
}

// parseNodeVersion 从 `node --version` 的输出里取版本串与主版本号。
//
// 容错两件事：首尾空白与噪声行（nvm shim 会打 `v20.11.1` 之外的引导语），
// 以及裸版本号（部分发行版/构建不打 `v` 前缀）。
func parseNodeVersion(out string) (ver string, major int, err error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		l := strings.TrimPrefix(line, "v")
		if l == "" {
			continue
		}
		n, ok := leadingInt(l)
		if !ok {
			continue // 非版本行（噪声），继续找
		}
		return line, n, nil
	}
	return "", 0, fmt.Errorf("无法解析版本号（输出 %q）", strings.TrimSpace(out))
}

// leadingInt 取字符串开头的整数（"25.8.1" → 25；"x1" → false）。
func leadingInt(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			if i == 0 {
				return 0, false
			}
			return n, true
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, len(s) > 0
}

// run 跑一条探测命令（注入点优先）。
func (r *NodeRuntime) run(ctx context.Context, name string, args ...string) (string, error) {
	if r.cfg.RunCmd != nil {
		return r.cfg.RunCmd(ctx, name, args...)
	}
	return defaultRunCmd(ctx, probeTimeout, name, args...)
}

// isDir 路径是否为目录（多版本根 vs 单可执行文件的分派依据）。
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// newestInVersionsRoot 在 nvm/fnm 式多版本根里定位 bin/node（取版本号最大者）。
//
// 不依赖 `ls` 的字典序（"v9" > "v10"），逐段比较数字 —— 与 plugin/browser 的
// versionOf/versionGreater 同口径，那份在 plugin 包内，此处独立实现。
func newestInVersionsRoot(root string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	best, bestVer := "", []int(nil)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ver := versionSegments(e.Name())
		p := filepath.Join(root, e.Name(), "bin", "node")
		if isExecutable(p) && versionGreaterSegs(ver, bestVer) {
			best, bestVer = p, ver
		}
	}
	return best, best != ""
}

// versionSegments 解析版本目录名（"v20.11.1" → [20,11,1]；不可解析返回 nil）。
func versionSegments(name string) []int {
	n := strings.TrimPrefix(name, "v")
	if n == "" {
		return nil
	}
	var segs []int
	for _, part := range strings.Split(n, ".") {
		v, ok := leadingInt(part)
		if !ok {
			return nil
		}
		segs = append(segs, v)
	}
	return segs
}

// versionGreaterSegs 逐段比较（a<b 返回 false；a 不可解析视为小于一切）。
func versionGreaterSegs(a, b []int) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}
