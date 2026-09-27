// Package memory 解析 Project Memory（auto memory）的存储位置与身份。
//
// 范围（技术方案《HAI-Harness-Memory能力技术方案》Step 1/Step 2）：只有两件事——
// ① 由工作目录派生出「记忆目录在哪」（§5.1）；② 判断某绝对路径是否落在记忆目录里（§5.4）。
//
// **本包不做记忆的读、写、注入。** 官方（memory#how-it-works）：
//
//	"Claude reads them on demand using its standard file tools when it needs the information."
//
// 记忆就是普通 Markdown，模型用既有的 read_file / write_file / edit_file 操作，
// 唯一的产品侧引导是启动时注入 MEMORY.md 的前 200 行 / 25KB（§5.2，Step 3 实现）。
// 因此这里没有 memory_view / memory_write，也没有写入准入校验链——官方没有这些机制。
//
// 官方依据（memory#storage-location）：
//
//	"Each project gets its own memory directory at ~/.claude/projects/<project>/memory/.
//	 The <project> path is derived from the git repository, so all worktrees and
//	 subdirectories within the same repo share one auto memory directory."
//
// 由此得到本包的两条硬约束：
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
type Dir struct {
	// ID 规范化后的仓库身份，同时是目录名。
	ID string
	// Path 记忆目录绝对路径 ~/.go-code/memory/repos/<ID>。
	//
	// 注意：官方**不创建**这个目录，首次写入时才有。本包只做路径计算，不做建目录——
	// 官方没有「启动即建目录」这一步，凭空创建会让「无记忆」与「有记忆」无法区分。
	Path string
}
