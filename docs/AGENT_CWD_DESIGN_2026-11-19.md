# AGENT_CWD_DESIGN_2026-11-19.md —— 部分恢复稿

> **本文件不是原件。** 原件（`docs/AGENT_CWD_DESIGN_2026-11-19.md`）在 2026-11-19 22:15
> 的误删中丢失，且从未提交到 git（6 个远端分支的 `docs/` 全部只有 35 个已提交文件）。
>
> 以下正文**逐字**来自误删前落盘在 `/private/tmp` 的文档分段与补丁脚本：
> `cwd_s11/s12/s14/s15/s16/s17.md`（18:48–21:14 写入）、`patch_cwd_doc.py`（§8/§9 全文）、
> `patch_cwd_v3.py`（§7.3 表、§11.2-B7 v3 版）、`patch_cwd_v3b.py`（§12.4 v3 版）。
>
> 恢复覆盖：**§8 §9 §11 §12 §14 §15 §16 §17 ≈ 2,200 行**（原文约 2,400+ 行）。
> **不可恢复**：§0–§7（除 §7.3 表）、§10、§13 —— 这些段落的正文没有在任何幸存的
> 补丁脚本里出现，脚本只按行号定位它们，不含内容。
>
> 生成时间：2026-11-20 00:02:53

---

## 恢复完整性对照

| 段 | 状态 | 来源 |
|---|---|---|
| §0 结论 | ⚠️ 仅 1 条 v3 补丁行 | `patch_cwd_v3b.py` |
| §1–§6 | ❌ 丢失 | 无 |
| §7.3 通道落地表 | ✅ v3 终版表格 | `patch_cwd_v3.py` |
| §8 开放问题／已答 | ✅ 全文 | `patch_cwd_doc.py` |
| §9 验证计划 | ✅ 全文 | `patch_cwd_doc.py` |
| §10 | ❌ 丢失 | 无 |
| §11 详细代码改动方案 | ✅ 全文 | `cwd_s11.md`（§11.2 另有 v3 改写，见附录） |
| §12 自测评估代码 | ✅ 全文（§12.4 另有 v3 改写，见附录） | `cwd_s12.md` + `patch_cwd_v3b.py` |
| §13 | ❌ 丢失 | 无 |
| §14 复审发现与修正 | ✅ 全文 | `cwd_s14.md` |
| §15 模型可见 cwd 载体（v3） | ✅ 全文 | `cwd_s15.md` |
| §16 实施记录 | ✅ 全文 | `cwd_s16.md` |
| §17 cd 频率评估 | ✅ 全文 | `cwd_s17.md` |

---

## §7.3 通道落地表（v3 终版）

**B7 工具描述改动** —— ⚠️ **已被 §15.2 取代**：不再靠"规则说明"，
而是**固定格式 + 实际当前工作目录**：

```go
// tools/builtin/bash.go
func (t *BashTool) DescribeFor(ctx context.Context) string {
	cwd := t.Cwd
	if t.wd != nil {
		cwd = t.wd.Current(workdir.KeyFrom(ctx))   // ← 按调用方分区取（主/子 agent 不同）
	}
	return t.describe(cwd)
}

func (t *BashTool) describe(cwd string) string {
	return fmt.Sprintf(
		"Executes a given bash command and returns its output.\\n"+
			"Working directory: %s\\n"+
			"Each command runs in a separate process: environment variables and other shell state do not persist between calls. "+
			"The working directory persists between calls in this session — a successful `cd` changes it for later commands "+
			"(the new directory is reported in this tool's output).",
		cwd)
}
```

> 落地需要三处配套：`tools.ContextDescriber` 接口 + `tools/engine.go:640` 优先用它 +
> `agents/agent_loop.go` 调 `toolSchemas` 时用 `workdir.WithKey(ctx, ac.workKey)` 带上分区键。详见 §15.2。

---

## 8. 开放问题（调研已回填；以下为**待拍板项**）

### 8.1 调研已答（不再开放）

- ✅ Claude Code 的 Bash **不是持久 shell**，但 cwd 跨调用保持（session 级状态；越界重置+提示；子 agent 不继承）—— §4.1
- ✅ Claude Code 的环境信息**在对话里、不在 system prompt**，每请求重算**只发差异**，载体是 `isMeta` user 消息 —— §4.4
- ✅ Claude Code 的 EnterWorktree 改的是 **session cwd + 写权限锚点 + 配置发现根**，不是进程 chdir —— §4.2
- ✅ hai 的系统提示词是**每 Run 重建一次**（`agent_loop.go:1044`），Run 内不变 —— §5.8
- ✅ hai 的动态注入通道全清单（A/B/C/D/E 五类）—— §5.9
- ✅ hai 的工具结果**直接进 `ac.Messages`**，不经 events→前端 —— §5.10
- ✅ hai 全仓**没有**任何"当前 cwd"的环境层 —— §5.12

### 8.2 待拍板（方案里给出推荐，实施前需确认）

| # | 问题 | 选项 | **推荐** |
|---|---|---|---|
| **Q1** | 文件工具的相对路径基准 | ① 当前 cwd ② workspace root（现状） | **①**（对齐 Claude Code；需同步改文案与契约测试） |
| **Q2** | cd **越界**（出 workspace root）怎么办 | ① 重置回 root + 工具结果提示（Claude Code）② 报错拒绝 ③ 允许但文件工具仍按 root | **①**（既对齐又保安全） |
| **Q3** | cwd 状态是否**进 checkpoint/replay** | ① 不进（内存态，恢复后重发完整 env 块）② 进（持久化 cwd） | **①**（零 checkpoint 风险；恢复后重发全量块即可） |
| **Q4** | rebuildLoop（切模型/切人格）后 cwd | ① 保留 ② 重置 | **①**（cwd 是会话状态，不该被切模型清掉） |
| **Q5** | 子 agent 的 cwd | ① 独立分区（现状 TaskId 分区，起点=root）② 继承父 cwd | **①**（与 Claude Code「子 agent 不继承」一致） |
| **Q6** | worktree 进入方式 | ① `create` 自动切 ② 新增 `enter`/`exit` 操作 ③ 不切，靠模型自己 cd | **②**（对齐 EnterWorktree 的 Scope 规则：只允许进本会话建的） |
| **Q7** | env 块是否**持久化**到 history | ① ephemeral（只进本次请求）② 进 history | **①**（对齐 Claude Code 的 isMeta 语义；不动 checkpoint） |
| **Q8** | 是否提供**跨会话**附加目录（`/add-dir` 等价物） | ① 本期不做 ② 做 | **①**（超出当前诉求，另开） |

---

## 9. 验证计划（总纲；详细测试代码见 §12）

必须能**证伪**的几条：

| # | 验证项 | 方法 | 对应测试 |
|---|---|---|---|
| V1 | cd 后**文件工具**跟着走 | cd 到子目录 → write_file 相对路径 → 断言落在子目录 | §12.3 `TestFileToolsFollowCwd` |
| V2 | 模型**知道** cwd 变了 | 断言下一轮 LLM 请求的 messages 里出现 `# Environment update` | §12.4 `TestEnvUpdateInjected` |
| V3 | 主/子 agent cwd 隔离不回归 | 现有 `bash_cwd_test.go` 保持绿 + 文件工具维度新增隔离测试 | §12.3 `TestCwdIsolatedPerCaller` |
| V4 | 切档位后 cwd 语义 | rebuildLoop 前后断言（按 Q4） | §12.5 |
| V5 | **checkpoint / replay 不受影响** | 沿用我们 events↔checkpoint 的逐字节等价性做法 | §12.6 |
| V6 | 越界不放大 | cd 出 root ⇒ 重置 + 提示；写文件被拒 | §12.3 `TestCwdResetOutsideRoot` |
| V7 | worktree 端到端 | 创建 → enter → 在 worktree 里改文件 → 断言落在 worktree 内 | §12.7 |
| V8 | env 块**不重复发** | 同一 cwd 连续 3 次请求 ⇒ 只发 1 次 | §12.4 `TestEnvNotResentWhenUnchanged` |
| V9 | **不破前缀缓存** | env 块不在 system 里（断言 system 消息字节不变） | §12.4 `TestEnvNotInSystemPrompt` |

---

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

---

## 12. 自测评估代码

> 分两类：**§12.1–12.7 自动化测试**（能证伪，随 PR 一起提交）、
> **§12.8 评估脚本**（用**真实会话事件日志**量化"模型还在写 `cd &&`"这件事，改造前取基线、改造后复测）。

### 12.1 `workdir/state_test.go`

```go
package workdir

import (
	"os"
	"path/filepath"
	"testing"
)

func mkSub(t *testing.T, root, name string) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCurrentFallsBackToRoot 未 cd 时当前目录 = 项目根。
func TestCurrentFallsBackToRoot(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if got := s.Current(MainKey); got != root {
		t.Fatalf("Current = %q, want %q", got, root)
	}
}

// TestSetUpdatesOnlyItsPartition 子分区互不影响（对齐 Claude Code：子 agent 不继承父级 cd）。
func TestSetUpdatesOnlyItsPartition(t *testing.T) {
	root := t.TempDir()
	sub := mkSub(t, root, "sub")
	s := New(root)

	if _, changed, err := s.Set(MainKey, sub); err != nil || !changed {
		t.Fatalf("Set: changed=%v err=%v", changed, err)
	}
	if got := s.Current(MainKey); got != sub {
		t.Fatalf("Current(main) = %q, want %q", got, sub)
	}
	if got := s.Current("task-1"); got != root {
		t.Fatalf("子分区被影响：Current(task-1) = %q, want %q", got, root)
	}
}

// TestSetOutsideRootRejected 越界被拒且**不改变**记忆（Q2 = 重置策略的前置）。
func TestSetOutsideRootRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	s := New(root)

	_, changed, err := s.Set(MainKey, outside)
	if err != ErrOutside {
		t.Fatalf("err = %v, want ErrOutside", err)
	}
	if changed {
		t.Fatal("越界不应算作 changed")
	}
	if got := s.Current(MainKey); got != root {
		t.Fatalf("越界后 cwd = %q, want %q（记忆不能被越界污染）", got, root)
	}
}

// TestResetAndForget Reset 回项目根；Forget 清空分区。
func TestResetAndForget(t *testing.T) {
	root := t.TempDir()
	sub := mkSub(t, root, "sub")
	s := New(root)
	_, _, _ = s.Set(MainKey, sub)

	if old := s.Reset(MainKey); old != sub {
		t.Fatalf("Reset 返回 %q, want %q", old, sub)
	}
	if got := s.Current(MainKey); got != root {
		t.Fatalf("Reset 后 Current = %q, want %q", got, root)
	}
	_, _, _ = s.Set("task-1", sub)
	s.Forget("task-1")
	if got := s.Current("task-1"); got != root {
		t.Fatalf("Forget 后 Current = %q, want %q", got, root)
	}
}

// TestContainsContains 判断是否在项目根之内。
func TestContainsContains(t *testing.T) {
	root := t.TempDir()
	sub := mkSub(t, root, "sub")
	outside := t.TempDir()
	s := New(root)

	if !s.Contains(root) {
		t.Error("项目根本身应算在内")
	}
	if !s.Contains(sub) {
		t.Error("子目录应算在内")
	}
	if s.Contains(outside) {
		t.Error("项目根之外不应算在内")
	}
	// 同名前缀的兄弟目录不算在内（root=/a/b，dir=/a/bc）
	sib := filepath.Dir(root) + filepath.Base(root) + "x"
	if s.Contains(sib) {
		t.Errorf("前缀相同的兄弟目录 %q 不应算在内", sib)
	}
}

// TestEnvSnapshotTracking 环境快照的记录/读取。
func TestEnvSnapshotTracking(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if _, ok := s.SentEnv(MainKey); ok {
		t.Fatal("初始不应有快照")
	}
	e := Env{WorkingDir: root, Platform: "darwin"}
	s.MarkEnvSent(MainKey, e)
	got, ok := s.SentEnv(MainKey)
	if !ok || !got.Equal(e) {
		t.Fatalf("SentEnv = %+v, ok=%v; want %+v", got, ok, e)
	}
}
```

### 12.2 `workdir/env_test.go`

```go
package workdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderFullMentionsWorkingDir(t *testing.T) {
	e := Env{WorkingDir: "/repo/src", IsGitRepo: true, Platform: "darwin", Shell: "zsh", OSVersion: "Darwin 24.5.0"}
	s := RenderFull(e)
	for _, want := range []string{
		"# Environment",
		"Primary working directory: /repo/src",
		"Is a git repository: true",
		"Platform: darwin",
		"Shell: zsh",
		"OS Version: Darwin 24.5.0",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("RenderFull 缺少 %q:\n%s", want, s)
		}
	}
}

// TestRenderFullWorktreeSentence worktree 时必须有"不要 cd 回主检出"的告诫
//（对齐 Claude Code 的原文；这条直接关系到 worktree 隔离）。
func TestRenderFullWorktreeSentence(t *testing.T) {
	s := RenderFull(Env{WorkingDir: "/repo/.claude/worktrees/w1", IsWorktree: true})
	if !strings.Contains(s, "git worktree") || !strings.Contains(s, "Do NOT") {
		t.Errorf("worktree 缺少告诫句:\n%s", s)
	}
}

func TestRenderDelta(t *testing.T) {
	old := Env{WorkingDir: "/repo"}
	cur := Env{WorkingDir: "/repo/src"}
	s := RenderDelta(old, cur)
	if !strings.Contains(s, "# Environment update") {
		t.Errorf("缺少增量标记:\n%s", s)
	}
	if !strings.Contains(s, "Primary working directory: /repo/src") {
		t.Errorf("缺少新目录:\n%s", s)
	}
}

func TestEnvEqual(t *testing.T) {
	a := Env{WorkingDir: "/x", Platform: "darwin"}
	b := Env{WorkingDir: "/x", Platform: "darwin"}
	if !a.Equal(b) {
		t.Error("相同快照应相等")
	}
	b.WorkingDir = "/y"
	if a.Equal(b) {
		t.Error("不同目录不应相等")
	}
}

// TestSnapshotDetectsWorktree .git 是文件（worktree）vs 目录（主检出）。
func TestSnapshotDetectsWorktree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(root)
	if e := s.Snapshot(MainKey); !e.IsGitRepo || e.IsWorktree {
		t.Fatalf("主检出判定错：IsGitRepo=%v IsWorktree=%v", e.IsGitRepo, e.IsWorktree)
	}

	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2 := New(wt)
	if e := s2.Snapshot(MainKey); !e.IsGitRepo || !e.IsWorktree {
		t.Fatalf("worktree 判定错：IsGitRepo=%v IsWorktree=%v", e.IsGitRepo, e.IsWorktree)
	}
}
```

### 12.3 `tools/builtin/cwd_follow_test.go`（V1 / V3 / V6）

```go
package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/workdir"
)

// cwdCtx 造一个带 ToolContext 的 ctx（taskId 区分调用方）。
func cwdCtx(runID, taskID string) context.Context {
	return events.WithToolContextForRunTask(context.Background(), runID, runID, taskID, 0, nil, nil, nil)
}

// findTool 按名字找工具。
func findTool(t *testing.T, ts []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, x := range ts {
		if x.Name() == name {
			return x
		}
	}
	t.Fatalf("找不到工具 %q", name)
	return nil
}

// TestFileToolsFollowCwd cd 之后，文件工具的相对路径落在**新目录**（V1）。
//
// 这是本次改造的核心断言：今天文件工具锚在 workspace root，cd 之后 write 会写错地方。
func TestFileToolsFollowCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	ft := NewFileTools(wd)
	ts := ft.Tools()
	bash := findTool(t, ts, "bash")
	write := findTool(t, ts, "write_file")
	ctx := cwdCtx("run-1", "")

	// 未 cd 时：相对路径落在项目根（**旧行为逐字保留**）
	if _, err := write.Call(ctx, "write_file", `{"path":"a.txt","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("未 cd 时应落在项目根：%v", err)
	}

	// cd 之后：相对路径落在新目录
	if _, err := bash.Call(ctx, "bash", `{"command":"cd `+sub+`"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Call(ctx, "write_file", `{"path":"b.txt","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sub, "b.txt")); err != nil {
		t.Fatalf("cd 后应落在 %s：%v", sub, err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err == nil {
		t.Fatal("cd 后不应再落在项目根（否则就是静默错位）")
	}
}

// TestCwdIsolatedPerCaller 子 agent 的 cd 不影响主 agent（V3，文件工具维度）。
func TestCwdIsolatedPerCaller(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	ft := NewFileTools(wd)
	ts := ft.Tools()
	bash := findTool(t, ts, "bash")
	write := findTool(t, ts, "write_file")

	mainCtx := cwdCtx("run-main", "")
	subCtx := cwdCtx("run-sub", "task-123")

	if _, err := bash.Call(subCtx, "bash", `{"command":"cd `+sub+`"}`); err != nil {
		t.Fatal(err)
	}
	// 主 agent 仍然在项目根
	if _, err := write.Call(mainCtx, "write_file", `{"path":"main.txt","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "main.txt")); err != nil {
		t.Fatalf("主 agent 的写应落在项目根：%v", err)
	}
	// 子 agent 在新目录
	if _, err := write.Call(subCtx, "write_file", `{"path":"sub.txt","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sub, "sub.txt")); err != nil {
		t.Fatalf("子 agent 的写应落在 %s：%v", sub, err)
	}
}

// TestCwdResetOutsideRoot cd 越界 ⇒ 重置回项目根 + 结果里明确提示（V6 / Q2）。
func TestCwdResetOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	wd := workdir.New(root)
	ft := NewFileTools(wd)
	ts := ft.Tools()
	bash := findTool(t, ts, "bash")
	ctx := cwdCtx("run-1", "")

	out, err := bash.Call(ctx, "bash", `{"command":"cd `+outside+`"}`)
	if err != nil {
		t.Fatalf("越界 cd 不应报错（对齐 Claude Code：重置而非拒绝）：%v", err)
	}
	if !strings.Contains(out, "Shell cwd was reset to") {
		t.Fatalf("结果里应有重置提示：\n%s", out)
	}
	if got := wd.Current(""); got != root {
		t.Fatalf("越界后 cwd = %q, want %q", got, root)
	}
}

// TestResolveStillRejectsEscapeWorkspace 相对路径**不得逃出项目根**（R3 安全边界不变）。
func TestResolveStillRejectsEscapeWorkspace(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	ft := NewFileTools(wd)
	write := findTool(t, ft.Tools(), "write_file")
	ctx := cwdCtx("run-1", "")

	// cwd = root/sub 时，../../etc/passwd 必须被拒
	if _, _, err := wd.Set("", sub); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Call(ctx, "write_file", `{"path":"../../etc/passwd","content":"x"}`); err == nil {
		t.Fatal("逃出项目根的相对路径必须被拒")
	}
}
```

### 12.4 `agents/cwd_env_test.go`（V2 / V8 / V9）

```go
package agents

import (
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/workdir"
)

// reqHasEnvText 判断请求里有没有含指定文本的 user 消息。
func reqHasEnvText(req *provider.StreamRequest, needle string) bool {
	for _, m := range req.Messages {
		if m.Role != core.User {
			continue
		}
		for _, c := range m.Content {
			if strings.Contains(c.Content, needle) {
				return true
			}
		}
	}
	return false
}

// reqSystemText 取请求里 system 消息的文本。
func reqSystemText(req *provider.StreamRequest) string {
	for _, m := range req.Messages {
		if m.Role == core.System {
			for _, c := range m.Content {
				return c.Content
			}
		}
	}
	return ""
}

// TestEnvBlockInjectedAndNotInSystem 环境块进**对话**、不进 system（R1 + R2）。
func TestEnvBlockInjectedAndNotInSystem(t *testing.T) {
	root := t.TempDir()
	wd := workdir.New(root)
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderStopRound("ok"),
	}}

	loop := newTestLoop(t, p, WithWorkDirState(wd))
	if _, err := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage("hi")}, nil, &RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) == 0 {
		t.Fatal("没有捕获到请求")
	}
	req := p.requests[0]
	if !reqHasEnvText(req, "Primary working directory: "+root) {
		t.Fatalf("环境块未注入对话：\n%+v", req.Messages)
	}
	if strings.Contains(reqSystemText(req), "Primary working directory") {
		t.Fatal("环境块不许进 system（会破 provider 前缀缓存）")
	}
}

// TestEnvNotResentWhenUnchanged cwd 没变 ⇒ 不重复发（省 token）。
func TestEnvNotResentWhenUnchanged(t *testing.T) {
	root := t.TempDir()
	wd := workdir.New(root)
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderStopRound("ok"),
		reminderStopRound("ok"),
	}}
	loop := newTestLoop(t, p, WithWorkDirState(wd))
	ctx := context.Background()
	if _, err := loop.RunStream(ctx, []core.Message{core.NewUserMessage("a")}, nil, &RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunStream(ctx, []core.Message{core.NewUserMessage("b")}, nil, &RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) < 2 {
		t.Fatalf("请求数 = %d, want >= 2", len(p.requests))
	}
	if !reqHasEnvText(p.requests[0], "Primary working directory") {
		t.Fatal("首轮应有完整环境块")
	}
	if reqHasEnvText(p.requests[1], "Primary working directory") {
		t.Fatal("cwd 未变时不该重复发环境块")
	}
}

// TestEnvUpdateAfterCwd cwd 变了 ⇒ 发增量块。
func TestEnvUpdateAfterCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	// 模拟 bash 的 cd 记忆
	if _, _, err := wd.Set("", sub); err != nil {
		t.Fatal(err)
	}
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderStopRound("ok"),
	}}
	loop := newTestLoop(t, p, WithWorkDirState(wd))
	if _, err := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage("hi")}, nil, &RunOptions{}); err != nil {
		t.Fatal(err)
	}
	req := p.requests[0]
	if !reqHasEnvText(req, "Primary working directory: "+sub) {
		t.Fatalf("未反映新 cwd：\n%+v", req.Messages)
	}
}
```

> ⚠️ `newTestLoop` 是**待补的测试夹具**（照 `agents/agent_loop_test.go` 里既有的 loop 构造改写即可），
> `reminderStopRound` / `captureRoundsProvider` 复用 `agents/agent_loop_test.go` 的既有定义。

### 12.5 V4：切档位（rebuildLoop）后 cwd 保留

```go
// desktop/bridge/cwd_rebuild_test.go
package bridge

// TestCwdSurvivesLoopRebuild 切模型/切人格（rebuildLoop）后 cwd 仍在（Q4 = 保留）。
//
// 回归点：cwd 记忆原先存在 BashTool 实例上，而实例每次 FileTools.Tools() 都新建
// ⇒ rebuildLoop 会把 cd 记忆清空。
func TestCwdSurvivesLoopRebuild(t *testing.T) {
	// 1. 建会话 → 拿 bs
	// 2. bash cd 到 sub
	// 3. bs.rebuildLoop(...)（换模型/换人格）
	// 4. 断言 bs 的 workdir.Current("") == sub
	// 5. 断言下一次 bash 命令的 cmd.Dir == sub
}
```

> 骨架：具体调用待实现时按 `desktop/bridge/main.go:6668` `rebuildLoop` 的实际签名补全。

### 12.6 V5：checkpoint / replay 不受影响

沿用我们做 events↔checkpoint 等价性时的做法（参考 `desktop/bridge/events_toolcalls_equivalence_test.go`）：

```go
// desktop/bridge/cwd_checkpoint_equivalence_test.go
package bridge

// TestCwdChangeDoesNotAlterCheckpoint cwd 变化**不改变** checkpoint 字节。
//
// 原理：cwd 状态是**内存态**（不进 ac.Messages ⇒ 不进 journal ⇒ 不进 checkpoint），
// 环境块是 ephemeral（不进 ac.Messages）⇒ 两条路产出的 checkpoint 必须逐字节相同。
func TestCwdChangeDoesNotAlterCheckpoint(t *testing.T) {
	// 1. 造一个会话，跑一轮：bash cd + 若干工具
	// 2. 记录 LoadRecordForView / MarshalBodyWithSpans 的字节
	// 3. 同样的事件流，但中间多了/少了 cd
	// 4. 断言两边字节相同
}
```

### 12.7 V7：worktree 端到端（P1）

```go
// tools/builtin/worktree_enter_test.go
package builtin

// TestWorktreeEnterSwitchesCwd enter 把会话 cwd 切到 worktree；
// exit 还原；进入非本会话创建的 worktree 被拒（对齐 Claude Code 的 Scope 规则）。
func TestWorktreeEnterSwitchesCwd(t *testing.T) {
	root := t.TempDir()
	// git init + commit（保证能建 worktree）
	// create → 记入 created
	// enter(path) → wd.Current("") == path
	// write_file("x.txt") → 落在 worktree 内
	// exit() → wd.Current("") == root
	// enter(外部目录) → 报错
}
```

### 12.8 评估脚本：`scripts/eval_cwd_redundancy.py`

> **目的**：用**真实会话事件日志**量化用户反馈的那个现象 ——「模型总是在写 `cd xxx && xxx`」。
> 改造前取基线，改造后复测，看指标是否下降。**这是唯一能证明"用户体感改善了"的量化证据。**

```python
#!/usr/bin/env python3
"""评估 agent 的「重复 cd」冗余度。

数据源：~/.go-code/events/<ws>/<sid>.jsonl（明文或 .gz 封存）。
指标：
  M1 cd_prefix_ratio   以 `cd ` 开头的 bash 命令占比        （越低越好）
  M2 cd_compound_ratio `cd X && ...` 形式的 bash 命令占比    （越低越好）
  M3 redundant_cd      「重复 cd 到同一目录」的次数/占比     （越低越好）

用法：
  python3 scripts/eval_cwd_redundancy.py                    # 全量
  python3 scripts/eval_cwd_redundancy.py --limit 200        # 只看最大的 200 个会话
  python3 scripts/eval_cwd_redundancy.py --root /path/ev    # 指定 events 根目录
  python3 scripts/eval_cwd_redundancy.py --baseline out.json --compare new.json
"""
import argparse
import collections
import glob
import gzip
import json
import os
import re
import sys

CD_RE = re.compile(r'^\s*cd\s+(.+)$')
TOOL_START = "tool_run_start"


def open_event_file(path):
    if path.endswith(".gz"):
        return gzip.open(path, "rt", encoding="utf-8", errors="replace")
    return open(path, "r", encoding="utf-8", errors="replace")


def session_files(root):
    for p in glob.glob(os.path.join(root, "*", "*.jsonl")):
        yield p
    for p in glob.glob(os.path.join(root, "*", "*.jsonl.gz")):
        yield p


def analyze_session(path):
    """返回该会话的 (bash_total, cd_prefix, cd_compound, redundant_cd, targets)。"""
    bash_total = 0
    cd_prefix = 0
    cd_compound = 0
    redundant = 0
    last_target = None
    # 按 (session, run) 分组跟踪"上一次 cd 的目标"——run 变了就是新的 agent 上下文
    per_run = {}
    for line in open_event_file(path):
        try:
            j = json.loads(line)
        except Exception:
            continue
        if j.get("event_type") != TOOL_START:
            continue
        if j.get("name") != "bash":
            continue
        args = j.get("arguments") or ""
        try:
            cmd = (json.loads(args) or {}).get("command") or ""
        except Exception:
            cmd = ""
        if not cmd.strip():
            continue
        bash_total += 1
        m = CD_RE.match(cmd)
        if not m:
            continue
        cd_prefix += 1
        rest = m.group(1)
        target = rest.split("&&")[0].strip()
        if "&&" in rest:
            cd_compound += 1
        run = j.get("run_id") or ""
        if per_run.get(run) == target:
            redundant += 1
        per_run[run] = target
    return bash_total, cd_prefix, cd_compound, redundant


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=os.path.expanduser("~/.go-code/events"))
    ap.add_argument("--limit", type=int, default=0, help="只看最大的 N 个会话（0=全量）")
    ap.add_argument("--baseline", help="基线 JSON（--compare 时用）")
    ap.add_argument("--compare", help="对照 JSON")
    ap.add_argument("--out", help="把本次结果写成 JSON")
    args = ap.parse_args()

    files = list(session_files(args.root))
    if args.limit:
        files.sort(key=lambda p: (os.path.getsize(p.replace(".gz", "")) if os.path.exists(p.replace(".gz", "")) else 0), reverse=True)
        files = files[: args.limit]

    tot = collections.Counter()
    per_session = []
    for p in files:
        try:
            b, c, cc, r = analyze_session(p)
        except Exception as e:
            print("skip %s: %s" % (p, e), file=sys.stderr)
            continue
        if b == 0:
            continue
        tot["bash"] += b
        tot["cd_prefix"] += c
        tot["cd_compound"] += cc
        tot["redundant"] += r
        per_session.append((c / b, b, c, cc, r, os.path.basename(p)))

    result = {
        "sessions": len(per_session),
        "bash_total": tot["bash"],
        "cd_prefix": tot["cd_prefix"],
        "cd_compound": tot["cd_compound"],
        "redundant_cd": tot["redundant"],
        "cd_prefix_ratio": (tot["cd_prefix"] / tot["bash"]) if tot["bash"] else 0.0,
        "cd_compound_ratio": (tot["cd_compound"] / tot["bash"]) if tot["bash"] else 0.0,
        "redundant_cd_ratio": (tot["redundant"] / tot["bash"]) if tot["bash"] else 0.0,
    }

    print("会话数        : %d" % result["sessions"])
    print("bash 命令总数 : %d" % result["bash_total"])
    print("M1 cd 前缀占比 : %.2f%%  (%d)" % (100 * result["cd_prefix_ratio"], result["cd_prefix"]))
    print("M2 cd&& 占比   : %.2f%%  (%d)" % (100 * result["cd_compound_ratio"], result["cd_compound"]))
    print("M3 重复 cd     : %d  (%.2f%%)" % (result["redundant_cd"], 100 * result["redundant_cd_ratio"]))

    per_session.sort(reverse=True)
    if per_session:
        print("\ncd 前缀占比最高的 10 个会话：")
        for ratio, b, c, cc, r, name in per_session[:10]:
            print("  %5.1f%%  bash=%-5d cd=%-4d 冗余=%-3d %s" % (100 * ratio, b, c, r, name[:48]))

    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            json.dump(result, f, ensure_ascii=False, indent=2)
        print("\n结果已写入 %s" % args.out)

    if args.baseline and args.compare:
        with open(args.baseline, encoding="utf-8") as f:
            base = json.load(f)
        with open(args.compare, encoding="utf-8") as f:
            cur = json.load(f)
        print("\n=== 基线 vs 现在 ===")
        for k in ("cd_prefix_ratio", "cd_compound_ratio", "redundant_cd_ratio"):
            b, c = base.get(k, 0), cur.get(k, 0)
            delta = (c - b) * 100
            arrow = "↓ 好" if delta < 0 else ("↑ 差" if delta > 0 else "= 不变")
            print("  %-22s %.2f%% → %.2f%%  (%+.2f 个百分点) %s" % (k, 100 * b, 100 * c, delta, arrow))


if __name__ == "__main__":
    main()
```

**跑法**：

```bash
# 改造前基线（现在就能跑，events 里有真实历史）
python3 scripts/eval_cwd_redundancy.py --out /tmp/cwd_baseline.json

# 改造后（用新版本跑一段真实会话后）
python3 scripts/eval_cwd_redundancy.py --out /tmp/cwd_after.json
python3 scripts/eval_cwd_redundancy.py --baseline /tmp/cwd_baseline.json --compare /tmp/cwd_after.json
```

### 12.9 人工验收清单（真实 bridge E2E）

| # | 步骤 | 期望 |
|---|---|---|
| E1 | 起一个会话，让 agent `cd` 到子目录 | 后续 bash 的 pwd 是新目录 |
| E2 | 同上，然后让它 `write_file("t.txt")` | 文件落在**新目录** |
| E3 | 看下一轮 LLM 请求的 messages | 末尾有一条 `# Environment` / `# Environment update`，含新目录 |
| E4 | 看 system 消息 | **不含** cwd（字节不变 ⇒ 前缀缓存不破） |
| E5 | 故意 `cd /tmp`（越界） | 工具结果含 "Shell cwd was reset to ..."；后续仍在项目根 |
| E6 | 子 agent 里 `cd` | 主 agent 的目录不受影响 |
| E7 | 切模型（rebuildLoop） | cwd **不丢** |
| E8 | 关掉应用重开会话 | cwd 回到项目根（内存态，符合 Q3），但模型仍被告知当前目录 |
| E9 | P1：worktree create → enter | cwd 切到 worktree；文件写在 worktree 内；环境块含 "git worktree" 告诫 |

---

---

---

## 14. 复审发现与修正（v2）—— 权威修正清单

> 2026-11-19 第二轮自审：**把方案里的每个假设拿代码验了一遍**，并用真实事件日志量化。
> 结论：**方案方向正确，但有 12 处遗漏/错误**，其中 3 处是阻断级（不改则方案不成立）。
> 本节**优先于 §11 的对应条目**；§11 里带 ⚠️ 标记的条目以本节为准。

### 14.0 复审方法

| 手段 | 覆盖 |
|---|---|
| 逐条 grep 验证方案里的每个"调用点"假设 | `NewFileTools` / `NewBashTool` / `WithWorkingDir` / `f.root` / `Config.WorkingDir` |
| **Go 实测正则语义**（不是 Python 猜的） | `splitCd` 的 `^cd\s+(.+)$` 对多行命令的行为 |
| 真实事件日志量化 | 494,060 条 bash 命令的全量统计 |
| 读实现确认调用顺序 | `BashTool.Call` 里 cd 拆分与后台识别的先后 |

### 14.1 ⛔ R1（阻断级）：`splitCd` 正则漏掉 **23.55%** 的 cd 命令

**Go 实测**（`go run` 直接验，勿用 Python 的 `re` 猜）：

```go
re := regexp.MustCompile(`^cd\s+(.+)$`)
// "cd /a && ls"                            => match=true
// "cd /a && python3 - <<'EOF'\nprint(1)\nEOF" => match=false   ← heredoc
// "cd /a && ls\n"                          => match=false   ← 尾换行
```

Go 的 `$`（无 `(?m)`）匹配**文本末尾**，`.` **不匹配 `\n`** ⇒ **任何多行命令都匹配不上**。

**真实数据量化**（494,060 条 bash）：

| 指标 | 数量 | 占比 |
|---|---|---|
| bash 命令总数 | 494,060 | |
| **真·以 `cd` 开头** | 467,289 | **94.58%** |
| `splitCd` **实际匹配上** | 357,229 | 72.30% |
| **★以 cd 开头但 splitCd 漏掉** | **110,060** | **22.28% of bash / 23.55% of cd** |
| `cd` 出现在命令**中间**（非开头） | 6,846 | 1.39% |

漏掉的典型形态（**heredoc**）：
```
cd /Users/admin/workspace/hai-super-assistant && python3 - <<'EOF'
import re
src=open('docs/design/04-implemen...
```

⇒ **现有的 cd 记忆只对 76.4% 的 cd 命令生效**；另外 23.55% 里**会话 cwd 静默不动**。
⇒ **模型重复 `cd` 在这部分是"有理有据"的** —— 这也部分解释了 §13.3 里 11-17 之后占比不降反升。

**修正（前置必修，PR-1 的第 0 步）**：

```go
// cdPrefixRe 匹配「命令以 cd <目标> 开头」。
//
// 2026-11-19 修正：**必须能匹配多行命令**（heredoc）。
// 原式 `^cd\s+(.+)$` 在 Go 里 `.` 不匹配 \n、$ 只在文本末尾 ⇒
// 「cd X && python3 - <<'EOF'\n…\nEOF」这类命令**匹配不上**
// ⇒ cd 记忆对它静默失效（实测：真实数据里 23.55% 的 cd 命令是这种形态）。
// 现在用 (?s) 让 . 跨行、只取**第一行**的 cd 目标（后续正文原样保留）。
var cdPrefixRe = regexp.MustCompile(`(?s)^cd\s+([^\n]+)`)
```
> `(?s)` + `([^\n]+)` ⇒ 只吃第一行 ⇒ heredoc 正文不受影响 ✓
> ⚠️ 尾换行（`"cd /a && ls\n"`）现在也能匹配 ✓（原来不行）

**配套**：目标目录含空格/引号的解析（原 `splitCd` 的引号处理）要在这条新正则下**重新验证**（见 §14.11 测试项）。

### 14.2 ⛔ R2（阻断级）：我自己引入了「双源」——`Config.WorkingDir` 与 `Config.WorkDir`

§11.4 让我**新增** `Config.WorkDir *workdir.State`，而 `Config.WorkingDir string` 还在（`agent_loop.go:123`）。
两者都是"工作区根" ⇒ **违反我自己定的 D6（单一真值源）**。

`Config.WorkingDir` 的**全部 5 个使用点**（grep 确认）：

| 位置 | 用途 | 该用哪个 |
|---|---|---|
| `agents/agent_loop.go:995` | `newArtifactCollector(a.cfg.WorkingDir)` | **root**（artifact 收集按工作区根） |
| `agents/agent_loop.go:1714/1718` | 系统提示词 `## Current Workspace` | **root**（语义是工作区根，不是当前 cwd） |
| `agents/agent_loop.go:2003` | `@引用` 展开 `expandUserRefsInMessages(..., a.cfg.WorkingDir, ...)` | **root** |
| `agents/agent_loop.go:2262` | 压缩输入前的 `@引用` 展开 | **root** |

**修正**：**不加第二个字段**，改为 ——

```go
// Config
WorkingDir string          // 保留：工作区根（Current Workspace 环境层 / artifact / @引用）
WorkDir    *workdir.State  // 新增：cwd 状态（root 与 WorkingDir 一致；cur 随 cd 变）
```
⇒ 两者**必须一致**（`WorkDir.Root() == WorkingDir`），由**装配处一次性保证**（同一处同时填两个），
并在 `NewAgentLoop` 里**断言**（不一致直接 panic，fail fast 优于静默漂移）：

```go
if c.WorkDir != nil && c.WorkDir.Root() != c.WorkingDir {
    panic(fmt.Sprintf("agent: WorkingDir(%q) 与 WorkDir.Root(%q) 不一致", c.WorkingDir, c.WorkDir.Root()))
}
```
> 或者更彻底：**删掉 `Config.WorkingDir`**，全部改读 `WorkDir.Root()`（5 处）——
> 但 `WorkingDir` 是**公开 SDK API**（`agents.Config` 被外部用），删了是破坏性变更。
> ⇒ **推荐**：保留 `WorkingDir`，加断言。装配点只有一个（`newSessionEngineAt`），保证一致。

### 14.3 ⛔ R3（阻断级）：漏掉的构造点（我原方案只覆盖了一半）

grep `NewFileTools(` / `WithWorkingDir(` 的**全部**结果：

| # | 位置 | 是什么 | 方案里 | **修正** |
|---|---|---|---|---|
| 1 | `desktop/bridge/main.go:6295` | 主会话引擎 | ✅ 已覆盖 | 传会话的 `wd` |
| 2 | `desktop/bridge/main.go:6084` | explore 引擎（独立 FileTools） | ✅ 已覆盖 | 传**同一个** `wd`（explore 走自己的分区键） |
| 3 | **`tools/builtin/run_python.go:167`** | **run_python 自带的 FileTools**（+ `:165 Cwd: workspace`） | ❌ **漏了** | 传 `wd`；`RunPythonTool.Cwd` 改为**每轮现取** `wd.Current(key)` |
| 4 | **`desktop/bridge/hub_workspace.go:648`** | **hub 跨工作区工具**：`NewFileTools(filepath.Dir(paths[0]))` —— **另一个 root（按设计）** | ❌ **漏了** | ⚠️ **必须给它一个独立的 State**（root = 那个目录）⇒ **绝不能跟随会话 cwd** |
| 5 | **`desktop/bridge/main.go:6120`** | **第 4 个 loop builder**（我原方案只列了 6009/6591） | ❌ **漏了** | 传 `wd` |
| 6 | **`desktop/bridge/hub_session.go:287`** | hub agent 的 loop（`ws.path` = hubAgentHome） | ❌ **漏了** | hub 有自己的 root ⇒ **独立 State**（root = hubAgentHome） |

⇒ **教训**：这类"装配点"必须用 grep 全量枚举，不能靠推理。方案里应附一张**装配点清单表**（§14.9）。

### 14.4 ⚠️ R4（高危）：gitignore 匹配器**必须锚在 workspace root**，不能跟 cwd

我原方案 §11.3 F10 写"ignore matcher 锚点改为解析出的 base" —— **这是错的，会改掉忽略语义**。

`tools/builtin/ignore.go:163/185/187` 显示匹配器会**从目标目录向上回溯**到 `m.root` 找 `.gitignore` 文件：
```go
rel, err := filepath.Rel(m.root, abs)          // :163
if dir == m.root { ... } else if parent := filepath.Dir(dir); parent != dir && strings.HasPrefix(dir, m.root) { ... }
```
⇒ 若 `m.root` 变成 cwd（`root/src`），**回溯范围被截断** ⇒ 根目录的 `.gitignore` 找不到
⇒ `node_modules/`、`dist/` 等**不再被忽略** ⇒ glob/grep 结果暴涨、模型被垃圾淹没。

**修正**：
| 锚点 | 取值 | 理由 |
|---|---|---|
| **路径解析基准** | **当前 cwd** | 模型心智模型（相对 cwd） |
| **越界安全边界** | **workspace root** | 既有不变量 |
| **gitignore 匹配器 root** | **workspace root**（不变） | **gitignore 是仓库级语义**，不随 cd 变 |

⇒ `grep.go:125` / `glob.go:118` 的 `newIgnoreMatcher(t.root, ...)` **保持 `t.root` 不变**；
`ignore.go` 的 `m.root` 语义不变 ⇒ **只改 `resolveFor` 的解析基准**。

### 14.5 ⚠️ R5（高危）：后台命令的 `cd` 会改掉会话 cwd（顺序错）

`tools/builtin/bash.go:Call` 的实际顺序（读实现确认）：

```
:250  cmd := a.Command; cwd := t.currentCwd(ctx)
:253  if target, rest, ok := t.splitCd(...); ok {        ← ① cd 拆分 + setCurrentCwd 在这里
:        t.setCurrentCwd(ctx, target)                       ← ★会话 cwd 已被改
      }
:274  background := a.RunInBackground
:276  if stripped, ok := splitTrailingBackground(cmd); ok { cmd = stripped; background = true }   ← ② 后台识别在后面
:296  if background { return t.startBackground(ctx, sbx, cmd, cwd) }
```

⇒ `cd X && long_job &` ⇒ **会话 cwd 被改到 X**（后台进程在独立进程组里跑，cd 只是它自己那个 shell 的事）。
⇒ **Claude Code 不会这样**（它读的是命令的**结束 cwd**，后台命令没有结束 cwd ⇒ 不改会话 cwd）。

**修正**：把 **② 后台识别提到 ① 之前**，并让**后台命令不更新会话 cwd**：
```go
// 后台意图识别**先于** cd 拆分（否则后台命令的 cd 会改掉会话 cwd ——
// 后台进程在独立进程组里跑，那个 cd 只是它自己 shell 的事）。
background := a.RunInBackground
if !background {
    if stripped, ok := splitTrailingBackground(a.Command); ok {
        cmd, background = stripped, true
    }
}
...
if target, rest, ok := t.splitCd(cmd, cwd); ok && !background {
    ...setCurrentCwd...
}
```
> **现实影响很小**：实测真·后台命令（以 `&` 结尾）**只有 3 条 / 494,060**（0.00%）。
> 但顺序错就是错，且改起来很便宜 ⇒ 修。

### 14.6 ⚠️ R6（高危）：同步子运行与主 agent **共用 cwd 分区**

`subagent/subagent.go:242-264` `buildOptions` —— **不设 `opts.TaskId`**：
```go
func (t *Tool) buildOptions(tc *events.ToolContext, childDepth int, name string) *agents.RunOptions {
	opts := &agents.RunOptions{Depth: childDepth, Name: name}
	if tc != nil {
		opts.ParentRunId = tc.RunId
		opts.ParentTaskId = tc.TaskId
		...
	}
	...
}
```
⇒ `runSync` 路径（`subagent.go:266-284`）的子运行 `opts.TaskId == ""`
⇒ `agents/agent_loop.go:2643` `taskID = opts.TaskId` = `""`
⇒ `events.ToolContext.TaskId` = `""` ⇒ `cwdKey(ctx)` = `""` ⇒ **与主 agent 同一个分区**
⇒ **同步子 agent 的 `cd` 会改掉主 agent 的 cwd**。

> 桌面 bridge 里 `Background: true` 恒成立（`subagent.go:213` / `main.go:6389`）⇒ 实际走不到 sync 路径；
> 但 **SDK / 库用法**会踩到 ⇒ 不能靠"反正走不到"。

**修正**：分区键必须"**每个 agent 唯一**"，不能只是"非空"。

```go
// agents/agent_loop.go（构造 ToolContext 处，约 :2643）
taskID := ""
if opts != nil {
    taskID = opts.TaskId
}
if taskID == "" && ac.Depth > 0 {
    // **同步子运行**（subagent.runSync 不设 opts.TaskId）必须有自己的分区，
    // 否则键为 "" = 主 agent 的分区 ⇒ 子 agent 的 cd 会改掉主 agent 的 cwd。
    // 键用 RunId+Depth：RunId 每次 Run 唯一（够区分不同同步子运行），Depth 区分嵌套层。
    taskID = fmt.Sprintf("sync:%s:%d", ac.RunId, ac.Depth)
}
```
> 后台子 agent 的键仍是 `opts.TaskId`（`task-*`，跨轮稳定 ✓）；
> 主 agent 的键仍是 `""`（跨轮稳定 ✓）；
> 同步子 agent 的键 `sync:<runid>:<depth>`（**该子运行内稳定** ✓，跨轮也稳定，因为 RunId 固定）。

### 14.7 ⚠️ R7（中）：glob / grep 给模型看的相对路径基准

`tools/builtin/glob.go:138` 与 `tools/builtin/grep_pipeline.go:225`：
```go
rel, err := filepath.Rel(t.root, p)     // 给模型看的相对路径
```
⇒ cwd 变了以后：cwd=root/src 时 glob 到 `root/src/a.go`，显示 `src/a.go`（相对 root）
⇒ 模型看到的是 `src/a.go`，但它以为自己在 `src/` ⇒ **心智模型与显示不一致**。

**修正（推荐）**：**显示基准改为 cwd**（相对当前目录），与模型心智一致：
```go
rel, err := filepath.Rel(t.cwd(ctx), p)   // 显示相对当前工作目录
```
> 风险：显示路径变了 ⇒ 前端若有依赖该字符串的地方会受影响。
> ⇒ **实施前 grep 前端是否消费 grep/glob 的输出路径**（`desktop/app/src`）⇒ 若消费，则保留 root 基准并只改解析。

### 14.8 ⚠️ R8/R9（中）：环境快照的两处实现错误

**R8：`IsGitRepo` / `IsWorktree` 在子目录里判错**
我原方案的 `os.Stat(dir/.git)` ⇒ cwd=root/src 时 **`root/src/.git` 不存在** ⇒ 误报 "Is a git repository: false"。
⇒ **必须向上回溯**：
```go
func isGitRepo(dir string) bool {
	// 向上回溯找 .git（cwd 常是仓库的子目录）
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d { return false }
		d = parent
	}
}
func isWorktree(dir string) bool {
	// worktree 的 .git 是**文件**（gitdir: 指针），主检出是目录
	for d := dir; ; {
		st, err := os.Stat(filepath.Join(d, ".git"))
		if err == nil {
			if !st.IsDir() { return true }   // 文件 ⇒ worktree
			return false                     // 目录 ⇒ 主检出（再往上没有意义）
		}
		parent := filepath.Dir(d)
		if parent == d { return false }
		d = parent
	}
}
```
> 或用 `git -C <dir> rev-parse --is-inside-work-tree` / `--git-dir`（更准，但每次请求 spawn 进程 ⇒ 见 R9 的缓存）。

**R9：`detectOSVersion` 每次请求 spawn 一个 `uname`**
每个 LLM 请求一次 ⇒ 累计可观的进程数 ⇒ **必须缓存**：
```go
var osVersionOnce sync.Once
var osVersionStr string

func detectOSVersion() string {
	osVersionOnce.Do(func() {
		if out, err := exec.Command("uname", "-sr").Output(); err == nil {
			osVersionStr = strings.TrimSpace(string(out))
		} else {
			osVersionStr = runtime.GOOS
		}
	})
	return osVersionStr
}
```
> 更省：`syscall.Uname`（零进程）。macOS/Linux 均可 ⇒ 推荐 `syscall.Uname`。
> `detectShell` 读 `os.Getenv("SHELL")` ⇒ 无成本 ✓。

### 14.9 📋 装配点清单（实施时逐条核对，勿再漏）

| # | 位置 | 根 | 该用哪个 State | 状态 |
|---|---|---|---|---|
| 1 | `desktop/bridge/main.go:6295` `NewFileTools` | 会话工作区 | **会话 wd** | ✅ 方案已覆盖 |
| 2 | `desktop/bridge/main.go:6084` explore 引擎 | 会话工作区 | **会话 wd**（独立分区键） | ✅ 方案已覆盖 |
| 3 | `tools/builtin/run_python.go:167` `NewFileTools` + `:165 Cwd` | 会话工作区 | **会话 wd** | 🆕 **本轮补** |
| 4 | `desktop/bridge/hub_workspace.go:648` `NewFileTools` | `Dir(paths[0])`（跨工作区） | **独立 State**（不跟会话 cwd） | 🆕 **本轮补** |
| 5 | `desktop/bridge/main.go:6120` loop builder | 会话工作区 | **会话 wd** | 🆕 **本轮补** |
| 6 | `desktop/bridge/hub_session.go:287` hub loop | `hubAgentHome` | **独立 State** | 🆕 **本轮补** |
| 7 | `desktop/bridge/main.go:6403` `NewGitWorktreeTool` | 会话工作区 | **会话 wd** | ✅ 方案已覆盖（P1） |
| 8 | `tools/builtin/builtin.go:140` `NewBashTool(f.root,…)` | 随 FileTools | 随 #1–#4 | 🆕 **本轮补**（原来是 `f.root` 硬编码） |

**loop builder 的 `WithWorkingDir` 全部 4 处**：`main.go:6009`（spawn）/ `:6120`（?）/ `:6591`（主）/ `hub_session.go:287`（hub）
⇒ **4 个都要拿到对应的 State**。

### 14.10 📋 其余修正（较小但要记）

| # | 问题 | 修正 |
|---|---|---|
| **R10** | `Config.WorkingDir` 的 artifact 收集（995）/ `@引用`（2003/2262）语义 | **保持用 root**，不跟 cwd（`@引用` 展开按工作区根最直观） |
| **R11** | 恢复会话后：cwd 是内存态 ⇒ 回到 root，但**历史消息里仍有旧 cwd 语境** | 文档写明"**环境块是唯一权威**"；可选增强：cwd 落 **sidecar**（不进 checkpoint）⇒ 见 §14.12 |
| **R12** | `cd` 的边界语法 | `cd -`（上一目录）、`cd ~user`、`cd`（无参 → HOME）、`pushd/popd`、`cd X; cmd`（分号）**都不进记忆**；实测 `cd` 出现在命令**中间**的占 1.39% ⇒ 明确写进工具描述，让模型知道"只有开头的 cd 会被记住" |
| **R13** | `splitCd` 是**前缀启发式**；Claude Code 用的是**命令结束时的真实 cwd** | 可选增强：让 shell 在 fd3 上回吐最终 `$PWD`（`sh -c '<cmd>; rc=$?; printf "%s" "$PWD" >&3; exit $rc'` + Go `ExtraFiles`）⇒ 覆盖 `cd X; cmd`、子 shell、`cd -` 等**全部**形态 ⇒ 但要动 sandbox/后台/输出截断 ⇒ **P2 评估**，P0 先修正则 |
| **R14** | 前端 UI 只显示"工作区"，agent cd 之后 UI 的工作空间是陈旧的 | 可选：UI 加一行"当前目录"（读 bridge 侧 `wd.Current("")`） |

### 14.11 🧪 复审新增的测试项（补进 §12）

| # | 测试 | 对应 |
|---|---|---|
| T1 | `splitCd` 能匹配**多行 heredoc** 命令 | R1 |
| T2 | `splitCd` 能匹配**带尾换行**的命令 | R1 |
| T3 | 后台命令（`&`）的 `cd` **不**改会话 cwd | R5 |
| T4 | 同步子运行的分区键 ≠ 主 agent 分区键 | R6 |
| T5 | cwd 在**仓库子目录**时，`IsGitRepo` 仍为 true、`IsWorktree` 判定正确 | R8 |
| T6 | gitignore 匹配器在 cwd=子目录时**仍能忽略根目录的 node_modules** | R4 |
| T7 | `hub_workspace` 的跨工作区工具**不跟随**会话 cwd | R3 |
| T8 | `run_python` 的 cwd **跟随**会话 cwd | R3 |

```go
// T1/T2 —— tools/builtin/bash_cwd_test.go 追加
func TestSplitCdMatchesMultilineHeredoc(t *testing.T) {
	// 2026-11-19 回归：原正则 ^cd\s+(.+)$ 匹配不上多行命令 ⇒ cd 记忆静默失效
	// （真实数据里 23.55% 的 cd 命令是 heredoc 形态）。
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	ctx := bashCtx("run-1", "")
	target := t.TempDir()
	cmd := "cd " + target + " && python3 - <<'EOF'\nprint(1)\nEOF"
	got, rest, ok := tool.splitCd(cmd, tool.currentCwd(ctx))
	if !ok {
		t.Fatalf("splitCd 没匹配上多行命令：%q", cmd)
	}
	if got != target {
		t.Fatalf("目标 = %q, want %q", got, target)
	}
	if !strings.Contains(rest, "print(1)") {
		t.Fatalf("剩余命令丢了 heredoc 正文：%q", rest)
	}
}

func TestSplitCdMatchesTrailingNewline(t *testing.T) {
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	if _, _, ok := tool.splitCd("cd /tmp\n", "/"); !ok {
		t.Fatal("带尾换行的 cd 应能匹配")
	}
}

// T3 —— 后台命令不改会话 cwd
func TestBackgroundCommandDoesNotChangeCwd(t *testing.T) {
	root := t.TempDir()
	sub := t.TempDir()
	tool := NewBashTool(root, 0, nil, nil)
	ctx := bashCtx("run-1", "")
	if _, err := tool.Call(ctx, "", `{"command":"cd `+sub+` && sleep 0.01 &"}`); err != nil {
		t.Fatal(err)
	}
	if got := tool.currentCwd(ctx); got != root {
		t.Fatalf("后台命令改了会话 cwd：%q, want %q", got, root)
	}
}

// T4 —— 同步子运行的分区键唯一
func TestSyncSubrunPartitionKeyDiffers(t *testing.T) {
	// 断言：opts.TaskId 为空但 Depth>0 时，agent loop 生成的 ToolContext.TaskId
	// 不能是 ""（否则与主 agent 同分区）。
	// 实现在 agents 侧；此处断言 cwdKey 产出不同分区。
}
```

### 14.12 🔄 修正后的落地顺序（v2）

| 步 | 内容 | 为什么是这个顺序 |
|---|---|---|
| **S0** | **修 `splitCd` 正则**（R1）+ 加 T1/T2 回归 | **前置**：不修则 23.55% 的 cd 记忆无效 ⇒ 后面全是白做 |
| **S1** | `workdir` 包（含 R8 的 git 判定、R9 的 uname 缓存） | 真值源 |
| **S2** | bash 改造（State 化 + R5 后台顺序 + R6 分区键 + 越界重置） | 工具侧 |
| **S3** | 文件工具改造（解析基准=cwd / 边界=root / **ignore 锚点不变** R4 / glob·grep 显示基准 R7） | 工具侧 |
| **S4** | agent loop 注入环境块（**不落 ac.Messages**）+ `Config` 双源断言（R2） | 模型侧 |
| **S5** | 装配点全量改造（§14.9 的 8 处）+ 文案 | 接线 |
| **S6** | 测试（§12 + §14.11 的 T1–T8） | 验收 |
| **P1** | worktree enter/exit + isWorktree 文案 | 独立 PR |
| **P2** | R13 真实 cwd 捕获（fd3 回吐 `$PWD`）/ R14 UI 显示 | 评估后再做 |

> **S0 单独先上**（它本身就是一个可独立验证的修复，且**当前就在损害用户**）——
> 建议**先提一个小 PR 只做 S0**，用 §13 的评估脚本对比前后 cd 记忆命中率。

---

---

## 15. 模型可见 cwd 的载体（v3）—— 工具描述固定输出 + 工具结果回执

> 2026-11-19 第三轮（用户提出）：**不要靠环境块这种"补丁式"通道**，
> 应该**在工具描述里明确、固定地输出当前工作目录**；并且**在对应的工具输出里带上路径**。
> 本节是**模型可见性部分**的权威版本（优先于 §3 / §7.3 / §11.2 B7 / §11.4 的对应条目）。

### 15.1 三条载体的取舍

| 载体 | 放哪 | 模型何时看到 | 压缩后存活 | 恢复会话后存活 | 成本 |
|---|---|---|---|---|---|
| **D' 工具描述** | **tools schema**（每请求重建） | **一直在**（每个请求都带） | ✅ 压缩不动 schema | ✅ 每请求重算 | ~10 token/请求；cd 时 schema 变 |
| **B 工具输出** | **tool result**（进 history） | **cd 那一刻最显著** | ⚠️ 可能被摘要掉 | ✅ | 一次 cd 一行 |
| **A 环境块** | 对话尾部（ephemeral） | 请求尾部 | ❌ | ❌ | 变化时一块 |

⇒ **D' + B 已经覆盖全部职责，A（环境块）可以砍掉**：

| 职责 | 原以为要 A | 实际由谁承担 |
|---|---|---|
| 首次 cd 之前知道起点 | A | **D'**（schema 一直带着 cwd） |
| cd 之后立刻知道"变了" | B/事件 | **B**（工具结果那一行） |
| 压缩后恢复 cwd 认知 | A | **D'**（schema 不参与压缩） |
| 恢复会话后纠正历史旧语境 | A | **D'**（每请求按当前 State 重算） |
| 越界被重置 | 工具结果 | **B**（同一行） |

⇒ **方案变简单**：砍掉 A ⇒ 少一个注入点、少一份 diff 状态、少一处 checkpoint 顾虑。

### 15.2 D' 的实现：工具描述里固定输出当前工作目录

**为什么可行（已验证）**：
- `tools/engine.go:620` `ToolParams(ctx)` —— **每请求调用**（唯一调用点 `agents/agent_loop.go:2802`），
  **无任何缓存**（每次遍历 `e.tools` 重建 schema）；
- `ToolParams(ctx)` **已经有 ctx** ⇒ 不用改 `ToolEngine` 接口；
- `agents/agent_loop.go` 的请求装配处 `req.Tools = a.toolSchemas(streamCtx, ac.Mode)` ⇒ **每次请求都重算**。

**① `tools/runtime.go`：加一个可选接口**
```go
// ContextDescriber 允许工具按**调用方上下文**给出描述。
//
// 用途：bash 工具的描述里要带上**当前调用方**的工作目录
// （主 agent 与子 agent 各有自己的 cwd ⇒ 静态 Description() 给不出正确的值）。
//
// 引擎侧：ToolParams(ctx) 优先用 DescribeFor(ctx)，没有则退回 Description()。
type ContextDescriber interface {
	DescribeFor(ctx context.Context) string
}
```

**② `tools/engine.go:620` `ToolParams`：优先用 ctx 版描述**
```go
	for _, name := range names {
		t := e.tools[name]
		...
		desc := t.Description()
		if cd, ok := t.(ContextDescriber); ok {
			desc = cd.DescribeFor(ctx)
		}
		schemas = append(schemas, core.ToolSchema{
			Name:        t.Name(),
			Description: desc,
			Parameters:  t.Parameters(),
		})
```

**③ `workdir`：分区键走 ctx**
```go
// workdir/ctx.go
package workdir

import "context"

type keyCtx struct{ key Key }

// WithKey 把分区键挂到 ctx 上（工具描述按调用方取值时用）。
func WithKey(ctx context.Context, key Key) context.Context {
	return context.WithValue(ctx, keyCtx{}, key)
}

// KeyFrom 取 ctx 上的分区键；没有 ⇒ MainKey（主 agent）。
func KeyFrom(ctx context.Context) Key {
	if ctx == nil {
		return MainKey
	}
	if k, ok := ctx.Value(keyCtx{}).(Key); ok {
		return k
	}
	return MainKey
}
```

**④ `agents/agent_loop.go`：请求装配处带上分区键**
```go
// req.Tools = a.toolSchemas(streamCtx, ac.Mode)     ← 改前
req.Tools = a.toolSchemas(workdir.WithKey(streamCtx, ac.workKey), ac.Mode)   ← 改后
```
> `toolSchemas` 内部还会用这个 ctx 调 `GetTool` / `mode.AllowTool` ⇒ **挂个 value 无副作用** ✓

**⑤ `tools/builtin/bash.go`：描述里固定输出当前工作目录**
```go
// DescribeFor 按调用方给出描述（含**该调用方当前的工作目录**）。
//
// 为什么必须带 cwd：模型每轮都看得到工具 schema ⇒ 这是**最稳的 cwd 载体**
// —— 压缩不动它、会话恢复后每请求重算、首轮就有。
func (t *BashTool) DescribeFor(ctx context.Context) string {
	cwd := t.Cwd
	if t.wd != nil {
		cwd = t.wd.Current(workdir.KeyFrom(ctx))
	}
	return t.describe(cwd)
}

// describe 拼描述。**格式固定**，只有 cwd 的值在变。
func (t *BashTool) describe(cwd string) string {
	return fmt.Sprintf(
		"Executes a given bash command and returns its output.\n"+
			"Working directory: %s\n"+
			"Each command runs in a separate process: environment variables and other shell state do not persist between calls. "+
			"The working directory persists between calls in this session — a successful `cd` changes it for later commands "+
			"(the new directory is reported in this tool's output).",
		cwd)
}

// Description 静态版（无 ctx 时用）：给根目录。
func (t *BashTool) Description() string { return t.describe(t.Cwd) }
```

**给模型看到的样子**：
```
Executes a given bash command and returns its output.
Working directory: /Users/admin/workspace/hai-super-assistant
Each command runs in a separate process: environment variables and other shell state do not persist between calls. The working directory persists between calls in this session — a successful `cd` changes it for later commands (the new directory is reported in this tool's output).
```

> ⚠️ **子 agent 的 cwd 不一样** ⇒ `DescribeFor` 必须按 `workdir.KeyFrom(ctx)` 取，**不能**用静态 `t.Cwd`。
> ⇒ 这是"静态描述做不到、必须走 ctx"的原因，也是加 `ContextDescriber` 接口的唯一理由。

### 15.3 B 的实现：cd 生效时，工具结果里回执一行

**位置**：`tools/builtin/bash.go` 的 cd 处理处（§14.2 B4 的代码块）——
**`setCurrentCwd` 成功且 cwd 真的变了** ⇒ 在**最终返回给模型的文本**末尾追加一行。

```go
// workdir/notice.go
package workdir

import "fmt"

// Notice 生成给模型看的 cwd 变更回执（空 = 无变化，不需要追加）。
//
// 措辞对齐 Claude Code 的 "Shell cwd was reset to <dir>"（越界重置那条），
// 但正常切换用 "Working directory is now ..."（不叫 "shell cwd" ——
// shell 的其它状态确实不保持，只有 cwd 保持，避免误导）。
func Notice(old, cur string) string {
	switch {
	case cur == "" || cur == old:
		return ""
	case old == "":
		return fmt.Sprintf("\nWorking directory is now %s", cur)
	default:
		return fmt.Sprintf("\nWorking directory changed to %s (was %s)", cur, old)
	}
}

// NoticeReset 生成越界重置的回执。
func NoticeReset(root string) string {
	return fmt.Sprintf("\nWorking directory was reset to %s (the requested path is outside the project root)", root)
}
```

**在 bash 工具里接上**：
```go
// cd 生效时：
old, changed, err := t.wd.Set(cwdKey(ctx), abs)
switch {
case err != nil:
	// 越界 ⇒ 重置 + 回执（Claude Code 同款语义）
	notice = workdir.NoticeReset(t.projectRoot())
default:
	if changed {
		notice = workdir.Notice(old, abs)
	}
}
```

**追加到哪里**：必须是**最终交给模型的那份文本**（`Call` 的返回值），
且要在 **spill / slim 之后**追加（输出超限被落盘时，回执仍要可见）：
```go
// tools/builtin/bash.go（Call 的收尾处）
out = spillOrSlim(out)
if notice != "" {
    out += notice        // ← 必须在裁剪之后追加
}
return out, nil
```

**模型看到的样子**：
```
$ cd /repo/src && ls
README.md  main.go

Working directory is now /repo/src
```

**顺带的好处**：这行也会出现在**前端 UI** 的工具输出里 ⇒ 用户也能看到 agent 换目录了（透明度）。

### 15.4 A（环境块）的处置：**默认砍掉**

原 §11.4 计划的"每请求重算 + 差分注入环境块"——**本期不做**。

理由：D' + B 已覆盖 §15.1 表里的全部五项职责；
而 A 有两个实打实的成本：
1. **多一个注入点**（`agents/agent_loop.go` 请求装配处）+ 多一份 diff 状态；
2. **ephemeral 消息的位置尴尬** —— 追加在请求尾部、下一轮即消失，
   模型可能压根没"注意到"它变了（Claude Code 靠 `isMeta` 标记 + 专门的渲染，hai 没有这一层）。

⇒ 若日后仍觉得模型反应不够，**再把 A 加回来作为第三层**（改动是局部的：加一个注入点 + 一个 `sent` 快照），
不影响 D'/B 的设计。

### 15.5 对方案其余部分的影响

| 影响点 | 变化 |
|---|---|
| **§11.4（agent loop 注入环境块）** | **删除** ⇒ 少一个注入点、少 `ac.envSent`、少 `workdir.Env` 的 diff 逻辑 |
| **§12.4（env 注入测试）** | 改为测 **D'**（描述带 cwd、per-agent 正确、cd 后描述更新）+ **B**（回执文本） |
| **§14 R2（Config 双源）** | 仍然成立（`Config.WorkingDir` vs `WorkDir.State`）⇒ 断言方案不变 |
| **V5（checkpoint 逐字节）** | ⚠️ **表述要改**：B 会把回执写进工具结果 ⇒ **进 history ⇒ 进 checkpoint** ⇒ 新会话的 checkpoint **会变**（这是**预期的**）。真正的不变量是「**同一事件流重放两次 ⇒ 逐字节相同的 checkpoint**」，不是「cwd 变化不改 checkpoint」 |
| **§14.12 落地顺序** | S4 从"注入环境块"改为"**bash 描述带 cwd + 工具回执**"，依赖更少（不需要 `workdir.Env`/diff） |
| **缓存** | tools schema 在 cd 时会变 ⇒ 该次请求的 **tools 段缓存**失效（消息前缀缓存不受影响）⇒ 频率 = cd 次数，可接受 |
| **前端** | 工具输出多一行回执 ⇒ UI 直接显示即可（前端**零改动**，因为它只渲染文本） |

### 15.6 修订后的"模型如何知道 cwd 变了"（最终答案）

```
① D' 工具描述：每个请求的 tools schema 里固定一行 "Working directory: <当前cwd>"
   ├─ 压缩不影响（schema 不进 history）
   ├─ 恢复会话后自动正确（每请求重算）
   ├─ 主/子 agent 各自正确（按 ctx 分区键取）
   └─ 首次 cd 之前模型就知道起点

② B 工具回执：cd 生效的那一次，工具结果末尾追加 "Working directory is now <新目录>"
   ├─ 位置最显著（模型刚读完的工具结果）
   ├─ 进 history ⇒ 后续轮次不用回忆也能看到
   └─ 越界重置用同一行告知

③ （已砍）环境块
```

⇒ 与 Claude Code 的差异（明确记录，避免误以为"没对齐"）：

| | Claude Code | hai（v3） |
|---|---|---|
| 载体 | 对话里的 `isMeta` user 消息 | **工具 schema（每请求）+ 工具结果** |
| 在 system prompt 里？ | ❌ 明确不放 | ❌ 不放（本来就不在 system 层） |
| 谁被通知 | 模型（对话层） | 模型（schema + 结果） |
| 压缩后 | 靠每请求重算的附件 | **靠 schema**（更稳） |
| 恢复会话后 | 靠 transcript 的 cwd + 每请求重算 | **靠 schema**（更稳） |
| 子 agent | 不继承父 cwd | 不继承（分区键）✓ 一致 |

---

---

## 16. 实施记录（2026-11-19，worktree `feat+agent-cwd`）

> 分支 `worktree-feat+agent-cwd`（worktree 路径 `.claude/worktrees/feat+agent-cwd`），
> 基线 `98028fb`（= origin/main）。**8 个提交**，26 个文件，+1410/−101。

### 16.1 提交清单

| # | 提交 | 内容 | 对应方案 |
|---|---|---|---|
| 1 | `9a72325` | **fix(tools)**：`splitCd` 正则漏匹配多行命令 —— 22% 的 cd 命令白记 | S0 / §14 R1 |
| 2 | `9dd90e5` | **feat(workdir)**：新增会话级 cwd 真值源包 | S1 |
| 3 | `f625b4c` | **feat(tools)**：bash 的 cwd 接到真值源 + 越界重置 + 目录回执 | S2 / §14 R5·R6 |
| 4 | `9fa527f` | **feat(tools)**：文件工具跟随 cwd（修掉静默落错地方） | S3 / §14 R4·R7 |
| 5 | `d4e3651` | **feat(tools)**：bash 描述里固定输出当前工作目录 | S4 / §15.2 |
| 6 | `e2fd636` | **feat(bridge)**：装配点接上会话级 cwd 状态 | S5 / §14.3 |
| 7 | `92a6be8` | **test**：cwd 不变式断言 + 变异验证 | S6 |
| 8 | `5ced09a` | **feat(tools)**：git_worktree 支持 enter/exit | P1 / §11.6 |

### 16.2 与原方案的偏差（**都是实施中发现并修正的**）

| # | 原方案 | 实际 | 为什么 |
|---|---|---|---|
| **D1** | 新增 `agents.Config.WorkDir`（解决 R2 双源） | **没加** | v3 砍掉环境块后，agent loop 不再需要知道工作区根 ⇒ 无双源。R2 **自然消失** |
| **D2** | `workdir.Env` 快照（git 判定 / platform / shell / OS） | **没做** | 环境块砍掉后没有消费者（只剩 cwd）⇒ 包只留 state / ctx / notice 三块 |
| **D3** | 评估脚本用 `Env.IsGitRepo` 等 | 不适用 | 同上 |
| **D4** | glob/grep 的**展示**基准改 cwd | **展示 + 匹配基准都改了** | 只改展示不够：匹配也用同一个 `rel` ⇒ cwd=子目录时 `glob("*.go")` 一个都匹配不到（实测） |
| **D5** | worktree 的 Scope = 「本会话创建的」 | **目录边界** | hai's 工具实例随引擎重建而重建（切模型/切人格）⇒ 记不住「本会话创建过哪些」 |
| **D6** | — | **新增 `tools.WorkdirAware`** | 结构化不变式断言（否则 run_python 另建 State 这类错位**行为层测不出来**，已用变异验证证伪过一次） |

### 16.3 实施中发现并修掉的问题

| # | 问题 | 处置 |
|---|---|---|
| 1 | `splitCd` 的 `^cd\s+(.+)$` 在 Go 里 `.` 不匹配 `\n`、`$` 只在文本末尾 ⇒ **heredoc 命令整条匹配不上** | 正则加 `(?s)` + splitCd 支持换行分隔符（S0） |
| 2 | 只改 `cdPrefixRe` 不够：多行命令里 cd 之后的正文会被**整段丢掉** | splitCd 加换行分隔符（`;` 刻意不做：引号路径里出现 `;` 比换行常见） |
| 3 | `BashTool.Call` 里 **cd 拆分在前、后台识别在后** ⇒ `cd X && job &` 把会话 cwd 带走 | 后台识别提前；后台命令**不**改会话 cwd |
| 4 | `subagent.buildOptions` **不设 `opts.TaskId`** ⇒ 同步子运行与主 agent **共用** cwd 分区 | `taskID == "" && ac.Depth > 0` ⇒ 合成 `sync:<runid>:<depth>` |
| 5 | 分区键有**两个来源**（workdir ctx key / ToolContext.TaskId）可能不一致 | `cwdKey(ctx)` 统一：优先前者、后者兜底；agent loop 两处注入同一个值 |
| 6 | 既有测试把 `cd` 指向**工作区根之外**的兄弟临时目录 ⇒ 新语义下会被重置 | 改成根内子目录；另加越界用例 |
| 7 | `run_python` 的工具描述写死 "workspace root" | 文案改准（它现在跟随 cwd） |
| 8 | 系统提示词的 `## Current Workspace`（工作区根）与 bash 描述的 `Working directory`（当前目录）**两个"当前"并存** | 系统提示词补一句区分（工作区根=边界，当前目录在 bash 描述里） |

### 16.4 变异验证（证明断言能抓住"装配漏了"）

| 变异 | 结果 |
|---|---|
| run_python 装配改回 `NewRunPythonTool(workspace, …)`（另建 State） | ✅ **测试红**（指针不等） |
| 文件工具改回 `NewFileTools(workspace)`（另建 State） | ✅ 不红是**正确**的 —— bash 恒来自 `ft.Tools()` ⇒ 两者结构性同源 |
| `splitCd` 正则改回 `^cd\s+(.+)$` | ✅ `TestSplitCdMatchesMultilineHeredoc` 红 |

### 16.5 还没做的（明确留到后续）

| # | 项 | 理由 |
|---|---|---|
| **R13** | 捕获进程**真实结束 cwd**（shell 在 fd3 回吐 `$PWD`） | 能覆盖 `cd X; cmd` / `cd -` / 子 shell 等**全部**形态，但要动 sandbox/后台/输出截断 ⇒ 收益不抵成本（当前前缀启发式已覆盖真实数据里 99.7% 的 cd 命令） |
| **R14** | 前端显示"当前工作目录" | UI 现在只显示工作区；agent cd 之后 UI 的"工作空间"是陈旧的。可选 |
| **评估复测** | 用 `scripts/eval_cwd_redundancy.py` 对比改造后的 cd 前缀占比 | 需要**改造后新产生**的真实会话（历史事件不会变）⇒ 部署后跑 |
| **部署验证** | 桌面端重启 + 真实会话 E2E | 需用户授权（不自行重启应用） |

### 16.6 已知的行为变更（**部署前要知道**）

| 变更 | 影响 |
|---|---|
| 相对路径按**当前 cwd** 解析 | 模型 cd 到子目录后，写文件落在子目录（**这是本次要修的**）；`../x` 回到工作区根内现在**允许**（越界边界仍是工作区根） |
| cd 到**工作区外** ⇒ 记忆被重置 | 命令本身仍在 shell 里跑到那个目录（只是**后续命令的起点**回到根），结果里有回执 |
| bash / 文件工具 / run_python 的**工具结果**多了 cwd 回执一行 | 回执进 history ⇒ 进 journal ⇒ 新会话的 checkpoint **会变**（预期内；重放一致性不变） |
| 工具描述里多了 `Working directory: <path>` 一行 | tools schema 每次 cd 后会变 ⇒ 该次请求的 **tools 段缓存**失效（消息前缀缓存不受影响） |
| `git_worktree create` 现在会**切目录** | 模型若只是想"建个隔离目录给别的任务用"，需要随后 `git_worktree exit` 回来（描述里已写明） |

---

---

## 17. 评估：Agent 运行时频繁 cd 会怎样

> 用户提问：「Agent 运行时多次 cd 这种频繁变更怎么办（理论上应该很少出现，如果明确工作区的话）」
> ⇒ 用**真实会话事件日志**量了一遍。结论：**「理论上很少」不成立**，但设计天然限住了上下文膨胀；
> 顺带**揪出两个实现缺陷**（含一个"回执在撒谎"的 bug）并修掉。

### 17.1 实测（真实会话 494,177 条 bash）

| 指标 | 值 |
|---|---|
| cd 命令总数 | **357,757** |
| 每会话 cd 次数 | **中位 150**、平均 748、p90 776、**最大 99,152** |
| 每个 run（一次用户消息）内 cd 次数 | 中位 19、p90 **119**、最大 842 |
| **重复 cd 到同一目录**（cwd 没变） | **299,897（83.8%）** |
| **cwd 真变化**（⇒ 产生一行回执） | **57,860（16.2%）** |
| 每会话**不同** cd 目标数 | 中位 **4**、平均 13.3 |
| 每个 run 内 cwd 真变化 | 中位 2、p90 19、p99 60、**最大 201** |
| **绝对路径 cd 中越出工作区的** | **61,337（17.29%）** |

**三个关键读数**：

1. **cd 一点都不少**（每会话中位 150 次）⇒ 前提假设不成立。
2. 但 **83.8% 是重复 cd 到同一目录** ⇒ 这正是「模型不知道 cwd 会保持」的表现
   （它只能靠反复 cd 来保证自己在对的目录）。⇒ **改造的直接收益之一**：
   模型学会后这 30 万次重复 cd 会消失（每次省一次 `sh -c` 进程 spawn）。
3. **不同目标数中位只有 4** ⇒ 模型其实只在少数几个目录间活动 ⇒ 真实变化是**稀疏**的。

### 17.2 上下文膨胀：天然被限住

回执只在**工作目录真的变了**时产生 ⇒ 83.8% 的 cd（重复）**一行都不加**。

| 场景 | 回执行数 | 估算 token |
|---|---|---|
| 典型 run（中位 2 次真变化） | 2 | ~40 |
| p90 run（19 次） | 19 | ~380 |
| p99 run（60 次） | 60 | ~1.2K |
| **最坏 run（201 次）** | 201 | **~4K** |
| 全量历史（所有会话累计） | 57,860 | ~1.2M（大部分会被压缩掉） |

⇒ **不需要额外的限流机制**：真变化本身就是「模型需要知道的信息」，
把它压掉反而丢信息。唯一的额外保护是**越界回执限流**（见 17.4）。

### 17.3 抖动（模型在 A/B 两个目录间来回切）的风险

| 风险 | 评估 | 缓解 |
|---|---|---|
| 文件工具把文件写错地方 | **有**（cwd 变 ⇒ 相对路径基准变） | 工具描述**每请求**都报当前 cwd ⇒ 模型每轮都能校正；回执在"刚 cd 完"的位置更显著 ⇒ 两个通道互补 |
| 模型心智模型跟不上 | **有** | 同上（描述=权威、 notice=显著性） |
| 状态 map 无限增长 | **无** | `cur` map 按**分区**（agent）计，不按 cd 次数计 ⇒ 上界 = agent 数 |
| 每次 cd 的执行成本 | 与一次普通 bash 调用同量级（一次 `sh -c`） | 无额外成本；改造后重复 cd 会因模型学会而消失 |

⇒ **结论：无新增回归风险**，且有明确收益（省掉重复 cd 的进程 spawn）。

### 17.4 评估中发现并修掉的两个缺陷

#### ① 回执文案与行为不符（**我实现的 bug**）

改造前：越界只是「不改记忆」⇒ cwd **留在原处**（可能是子目录），
而回执写着 `"Working directory was reset to <root>"` ⇒ **在子目录里越界时它在撒谎**。

Claude Code 的语义是 *"resets to the project directory"* ⇒ 照它改：
**越界 ⇒ 真的重置回项目根**（`wd.Reset`）+ 回执。

#### ② 连续越界 ⇒ 回执刷屏

17.29% 的绝对路径 cd 是越界的 ⇒ 每次都提示 = 同一句话说几十遍。

**限流**：同一分区**自上次真实变化以来**只提示一次
（`workdir.State.FirstOutOfBounds`；真实变化会清掉标志 ⇒ 下次越界仍提示一次）。

⇒ 模型至少知道一次（否则它以为 cd 成功了 ⇒ 会陷入"以为在 /tmp、实际在根"的循环），
但不刷屏。

### 17.5 新增的不变式（测试钉住）

| 不变式 | 测试 |
|---|---|
| 重复 cd 到同一目录 ⇒ **无回执**（上下文膨胀的闸门） | `TestBashCwdRepeatedSameDirNoNotice` |
| 越界 ⇒ cwd **真的**回到项目根 | `TestBashCwdOutsideRootResetsToProjectRoot` |
| 连续越界只提示一次；真变化后再提示 | `TestBashCwdOutOfBoundsNoticeThrottled` |

---

# 附录 A —— §11.2 B7 的 v3 改写（取代 §11 中同名小节）

**B7 工具描述改动** —— ⚠️ **已被 §15.2 取代**：不再靠"规则说明"，
而是**固定格式 + 实际当前工作目录**：

```go
// tools/builtin/bash.go
func (t *BashTool) DescribeFor(ctx context.Context) string {
	cwd := t.Cwd
	if t.wd != nil {
		cwd = t.wd.Current(workdir.KeyFrom(ctx))   // ← 按调用方分区取（主/子 agent 不同）
	}
	return t.describe(cwd)
}

func (t *BashTool) describe(cwd string) string {
	return fmt.Sprintf(
		"Executes a given bash command and returns its output.\\n"+
			"Working directory: %s\\n"+
			"Each command runs in a separate process: environment variables and other shell state do not persist between calls. "+
			"The working directory persists between calls in this session — a successful `cd` changes it for later commands "+
			"(the new directory is reported in this tool's output).",
		cwd)
}
```

> 落地需要三处配套：`tools.ContextDescriber` 接口 + `tools/engine.go:640` 优先用它 +
> `agents/agent_loop.go` 调 `toolSchemas` 时用 `workdir.WithKey(ctx, ac.workKey)` 带上分区键。详见 §15.2。

---

# 附录 B —— §12.4 的 v3 改写（取代 §12.4 原节）

### 12.4 D' / B 测试（V2 / V8 / V9 —— v3 取代原"环境块注入"测试）

> v3：模型可见 cwd 的载体改成 **D'（工具描述固定输出 cwd）** + **B（工具结果回执）**，
> **环境块取消** ⇒ 原 §12.4 的三个测试相应替换。

```go
// tools/builtin/bash_describe_test.go
package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/workdir"
)

// TestDescribeForIncludesWorkingDir 工具描述里**固定输出当前工作目录**（D'）。
func TestDescribeForIncludesWorkingDir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd

	ctx := workdir.WithKey(context.Background(), workdir.MainKey)
	desc := tool.DescribeFor(ctx)
	if !strings.Contains(desc, "Working directory: "+root) {
		t.Fatalf("描述里没有当前目录：\\n%s", desc)
	}

	// cd 之后，描述要跟着变（每请求重算）
	if _, _, err := wd.Set(workdir.MainKey, sub); err != nil {
		t.Fatal(err)
	}
	desc2 := tool.DescribeFor(ctx)
	if !strings.Contains(desc2, "Working directory: "+sub) {
		t.Fatalf("cd 后描述没更新：\\n%s", desc2)
	}
}

// TestDescribeForPerCaller 子 agent 拿到**自己的** cwd（不是主 agent 的）。
func TestDescribeForPerCaller(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd

	if _, _, err := wd.Set(workdir.MainKey, root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wd.Set("task-1", sub); err != nil {
		t.Fatal(err)
	}
	mainDesc := tool.DescribeFor(workdir.WithKey(context.Background(), workdir.MainKey))
	subDesc := tool.DescribeFor(workdir.WithKey(context.Background(), "task-1"))
	if !strings.Contains(mainDesc, "Working directory: "+root) {
		t.Fatalf("主 agent 描述错：\\n%s", mainDesc)
	}
	if !strings.Contains(subDesc, "Working directory: "+sub) {
		t.Fatalf("子 agent 描述错（应为自己的 cwd）：\\n%s", subDesc)
	}
}

// TestBashResultCarriesCwdNotice cd 生效时，**工具结果**里回执新目录（B）。
func TestBashResultCarriesCwdNotice(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	out, err := tool.Call(ctx, "", `{"command":"cd `+sub+` && echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Working directory is now "+sub) {
		t.Fatalf("结果里没有 cwd 回执：\\n%s", out)
	}
}

// TestBashResultCarriesResetNotice 越界 ⇒ 回执重置（Claude Code 同款语义）。
func TestBashResultCarriesResetNotice(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	wd := workdir.New(root)
	tool := NewBashTool(wd.Root(), 0, nil, nil)
	tool.wd = wd
	ctx := bashCtx("run-1", "")

	out, err := tool.Call(ctx, "", `{"command":"cd `+outside+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Working directory was reset to "+root) {
		t.Fatalf("结果里没有重置回执：\\n%s", out)
	}
}
```

```go
// tools/engine_test.go（追加）—— 引擎优先用 ctx 版描述
func TestToolParamsPrefersContextDescriber(t *testing.T) {
	e := NewEngine()
	e.RegisterTool(context.Background(), &ctxDescTool{})
	schemas, err := e.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schemas[0].Description, "ctx-aware") {
		t.Fatalf("引擎没优先用 DescribeFor：%q", schemas[0].Description)
	}
}
```
