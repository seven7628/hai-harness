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
