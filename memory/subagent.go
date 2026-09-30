package memory

import (
	"path/filepath"
	"strings"
)

// 子 agent 的独立记忆目录（官方 memory#how-it-works / subagent 语义，2026-09-30 补）。
//
// 官方原文：
//
//	"The main conversation's auto memory **isn't loaded into subagents**; the
//	 exception is a **fork**, which inherits the parent conversation and system
//	 prompt. **A subagent's own auto memory, enabled with the subagent `memory`
//	 field, is a separate directory.**"
//
// 三句话对应三件事：
//
//	① 主会话的 auto memory **不进子 agent** —— 我们本来就没注入（buildPersonaLoop
//	   不带 WithMemoryInjection），现在有测试钉住，见 memory_wiring_test.go。
//	② fork 例外 —— 我们没有 fork 形态（子 agent 都是新 loop + 新 system prompt），
//	   故无例外可实现。记在这里是为了将来加 fork 时知道它**必须**继承。
//	③ `memory` 字段启用，且是**独立目录** —— 本文件实现的部分。
//
// # 目录布局：为什么用 `agents/` 而不是塞进 `repos/<id>/` 里
//
//	~/.go-code/memory/
//	├── repos/<repo-id>/          主会话（一个仓库一份）
//	└── agents/<repo-id>/<name>/  子 agent（每个启用 memory 的子 agent 一份）
//
// 两个命名空间**互不相交**，这样 `ls ~/.go-code/memory` 不会把子 agent 的记忆
// 混进仓库记忆里（子 agent 数量会随定义数增长，混在一起很快没法看）。代价是
// 同一仓库的 N 个子 agent 会各有一层 `<repo-id>` 重复，但换来的是可辨认——
// 这与 idFromRemote / idFromPath 取舍同源。
//
// # 为什么"路径 → 目录"不需要知道"谁在写"
//
// 主 agent 与子 agent **共用同一个 ToolEngine 实例**（desktop/bridge 的 eng），
// 所以 write_file 上的 postWrite 钩子只有 absPath，拿不到调用者身份。
//
// 但这不需要解决：钩子要回答的是「这个路径属于哪份记忆」，而两份记忆的目录
// **互不相交**，路径本身就唯一确定归属。钩子的判据因此是路径，不是身份——
// 也正因如此，子 agent 记忆必须放在与主目录**不相交**的位置，这里用
// `<root>/agents/` 而不是 `<root>/repos/<id>/agents/`。

// agentsRootDirName 子 agent 记忆在记忆根下的目录名。
const agentsRootDirName = "agents"

// agentsRoot 返回子 agent 记忆的根目录 <记忆根>/agents。
//
// 必须是 **Config 的方法**而不是包级函数（2026-09-30 修正）：包级版本用
// MemoryRoot() 取真实的 ~/.go-code/memory，**绕过了 Config.Directory**。于是
// 测试里 `Config{Directory: <tmp>}` 的主目录正确落在临时目录，子 agent 目录却指向
// 用户真实的 ~/.go-code/memory —— 方向相反的两个根，归属判定必然失效，而且测试会
// 往真目录里写。
//
// 形态与 idFromGitDir 那条同源：**"当前恰好没出问题"不是"这一项正确"**。
func (c Config) agentsRoot() (string, error) {
	root, err := c.root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, agentsRootDirName), nil
}

// SubagentDir 返回某个子 agent 的独立记忆目录（由主会话目录派生，保持仓库作用域）。
//
// name 是子 agent 定义名。非法（空 / 纯分隔符 / 化简后为空）时返回零 Dir 与 false——
// 调用点据此**不启用**该子 agent 的记忆，而不是落到某个意外目录上。
//
// 仓库作用域为什么保留：子 agent 在某个工作区里跑，它记的东西（"这个仓库的测试
// 要怎么跑"）确实只对该仓库有意义。按 agent 名单层目录会让两个仓库的同名子 agent
// 互相污染。
func (c Config) SubagentDir(repo Dir, name string) (Dir, bool) {
	root, err := c.agentsRoot()
	if err != nil {
		return Dir{}, false
	}
	seg := sanitizeSegment(name)
	// sanitizeSegment 保留 '.'（为了 remote 派生的 github.com-owner-repo 可辨认），
	// 只 trim 掉 '-'，所以 "." / ".." 会原样通过并让 filepath.Join 退到上一层。
	// 这不是洁癖：`<根>/agents/<repo-id>/..` = `<根>/agents`，即子 agent 的记忆
	// 直接落到整个 agents 命名空间上。
	if seg == "" || seg == "." || seg == ".." {
		return Dir{}, false
	}
	path := filepath.Join(root, repo.ID, seg)
	return Dir{ID: filepath.Join(repo.ID, seg), Path: path}, true
}

// SubagentDirFromPath 是 SubagentDir 的逆：由 absPath 反推它属于哪个子 agent 的记忆目录。
//
// 返回 false 表示该路径不属于任何子 agent 记忆目录（调用点据此走普通文件流程）。
// 它只做词法判定：不 stat、不 EvalSymlinks——与 IsInMemoryDir 同一口径。
//
// 仓库 ID 一段是必要的：`<根>/agents/` 下可能有多个仓库的子 agent 记忆，只按
// 最后一段名字取会让跨仓库同名子 agent 撞车。
func (c Config) SubagentDirFromPath(repo Dir, absPath string) (Dir, bool) {
	if repo.ID == "" || !filepath.IsAbs(absPath) {
		return Dir{}, false
	}
	root, err := c.agentsRoot()
	if err != nil {
		return Dir{}, false
	}
	prefix := filepath.Join(root, repo.ID) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(absPath), prefix) {
		return Dir{}, false
	}
	rel := strings.TrimPrefix(filepath.Clean(absPath), prefix)
	seg := rel
	if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
		seg = rel[:i]
	}
	// seg 已经过按分隔符切分，不含分隔符；sanitizeSegment 只把分隔符变成 '-'，
	// 保留 '.'，所以唯一能逃出目录的是 seg 恰为 "." 或 ".."。精确拒这两者即可
	// （拒 "a..b" 这类只会误伤正常名字）。
	if seg == "" || seg == "." || seg == ".." {
		return Dir{}, false
	}
	return Dir{
		ID:   filepath.Join(repo.ID, seg),
		Path: filepath.Join(root, repo.ID, seg),
	}, true
}

// 编译期提醒：agentsRoot 必须走 Config.root()。若有人把它改回包级 MemoryRoot()，
// 下面这条测试会失败（它断言子目录也在临时根之下）。
