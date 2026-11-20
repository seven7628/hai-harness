---

## 11. 详细代码改动方案

> 本节是**实施级**方案：逐文件、逐函数、逐签名。涉及的新增代码给出完整可编译的骨架；
> 改动点给出精确位置（file:line）与前后对照。
>
> **依赖前提**：`workdir` 包是新增的顶层包（**不能放 `/internal/`** —— `.gitignore:23` 的 `/internal/*` 会让它不入版本控制，
> 与当初 `eventslog` 同样的坑）。

### 11.0 总体形态

```
                      ┌──────────────────────────────────────────┐
                      │  workdir.State（会话级，唯一的 cwd 真值源） │
                      │    root  string   项目根（不可变安全边界）   │
                      │    cur   map[Key]string  分区 → 当前 cwd    │
                      │    sent  map[Key]Env    分区 → 上次发给模型的  │
                      └────┬──────────────┬──────────────┬────────┘
                           │              │              │
              ┌────────────▼───┐  ┌───────▼────────┐  ┌──▼──────────────┐
              │ tools/builtin  │  │ agents/agent_  │  │ desktop/bridge  │
              │  bash: cmd.Dir │  │ loop.go        │  │  worktree 工具   │
              │  file: 相对基准 │  │  每次请求注入   │  │  切/还原 cwd     │
              │                │  │  环境块         │  │                  │
              └────────────────┘  └────────────────┘  └─────────────────┘
                                     │
                              ┌──────▼──────────────────────────────┐
                              │ provider.StreamRequest.Messages      │
                              │ 末尾追加 ephemeral user 消息          │
                              │ （不落 ac.Messages ⇒ 不动 checkpoint）  │
                              └─────────────────────────────────────┘
```

**四条设计红线**（都来自调研结论，不是拍脑袋）：

| # | 红线 | 依据 |
|---|---|---|
| R1 | **cwd 通知不许进 system 层** | Claude Code 官方明文（环境信息在对话里，不在 system prompt）+ hai 自己的缓存论证（`agents/commands.go:85-89`） |
| R2 | **cwd 通知不许落 `ac.Messages`** | 落进去 ⇒ 进 checkpoint ⇒ 动我们已验证过的逐字节等价性 |
| R3 | **文件工具的越界判定仍以 project root 为界**（只是**解析基准**改为 cwd） | 保持"逃不出工作区"这个既有不变量（`file_path_contract_test.go:22`） |
| R4 | **默认路径（未 cd）行为必须逐字节不变** | cwd==root 时新旧实现完全等价 ⇒ 回归面最小 |

### 11.1 新增：`workdir/` 包

#### 11.1.1 `workdir/state.go`

```go
// Package workdir 是「当前工作目录」的单一真值源。
//
// 为什么必须单独一个包：cwd 要同时被三类代码读 ——
//	· tools/builtin（bash 的 cmd.Dir、文件工具的相对路径基准）
//	· agents（渲染给模型的环境块）
//	· desktop/bridge（worktree 工具、session 生命周期）
// 而 tools/builtin ⇄ agents 互相 import 会成环 ⇒ 只能放在两者都能 import 的叶子包上。
//
// 语义对齐 Claude Code（tools-reference 文档 + 2.1.295 实现）：
//	· Bash 每条命令独立进程，但 cwd 是 session 级状态，下次 spawn 时用它
//	· 每个 subagent 有自己的 cwd 分区，不继承父级的 cd
//	· cd 越界（不在项目目录内）⇒ 重置回项目目录，并在工具结果里提示
package workdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Key 是分区键：主 agent 为 MainKey，子 agent 为其 task id。
//
// 为什么按 task id 分区：工具实例在注册表里共享（主 agent 与后台子 agent 共用同一个
// ToolEngine），cwd 若挂在实例字段上，子 agent 的 cd 会改掉主 agent 的目录。
// 现状已有这个机制（tools/builtin/bash.go:504-517 的 cwdKey），本包把它上移一层。
type Key = string

// MainKey 是主 agent 的分区键（跨轮稳定）。
const MainKey Key = ""

// ErrOutside 表示目标目录越出了项目根。
var ErrOutside = errors.New("workdir: outside project root")

// State 是会话级的 cwd 状态。
type State struct {
	mu   sync.RWMutex
	root string            // 项目根：不可变锚点 + 安全边界
	cur  map[Key]string   // 分区 → 当前工作目录
	sent map[Key]Env      // 分区 → 上次发给模型的环境快照
}

// New 建一个以 root 为项目根的状态。
func New(root string) *State {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &State{
		root: root,
		cur:  make(map[Key]string),
		sent: make(map[Key]Env),
	}
}

// Root 返回项目根。
func (s *State) Root() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.root
}

// Current 返回该分区的当前工作目录；未设置时回落到项目根。
func (s *State) Current(key Key) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d := s.cur[key]; d != "" {
		return d
	}
	return s.root
}

// Set 把该分区的当前目录设为 dir。
//
// 越界（dir 不在项目根之内）⇒ **不改记忆**，返回 ErrOutside；
// 调用方（bash 工具）负责重置回项目根并在工具结果里提示（对齐 Claude Code 的
// "Shell cwd was reset to <dir>"）。
func (s *State) Set(key Key, dir string) (old string, changed bool, err error) {
	abs, aerr := filepath.Abs(dir)
	if aerr != nil {
		abs = dir
	}
	if !s.Contains(abs) {
		return s.Current(key), false, ErrOutside
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old = s.cur[key]
	if old == abs {
		return old, false, nil
	}
	s.cur[key] = abs
	return old, true, nil
}

// Reset 把该分区重置回项目根。
func (s *State) Reset(key Key) (old string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old = s.cur[key]
	delete(s.cur, key)
	return old
}

// Forget 清理该分区（子 agent 结束回收；避免长会话 map 无限增长 —— 现状 curCwd 从不清理）。
func (s *State) Forget(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cur, key)
	delete(s.sent, key)
}

// Contains 判断 dir 是否位于项目根之内（根本身也算）。
func (s *State) Contains(dir string) bool {
	s.mu.RLock()
	root := s.root
	s.mu.RUnlock()
	if root == "" {
		return true
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// SentEnv 返回该分区最后一次发给模型的环境快照。
func (s *State) SentEnv(key Key) (Env, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.sent[key]
	return e, ok
}

// MarkEnvSent 记录该分区最后一次发给模型的环境快照。
func (s *State) MarkEnvSent(key Key, e Env) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[key] = e
}
```

#### 11.1.2 `workdir/env.go`

```go
package workdir

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Env 是「模型可见的环境快照」。
// 字段与渲染文案对齐 Claude Code 2.1.295 的 # Environment 块原文。
type Env struct {
	WorkingDir string
	IsGitRepo  bool
	IsWorktree bool
	Platform   string
	Shell      string
	OSVersion  string
}

// Snapshot 取某分区的当前环境快照。
func (s *State) Snapshot(key Key) Env {
	dir := s.Current(key)
	return Env{
		WorkingDir: dir,
		IsGitRepo:  isGitRepo(dir),
		IsWorktree: isWorktree(dir),
		Platform:   runtime.GOOS,
		Shell:      detectShell(),
		OSVersion:  detectOSVersion(),
	}
}

// RenderFull 渲染完整环境块（对齐 Claude Code 首轮块）。
//
//	# Environment
//	You have been invoked in the following environment:
//	  - Primary working directory: /path
//	  - Is a git repository: true
//	  - Platform: darwin
//	  - Shell: zsh
//	  - OS Version: Darwin 24.5.0
func RenderFull(e Env) string {
	var b strings.Builder
	b.WriteString("# Environment\n")
	b.WriteString("You have been invoked in the following environment:\n")
	fmt.Fprintf(&b, "  - Primary working directory: %s\n", e.WorkingDir)
	fmt.Fprintf(&b, "  - Is a git repository: %t\n", e.IsGitRepo)
	if e.IsWorktree {
		b.WriteString("  - This is a git worktree — an isolated copy of the repository. " +
			"Run all commands from this directory. Do NOT `cd` to the original repository root.\n")
	}
	fmt.Fprintf(&b, "  - Platform: %s\n", e.Platform)
	fmt.Fprintf(&b, "  - Shell: %s\n", e.Shell)
	fmt.Fprintf(&b, "  - OS Version: %s\n", e.OSVersion)
	return b.String()
}

// RenderDelta 渲染增量块（对齐 Claude Code 的 "# Environment update"）。
//
// 注意：hai 不落 history ⇒ 模型上下文里没有"旧值" ⇒ 这里**刻意不带 (was X)**，
// 只给当前值（Claude Code 敢带 was X 是因为它的环境块是持久在对话历史里的）。
func RenderDelta(old Env, cur Env) string {
	var b strings.Builder
	b.WriteString("# Environment update\n")
	fmt.Fprintf(&b, "  - Primary working directory: %s\n", cur.WorkingDir)
	if cur.IsWorktree != old.IsWorktree {
		if cur.IsWorktree {
			b.WriteString("  - This is a git worktree — an isolated copy of the repository. " +
				"Run all commands from this directory. Do NOT `cd` to the original repository root.\n")
		} else {
			b.WriteString("  - The primary working directory is no longer a git worktree.\n")
		}
	}
	return b.String()
}

// Equal 判断两份快照是否等价（决定要不要重发）。
func (e Env) Equal(o Env) bool {
	return e.WorkingDir == o.WorkingDir && e.IsGitRepo == o.IsGitRepo &&
		e.IsWorktree == o.IsWorktree && e.Platform == o.Platform &&
		e.Shell == o.Shell && e.OSVersion == o.OSVersion
}

func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func isWorktree(dir string) bool {
	if dir == "" {
		return false
	}
	// worktree 里 .git 是一个文件（gitdir: 指针），主检出里是目录。
	st, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && !st.IsDir()
}

func detectShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return filepath.Base(sh)
	}
	return "sh"
}

func detectOSVersion() string {
	out, err := exec.Command("uname", "-sr").Output()
	if err != nil {
		return runtime.GOOS
	}
	return strings.TrimSpace(string(out))
}
```

### 11.2 改：`tools/builtin/bash.go`

| # | 位置 | 改动 |
|---|---|---|
| B1 | `:39` `BashTool.Cwd` | 保留（作为 `wd == nil` 时的兜底），新增 `wd *workdir.State` |
| B2 | `:46-52` `mu`/`curCwd` | **删除**（状态上移到 `workdir.State`） |
| B3 | `:119-139` `NewBashTool` | 签名改为 `NewBashTool(wd *workdir.State, timeout time.Duration, sbx sandbox.Sandbox, appAllow []string)`；`Cwd: wd.Root()` |
| B4 | `:244-260` cd 处理 | 加**越界重置**与**结果提示**（见 B4 代码） |
| B5 | `:504-517` `cwdKey` | **不变**（分区键语义正确，直接复用） |
| B6 | `:519-534` `currentCwd`/`setCurrentCwd` | 改为读写 `workdir.State` |
| B7 | `:123` 工具描述 | 补"cd 保持 + 当前目录在 Environment 块里" |
| B8 | `:536-583` `splitCd` | 返回值改为**绝对路径**（现状 `filepath.Join(cwd, after)` 已是绝对，确认即可） |

**B4 代码**（替换 `:244-260`）：

```go
	cmd := a.Command
	cwd := t.currentCwd(ctx)
	resetNote := ""
	if target, rest, ok := t.splitCd(a.Command, cwd); ok {
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			abs, aerr := filepath.Abs(target)
			if aerr != nil {
				abs = target
			}
			if serr := t.setCurrentCwd(ctx, abs); serr != nil {
				// 越界（Q2 = 重置）：cd 目标不生效，命令仍在原 cwd 执行；
				// 结果里明确告诉模型"cwd 被重置"，否则模型会以为 cd 成功了。
				resetNote = fmt.Sprintf("\nShell cwd was reset to %s", t.projectRoot())
			} else {
				cwd = abs
				if rest != "" {
					cmd = rest // cd 前缀已由 spec.Cwd 兑现，只跑剩余命令
				}
			}
		}
	}
```

> ⚠️ 与 Claude Code 的**有意偏差**：Claude Code 越界时**命令仍在越界目录执行**、只是把*后续命令的起点*重置回项目目录。
> hai 这里让命令**留在原 cwd 执行** —— 更保守，且与 Seatbelt 的路径范围意图一致（沙箱本就不该让命令跑到工作区外）。
> ⇒ 需在评审时确认这一条（见 §8.2 Q2）。

**B6 代码**：

```go
// currentCwd 取该调用方分区的当前工作目录。
func (t *BashTool) currentCwd(ctx context.Context) string {
	if t.wd != nil {
		return t.wd.Current(cwdKey(ctx))
	}
	return t.Cwd
}

// setCurrentCwd 更新该调用方分区的当前工作目录。
// 返回 ErrOutside 表示越界（调用方负责重置 + 提示）。
func (t *BashTool) setCurrentCwd(ctx context.Context, dir string) error {
	if t.wd != nil {
		_, _, err := t.wd.Set(cwdKey(ctx), dir)
		return err
	}
	return nil
}

// projectRoot 返回项目根（越界重置的目标）。
func (t *BashTool) projectRoot() string {
	if t.wd != nil {
		return t.wd.Root()
	}
	return t.Cwd
}
```

**B7 工具描述改动**（`tools/builtin/bash.go:123` 附近）：

```diff
- "Each command runs as an isolated process (env/state other than the directory does not persist)."
+ "Each command runs as an isolated process: environment variables and other shell state do NOT persist between calls."
+ "The working directory DOES persist between calls in this session — after a successful `cd`, later commands start from that directory."
+ "The current working directory is reported in the conversation's Environment block; if it is not there, assume the workspace root."
```

### 11.3 改：`tools/builtin/builtin.go`（+ `grep.go` / `glob.go`）

| # | 位置 | 改动 |
|---|---|---|
| F1 | `:27-29` `FileTools` | 新增 `wd *workdir.State`；`root` 保留为项目根 |
| F2 | `:116` `NewFileTools` | 签名改为 `NewFileTools(wd *workdir.State)` |
| F3 | `:130-142` `Tools()` | `NewBashTool(f.wd, 0, f.sbx, nil)` |
| F4 | `:137-138` 过时注释 | 修正（"下条命令回到工作区起点" 已不成立） |
| F5 | `:172-174` `pathParamDesc` | 文案改（见下） |
| F6 | `:195/:199/:201-217` `resolve`/`resolveWrite`/`resolveFor` | **加 ctx 参数**；解析基准改为 cwd；**越界判定仍以 root 为界**（R3） |
| F7 | `:456/:536` `read_file` | `t.resolve(ctx, a.Path)` |
| F8 | `:893/:943` `write_file` | `t.resolveWrite(ctx, a.Path)` |
| F9 | `:1072/:1119` `edit_file` | `t.resolveWrite(ctx, a.Path)` |
| F10 | `grep.go:39/117/125` | `t.resolve(ctx, start)`；ignore matcher 锚点改为解析出的 base |
| F11 | `glob.go:34/108/118` | 同上 |

**F2 代码**：

```go
// NewFileTools 造一组以 wd 为工作目录的文件工具。
//
// wd 持有项目根与各分区的当前目录；相对路径按**调用方分区的当前 cwd** 解析，
// 但**不得逃出项目根**（安全边界不变，见 resolveFor）。
func NewFileTools(wd *workdir.State) *FileTools {
	return &FileTools{root: wd.Root(), wd: wd, sbx: sandbox.NoSandbox{}}
}
```

**F6 代码**（R3 的核心：解析基准 = cwd，越界边界 = root）：

```go
func (f *FileTools) resolveFor(ctx context.Context, raw string, op PathOp) (string, error) {
	if raw == "" {
		return "", errors.New("path is required")
	}
	base := ""
	if !filepath.IsAbs(raw) {
		base = f.cwd(ctx) // ← 当前工作目录（会话内可随 cd 移动）
	}
	p := filepath.Clean(filepath.Join(base, raw))
	// 越界边界**仍然是项目根**（不是 cwd）：cwd 可以是 root 的子目录，
	// 但任何相对路径都不得逃出 root（保持既有不变量，见 file_path_contract_test.go）。
	if !within(f.root, p) {
		return "", fmt.Errorf("path %q escapes workspace root %q", raw, f.root)
	}
	if err := denyHarnessConfigPath(p, op); err != nil {
		return "", err
	}
	return p, nil
}

// cwd 取该调用方分区的当前工作目录。
func (f *FileTools) cwd(ctx context.Context) string {
	if f.wd != nil {
		return f.wd.Current(cwdKey(ctx))
	}
	return f.root
}
```

> **`within` 的语义变化需要复核**：现状是 `p != base && !strings.HasPrefix(p, base+sep)` ⇒ 拒绝逃出 base。
> 新版 base=cwd、边界=root ⇒ 当 cwd==root（默认）时**逐字节等价** ✓（R4）；
> 当 cwd 是子目录时，`../x`（指向 root/x）**允许**（旧版在 root 下也不允许 `../x`，但那时 `../x` 本来就等于逃出 root）——
> ⇒ 新语义是**旧语义的严格超集**？不，是**不同**：旧版 `../x` 一律拒，新版在 cwd=root/src 时允许。
> ⇒ 这条要在评审时明确（见 §8.2 Q1），并同步改 `pathParamDesc` 与契约测试。

**F5 文案改动**（`tools/builtin/builtin.go:172-174`）：

```diff
- "A relative path is resolved against the workspace root and may not escape it (a leading ../ is rejected); use an absolute path for files outside the workspace."
+ "A relative path is resolved against the current working directory (the workspace root until a `cd` changes it) and may not escape the workspace root; use an absolute path for files outside the workspace."
```

### 11.4 改：`agents/agent_loop.go`

| # | 位置 | 改动 |
|---|---|---|
| A1 | `:120-123` `Config` | 新增 `WorkDir *workdir.State` |
| A2 | `:417-423` `WithWorkingDir` | 旁边新增 `WithWorkDirState(wd *workdir.State) Option` |
| A3 | `AgentContext` | 新增 `workKey string`（= `opts.TaskId`）、`envInjected bool` |
| A4 | `:2643` 附近 | `ac.workKey = opts.TaskId`（Run 级别 ⇒ 子 agent 拿到自己的分区） |
| A5 | `:2024` 之后、`:2028` 之前 | **注入环境块**（ephemeral） |
| A6 | `:134` / `:1147` | 两条过时注释修正 |

**A5 代码**（紧跟 `postToolNudgePending` 那段，照抄它的 ephemeral 范式）：

```go
	// 环境块（对齐 Claude Code 的 # Environment / # Environment update）：
	//   · 每次请求**重算**，与上次发过的快照比较；变了才发，没变不发（省 token）
	//   · 载体是 ephemeral user 消息：**不落 ac.Messages ⇒ 不进 checkpoint**（R2）
	//   · 追加在消息末尾 ⇒ 不动前缀 ⇒ **不破 provider 前缀缓存**（R1）
	if a.cfg.WorkDir != nil {
		if text, ok := a.envBlock(ac.workKey); ok {
			requestMessages = append(requestMessages, core.Message{
				Role:    core.User,
				Content: []core.Content{{Type: core.ContentTypeText, Content: text}},
			})
		}
	}
```

**`envBlock` 实现**（放 `agents/agent_loop.go` 或新文件 `agents/cwd_env.go`）：

```go
// envBlock 计算本次请求要注入的环境块。
//
// 首块（该分区从未发过）⇒ 完整块；之后有变化 ⇒ 增量块；没变化 ⇒ 不注入。
func (a *AgentLoop) envBlock(key string) (string, bool) {
	env := a.cfg.WorkDir.Snapshot(key)
	prev, had := a.cfg.WorkDir.SentEnv(key)
	if !had {
		a.cfg.WorkDir.MarkEnvSent(key, env)
		return workdir.RenderFull(env), true
	}
	if env.Equal(prev) {
		return "", false
	}
	a.cfg.WorkDir.MarkEnvSent(key, env)
	return workdir.RenderDelta(prev, env), true
}
```

> **为什么状态放在 `workdir.State` 而不是 `AgentContext`**：`AgentContext` 是**每 Run**新建的
> ⇒ 放那里的话每条用户消息都会重发一次完整块。放 `State`（会话级、跨 rebuildLoop 共享）⇒ 跨 turn 只发增量。
> **恢复会话后** `State` 是新的 ⇒ 没有 prev ⇒ 首块完整 ⇒ 正确（模型上下文里确实没有旧的环境块）。

### 11.5 改：`desktop/bridge/main.go`

| # | 位置 | 改动 |
|---|---|---|
| D1 | `:6281` `newSessionEngineAt` | 建 `wd := workdir.New(workspace)` |
| D2 | `:6295` `builtin.NewFileTools(workspace)` | → `builtin.NewFileTools(wd)` |
| D3 | `:6525` 之后（loop 装配） | `agents.WithWorkDirState(wd)` |
| D4 | `:6084` `exploreToolEngine` | 传入同一个 `wd`（explore 是独立 loop，但共享 State ⇒ 自己的分区） |
| D5 | `:6385` `buildExploreLoop` | `agents.WithWorkDirState(wd)` |
| D6 | `:6403` `builtin.NewGitWorktreeTool(workspace)` | → `NewGitWorktreeTool(wd)`（P1 阶段） |

> ⚠️ **`wd` 的生命周期必须是「会话级」**：`newSessionEngineAt` 每次会话建一次 ⇒
> `rebuildLoop`（`:6668`，切模型/切人格）**复用同一个 `wd`** ⇒ cwd 不会因切模型丢失（Q4 = 保留）✓。

### 11.6 改：`tools/builtin/worktree.go`（P1 阶段）

| # | 位置 | 改动 |
|---|---|---|
| W1 | `:32` `root` | 保留；新增 `wd *workdir.State` |
| W2 | `:42` `NewGitWorktreeTool` | 签名改 `(wd *workdir.State)` |
| W3 | `:171-211` `create/list/remove` | 新增 `enter` / `exit` 两个操作 |
| W4 | 工具实例内 | 新增 `created map[string]bool`（本会话创建的 worktree，对齐 Claude Code 的 Scope 规则） |

**enter / exit 语义**（对齐 Claude Code 的 EnterWorktree / ExitWorktree）：

```go
// enter 进入一个本会话创建的 worktree（把会话的当前工作目录切过去）。
// 不在 {root}/.go-code/worktrees/ 下 ⇒ 拒绝（防越界）。
func (t *GitWorktreeTool) enter(ctx context.Context, path string) (string, error) {
	abs, _ := filepath.Abs(path)
	if !t.wd.Contains(abs) {
		return "", fmt.Errorf("path %q is outside the project root", path)
	}
	if !t.created[abs] {
		return "", fmt.Errorf("worktree %q was not created in this session; "+
			"only worktrees created by this tool can be entered", abs)
	}
	if _, _, err := t.wd.Set(cwdKey(ctx), abs); err != nil {
		return "", err
	}
	return abs, nil
}

// exit 退出 worktree，把当前工作目录还原回项目根。
func (t *GitWorktreeTool) exit(ctx context.Context) string {
	old := t.wd.Reset(cwdKey(ctx))
	return old
}
```

> **worktree 的目录约定**（hai 现状 vs Claude Code）：
> | | hai 现状 | Claude Code | 建议 |
> |---|---|---|---|
> | 位置 | `{workspace}/.go-code/worktrees/<name>` | `.claude/worktrees/<name>` | **保持 hai 现状**（`.go-code/` 已在 gitignore 惯例里） |
> | 分支 | 现状自定 | `worktree-<name>` | 建议对齐 `worktree-<name>`（可读性） |
> | baseRef | 无 | `fresh`(默认，从 origin 默认分支) / `head` | 本期不做（`git worktree add` 默认从 HEAD） |

### 11.7 文案改动清单（**容易漏，必须一起改**）

| 文件:行 | 现文案 | 改后 |
|---|---|---|
| `tools/builtin/bash.go:123` | "Each command runs as an isolated process (env/state other than the directory does not persist)." | 见 §11.2 B7 |
| `tools/builtin/builtin.go:172-174` | "resolved against the workspace root" | 见 §11.3 F5 |
| `tools/builtin/builtin.go:137-138` | "下条命令回到工作区起点" | "cd 在后续调用中保持（越界会被重置回工作区根）" |
| `agents/agent_loop.go:134` | 提到已不存在的 `appendLoadedSkill` | 删除该引用 |
| `agents/agent_loop.go:1147` | "系统提示词每轮重建" | "每个 Run 重建" |
| `agents/agent_loop.go:1713-1718` | `## Current Workspace` | **保留**（它声明的是**工作区根**，语义与"当前 cwd"不同）；可加一行 "当前工作目录见对话中的 Environment 块" |
| `agents/prompt/product.go:38` | "the working directory set to the workspace" | 视 run_python 是否跟随 cwd 而定（P1 再议） |

### 11.8 改动汇总（按文件）

| 文件 | 改动性质 | 风险 |
|---|---|---|
| `workdir/state.go`（新） | 新增 | 低 |
| `workdir/env.go`（新） | 新增 | 低 |
| `tools/builtin/bash.go` | 改签名 + 删字段 + 加越界处理 | **中**（cd 语义变了） |
| `tools/builtin/builtin.go` | 加 ctx 参数（5 处调用点）+ 解析基准 | **中**（路径解析） |
| `tools/builtin/grep.go` / `glob.go` | 加 ctx 参数 + ignore 锚点 | 中 |
| `tools/builtin/worktree.go` | 新增 enter/exit | 低（P1） |
| `agents/agent_loop.go` | 注入点 + Config 字段 + 注释 | **低**（不改 history/system） |
| `desktop/bridge/main.go` | 装配 State | 低 |
| `core/message.go` | **可能不需要改**（ephemeral 用现成的 `core.Message{Role: core.User, ...}` 范式） | 无 |
| `tools/builtin/bash_background_test.go` / `bash_cwd_test.go` | 调用点跟着改 | 低 |

### 11.9 落地顺序（PR 划分）

| PR | 内容 | 能否独立验证 |
|---|---|---|
| **PR-1** | `workdir` 包 + bash 改造 + 文件工具改造 + agent loop 注入 + 文案 + **全部测试** | ✅ 必须一起：模型认知与工具行为必须同时变，否则出现"模型被告知 cwd=X 但文件仍写老地方"的新错位 |
| **PR-2** | `worktree` 的 enter/exit + `isWorktree` 环境文案 + 分支命名对齐 | ✅ 独立 |

> **为什么 PR-1 不拆成两个 PR**：先改工具不改模型 ⇒ 模型不知道 cwd 变了，行为错位；
> 先改模型不改工具 ⇒ 模型以为 cwd 变了但工具没动，更错位。⇒ 必须同 PR。

---
