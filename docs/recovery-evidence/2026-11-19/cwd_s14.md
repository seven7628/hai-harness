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
