# go-code 桌面端应用设计（macOS）

> 状态：设计提案（2026-08-11）
> 决策已定：**Electron + React/TS + Code Persona 先行**（壳/Framework/首 Persona 三项用户拍板）
> 定位：基于 go-code SDK 的 macOS 桌面端；SDK 保持纯库不变，桌面端加一层 stdio bridge 适配层

## 1. 目标与差异化

构建类似 Claude Code Desktop / Codex 的 macOS 桌面应用，但**以 go-code SDK 的事件驱动能力为第一等公民**：

| SDK 能力 | 桌面端机会 |
|---|---|
| 事件流（LLMStart/End、Chunk、ToolStart/Response、AgentStart/End、Task 事件）| UI = 事件流可视化 + 控制面，零轮询 |
| 事件自带 Timestamp/RequestId/RunId/ParentRunId/Depth | TTFT/耗时/成本/缓存占比/父子 Run 树 全部可精确推导 |
| HITL（ToolApprovalRequested + Approve(id, decision)）| 审批中心：正中焦点、可编辑参数后批准 |
| 并行工具 / 异步子 agent | 活动通道 Run 树 + 叙述通道并行扇入徽标 |
| 上下文压缩 / 前缀缓存记账（CacheRead）| 上下文环 + 缓存占比趋势（把缓存数学画出来）|
| 恢复/检查点（FileStore）| 会话 = 工作目录内的可恢复状态，侧栏即 FileStore 列表 |
| SystemPrompt 分层 + 工具注入 | Work/Code 双 Persona = 不同 profile，内核零改动 |

**核心差异化主张：双通道布局**——把"模型叙事"与"机制事件（工具/时序/并行）"拆成两条独立通道，解决事件驱动+并行+HITL 下用户难以聚焦核心内容的问题（见 §5.1）。

## 2. 竞品调研结论（2026-08）

头部产品共性：会话侧栏 + 中心聊天 + diff 窗格 + 上下文占用环 + 权限模式。**无人把耗时/缓存占比/并行树做成一等公民**。

- **Claude Code Desktop**：可拖拽多窗格（chat/diff/browser/terminal/file/plan/tasks/subagent）；Transcript 三视图（Normal 工具折叠/Verbose/Summary）；`+12 -1` 徽标 → diff；权限模式（Manual/Accept edits/Plan/Auto）；context ring；多会话 worktree 隔离 + tasks pane 看后台子 agent；调度任务/cloud/SSH。
- **Codex**：CLI TUI + `codex app` + Web；用量/cost 展示；多会话。
- **Cline**：VS Code 扩展；Plan/Act 双模式；每编辑即 diff + checkpoint 回滚；token 用量 + API cost 面板。
- **opencode**：TUI + 桌面 beta；Tab 切 agent（build/plan/general）。

## 3. 技术栈决策

| 层 | 选型 | 理由 |
|---|---|---|
| 壳 | **Electron** | Monaco/xterm 在 Chromium 稳定（WKWebView 有 Monaco 字体/IME 已知坑）；Node spawn Go bridge + NDJSON 管道最简；Cursor/VS Code/Claude Desktop 同栈。代价：包体 ~100MB |
| 前端 | **React + TS + Vite + Tailwind** | Monaco（@monaco-editor/react）、xterm.js、TanStack Virtual（流式虚拟列表）生态最佳 |
| 状态 | Zustand | 事件流 → store 单向注入，组件订阅切片 |
| 通信 | **每 workspace 一个 bridge 进程**（进程内多会话 tab），stdout NDJSON 事件 + 命令请求/响应（correlation id）| 共享 provider 连接池（多 tab 首轮更快）；单 workspace 单进程天然匹配目录绑定 |
| Markdown | react-markdown v10 + remark-gfm + shiki（worker）| 流式：防抖重解析 + 增量 append；shiki 异步高亮不阻塞 UI |
| 代码 diff | v1 **unified diff 渲染**；M3 上 Monaco（@monaco-editor/react）| v1 轻量；Monaco 推迟到"原地编辑 diff + git 集成" |

> 备选：Tauri v2（包体小、原生感）——壳与 UI 解耦、bridge 层保持薄，v2 可迁。原生 SwiftUI 成本最高不选。

### 3.1 组件选型（2026-08-11 拍板：叙事通道自建、布局 react-resizable-panels）

| 层 | 组件 | 选型 | 交互要点 |
|---|---|---|---|
| 基础原语 | shadcn/ui（Radix 基座）+ lucide 图标 + Tailwind v4 | 可定制、暗色友好、主流 | Button/Dialog/Dropdown/Tabs/Tooltip/ScrollArea/Collapsible |
| 对话渲染 | react-markdown + remark-gfm + shiki（worker）| 流式防抖重解析 + 增量 append | 代码块语言标签/复制/折叠 |
| 流式性能 | TanStack Virtual + 自研自动滚动 | 只渲染可见消息；上翻即停跟随 | 万 token 输出不卡 |
| 思考折叠 | 自建（ReasoningChunk）| 默认折叠 + token 计数 | Focus 不被打断 |
| 工具卡片 | 自建（ToolStart/Response）| 一行徽标 + 展开参数/结果；并行扇入 | 变更徽标点击直达 diff |
| 审批中心 | 自建 + Radix Dialog（ToolApprovalRequested）| 居中 + 参数高亮 + 编辑参数 + 超时倒计时 | 多待审批堆叠 |
| Run 树 | 自建（AgentStart/End + Task）+ Virtual | 父子树 + 状态 + 耗时 | 活动通道 |
| 使用统计 | 自绘 SVG | Token 活动热力图（每日/每周/累计）+ 分模型趋势折线 + 指标卡（设置 → 数据与统计）| 不引重 chart 库 |
| diff 窗格 | 自建 unified 渲染 | 按文件聚合 + ✓/✗/⭯ | M3 上 Monaco |
| 布局 | react-resizable-panels | 分栏可拖 + 左右收缩图标条 + 聚焦模式预设 | M3 评估 dockview |
| 侧栏 | 自建 + Virtual | 会话列表（FileStore）/文件树 tab | |
| Composer | 自建 | @文件、/命令、附件拖拽、多行、Enter 发送 | 底部模型/Effort/模式/上下文环 |
| 通知/命令 | sonner（Toast）/ cmdk（命令面板）| | |
| 终端（M3）| @xterm/xterm + node-pty | Code persona 独有 | |

**自建 vs 现成判断标准**：数据源是事件流且交互高度定制（工具卡/审批/指标/Run 树）→ 自建；通用交互原语（按钮/弹窗/菜单）→ shadcn；通用渲染（markdown/语法）→ 成熟库。
叙事通道用**自建**而非 assistant-ui：其 Thread/Message 抽象约束单聊天流、审批内联，与双通道/居中审批中心冲突；我们的事件流即数据模型，零适配层。

### 3.2 视觉风格

- 暗色为主 + 亮色可切换（shadcn CSS 变量主题系统）
- 设计语言对齐 Claude Desktop / Vercel：低饱和、细边框（1px 低对比）、圆角 8-12px、克制强调色
- 层级：`background → surface（卡片/面板）→ elevated（悬浮/弹窗）` 三级 CSS 变量
- 字体：正文系统栈（`-apple-system, "PingFang SC"`）；代码 JetBrains Mono / SF Mono
- 状态色：运行=蓝、成功=绿、错误=红、审批待决=琥珀、思考=灰

### 3.3 会话状态机与交互

```
idle → running(streaming → tools → streaming…) → awaiting_approval → running → done/error
   ↘ 任意时刻：停止(Esc) / 插话（输入不打断，下一轮读——复用 Session inbox）
```

交互要点：
1. 流式：增量 append + 防抖重解析 + 虚拟列表；用户上翻自动停跟随
2. 思考默认折叠；工具叙述通道一行徽标、活动通道展开
3. 审批居中弹窗 + 编辑参数 + 超时倒计时（SDK 超时=拒绝已有）；多待审批堆叠
4. 指标内联：每轮工具徽标旁耗时；状态栏常驻缓存/成本；跨会话用量（累计/峰值/连续天数/成本 + 热力图 + 趋势图）只在设置「使用统计」页看
5. 快捷键：Cmd+K 命令面板、Cmd+L 聚焦、Esc 停止、Cmd+Enter 批准、Ctrl+O 视图切换（聚焦模式循环）、Cmd+B 左栏收缩、Cmd+\ 右栏预览（⌘S 保留给 macOS Save，不占用）；全部图标按钮 aria-label + focus ring

## 4. 总体架构

```
┌──────────────────────────── Electron 壳 ────────────────────────────┐
│ Renderer (React)  ← IPC ← Main (Node)                               │
│  事件订阅 → UI 更新        │  spawn/pipe / 命令转发                  │
└─────────────────────────────────────────────────────────────────────┘
                               │ stdin 命令 NDJSON / stdout 事件 NDJSON
┌──────────────────────────── hai-bridge (Go) ───────────────────┐
│ serve 循环：读 stdin 命令 → 调用 SDK（Session/AgentLoop/Engine）→     │
│ 事件 → 逐行写 stdout                                                │
│  - ask/approve/interrupt/compact/switch-persona/…（命令→Session 方法）│
│  - diff：单次调用由工具产出（ToolResponse.Diff，SDK 层）；累计视图   │
│    用 git diff（非 git 目录才做会话级轻量基线）                      │
│  - 指标推导：TTFT/耗时/缓存占比/成本（归位原则：SDK 不加字段）        │
│  - profile：Code/Work = {系统提示词, 工具集, 布局预设, 模型/Effort, 价表}│
├─────────────────────────────────────────────────────────────────────┤
│                      go-code SDK（纯库，零改动）                     │
└─────────────────────────────────────────────────────────────────────┘
```

- **SDK 保持纯库**：bridge 是宿主层（类似 examples/flappy 的 serve 化），SDK 不感知桌面。
- **每 workspace 一个 bridge 进程**：进程内多会话（tab），共享 provider 连接池；多 workspace 并排才各自进程（隔离）。崩溃隔离靠 SDK panic recover + 会话级兜底。
- **协议**：行分隔 JSON。事件行带 `event_type` 判别器（已具备）；命令 `{id, type, payload}` 请求/响应对。

## 5. 布局设计（逐条对应 5 个用户考量）

### 5.1 聚焦核心内容 → 双通道布局

```
┌ Top bar: [工作目录 ▾] [Persona: Code|Work] [模型 ▾][Effort ▾][权限模式 ▾] (上下文环)
├──────────┬────────────────────────────────────────────┬───────────────┤
│ Sessions │  叙述通道（默认聚焦）                         │ 预览面板 Tab    │
│ 会话列表  │   user: 修复登录 bug                          │ 执行链路/Git/  │
│ (FileStore│   agent: 我查一下…                          │ 文件/网页       │
│  扫描)    │     ▪ read auth.go [12ms]                   │ ─ Run 树 ─     │
│          │     ▪ write auth_test.go [+42 -3] ▸        │   run0 main    │
│ File 树  │   agent: 完成，新增 42 行，3 个用例…          │   ├ llm #1     │
│          │  ┌────────────────────────────────────────┐ │   ├ llm #2     │
│          │  │ Composer: [@file /skill 输入……]         │ │   ├ tool ×3    │
│          │  │   [◆ opus-4.8 ▾]  ctx 45k/200k ▸      │ │   └ subagent d1│
│          │  └────────────────────────────────────────┘ │ ─ 文件/网页 ─   │
│          │                                            │ 链路 · 变更    │
│          │  (diff 并入右栏 Git Tab，底部不再单列)          │ 任务 · 文件    │
│          │                                            │ [待审批:1]     │
├──────────┴────────────────────────────────────────────┴───────────────┤
│ status bar: run0·turn3 | 缓存 88% | ctx 32k/200k | $0.0072 | 耗时 9.4s  │
└───────────────────────────────────────────────────────────────────────┘
```

- **叙述通道（中心）**：只呈现模型叙事（正文 + 变更徽标）。工具调用折叠成状态徽标（`▪ write_file auth.go [+42 -3]`），点开展开结果或跳 diff。三视图态（Normal/Verbose/Summary）只作用于叙述通道。
- **活动通道 = 预览面板的「执行链路」Tab**：全部工具事件 + 时序 + Run 树（ParentRunId/Depth 直接重建）。调试/审计不污染叙事。右栏详细形态与聚焦模式见 §5.7。
- 并行工具：叙述通道水平扇入徽标组；活动通道在 Run 树对应节点展开。

### 5.2 审批中心（HITL = 正中焦点）

- `ToolApprovalRequested` 到达 → **审批卡片居中弹窗**：工具名 + 参数 JSON 高亮 + 已等待时长 + 可**编辑参数后批准**（我们的模式机制应有形态）。
- 多待审批 = 堆叠决策队列；超时 = 拒绝（SDK 已实现），错误文本回传模型可见。
- 审批决策走 `Session.Approve(id, decision)`；编辑参数走「PreToolUse 改写」路径（hooks 已支持）。

### 5.3 文件 +/- 变更对比 → 两层 diff（工具级 + git 级）

**单次调用 diff 由 file 工具直接产出**（信息源头原则——工具是唯一同时持有旧/新内容的点）：

- `edit_file` 已持锁读旧全文，替换后新旧都在手 → 免费算 diff；
- `write_file` 覆盖路径已在 `atomicReplace` 持锁内，锁内补读一次旧内容即可（零竞态，成本 = 一次已打开 fd 的 Read）。
- 机制照 `ToolUsageProvider` 先例：`ToolDiffProvider` 可选接口 → engine 填进 `ToolResponse.Diff`（结构化，事件层，**不进 LLM 上下文**——事件 ≠ 消息历史）；Result 文本加 `(+N -M)` 摘要（模型可见）。
- 归位原则边界：耗时可推导 → SDK 不加字段；**diff 无法从事件推导，必须由产出者给**（同 `ToolResponse.Arguments` / `ToolUsageProvider` 先例）。

**业界对照（2026-08 调研：Claude Code / Cline / Codex）**：

| 产品 | baseline（旧内容）来源 | diff 计算方 | 渲染 |
|---|---|---|---|
| Claude Code | 宿主侧编辑前文件快照（`~/.claude/file-history/<session>/`，首份 = session baseline）| 宿主（快照 vs 当前）| 终端 unified / VS Code 原生 diff / 桌面 pane |
| Cline | shadow git 仓库（独立于真实历史，每次工具用后 commit）| git | 编辑器原生 diff 查看器（Compare 按钮）|
| Codex | patch 上下文（`apply_patch` 输入 unified diff，context 行匹配 = 旧内容）| **工具即 diff** | TUI / patch 本身 |

三个跨产品结论：① **unified diff 是通用货币**（Monaco/git/编辑器都能吃——验证 `FileDiff.Unified` 选型）；② baseline 三种流派互不冲突（宿主快照 / shadow git / patch 上下文）；③ **没人把 diff 塞进事件流**——那是他们没有事件流架构；Codex（patch 即工具 IO）最近，Cline（每次 commit）用 git 换"每调用粒度"。我们的 `ToolResponse.Diff` = Codex 流派 + 事件流特性，非 git 目录也能给每调用 diff。

**两层分工（主/次摆正）**：

1. **主：单次调用 diff** = 工具产出（对齐 Codex）——活动通道每调用展示，非 git 场景唯一来源。
2. **次：会话累计 diff** = **git 优先**（对齐 Claude/Cline）——git 仓库 `git diff`（免费、battle-tested）；非 git 目录才降级：事件 diff 聚合或会话级轻量基线。

**字段**（v1 不携带 old/new 全文，unified 有界截断；diff 窗格用 unified 渲染即可，无需 Monaco）：

```go
type FileDiff struct {
    Path      string `json:"path"`
    Added     int    `json:"added"`
    Removed   int    `json:"removed"`
    Unified   string `json:"unified,omitempty"` // 变更行 unified diff（超阈值截断）
    Truncated bool   `json:"truncated,omitempty"`
}
```

并行工具乱序改多文件 → diff 面板**按文件聚合**（时间序 diff 会误导）：叙述通道只出徽标，右栏 **Git Tab** 列出「N 个文件已改」，逐文件 `+N -M` + unified diff + ✓/✗/⭯；拒绝/回滚 = 恢复会话级基线（git checkout / 非 git 快照）。

**回滚能力（M3 可选）**：Claude 用 file-history 快照、Cline 用 shadow git 实现"回到任意一步"。我们若要同款体验，M3 加轻量快照层（每工具调用后把变更文件存 workspace `.go-code/snapshots/`，与 diff 事件互补：快照完整可回滚、事件轻量展示）。当前不承诺，按需启用。

### 5.4 指标可视化（全部事件推导，SDK 不加字段）

| 指标 | 来源（事件 Timestamp 差分） |
|---|---|
| 首字 TTFT | LLMStart.ts → 首个 Content/ReasoningChunk |
| LLM 单轮耗时 | LLMStart → LLMEnd |
| 工具耗时 | ToolStart → ToolResponse（同 id）|
| 任务耗时 | AgentStart → AgentEnd；Run 树每节点自带 |
| 上下文占用 | 累计 Usage.Input+Output vs 模型窗口 → 顶部上下文环 |
| 缓存占比 | CacheRead/(Input+Output)，每轮条形 + 累计趋势（低命中轮 = 新增必 miss，直接画出来）|
| 成本 | bridge 按模型价表换算（SDK 只聚合 token）|

会话级实时指标：顶部上下文环 + 底部 status bar 常驻（缓存% / ctx / 成本 / 耗时），每轮卡片与耗时条内联在链路 tab。

跨会话用量（`metrics` 命令聚合宿主 `~/.go-code/metrics.jsonl`）**只在设置「数据与统计 → 使用统计」页看**：
指标卡（累计 Token / 峰值 Token / 最长聊天时长 / 当前·最长连续天数 / 累计成本）→ Token 活动热力图
（每日 / 每周 / 累计；hover 出当日 + 截至当日累计）→ 时间范围（近 7 日 / 近 30 日）+ 每日 Token 趋势图
（按 `provider/model` 分线，>5 个模型合并「其他」）。
口径：**指标卡与连续天数为全历史**（不受时间范围影响），热力图窗口 = 近 53 周、趋势图窗口 = 7/30 天；
报告一次拉取覆盖一年，两种粒度都在前端切同一份数据（全量扫描数百毫秒，不随切换重跑）。空会话
（无消息）中部复用同一份报告的 **近 90 天热力图**（`UsageActivityMini`：5 行 × 18 天 = 90 格，全部落在
窗口内没有占位格；`heatGridRows` 把「列 = 周」的网格重排成按天从左到右的横排 —— 13~14 列 × 7 行在
16:9 屏上近正方形、头重脚轻，横条 ≈3.6:1 才和标题/范围文案同一读法；格子随卡片宽度等比缩放，
网格水平居中、空态整块在可用高度里垂直居中；hover 把「日期 · 当日 · 累计」内联到标题下一行 ——
卡片窄，浮层会压住半张图）。

### 5.5 工作目录绑定 → Workspace 一等概念

- 启动即工作目录选择器（记住历史）。Workspace = 根目录：Agent.md 递归发现根、文件工具沙箱根。
- **存储分层（APP 级 vs 工作区级）**：会话 / 事件 / 指标 / gocache 都是 **APP 层面（跨 workspace）**，
  统一落 `~/.go-code/` 下，按 workspace 子目录隔离；**工作区目录内只有 skills 管理**（与 git worktree）：
  - `~/.go-code/sessions/<workspaceKey>/<sid>.jsonl` — 会话 FileStore
  - `~/.go-code/events/<workspaceKey>/<sid>.jsonl` — 事件日志
  - `~/.go-code/metrics.jsonl` / `~/.go-code/cron.json` — 指标 / 定时任务（全局）
  - `~/.go-code/skills/` — 全局 go-code 技能（兼容旧路径）；`~/.agents/skills/` — 全局 Agent Skills 主路径（低优先级层）；`{ws}/.agents/skills` + `{ws}/.go-code/skills` — 工作区技能（同名覆盖全局，工作区 .go-code 最高优先）
  - `{ws}/.go-code/worktrees/` — Git Worktree 隔离目录
  - `<workspaceKey>` = 工作区绝对路径的安全扁平编码（可读 + 短哈希保证唯一、不同 workspace 不冲突）
- 会话绑定 workspace；侧栏会话列表 = FileStore 会话，点击恢复。
- bridge 以 `--workspace <dir>` 启动；文件工具以此为根，越界写拦截（沙箱）。

### 5.6 Work vs Code 双 Persona

`Persona = {系统提示词, 工具集, 布局预设, 默认模型/Effort, 价表}`，同一 bridge/内核：

| | Code | Work（办公助手）|
|---|---|---|
| 工具集 | 文件 + 终端 + git + git_worktree + agent_*/subagent_explore + todo | 文件/文档 + 受限 bash（或无）|
| 布局预设 | 终端/diff/浏览器窗格 | 叙事居中，活动栏默认折叠 |
| 系统提示词 | 资深工程师 | 办公助理 |
| 差异化 | Monaco + git 集成 | markdown/文档预览 |

**Code 专属工具（仅 Code 模式引入，Work 不注册）：**
- `git_worktree`：创建 / 列出 / 移除 Git Worktree，多任务并行时在隔离目录开发。所有 worktree 统一放
  `{workspace}/.go-code/worktrees`（目录自动创建），创建时必须指定 worktree 名字（`name`），可选
  `start_point`（分支/commit/tag）。路径绑定工作区根，拒绝逃逸。
- `subagent_explore`：专职用户问题分析的 Explore 子 agent，仅注入 `read_file / grep / bash / glob`
  四个工具（分析为主，不落写变更），最终回复为自包含的分析结论供主 agent 采纳。
- `agent_spawn` 派生的后台子 agent：**继承主 Agent 的全部工具**（共享同一 ToolEngine 实例），
  子任务可读可写可执行，而非早期的只读子集。

正是 SystemPrompt 分层 + 工具注入架构的用法：桌面只传不同 profile。

### 5.7 布局状态与聚焦模式（2026-08-12 定稿）

三栏：左 = 会话 + 工作区（可收缩）；中 = 叙述通道 + Composer；右 = **预览/详情面板**（可收缩）。底部 diff 窗格取消，并入右栏 Git Tab。

**右栏 = 四 Tab 面板**：

| Tab | 内容 | 数据源 |
|---|---|---|
| 执行链路 | Run 树 + 时序 + 每轮指标（原活动通道）| AgentStart/End + Tool + Usage |
| Git | 会话累计变更：按文件聚合 unified diff + ✓/✗/⭯ + 回滚 | ToolResponse.Diff 聚合 / git diff |
| 文件 | 文件预览（文件树 / 工具结果点击）| Read 工具事件 / 文件树 |
| 网页（M3）| browser pane | — |

> 原「指标」tab 已下线：会话级指标留在状态栏与链路 tab，跨会话用量迁到设置「使用统计」页（见 §5.4）。

**多标签与面板插件（2026-11-13，创造模式 V0.1 落地）**

右栏从「四个固定 Tab」变成**多标签**：标签集按 scope（工作区/会话）隔离存在 `tabsByScope`，每个标签一个实例；内置四类（activity / git / file / web）由 `surfaces/registry` 注册表提供，文档与媒体预览不是 kind，而是 `file{mode:'preview'}` 的**渲染器注册表**（text / image / csv / pdf / office / media / html）。`plugin:<id>` 是第五类 kind，由插件注册。

| 交互 | 行为 |
|---|---|
| 溢出 | `TABS_MAX_VISIBLE = 5`，其余进 `+N` 下拉；**活动标签始终在可见槽位**（落在窗口外时替换最后一槽，不改 `tabs` 顺序）；菜单键盘可达（Enter 开 / ↑↓ 移焦点 / Esc 还焦点） |
| 右键 | 关闭 / 关闭其他 / 关闭右侧 / 关闭全部 / 重开；关闭全部对 dirty 标签走确认队列，不静默丢弃 |
| 键盘 / 鼠标 | `⌃⇥` / `⌃⇧⇥` 前后循环（回绕，输入框里也生效；`⌘1..9` 仍是切最近会话）；中键关闭（有 dirty 守卫，不误激活） |
| 空态 | 不是一句话，而是「能力面板」：把可开的面板逐个列出来，点一下即开 |

**每个面板一个边界**（`SurfaceBoundary`）：插件/面板在 render 期崩溃只黑**内容区**，标签条、收起钮、＋ 菜单不受影响；错误态按 `tabId` 复位（**不挂 key** —— key 会重挂 children、破坏 web 标签的原地复用）。连续 3 次失败自动禁用（`localStorage['surface.disabled']`，成功渲染一次即归零），被禁用的面板显示**墓碑**：说明原因、可重新启用（内置走本地禁用集，插件走 index 的 `enabled`）、可移除标签（走 `closeTabs`，脏内容会问一次）。

**插件面板（同源可信代码）**：装在 `~/.go-code/surfaces/<id>/{surface.json,panel.js}`，索引是 `index.json`（有序、带 hash、`.bak` 回滚点），卸载进 `.trash/<id>-<ts>`，每个动作写 `audit.jsonl`。字节经 `hai-surface://<id>/<path>` 送出，**只送 enabled 的插件**，路径校验（`..`/绝对/NUL/realpath 后再判扩展名与包含）与媒体协议同一套纪律。`ctx.host.command` 由宿主代理，只放行 manifest 声明的 ∩ 宿主白名单（今天四条只读：`git_snapshot` / `workspace_list` / `session_list` / `ui_inspect`），未声明直接 `denied` 并审计。设置里「面板插件」页负责列表 / 启用停用 / 卸载（二次确认）/ 查看调用记录。

**给 agent 的自观测通道**：`ui` 信封（`{type:"ui", payload:{op,data}}`）把右栏快照送进 Go 内存缓存（**不进事件流、不落盘、不重放**），`ui_inspect` 工具返回对象并带 `ageMs`（>5s 标 `stale`）—— CodeMode 脚本里 `await tools.ui_inspect({what:"tabs"})` 直接拿到 `tabs` 数组，不是 JSON 字符串。快照只含当前 scope 的标签元数据（id/kind/标题截断 200），不含文件内容，不泄露其它会话。

**快捷键口径**：⌘S 只在编辑器内拦截（不绑全局），⌘B 左栏、⌘\ 右栏，⌘1..9 / ⌘0 切最近会话。

**聚焦模式 = 布局预设**（react-resizable-panels 换预设；Ctrl+O 循环；切换只改宽度/显隐、不重挂组件、保留滚动位置；**宽度即时切换不补间**——避免 reflow，预览内容用 transform 滑入/交叉淡化，时长 150ms；`prefers-reduced-motion` 下全关）：

| 聚焦 | 自动触发（半自动）| 布局分配 |
|---|---|---|
| 会话 Chat | 发消息 / 聚焦 Composer / 关闭全部预览 | 中 max 800px 居中；右 = 图标条；左按收缩规则 |
| 预览 Preview | 点变更徽标 / 文件树点文件 / 打开网页预览 | 右 ≈54%；中 = 窄摘要（最近几轮 + 关键变更徽标，点回全文）；左 = 图标条 |
| 执行 Chain | 展开执行链路 / 点 Run 树节点 | 右 ≈54% 链路；中 = 窄 |

**收缩规则**（左 = 保守自动；右 = 预览驱动自动）：

| 面板 | 手动 | 自动 |
|---|---|---|
| 左栏 | ⌘B（VS Code 惯例；⌘S 留给 macOS Save 不占用）/ 拖到宽度阈值 / 顶栏按钮 | 仅窗口 <960px 或进入预览聚焦 → 图标条 |
| 右栏 | ⌘\ / 拖到阈值 / Tab 逐个关 | 会话聚焦 → 默认图标条（Work persona 尤其）；打开 git/文件/网页预览 → 自动展开并切对应 Tab |

图标条悬停滑出（peek）、点击钉住（pin）。

**Composer**（模型 + 上下文占用）：

- 左下 `[◆ 模型 ▾]`：v1 只显示当前模型；M2 接切换弹层（模型窗口大小 + 价表；切换 = 新轮次生效，历史保留）。
- 右下 `ctx 45k/200k` 迷你环：<90% 灰蓝 / 90%+ 琥珀（将压缩）/ 95%+ 红（建议手动压缩）；点击 → 压缩确认。
- ctx 环 hover → 上下文构成面板（`context_breakdown` 命令）：分区占比（消息上下文/工具/MCP/系统提示词/技能/其他）+ 分段条形图 + 两行缓存命中率（当前上下文 / 会话累计）。
  口径（2026-09-20 重定，见 `docs/CTX_ACCOUNTING_FIX_2026-09-20.md`）：**静态分区**（系统提示词/技能/工具/MCP/其他）按 SDK「请求组装字节」估算（chars/4）并**固定展示**——每轮原样进请求，只随工具集/技能集/模式变化；**消息上下文 = 总量 − 静态合计**（残差，估算误差全落在它身上）；**总量 = 锚点**（最近一轮真实 usage 的 Input；压缩后为压后估算，此时 ctx 环读数带 `≈`、面板说明标「估算」），与 ctx 环逐字一致。旧实现是「按真实用量等比缩放所有分区」，会把会话内容摊派到静态块上（实测 6k 的工具 schema 在 93k 上下文里显示成 82k），已废弃。面板是 ctx 的同级节点，点面板不触发压缩。
- 与顶部上下文环（累计）、状态栏（常驻）三处联动。
- `/skill` 命令 = Composer 内联命令面板（弹层），不改变布局。

### 5.8 UI/UX 规则校验（2026-08-12，ui-ux-pro-max）

> 色板弃用 DB 泛化推荐（Newsletter 模板/slate 不匹配 dev tool），保留 GitHub-dark token；规则集全量套用。

**修正清单**（1-3 已并入上节，4-9 为约束）：

1. 聚焦切换宽度不补间（§5.7 已改）：transform 滑入/交叉淡化，禁 width/height 动画。
2. 动画 token 统一：入场 150ms ease-out / 出场 100ms ease-in；`prefers-reduced-motion` 全关。
3. 左栏切换 = ⌘B；⌘S 留给 macOS Save（§3.3 已改）。
4. 图标条所有按钮 `aria-label` + hover/focus tooltip + focus-visible ring 2-4px；点击目标 ≥32px。
5. Composer ctx 环：`45k/200k` 数值 + 百分比文字并排（不靠颜色），tabular figures；`role=progressbar`；流式增长 aria-live polite 低频播报。
6. 强调文字用 indigo-400（indigo-500 对 #0d1117 仅 4.4:1）；indigo-500 留边框/填充/hover。
7. z-index scale：0/10/20/40/100（content→rail→popover→审批 modal）；审批 modal scrim 40-60% 黑；React/Radix 组件自理 focus（不手写 querySelector focus）。
8. 共享元素：叙述通道变更徽标 → 右栏 Git Tab 对应文件行高亮 + 滚动到。
9. 空状态与骨架：会话恢复 skeleton（>1s）、使用统计骨架、空会话/空预览给引导+动作（空会话中部挂近 90 天用量热力图，整块在可用高度里垂直居中）；diff 计数用文字徽标（color-not-only，已在）。

## 6. 组件清单（事件 → UI 映射）

| 组件 | 数据源（SDK 事件/API） |
|---|---|
| SessionList（侧栏）| FileStore 扫描 + SessionPersistError |
| Transcript（叙述通道）| ContentChunk/ReasoningChunk 聚合、LLMEnd、AgentEnd |
| ToolChip | ToolStart/ToolResponse |
| RunTree（活动通道）| AgentStart/End + TaskStarted/End（ParentRunId/Depth）|
| ApprovalCard | ToolApprovalRequested + Session.Approve |
| MetricsPanel | LLMStart/End、ToolStart/Response、Usage（CacheRead/Input）|
| ContextRing | 累计 Usage |
| CompactionNotice | CompressStart/End |
| ErrorToast | LLMError（Attempt/WillRetry/RetryDelayMs）、SessionRunError、SessionBudgetExceeded |
| DiffPane | ToolResponse.Diff（工具产出；unified 渲染，v1 无需 Monaco）|
| Terminal | bash 工具后端（sandbox）|
| Composer | ask / Session.RegisterCommand（/skills、/compact）|

## 7. 里程碑

- **M0 桥接验证**：bridge serve 循环 + NDJSON 事件流；Electron 最小壳显示事件流（替代 trace.jsonl）。
- **M1 核心会话**：workspace 绑定 + 会话侧栏 + 双通道布局 + 流式渲染 + 审批中心 + 上下文环（用量看板见 §5.4）。
- **M2 文件能力**：工具级 diff（ToolDiffProvider + ToolResponse.Diff）+ unified diff 窗格 + 每文件 ✓/✗/⭯ + 回滚。
- **M3 Code 深化**：终端窗格 + git 集成 + 并行工具扇入 + Run 树可视化 + 视图三态。
- **M4 Persona 化**：profile 抽象 + Work 布局预设 + 文档预览。

## 8. 风险与开放问题

- **Monaco 体积**：按需懒加载（仅 diff/编辑时拉取），v1 可先用轻量 diff（diff-match-patch 渲染）降首载。
- **bridge 协议演进**：事件行需兼容追加字段（Go json 忽略未知字段，天然向前兼容）。
- **多 workspace 进程内存**：每 workspace 一个进程，多 workspace 并排时内存叠加；先接受，M3 后评估是否需要统一进程看护。
- **权限模式映射**：SDK 的 mode.AllowTool 与桌面 Manual/AcceptEdits 的映射粒度（工具级 vs 文件操作级）待 M1 细化。

## 9. 前置决策定稿（2026-08-11）

用户授权按原则定稿：**A 组性能优先；B/C/D 组体验优先 + 尽可能小依赖**，注重字体/颜色/布局/交互。

### A. 桥接与进程模型（性能优先）

| 项 | 决策 | 理由 |
|---|---|---|
| 进程模型 | **每 workspace 一个 bridge 进程，进程内多会话（tab）** | 共享 provider 连接池（HTTP keep-alive 复用，多 tab 首轮更快）；单 workspace = 单进程天然匹配"绑定本地工作目录"；多 workspace 并排才各自进程（隔离）。崩溃隔离靠 SDK panic recover + 会话级兜底 |
| 命令协议 | `{id, type, payload}` 请求/响应对（correlation id）；**ask 异步受理**（响应 = 已受理，任务完成由 AgentEnd / SessionClosed 事件表达）| 事件驱动 UI：命令立即确认，流式事件驱动渲染，不阻塞 UI 等待任务跑完 |
| 事件协议 | SDK Event 平铺 + `session_id` 字段（多会话路由键）；stderr 仅 bridge debug 日志 | 延续事件自包含原则；Go json 忽略未知字段 → 向前兼容 |
| 命令集 | `new_session`(新建/恢复 FileStore) / `ask` / `interrupt` / `approve` / `compact` / `shutdown` / `switch_mode` | 复用 SDK 恢复/Shutdown/压缩/插话能力，bridge 只做命令→Session 方法映射 |
| 生命周期 | stdin EOF / SIGTERM → 优雅 Shutdown 全部会话；Electron 心跳检测进程存活 | 复用 Session.Shutdown（abort 收尾 + 状态落盘）|
| SDK 接入 | bridge 薄宿主直接调 Session（flappy serve 化）；**per-session 串行命令队列**；EventHandler → 带锁行缓冲写 stdout | 零抽象层；SDK 纯库不变；多会话并发事件行不交错 |
| 安全 | token 走 Electron safeStorage(Keychain) → 环境变量注入 bridge（不入 stdin）；renderer sandbox + contextIsolation，IPC 只经 contextBridge 白名单 | 最小暴露面；进程边界清晰 |

### B. 审批 / HITL（体验优先 + 小依赖）

- **编辑参数 = JSON 视图**：自建（textarea + 语法高亮 + 即时校验，无效 JSON 禁用批准按钮）；表单视图后置（v1 不引 react-jsonschema-form）
- **权限模式映射**（bridge 配置表驱动，非 SDK 改动）：
  - Plan：只读工具可见（SDK AllowTool 过滤只读桶）
  - Manual：写 / 命令类触发审批（ApprovalRequired）
  - AcceptEdits：write/edit 自动批准，命令仍审批
- **审批队列**：SDK preApprove 顺序阻塞 → UI 单审批卡 + 「第 N/M 个」进度；等待窗口 30 分钟（2026-09-20 起，原 60s）倒计时（末段琥珀预警），超时 = 自动拒绝且工具结果明确写「无人决策（不是用户拒绝）」→ 模型可见后修正；审批卡停靠在输入框上方（非全屏，见 DESKTOP_HANDOFF §5 #12）

### C. Persona / Workspace（体验优先 + 小依赖）

- **profile = JSON 配置文件**：`{name, system_prompt, tools 可见性, layout_preset, model/effort, price_table}`；内置 `profiles/` + 用户 `~/.go-code/profiles/`；**per-session 定 profile**（workspace 可设默认，运行中不切换）
- **存储布局**：会话/事件等 APP 级状态落 `~/.go-code/{sessions|events}/<workspaceKey>/`（跨 workspace，见 §5.5）；`{ws}/.go-code/` 仅存 skills 管理（与 git worktree）
- **安全边界**：文件工具根 = workspace（SDK resolve 已拦越界）；bash 沙箱后端 bridge 配置

### C2. 会话页样式 V3（组件皮肤）—— **已于 2026-11 移除**

> 现状：会话页只有 **runtime** 一套皮肤（`<html data-skin="runtime">` 静态写在
> `desktop/app/index.html` / `mobile.html` 上，`src/runtime-skin.css` 的
> `html[data-skin='runtime']` 前缀因此不用重写）。`current` / `v3` 两套皮肤、设置里的
> 「会话页样式」选择项、`src/narrative-skin.css` 以及组件里的 `useSkin()` 结构分支全部删除。
> 下面这段是 V3 当时的设计与踩坑记录，**只作历史参考**：里面提到的 `.plan-card`、`.ic`
> 隐藏规则、`verify_plan_card_skin.mjs` 等都已不存在。

- **思考正文段落间距**（2026-09-19 用户报障）：思考正文是 pre-wrap 纯文本，段落用空行分隔 → 空行渲染成
  一整行盒（≈2 倍行高），比正文段落（`.msg .body p` 的 4px）松一个数量级，V3 下尤其明显。修法 =
  `lib/thinkingText.ts` 按空行切段（围栏代码块内的空行不切、CRLF 剥 `\r`）→ 组件渲染 `<p class="tk-p">`
  → 间距交给 CSS：默认皮肤 `1.65em`（= 原空行高度，观感逐像素不变）、V3 `4px`（与正文同节奏）。
  皮肤规则特异性要压过 `.msg .body .thinking`（0,3,0）与 `.thinking .thinking-body .tk-p`（0,3,0）→ 用 0,4,1。
- **计划卡「太贴边」**（2026-09-20 用户报障）：V3 皮肤把 `.plan-hd` 的 padding 归零（行语言：图标/详情/耗时
  贴齐消息列），但 `.plan-card` 的**卡壳**（边框 + 底色 + 圆角）没销 —— 头部于是被卡边框夹住（左右各 1px），
  V3 下看着就是"内容顶在卡片边上"。按设计语言（`design-research/07` §9.3：AI Elements 的 Plan 是带边框的
  Card，与「一屏只有一个边框」冲突 → 只借信息结构）**销掉卡壳**：计划 = 一行 + 一条左竖线，展开的清单挂在
  竖线上；`.plan-raw` / `.plan-err` 的原 26px / 31px 左缩进是挂在卡内盒子上的，一并改成与 `.tool-row-body` /
  `.tool-error-info` 同一套展开体语言（左竖线 + 12px）。默认皮肤一条都没动（卡壳照旧 1px / 8px 圆角）。
  移植复盘：lab 的 `restyle-app-only.css` 里**没有** `.plan-card`（lab 那套 DOM 根本没有这层壳），
  §16.5 只复盘了"lab 有、本仓没有"的类名，这条是反向漏项。（当时锁它的
  `desktop/app/e2e/verify_plan_card_skin.mjs` 已随后续改动一起退场：计划卡在 `45310be`
  改成一张表之后，`.plan-card` 已经没有任何组件产出。）

### D. 指标 / 流式 / 工程（体验优先 + 小依赖 + 字体颜色布局交互）

- **指标口径**：缓存占比 = 状态栏累计 + 链路 tab 每轮条形（低命中轮标「新增必 miss」）；成本按 profile 价表换算（跨会话累计成本在「使用统计」页按价表现算，改价可回溯重算）；状态栏定稿 `run0·turn3 | 缓存90% | ctx 32k/200k | $0.0148 | 耗时107s`；上下文环 = 累计 vs 模型窗口
- **流式策略**：markdown 100ms 防抖重解析（仅流式中的消息）+ 完成立即全量；TanStack Virtual 动态测量 + 距底 <200px 跟随；思考折叠流式「思考中…」/ 完成态 `思考 424 tok`（chunk 阶段按字符估算，完成用 LLMEnd.Usage.Reasoning 精确值）；超长消息折叠头部
- **字体**：正文 `-apple-system, "SF Pro Text", "PingFang SC"`；代码/指标/JSON 等宽 `"SF Mono", "JetBrains Mono", Menlo`；字号 正文 13-14px / 代码 12.5-13px / 状态栏 11.5px
- **颜色**（GitHub dark 系 token，亮色可切）：bg #0d1117 → surface #161b22 → elevated #1f242b；强调 indigo-500（交互）+ cyan（运行态/流式光标）；语义 成功 #3fb950 / 错误 #f85149 / 警告 #d29922 / 运行 #58a6ff / 次要 #8b949e；diff 新增 #238636 底 / 删除 #da3633 底；边框 1px rgba(255,255,255,0.08)
- **布局**：中心消息流最大 800px 居中留白；右栏 320px 可折叠 40px；底部 diff 默认 40% 高可拖；消息间距 24px、工具徽标 28px 高、代码块圆角 8px
- **交互**：流式光标（青）、工具徽标 hover 展开 / 点击跳 diff、审批卡「编辑后批准 / 批准 / 拒绝 / 稍后」、动画 120-200ms 克制（透明度+位移）、空状态引导、Skeleton 加载态
- **工程**：桌面端 = go-code 仓库内 `desktop/`（`bridge/` Go 包 + `app/` Electron+React，单仓库版本同步）；electron-builder dmg + 代码签名；UI 状态 Zustand persist → appData
