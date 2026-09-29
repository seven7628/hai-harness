// Package memory 实现 Claude Code 兼容的 Auto Memory（技术方案《HAI-Harness-Memory能力技术方案》）。
//
// # 本包做什么（Step 1~4）
//
//	① memory.go / identity.go  §5.1 由工作目录派生记忆目录与仓库身份
//	② path.go                 §5.4 记忆路径的范围识别（Dir.Owns / IsTopic / IsIndex）
//	③ index.go                §5.2 启动注入有界索引（前 200 行 / 25KB）
//	④ stamp.go                §5.3 写后补 `modified` 时间戳
//	⑤ config.go               §8 待定项 2  用户可见开关 + 官方三个环境变量
//	⑥ bootstrap.go            接线入口：Bootstrap(Step 3) + Config.StampModified(Step 4)
//
// # 官方形态的核心：不使用专用记忆工具
//
// 官方原文（memory#how-it-works）：
//
//	"Claude doesn't load topic files such as user_role.md at startup.
//	 Claude reads them on demand using its standard file tools when it needs the information."
//
// 记忆就是普通 Markdown 文件，模型用既有的 read_file / write_file / edit_file 操作。
// 因此本包**没有** memory_view / memory_write，也没有写入准入校验链——官方没有这些机制。
// 唯一的产品侧引导是启动时把 MEMORY.md 的前 200 行 / 25KB 放进 context（§5.2）。
//
// # 本包明确不做（§4.5 官方未规定，不得自行发明）
//
//	索引行格式（官方只有"one line per memory"，是纯文件名列表，无 description/title/摘要）
//	frontmatter 的 description 字段（官方只有 type 与 modified）
//	索引排序、冲突时的优先级、过期 / 淘汰 / 自动清理、向量检索、/memory UI、
//	subagent 记忆继承、Skill Candidate、任何形式的自进化
//
// # 当前状态：本包**未被任何生产文件 import**（有意为之）
//
// Step 0 的三臂行为基线（desktop/bridge/memory_baseline_test.go 的
// baselineEnforceEmptyMemoryState）断言"仓库没有可被装配路径消费的 memory 实现"——
// 一旦接线，那次基线测到的就不是模型的行为而是实现的行为。详见 bootstrap.go 顶部的说明。
//
// 官方依据（memory#storage-location）：
//
//	"Each project gets its own memory directory at ~/.claude/projects/<project>/memory/.
//	 The <project> path is derived from the git repository, so all worktrees and
//	 subdirectories within the same repo share one auto memory directory."
//
// 由此得到本包的一条硬约束：
//   - 身份是**仓库**身份，不是目录身份：同一仓库的所有 worktree 与子目录必须落到同一份记忆；
//   - worktree 共享靠 `git rev-parse --git-common-dir`（worktree 各自的 .git 是一个文件，
//     指向主仓库的 common dir），所以无 remote 时用它派生 ID。
package memory

import (
	"errors"
	"os"
	"path/filepath"
)

// 身份解析的哨兵错误（调用点用 errors.Is 判别，不解析错误字符串）。
var (
	// ErrNoGit 工作目录不在 git 工作树内。
	//
	// 官方在 git 仓库之外改用「项目根目录」（memory#storage-location），
	// 但**具体怎么把项目根目录变成一个目录名官方没有规定**，且《技术方案》§8 待定项 1
	// 倾向默认关闭（绝对路径哈希会让同一项目移动后失去记忆）。
	// 故此处按「返回明确错误、不启用记忆」实现，等待该项决策。
	ErrNoGit = errors.New("memory: 工作目录不在 git 工作树内，不启用记忆")
	// ErrNoHome 取不到 home 目录，无法定位 ~/.go-code/memory。
	ErrNoHome = errors.New("memory: 无法定位 home 目录")
	// ErrNoIdentity 无法由 git 派生出仓库身份（如 ID 派生结果为空或含路径分隔符）。
	ErrNoIdentity = errors.New("memory: 无法确定仓库身份")
)

// 记忆存储的目录约定（技术方案 §3.1）：
//
//	~/.go-code/memory/repos/<canonical-repo-id>/
//	  MEMORY.md      索引，一行一条
//	  <topic>.md     一条记忆一个文件
//
// 与会话 / 事件同属 APP 级数据（跨 workspace），与 skills 的工作区级存储不同。
const (
	// appDirName go-code 应用数据目录名（~/.go-code，与 desktop/bridge/appdata.go 同一约定）。
	appDirName = ".go-code"
	// memoryDirName 记忆根目录名（~/.go-code/memory）。
	memoryDirName = "memory"
	// reposDirName 记忆按仓库分层的子目录（~/.go-code/memory/repos）。
	reposDirName = "repos"
)

// MemoryRoot 记忆根目录 ~/.go-code/memory（**所有**仓库记忆的公共父目录）。
//
// session 保留期清理要整体排除它：官方 memory#storage-location 明确
// "excludes the memory files in the memory directory from that retention sweep"。
func MemoryRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", ErrNoHome
	}
	return filepath.Join(home, appDirName, memoryDirName), nil
}

// ReposRoot 记忆的按仓库分层根目录 ~/.go-code/memory/repos。
func ReposRoot() (string, error) {
	root, err := MemoryRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, reposDirName), nil
}

// Dir 一份 Project Memory 的定位结果（官方语境下的 <project>/memory/）。
//
// 它同时是 Step 3 与 Step 4 的输入：Step 3 读 <Path>/MEMORY.md，Step 4 用
// IsTopic 判断某个写入是否落在记忆文件上。
type Dir struct {
	// ID 规范化后的仓库身份，同时是目录名。
	ID string
	// Path 记忆目录绝对路径 ~/.go-code/memory/repos/<ID>。
	//
	// 注意：官方**不创建**这个目录，首次写入时才有。本包只做路径计算，不做建目录——
	// 官方没有「启动即建目录」这一步，凭空创建会让「无记忆」与「有记忆」无法区分。
	Path string
}
