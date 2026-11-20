# docs/ 未提交文档恢复审计 —— 2026-11-19

> 生成时间：2026-11-20 00:03:32
> 对象：`/Users/admin/workspace/go-code` 中**从未进入 git、随 22:15 误删一并丢失**的文档。

## 一、结论

**绝大多数不可恢复。** 唯一例外是 `docs/AGENT_CWD_DESIGN_2026-11-19.md`，
其约 2,300 行正文可从误删前落盘的 `/private/tmp/cwd_s*.md` + 补丁脚本中**逐字**拼回
（已生成 `AGENT_CWD_DESIGN_2026-11-19.partial.md`），另有一个孤立片段 `part22.md`。

## 二、逐条排除的恢复来源（全部实测）

| # | 来源 | 实测结果 | 能救 docs？ |
|---|---|---|---|
| 1 | git（6 个远端分支全量比对） | 所有分支 `docs/` 均为同一份 35 文件，无额外文档 | ❌ 集合完全相同 |
| 2 | git 悬挂对象 `git fsck` | 仅 1 个 dangling commit（= `stash recovery-verify`），不含 docs | ❌ |
| 3 | GoLand **LocalHistory** | `changes.storageData` 逻辑 102MB 但**仅 36KB 真实数据**：非零区只有文件头 7 字节 + 尾部 43KB；尾部全部是 Go SDK / Python stdlib / node_modules 的**文件名列表**，`doc-like token = 0`、`path-like = 9`、无 `PK`/zlib/gzip 流。目录 22:18:47 才创建，**晚于 22:15 的删除** | ❌ 从未记录过本仓库 |
| 4 | GoLand **fileHistory** | 唯一文件 `agent-server-g...-ngram`，路径全是 `~/cybertron-gogroup/agent-server-g/**` | ❌ |
| 5 | GoLand index/ | 无 go-code 路径 | ❌ |
| 6 | Claude Code 对话记录 `~/.claude/projects/` | 15 份 jsonl；`-Users-admin-workspace-go-code` 下写入过的 `.md` 仅 3 个，全部是**恢复过程中新建**的 `docs/RECOVERY_2026-11-19.md`、`docs/RECOVERY_STATUS.md`、`docs/fix_rm_guard_proposal.md` | ❌ 无删除前的 docs |
| 7 | 本仓库自身的会话记录 `~/.go-code/sessions` | 沙箱禁止读取；幸存副本在 `/private/tmp/hai-lab/.go-code/`（793MB）但属 `agent-server-g` 工作区，`grep 'workspace/go-code'` = **0** | ❌ |
| 8 | 20 个 E2E 隔离 HOME `/private/tmp/*-home-*/.go-code` | 全部 0 命中本仓库 | ❌ |
| 9 | `~/.Trash` | 空 | ❌ |
| 10 | APFS 本地快照 `tmutil listlocalsnapshots` | 只有 3 个 `com.apple.os.update-*`（系统更新），无用户数据快照 | ❌ |
| 11 | Time Machine `tmutil destinationinfo` | 未配置任何备份目标 | ❌ |
| 12 | 全盘 `*.md.bak / *.orig / *~ / *.swp` | 0 个 | ❌ |
| 13 | `/private/tmp` 拷贝 | 只有代码类：`main.go.bak/bak2/bak3`、`patch_*.py` | ❌ 无其它 docs 正文 |

## 三、✅ 已恢复

### 3.1 `docs/AGENT_CWD_DESIGN_2026-11-19.md`（部分，逐字）

输出：`AGENT_CWD_DESIGN_2026-11-19.partial.md`（2,438 行 / 102 KB）

| 段 | 状态 | 来源（误删前落盘时间） |
|---|---|---|
| §7.3 通道落地表 | ✅ v3 终版 | `patch_cwd_v3.py` 19:0x |
| §8 开放问题/调研已答 | ✅ 全文 | `patch_cwd_doc.py` 18:46 |
| §9 验证计划 V1–V9 | ✅ 全文 | `patch_cwd_doc.py` 18:46 |
| §11 详细代码改动方案 | ✅ 全文 | `cwd_s11.md` 18:48 |
| §12 自测评估代码 | ✅ 全文 | `cwd_s12.md` 18:49 |
| §14 复审发现 R1–R? | ✅ 全文 | `cwd_s14.md` 19:05 |
| §15 载体（v3） | ✅ 全文 | `cwd_s15.md` 19:11 |
| §16 实施记录 | ✅ 全文 | `cwd_s16.md` 20:05 |
| §17 cd 频率评估 | ✅ 全文 | `cwd_s17.md` 21:14 |
| §0 结论 | ⚠️ 仅 1 行 v3 补丁 | `patch_cwd_v3b.py` |
| §1–§6、§10、§13 | ❌ 丢失 | 补丁脚本只引用行号，不含正文 |

> 注意：原文是"分段写入 + 按行号打补丁"生成的，因此 §7.3/§11.2/§12.4 存在
> v1 与 v3 两个版本；恢复稿把 v3 版放进**附录 A/B**，正文保留 v1 原段。

### 3.2 `part22.md` 片段

输出：`FRAGMENTS/part22-CodeMode支持.md`（581 行 / 28 KB，21:21 落盘）
内容是"## 22. CodeMode 支持（脚本内调用这 9 个工具）"。**父文档无法确定** ——
幸存 docs 中没有任何含 §21/§22 编号的文档。

## 四、仓库中"被引用但不存在"的自有文档（44 个）

这些名字出现在代码/文档引用里，但工作区和 git 里都没有。其中大部分很可能是
**更早期就已删除的历史文档**（引用残留），只有顶部少数与本次事故同期。

```
docs/ARCHITECTURE.md    (被引用 2 次)
docs/ASYNC_TOOLS_AND_PROMOTE.md    (被引用 4 次)
docs/BROWSER_COOKIE_IMPORT.md    (被引用 4 次)
docs/BROWSER_USE_EMBED.md    (被引用 1 次)
docs/BROWSER_USE_EMBED_V2.md    (被引用 3 次)
docs/CODEBASE_REVIEW_V2.md    (被引用 3 次)
docs/CODEMODE_CALL_CONTRACT_2026-10-28.md    (被引用 2 次)
docs/COMPUTER_USE_DESIGN.md    (被引用 3 次)
docs/CTX_ACCOUNTING_FIX_2026-09-20.md    (被引用 3 次)
docs/DESIGN.md    (被引用 4 次)
docs/DESKTOP_GIT.md    (被引用 4 次)
docs/DESKTOP_HANDOFF.md    (被引用 1 次)
docs/EXECUTION.md    (被引用 5 次)
docs/IM_INTEGRATION.md    (被引用 14 次)
docs/MCP_CONFIG_LAYERS.md    (被引用 1 次)
docs/MCP_HOOKS_SETTINGS_PANEL_2026-09-21.md    (被引用 5 次)
docs/NARRATIVE_STYLE_DESIGN_2026-09-18.md    (被引用 1 次)
docs/PLAN.md    (被引用 3 次)
docs/PROMPT_STANDARD.md    (被引用 3 次)
docs/PROVIDER_CHAIN_REVIEW.md    (被引用 4 次)
docs/PROVIDER_MANAGEMENT_REDESIGN.md    (被引用 2 次)
docs/PROVIDER_USAGE_BILLING.md    (被引用 3 次)
docs/README.md    (被引用 1 次)
docs/RIGHT_PANEL_AND_PREVIEW_PLAN.md    (被引用 1 次)
docs/ROADMAP.md    (被引用 4 次)
docs/SEATBELT_SANDBOX.md    (被引用 2 次)
docs/SESSION_JSONL_STATE_O2_FIX.md    (被引用 2 次)
docs/SESSION_MESH_COLLABORATION.md    (被引用 7 次)
docs/SESSION_RECOVERY_FIX_2026-08-26.md    (被引用 7 次)
docs/SUBAGENT_OUTPUT_JOURNAL_TASKOUTPUT.md    (被引用 6 次)
docs/TRACE_CHAIN_REDESIGN.md    (被引用 13 次)
docs/adr-014-cache-backend.md    (被引用 2 次)
docs/agent-hub/STATE-REGISTRY.md    (被引用 1 次)
docs/extensions.md    (被引用 3 次)
docs/leak-full.md    (被引用 1 次)
docs/mobile-remote/TECH_PLAN.md    (被引用 2 次)
docs/office/README.md    (被引用 1 次)
docs/office/SKILL_SMOKE_2026-09-18.md    (被引用 1 次)
docs/office/TECH_PLAN.md    (被引用 4 次)
docs/r.md    (被引用 1 次)
docs/runbook-oncall.md    (被引用 9 次)
docs/tab-discarding-and-reloading.md    (被引用 2 次)
docs/trace_waterfall_lab.html    (被引用 1 次)
docs/tunnel-identifier.md    (被引用 3 次)
```

## 五、不可恢复的根因

1. **从未 commit** → git 全分支、`fsck` 悬挂对象里都没有 blob。
2. **删除发生在 22:12–22:20**，而 GoLand 的 LocalHistory 目录 **22:18:47 才创建**
   —— 时间上不可能记录过。
3. 唯一的串行记录者（本仓库的 agent 会话 `~/.go-code/sessions`）随目录一起被删，
   幸存的 `hai-lab` 副本属于另一个工作区。
4. 没有 Time Machine、没有用户数据快照、Trash 已清空。

## 六、下次如何避免

- 设计文档落地即 `git add`（哪怕提交到 `docs-wip` 分支也比裸文件强）。
- 或让文档写入 `/private/tmp` 之外的**仓库内**路径，并在会话结束前 `git commit`。
- GoLand 的 Local History 默认只保留有限天数且**不跨重装/不跨项目删除**，
  不能当备份用。

---

# 附：三个具体目标（2026-11-19 最后几条工作线）的恢复结论

## 1. `feat+agent-cwd` worktree —— Bash cd 按 Session 维度保持

| 产物 | 状态 |
|---|---|
| 设计文档 `docs/AGENT_CWD_DESIGN_2026-11-19.md` | ✅ **部分恢复**（2,438 行，见 `AGENT_CWD_DESIGN_2026-11-19.partial.md`） |
| 代码 `workdir/` 包 + `tools/builtin/workdir.go` 等 | ✅ 前次已恢复，现在就在工作区（`git status` 显示为未跟踪） |
| worktree 本体（分支 `feat+agent-cwd`、未提交改动） | ❌ 丢失 |

来源：`cwd_s11/12/14/15/16/17.md` + `patch_cwd_doc.py` + `patch_cwd_v3*.py`。
其中 `cwd_s16.md`（§16）本身就是这条线的**实施记录**，含提交清单与偏差说明。

## 2. 右侧栏内嵌 Bash（终端）—— worktree `terminal-panel`

| 产物 | 状态 |
|---|---|
| 设计文档 `docs/TERMINAL_DESIGN.md` | ❌ **正文全丢**。全盘只剩 **7 处代码注释引用**（§5.1 / §8 / §9），没有任何正文、目录或片段 |
| 实现代码（Go PTY 桥 + 前端 term 标签） | ❌ **main 里完全没有**（`\bPTY\b\|pty_` 精确匹配：0 处实现） |
| 前端构建产物 | ⚠️ `/private/tmp` 里存有当时的 bundle 与测试：`builtin-surfaces.test.cjs`(1.9 万行)、`p8-host-seams-*.cjs`、`term-pane/format/frame/context.test.cjs` |
| 调研证据 | ⚠️ `t_pty.py`/`t_pty2.py`（PTY 行为探针）、`dbg-term.mjs`、6 张 UI 截图（19:43–22:05） |

**为什么这份救不回来**：AGENT_CWD 与 ARTIFACT 两份文档都是"写到 `/tmp` 再脚本拼装"，
每一步都在磁盘留下正文；而 `TERMINAL_DESIGN.md` 是**直接 write 进仓库工作区**的，
过程中没有 `/tmp` 中转副本，patch 脚本也一个都没碰过它（只有代码注释引用）。
所以它是"只有最终态、且最终态在工作区里"——随工作区一起没了。

## 3. Artifacts / Design 技术调研与实现方案

| 产物 | 状态 |
|---|---|
| `docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md` | ⚠️ **正文/章节结构丢失；代码块级内容已抢救** |
| 抢救产物 | `FRAGMENTS/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.fragments.md`（177 段，61 KB） |

恢复方式：该文档把实现方案写成"文档内嵌可编译代码块"，旧会话用 **35 个补丁脚本**
（`patch_doc.py`、`patch2..patch17.py`、`add_kinds.py`、`patch_ws.py` …）反复
`rep(原块 → 新块)` 修订。两个参数**都是文档正文**，因此：
- 88 段 "ORIGINAL" = 被替换掉的原稿代码块（文档早期版本）
- 89 段 "UPDATED" = 修订后的新块（文档最终版本）
- 内容涵盖：`selectionRef`/`selectionScope` 选区结构、artifact bundle 传输、
  `artifactPageRef` 多工作区路径、prompt 契约、前端 reducer 字段等

**丢失的部分**：所有散文、章节标题、决策理由、调研对比、验证计划 —— 这些从不在
代码块里，无从恢复。原文估计 3–4 千行，抢救回的是其中的代码部分。

## 结论一句话

| # | 目标 | 文档 | 代码 |
|---|---|---|---|
| 1 | agent-cwd（cd 按会话保持） | ✅ 2,438 行（缺 §1–6/§10/§13） | ✅ 已恢复在工作区 |
| 2 | 右栏内嵌终端 | ❌ 全丢 | ❌ 全丢（仅剩 bundle/截图/探针） |
| 3 | Artifacts/Design | ⚠️ 仅 177 个代码块 | — |

---

# 最终结果（2026-11-20 00:35）

## 已恢复并入库

| 目标 | 产物 | 位置 | 提交 |
|---|---|---|---|
| `feat+agent-cwd` 代码 | `workdir` 包 + 全部接线（35 文件） | main | `ad777a7` |
| `feat+agent-cwd` 文档 | `docs/AGENT_CWD_DESIGN_2026-11-19.md`（2,438 行） | worktree `feat+agent-cwd` | `bec8ea9` |
| `terminal-panel` 前端 | 9 个模块 / 526 行（编译后 JS） | worktree `terminal-panel` | `cf7c815` |
| `terminal-panel` 文档 | `docs/TERMINAL_DESIGN.md`（重建稿，13 KB） | worktree `terminal-panel` | `cf7c815` |
| Artifacts 文档 | `docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md`（177 段 / 61 KB） | main | `09b233e` |
| 恢复工具与记录 | `session/recover_sessions_test.go` + 3 份 docs | main | `959adf9` |

## worktree 已重建

```
/Users/admin/workspace/go-code/.go-code/worktrees/feat+agent-cwd   [feat+agent-cwd]  bec8ea9
/Users/admin/workspace/go-code/.go-code/worktrees/terminal-panel   [terminal-panel]  cf7c815
```

## 新增证据：paneMachine 状态机（原设计未见记载）

从还原的 `paneMachine.ts` 可见终端的完整状态机：
`idle → creating → connecting → replaying → live → exited / failed`，
副作用 `create-pty` / `connect` / `close-socket` / `close-pty` / `toast`，
并有 `create`/`connect` 两个超时文案。**`replaying` 状态证明存在输出回放机制**。

## 仍然不可恢复

1. **Go 侧 PTY 桥**（spawn / 读字节 / resize / ticket 签发 / `/pty` WS 端点）
2. **WS 数据面协议**（帧格式、背压、重连）
3. `TERMINAL_DESIGN` 的 §0–§4、§6、§10+ 散文
4. `AGENT_CWD_DESIGN` 的 §0–§6、§10、§13
5. `ARTIFACT_SYSTEM_TECH_PLAN` 的全部散文与章节结构
6. 两个 worktree 的 git 历史（分支从未推送）

---

## 补充（00:55）：bundle 汇总扫描把终端还原范围扩大 2.4 倍

首轮只解析了 `builtin-surfaces.test.cjs` 一个 bundle（9 模块 / 526 行）。
改为扫描 `/private/tmp` 下**全部 46 个**含 `// src/` 标记的构建产物（223MB，
depth≤2，排除 node_modules 与 go module 缓存）后，按路径取最完整版本：

| | 首轮 | 汇总后 |
|---|---|---|
| 模块数 | 9 | **16** |
| 行数 | 526 | **1,273** |

**新增模块**（首轮没有的）：
- `commandContext.ts`（67 行）+ 测试（100 行）—— 从 PTY 字节流还原「用户提交了
  哪条命令」：回车提交、退格、`Ctrl-U/C` 清行、`Tab`/`Esc` 视为补全转义 ⇒ 标记
  dirty 不算用户输入。用于给 AI 提供终端上下文
- `paneMachine-test.ts`（120 行）、`writer-test.ts`（147 行）、
  `frame-test.ts`（50 行）、`formatSelection-test.ts`（80 行）
- `src/p8-host-seams-entry.tsx`（105 行，host seams 测试入口）

**补全**：`termSurface.tsx` 20 → 195 行（首轮拿到的是残缺版本）。

对应提交：worktree `terminal-panel` 的 `cf86725`。

---

## 补充（01:30）：验证「脚本里是否装着文档的完整内容」

结论：**只装着一部分，而且边界非常清晰。**

补丁脚本（35 个）对文档做了两类操作：

| 写法 | 内容 | 提取结果 |
|---|---|---|
| `rep(原块, 新块)` | 两个参数**都是文档正文**（改前 / 改后） | 177 段 / 61 KB |
| `lines.insert(...)` / `L[i] = '''…'''` | **整节撰写** | 142 段 / 44.7 KB，其中 21 段带标题即整节正文 |

合计约 **105 KB** 逐字内容（另有 `part22.md` 的 §22–§24 全文 28 KB，那是文档分节文件而非脚本）。

**但脚本只碰过 §21、§22、§23 三节**——实测脚本字面量里出现的顶层节号：

```
§21: 16 次    §22: 1 次    §23: 6 次    §1–§20: 0 次
```

即 §1–§20（背景、目标、调研对比、总体架构、协议概览）从来没有进入任何补丁脚本，
它们是用 `write_file` 一次性写入的，工作区删除后没有任何副本。

**方法论教训**：脚本能恢复的，是"被改过的块"；一次写成、之后再没动过的部分，
不会在脚本里留下任何痕迹。这也解释了为什么 `TERMINAL_DESIGN.md` 完全救不回来——
它连一次脚本修订都没有。
