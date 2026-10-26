# Code Agent 桌面端会话界面：三家实拍对照 + 高密度输出规范

2026-10 · 取证与结论。所有数字来自**实拍截图**或**开源仓库源码**，来源见第 1 节；目测值一律标"目测"。

---

## 0. 先给结论

1. **三家都收敛到同一个骨架**：左侧会话列表（不是项目树）+ 中间单列会话流 + 右侧可停靠的产出物（diff / preview / 文档）。没有一家把工具调用画成卡片列表。
2. **折叠单位是"轮"，不是"消息"，也不是"事件"**。T3 的折叠行只有 `Worked for 3m 43s ›`，Claude Code 只有 `✳ 3m 22s · ↓160 tokens`，Codex 把动作压成 `Thought 7s` / `Explored 2 files` / `Edited hero.tsx +16 −6`。**没有一家在折叠行里报工具分类计数**——那是我们的语言，也是我们最大的差异化，同时也是我们最大的噪音来源。
3. **正文是唯一的高亮区**。三家正文都接近纯白/纯黑，工具活动一律降到 secondary（≈ 5:1 对比度），颜色只留给语义：`+绿 / −红 / 等待琥珀 / 链接蓝`。
4. **密度靠"一行一动作 + 动词摘要 + 数字着色"解决，不靠缩字号**。元信息最小到 10px（T3 的 `--text-3xs`），但只在真正次要的位置出现一次。
5. **长会话的可用性靠导航，不靠折叠**。T3 有 `TimelineMinimap` + `ThreadFindBar` + 固定 status line——这是三家唯一为"几千行会话"专门做的部件，也是最值得我们抄的一块。

---

## 1. 证据

| 文件 | 来源 | 证明了什么 |
| --- | --- | --- |
| `shots/claude-code-desktop-thread.png` | VentureBeat 实拍（2026-04-15，Claude Code 桌面重设计） | 三栏骨架、正文排印与 measure、右坞四个 pane（Preview/Tasks/Plan/Terminal）、近黑 + 陶土橙 |
| `shots/claude-code-work-indicator.png` | 同上，实拍裁切 | 工作指示器的**全部**信息：`✳ 3m 22s · ↓160 tokens` |
| `shots/claude-code-composer.png` | 同上，实拍裁切 | composer 解剖 + context ring hover：`Context 20.1k / 200.0k (10%) · Plan 24%` |
| `shots/claude-code-desktop-illustration.png` | Anthropic 官方示意 | `35s ↑568 tokens` 底部状态 + `Accept edits` 贴在嘴边的审批 + `Edited weather.ts +9 −5` |
| `shots/codex-app-activity.png` | OpenAI 官方文档页实拍（`developers.openai.com/codex/app`）放大 | 活动行模型：`Thought 7s` / `Explored 2 files` / `Edited hero.tsx +16 −6` |
| `shots/codex-app-empty.png` | verdent.ai 评测实拍 | 侧栏分组（Codex/ChatGPT/OpenAI Web）、空态、composer 圆角与 `Local · Worktree · Cloud` |
| `shots/t3code-thread-diff.png` | t3.codes 官网产品图（真实会话） | `Worked for 3m 43s ›`、正文内联 PR chip、`2 changed files +29 −12 / Open diff`、右坞 Diff 带 Turn 选择器与未修改行折叠 |
| 源码 `pingdotgg/t3code` | 仓库 main | 全部间距/字号/折叠与测高逻辑（见 2.3） |
| 源码 `openai/codex` `codex-rs/tui` | 仓库 main | activity cells 与 `Rich/Raw` 双渲染、终端底色自适应配色 |

---

## 2. 三家的会话解剖

### 2.1 Claude Code 桌面（2026-04 重设计）

**布局**：左栏 ≈275px（Code 标签 / New session / Routines / Customize / Pinned / Projects 分组 / ⌘K）；中间会话流；右栏是**可停靠 pane 栈**（Preview、Tasks、Plan、Terminal），每个 pane 有自己的标题栏、✕、折叠。官方把这叫 drag-and-drop 网格。

**会话流**：没有气泡、没有头像、没有常驻时间戳；正文段落 measure 目测 ≈62ch，行高 1.6–1.65，字号目测 15px。Markdown 标题几乎不放大——`Decision Points to Resolve First` 是同字号 bold + 上下留白，靠字重而不是字阶分层。列表用 4px 圆点，挂 18px。

**工作指示器**：一行，`✳`（品牌火花，橙色）+ 已用时 + `↓ tokens`。没有工具计数、没有分类、没有卡。要更多信息就 hover：context ring 弹 `Context 20.1k / 200.0k (10%) · Plan 24%`。

**composer**：单个圆角输入（r≈10）+ 左侧 `Accept edits / 附件 / 语音 / 更多` + 右侧 `Sonnet 4.6 · Medium` 与一个 14px 的环形 context 指示。审批（Accept edits）**贴在 composer 上沿**，不在流里插卡。

**其它**：`⌘;` Side Chat（不污染主历史，CLI 里是 `/btw`）；Plan 视图 + 可读的推演叙述；为大型 changeset 重建的 diff viewer；内置文件编辑器。

**配色**：近黑底（目测 #0b0b0c 一带）+ 单一陶土橙（#d97757 一带）；语义色只出现在 diff 与 gate 上。

### 2.2 Codex app（2026-02）

**活动行模型**（最重要的一张证据）：

```
Thought 7s
Explored 2 files
Edited  hero.tsx  +16  -6
Read build.py
Edited  build.py  +5  -5
```

一行一动作；动词 + 对象；只有**文件名（蓝）**与**行数增减（绿/红）**着色；行距 ≈33px（截图 2.2× 换算），无边框、无背景、无 chevron。答案正文纯白、目测 17px/1.5、measure ≈60ch。

**空态**：`Let's build` + provider pill（`Codex ⌄`）+ 大圆角输入（r≈18，`+` 在左下、圆形 ↑ 在右下）+ 下方 `Local · Worktree · Cloud` 与 `⌘⏎`。

**侧栏**：New thread / Automations / Skills / Threads，按**来源**分组（Codex / ChatGPT / OpenAI Web），行 = 标题 + 相对时间（4h/8h/2h），选中态是 raised 圆角无边框。

**源码（TUI）事实**：
- 会话由 **activity cells** 组成：`ExecCell`、`ReasoningSummaryCell`、`UnifiedExecInteractionCell`、`ComputerActivityCell`；相邻同类 cell 的细节被 `push_detail` 吸收成组。
- 每组有 `expanded` / `compact` 两种渲染 + 一行 `activity_disclosure`。
- 全局两种渲染模式：`HistoryRenderMode::{Rich, Raw}`（Rich = 归组摘要，Raw = 一事件一行）。
- `color.rs`：按终端底色做 CIE76 感知距离 + alpha blend 自适应前景色——**主题不是选一套色板，而是随背景求解**。

### 2.3 T3 Code（开源，截图 + 源码）

**折叠行**：`Worked for 3m 43s ›`——只有时长，可展开。正文里嵌实体 chip（`PR #7723`），文件变更压成一条摘要条：`› 2 changed files +29 −12 · Show files` + 右侧 `Open diff`。用户消息是**灰卡片**（r≈12），不是气泡。右坞 Diff：`Turn 2 ⌄` 选择器 + `−29 +12` + 未修改行折叠（`1143 unmodified lines`）+ 语法高亮与绿/红导轨。

**源码事实（可直接量）**：

| 项 | 值 |
| --- | --- |
| 会话内容宽 | `--chat-content-max-width: 46rem`（736px） |
| 段/列表间距 | `margin: 0.65rem 0`（10.4px） |
| 标题阶 | h1 1.25rem / h2 1.125 / h3 1rem / h4–h6 0.875，`font-weight: 600`，`margin: 1.25rem 0 .5rem` |
| 元信息字号 | `--text-3xs: .625rem`(10px)、`--text-4xs: .5rem`(8px) |
| 工作行 | `min-h-6`(24px) + `text-sm`(14px) + `leading-relaxed` + **truncate + nowrap**；分组时 `py-0` |
| 详情面板 | `max-h-96`(384px) 滚动 + **live-edge 4px** 才自动跟随 |
| 选择行为 | 行 `select-none`，详情 `select-text` |
| 列表缩进 | `--list-gutter` **按 marker 宽度动态加宽**（≥10 项时从 1.25rem 加到 3ch） |
| 暗色 token | 背景 `neutral-950`、前景 `neutral-100`、次要 `neutral-400` |
| 亮色 token | 背景 `zinc-25`、前景 `zinc-900`、次要 `zinc-500` |
| 字体 | 全部可换：`--font-sans` / `--font-mono` / `--font-composer`，字号与代码字号独立可调（`--font-size-code`） |
| 时间轴部件 | `TimelineMinimap`、`ThreadFindBar`、`TimelineWorkGroupRows`、`WorkLog`、`AgentElapsed`、`ThreadStatusLine`（居中 46rem、`text-xs`、truncate）、`QueuedRunsControl` |
| frame html | `HtmlRenderFrame`：**附件 URL**（minter 签发、可刷新，不是 `srcdoc`）+ 固定列宽测高（`HTML_RENDER_COLUMN_WIDTH`，iframe 回传 `contentHeight`）+ hover「Open full screen」进 dialog；coarse pointer 常显 |

---

## 3. 对照矩阵

| 维度 | Claude Code 桌面 | Codex app | T3 Code | 我们（v2 规范 / 生产 app） |
| --- | --- | --- | --- | --- |
| 会话单位 | 轮 | 轮（activity cells） | 轮 | 轮 ✅ |
| 折叠单位 | 工作段 | 同活动类的 cell 组 | 工作段 | 工作段（按 LLM 轮） |
| 折叠行内容 | `✳ 3m22s · ↓160 tokens` | `Thought 7s` / `Explored 2 files` | `Worked for 3m 43s ›` | `Worked for 136 s · 36 calls, 18 reads, 8 searches, 6 commands, 4 edits` |
| 工具行 | 折叠后只剩指示器 | 一行一动作，动词+对象 | 折叠后不显示 | 一行一调用：图标+名称+参数+统计+耗时 |
| 正文 | ~15px/1.6，标题同字号 bold | ~17px/1.5 | 16–17px，标题 1rem/1.125 | 15.5px/1.66，标题 16px **serif** |
| measure | ≈62ch | ≈60ch | **46rem (736px)** | 800px（生产），无 ch 约束 |
| 代码块 | 语法高亮 | 语法高亮 | shiki + 可调字号 | 轻量高亮 + 复制 |
| diff | 重建的 diff viewer（右坞） | 右坞 + 行内评论 | 右坞 + Turn 选择 + 未修改行折叠 | Transcript 内联文件卡 + diff 行 |
| 工具输出 | 折叠 | 折叠（compact/Raw 双模） | 折叠 | 井（well）+ 截断提示 |
| frame html | 右坞 Preview pane（带浏览器 chrome） | 右坞文档 tab | **正文内联**、附件 URL、按内容测高、hover 全屏 | 正文内联 iframe 136–178px、demo 用 `srcdoc`+`allow-same-origin` |
| 审批 | 贴 composer（Accept edits） | 贴 composer | 贴 composer（pending approval panel） | `agent-runtime-session.html` 是流内 amber 卡；v2 里 gate 是独立 moment，没进流 |
| 用户消息 | 无气泡 | 灰底块 | 灰卡片 | 左边框引用块 + `你 · 19:20:02` |
| 导航 | 侧栏 + 会话列表 | 侧栏分组 | **minimap + find + status line** | 历史折叠（13 行） |
| 主题 | 近黑 + 陶土橙 | 中性灰 | 中性灰（可换字体/字号/代码字号） | 朱砂 + serif 标题（个性最强） |
| 字体 | 系统 sans | 系统 sans | 可配置 | IBM Plex Sans/Mono + Noto Serif 标题 |
| 等待 | 品牌火花 + 计时 | 文本 + shimmer | LiveActivity 行 shimmer | 呼吸点 + 计时（无 spinner）✅ |
| 空态 | 会话列表空 | `Let's build` + glyph | Draft hero | 无专门空态 |

---

## 4. 我们强在哪、弱在哪

**强（应当保住）**

1. **审计性**：ledger 一行给 calls / tokens / cache / wall；成本口径（`Priced` + `Source`、缺额≠0）；四视图（Transcript / Log / Timeline / Digest）——三家都只有"聊天 + diff"，**Log 与 Timeline profiler 是我们的独有资产**。
2. **真实密度**：我们测的是真会话（17 runs / 311 calls / 3885s，单轮 36 调用 16 请求 249s）。三家公开截图里的会话都比这稀（Codex 那张 5 个动作，T3 那张 4 个动作）。**我们的压力样本更硬，这既是优势也是负担**。
3. **CJK**：三家都没有中文行高/字重/断行补偿。我们有 serif 标题 + IBM Plex + 中文 1.7 行高的实测。
4. **失败留痕**：retry/backoff、截断通知、silent end —— 三家都在"安静地成功"，没有失败态的语言。

**弱（应当改）**

1. **折叠行语言学**：我们只有"计数 + 分类"，缺少 **动词 + 对象** 这一层（`Explored 2 files` / `Edited hero.tsx +16 −6`）。计数回答"花了多少"，动词回答"做了什么"——高密度时后者更救命。
2. **常驻元信息太多**：`你 · 19:20:02`、每轮 `model · 时间` + 折叠键、ledger、文件卡、stats —— 一屏里常驻四层噪音。三家都只留 1–2 处。
3. **长会话导航缺失**：没有 minimap / find / 固定 status line。我们目前靠"折叠 13 轮"，那是省空间，不是导航。
4. **frame html 的安全与高度**：`srcdoc` + `allow-same-origin`（demo 标注了，但结构上没解决）；T3 的做法是独立 URL + 内容回调测高 + 全屏。我们内联 136px 的 iframe 在真实 1440 宽度下会**嵌套滚动**。
5. **measure 无约束**：800px 在 1440 屏上是 62ch（还行），但在 1920 全屏就是 90ch+，读不下去。应改 `max-width: 46rem` 一类的硬约束。

---

## 5. 高密度持续输出的设计规范（10 条）

> 场景：一轮 36 次调用、249 秒、连续 llm_text 分片；一屏内可能有 3 轮在跑。
> 每条给"规则 / 为什么 / 我们怎么落"。

**R1 · 会话的原子是轮。** 一轮 = 你一句 + 工作（可折叠）+ 一个结论（常显）。折叠以轮为单位，不按事件、不按消息。跨轮历史只留"一行一轮"。
→ 三家一致；我们已实现（`article.turn` + `data-state`）。

**R2 · 语域三档，只差一档。** 推理 11.5–12.5px/ink-3 → 叙述 13.5/ink-2 → 回答 15.5/ink。相邻档只差 1–2px 与一档亮度，靠**字重与亮度**而非字号跳变。
→ 依据：Claude 正文 15px + 标题同字号 bold；我们 v2 已修（此前 `.cy-say` 吃到 15px，层级是塌的）。

**R3 · 工作行必须是"动词 + 对象 +（计数）"。** `Explored 2 files` / `Edited hero.tsx +16 −6` / `Read build.py`。计数可以留，但**不能只有计数**。
→ 我们要加的一层：`Read 12 files · top: internal/bridge/mock.ts`。

**R4 · 折叠行 = 时间 + 规模 + 变化量。** `Worked for 3m 43s · 36 calls · 6 files +128 −41`。数字是折的理由，必须留在折叠行上，展开给细节。
→ T3 只留时间（因为他们有 diff 面板兜底）；我们留数字更符合"审计"定位，但三项封顶。

**R5 · 颜色只给语义。** 正文与元信息全灰阶；`+绿 / −红 / 等待琥珀 / 链接蓝 / 危险红`。品牌色只在**一个**位置常驻（我们的朱砂 = 强调/焦点）。
→ 三家一致；我们已做到（琥珀 1.82% / 高亮白 0.40% 的配额）。

**R6 · 元信息三档，常驻最多两处。** 10 / 11.5 / 12.5px。轮头与底部 ledger 二选一常驻，其余 hover 或展开。
→ 我们要减：`你 · 19:20:02` 降为 hover；轮头 `model · 时间` 合并进折叠行。

**R7 · 正文 measure 42–48rem，行高 1.6–1.7（中文 ≥1.7）。** 段落 6–8px，列表按 marker 宽度自适应缩进。
→ T3 硬编码 46rem，并给 `--list-gutter` 做多位数补偿（≥10 项加宽到 3ch）——我们手机上刚踩过同一个坑。

**R8 · 流式必须稳。** 块级入场只跑一次；流式期间不改变行高；数字用 tabular-nums；caret 固定宽度。滚动只在"贴底"时跟随（T3：4px live-edge）。
→ 我们已做前四条，缺 live-edge 判据（长输出时手动上滑会被拽回去）。

**R9 · 长会话要能跳，不是能折。** 三件套：minimap / 时间轴缩略、find bar（跨轮搜索）、固定 status line（当前在做什么 + 计时）。折叠解决"屏幕不够"，导航解决"找不到"。
→ **我们最缺的一块**；T3 是唯一做全的。

**R10 · 产出物不进正文流。** 正文只留一行"产出物"（名字 + 大小 + 状态），展开进右坞或全屏；iframe 高度由内容回传，URL 用独立 origin（minted URL / preview server），**不用 `srcdoc` + `allow-same-origin`**。
→ 依据：T3 的 `HtmlRenderFrame`、Claude 的 Preview pane、Codex 的文档 tab 三种做法，共同点是"正文里不嵌可滚动的框"。

**R11 · 审批贴着嘴。** gate 贴在 composer 上沿（Claude 的 Accept edits、我们 v2 的 amber 卡），不在流里插一张大卡；同一个 FIFO 只能有一处可视入口。
→ 三家一致；我们的故事线（`agent-runtime-session.html`）把 gate 插在流中间，读起来像「审计留痕」而不是「等你点头」，值得评估改成贴嘴。

**R12 · 空态与等待不用 spinner。** 品牌 glyph 呼吸 + 计时 + 一句在做什么。
→ 三家都用品牌化的等待语言；我们已做（3.2s 呼吸 + 计时）。

---

## 6. 我们下一步的落地顺序（按性价比）

1. **工作行加"动词摘要"层**（R3）——只改渲染，不动数据。
2. **折叠行加"文件变化"**（R4）：`Worked for 249 s · 10 calls · 4 files +128 −41`。
3. **measure 硬约束 46rem + 段落/列表重排**（R7）。
4. **常驻元信息减半**（R6）：用户时间戳转 hover；轮头并进折叠行。
5. **live-edge 滚动**（R8）与 **find bar / status line**（R9）。
6. **artifact 改 URL + 内容测高 + 全屏**（R10），去掉 `allow-same-origin`。
7. **gate 贴嘴**（R11）。

---

## 7. 诚实边界

- Claude Code 与 Codex app 都是闭源；它们的**字号、行高、色值**是像素目测（标了"目测"），不是官方 token。T3 与 Codex CLI 的数字来自源码，可复核。
- Claude Code 桌面是 2026-04 重设计版；截图来自当时的媒体实拍，之后可能已变。
- 三家的"高密度"都只到"十几行动作"，**没有一家公开过 36 调用 / 249 秒的真实会话**——R1–R12 里带"我们最缺"的条目，依据的是结构推演 + 我们自己的真数据，不是三家已验证的结论。
- T3 的 thread 截图来自官网产品图（是真实会话，但经过挑选），不代表它最糟的密度。
