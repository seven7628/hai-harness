# docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md —— 结构化恢复稿

> ## ⚠️ 本文不是原件，但已恢复到你手上能用的程度
>
> 原件在 2026-11-19 22:15 误删中丢失，且从未入 git。该文档的特点是「把实现方案写成
> 文档内嵌可编译代码块」，旧会话用 **35 个补丁脚本**反复 `rep(原块, 新块)` 修订。
> 这些脚本的字符串字面量**本身就是原文**，因此可逐字抽出：
>
> | 恢复层 | 数量 | 状态 |
> |---|---|---|
> | 章节标题（§21–§23） | 28 条 | ✅ 逐字 |
> | 设计表格行 | 82 条 | ✅ 逐字（8 种引用类型 / payload / 工具替换 / 新增文件清单） |
> | 散文行（`**定位**` 之类） | 261 条 | ✅ 逐字 |
> | 内嵌代码块 | 177 段 61 KB | ✅ 逐字（原稿 88 + 修订后 89） |
> | §22 整节 | 581 行 | ✅ 全文（`/private/tmp/part22.md`） |
> | §1–§20 的标题与正文 | — | ❌ 不可恢复 |
> | 段落正文 / 论证 / 调研对比 | — | ❌ 不可恢复 |

_生成于 2026-11-20。来源：/private/tmp 下 35 个补丁脚本。_

---

## 一、章节骨架（按节号排序，逐字）

| 标题 | 出处脚本 |
|---|---|
| `#### E-ArtifactsBlock.tsx-1：`bundles` prop` | patch16.py, patch17.py |
| `#### E-ArtifactsBlock.tsx-2：渲染位置` | patch16.py, patch17.py |
| `#### E-Narrative.tsx-1：把 bundles 传进产出卡` | patch16.py |
| `#### E-agents/agent_loop.go-5：收尾合并快照 + 声明 + 分类` | patch2.py |
| `#### E-command_registry.go-1：命令名常量` | patch11.py |
| `#### E-main.go-7：命令分发` | patch11.py |

> 只恢复出 §21–§23。§1–§20 存在过（文档编号连续到 §23），但没有任何脚本引用其标题。

---

## 二、设计表格（逐字，按脚本分组）

### 来自 `patch12.py`

| 需要的能力 | 本仓现状 | 结论 |
|---|---|---|
| HTML/CSS/JS 引用解析 | 自写正则（§6.4，~250 行） | 不引入 `golang.org/x/net/html`：只要 src/href/import，DOM 解析器是杀鸡用牛刀，且要多一个依赖 |
| 需要的能力 | 方案 | 结论 |
|---|---|---|
| HTML/CSS/JS 引用解析 | `golang.org/x/net/html` tokenizer（§6.4） | **唯一新依赖**。为什么不用正则：属性可以无引号/跨行、`<style>` 里的 `url()` 与 `@import` 形态多，正则漏掉时的表现是「页面看起来正常、依赖没进包」——最难发现的一类 bug。x/net 是 Go 官方模块，无额外传递依赖树 |
| `go.mod` | 0（无新依赖） | — | §21.14 |
| `go.mod` / `go.sum` | +1 模块 | A1 | §21.14 |
| `x/net` 依赖引入 | 低 | 构建变化 | 只新增一个官方模块；`go mod tidy` 审查 |
| `x/net` 依赖引入 | 低 | 构建变化 | 只新增一个 Go 官方模块（无传递依赖树）；`go mod tidy` 后核对 `go.sum` 只多两行 |

### 来自 `patch13.py`

| N23 | `scripts/artifact_eval.py` | 指标评估 | ~40 行 | §18.3 |
| N23 | `scripts/artifact_eval.py` | 指标评估 | ~40 行 | §18.3 |
| N25 | `desktop/app/electron/artifact-verify.ts` | `verifyArtifact` / `screenshotArtifact` / `inspectArtifact` | ~120 行 | §11.7.1 |
| N26 | `desktop/app/src/lib/bridge.ts`（**修改**） | `artifactVerify` / `artifactScreenshot` / `artifactInspect` | +12 行 | §11.7 |
| `desktop/app/src/components/Narrative.tsx` | 1 | A3 | §21.12 |
| `desktop/app/src/lib/bridge.ts` | 1（3 个函数） | A4 | §21.13 |
| `desktop/app/src/components/Narrative.tsx` | 1 | A3 | §21.12 |

### 来自 `patch14.py`

| `desktop/bridge/main.go` | 8 | A3/A4/B | §21.10 |
| `desktop/bridge/main.go` | 10 | A3/A4/B | §21.10 |
| 文件 | 处数 | PR | 本章 |
|---|---|---|---|
| `core/tool.go` | 2 | A1 | §21.3 |
| 文件 | 处数 | PR | 本章 |
|---|---|---|---|
| `core/tool.go` | 2 | A1 | §21.3 |
| A 基座（检测/判定/版本/预览/产出卡） | 16 | 16 | ~4,200 行 | 是（4 个 PR） |
| A 基座（检测/判定/版本/预览/产出卡） | 17 | 17 | ~4,400 行 | 是（4 个 PR） |

### 来自 `patch3.py`

| N6 | `desktop/bridge/artifact_prompt.go` | `artifactSystemPrompt` | ~25 行 | §6.9 |
| N6 | `desktop/bridge/artifact_prompt.go` | `artifactPromptContract` | ~25 行 | §6.9 |

### 来自 `patch_chip.py`

| `TestSelectionRefDedup` | 同 artifact+selector+version 两次引用 → 一枚 chip |
| `TestSelectionRefDedup` | 同 artifact+selector+version 两次引用 → 一枚 chip |
| `TestSelectionKindsAccepted` | 8 种 kind 各自 Put/Get 成功；`kind` 缺失按 `element` 兜底 |
| `TestSelectionUnknownKindRejected` | `kind:"dom"` → 400，不落表 |
| `TestSelectionFileWithoutArtifact` | `kind:file` + `selection.path` → 通过（version 可空）；缺 path → 400 |
| `TestSelectionElementsTruncated` | 框选 30 个 → payload 截到 20 + `truncated:true` |

### 来自 `patch_drag.py`

| 元素（默认） | 直接点 | `element`；Shift 点 = `elements` |
| 区域 | 工具条「区域」或按住 `R` 拖 | `region` |
| 元素（默认） | 直接点；Shift 点追加；**拖框** | `element` / `elements`（拖框 = 框选，按 LCA 成组，≤20） |
| 区域 | 工具条「区域」或按住 `R` 拖 | `region`（不要求 DOM，canvas/跨域 iframe 也能用） |

### 来自 `patch_final.py`

| N14b | `desktop/bridge/artifact_commands.go` |
| N14c | `desktop/bridge/artifact_toolbase.go` | `artifactToolBase` / `version` / `readVersionFile` / `emit` / `lock` | ~110 行 | §22.3 |
| N14d | `desktop/bridge/selection_refs.go` | `selectionRefStore` / `Put` / `Get` / `Drop` / `Sweep` | ~120 行 | §23.2 |
| N14b | `desktop/bridge/artifact_commands.go` |
| `desktop/bridge/main.go` | 10 | A3/A4/B | §21.10 |
| `desktop/bridge/main.go` | 11 | A3/A4/B | §21.10 |
| S11 | N19 + E-session guard（已在 S4）+ E-main.go-7 的 selection 分支 + N16 `source.*` | `go test ./desktop/bridge -run TestSelectionGuard` | 过 |
| S11 | N19 + E-session guard（已在 S4）+ E-main.go-7 的 selection 分支 + N16 `source_*` | `go test ./desktop/bridge -run TestSelectionGuard` | 过 |
| S12 | N14c/N14d（toolbase + selection refs）+ §22 的 Exposure/Annotations/Structured | `go test ./desktop/bridge -run 'TestArtifactTool|TestSelectionRef'` | 过 |
| S13 | §23.4 Composer chip + §23.3 `selection_refs` 注入 | `cd desktop/app && npx tsc --noEmit`；e2e `verify_selection_ref.mjs` | 过 |

### 来自 `patch_meta.py`

| 协作（分享/评论/共编） | **后置到 Phase D，且先做 handoff 包** | 本仓是本地单机优先；实时共编需要服务端与 Yjs，和现有架构不同量级 |
| 协作（分享/评论/共编） | **后置到 Phase D，且先做 handoff 包** | 本仓是本地单机优先；实时共编需要服务端与 Yjs，和现有架构不同量级 |
| 工具名 `artifact.publish`（点号） | **wire 名 `artifact_publish`（下划线）** | OpenAI/Anthropic 函数名不允许点号，且 codemode 脚本里 `tools.artifact.publish` 是语法错误（§22.1）；点号只保留为文档别名 |
| 选中即发一条改稿任务 | **默认「引用到 Composer」，另给「直接改」** | 用户多数是「先引用、再自己写一句」；引用不激活写守卫（§23.1） |
| A 基座（检测/判定/版本/预览/产出卡） | 17 | 17 | ~4,400 行 | 是（4 个 PR） |
| A 基座（检测/判定/版本/预览/产出卡） | 18 | 17 | ~4,600 行 | 是（4 个 PR） |
| B 选区改稿 | 5 | 9 | ~1,800 行 | 是（3 个 PR） |
| B 选区改稿 | 6 | 9 | ~2,000 行 | 是（3 个 PR） |
| `go.mod` / `go.sum` | +1 模块 | A1 | §21.14 |
| `go.mod` / `go.sum` | +1 模块 | A1 | §21.14 |

### 来自 `patch_ref2.py`

| `desktop/bridge/main.go` | 11 | A3/A4/B | §21.10 |
| `desktop/bridge/main.go` | 12 | A3/A4/B | §21.10 |

### 来自 `patch_review2.py`

| R16 | **P2** | §22 | 工具名/结果形状没有 CodeMode 契约 | 新增 §22 整章 |
| R16 | **P2** | §22 | 工具名/结果形状没有 CodeMode 契约 | 新增 §22 整章 |
| R17 | **P1** | §9 `artifact_edit` | 先写工作区再 commit：commit 失败会留下「工作区已改、版本没变」的脏状态 | commit 失败时用 base 原子回写（best-effort），并在结果里说明 |
| R18 | **P1** | §10.4 激活时机 | 用户在**普通 run 还在跑**时点「直接改」→ scope 立刻生效，会把**当前这条 run** 的工具一起拦掉 | 激活前检查 `bs.running != nil` → 返回 `session_busy`（前端提示「等这轮结束」）；引用路径不受影响 |
| R19 | **P1** | `subagent/subagent.go:243` | 子 agent 的 `RunOptions` 只继承 Hooks，不继承 `ArtifactRun` → 子 agent 里 `artifact_publish` 拿到 nil，声明丢失（子卡片永远没有包） | 子 RunOptions 加 `ArtifactRun: tc.ArtifactRun`（§21.16） |
| S13 | §23.4 Composer chip + §23.3 `selection_refs` 注入 | `cd desktop/app && npx tsc --noEmit`；e2e `verify_selection_ref.mjs` | 过 |
| S13 | §23.4 Composer chip + §23.3 `selection_refs` 注入 | `cd desktop/app && npx tsc --noEmit`；e2e `verify_selection_ref.mjs` | 过 |
| S14 | §21.16 子 agent 继承 `ArtifactRun` | `go test ./subagent -run TestArtifactRunInherited` | 过 |

---

## 三、散文行（逐字，按脚本分组）

### 来自 `add_kinds.py`

- Payload  json.RawMessage // 已裁剪（≤2KB）的选区数据，原样来自 inspector
- Label    string          // chip 上显示的文字（tag + 文本前 24 字）
- Artifact string // page/artifact/region/element… 都归属一个 artifact；file 可为空
- Payload  json.RawMessage // 已裁剪（≤2KB）的选区数据，原样来自 inspector
- Label    string          // chip 上显示的文字（tag + 文本前 24 字）
- // 引用类型白名单 + 每种类型的 payload 上限（元素列表类按条数截断）。
- selMaxElements = 20 // elements 类的条数上限（设计稿：框选上限 20）

### 来自 `add_sweepfns.py`

- // SweepRefs 清掉指向**已不存在版本**的预览索引（周期清扫的一部分）。
- // 不清的话：用户删了版本/清了工作区，旧 ref 还在，URL 仍能生成，但 serveFile 每次 404 ——
- // 表现是「预览白屏」，排查成本很高。
- // RotateIfLarge 账本 > 8 MiB 时轮转成 metrics.jsonl.1（只保留 1 份）。
- // 这是追加型账本，读路径只做统计分析；留全部历史没有意义，还会拖慢清扫时的目录遍历。

### 来自 `patch12.py`

- **不改。** 逐项核对：
- **只加一个依赖**：`golang.org/x/net`（HTML tokenizer，§6.8 已给 diff）。逐项核对：
- go.mod / go.sum              + golang.org/x/net（唯一新依赖，§6.8）

### 来自 `patch13.py`

- > `embed.ts` **不改**：`createEntry` / `openEmbedTarget` 已有 `srcPath` 去重语义
- **定位**：`desktop/app/src/lib/bridge.ts` 末尾（与 `embedOpenTarget` 同族）。
- // 产物预览/自检：渲染进程只拿 URL 与结果，不碰 Chromium。
- > `embed.ts` **不改**：`createEntry` / `openEmbedTarget` 已有 `srcPath` 去重语义

### 来自 `patch15.py`

- **定位**：`desktop/bridge/main.go:764-800` 的 `switch ev := e.(type)` 里 `case *events.AgentEnd:`
- 分支（`bs.resetLive()` 之后）。
- **定位**：`desktop/bridge/main.go:954` 的 `case *events.AgentEnd:` 分支（在 `switch ev := e.(type)`
- 里，`bs.resetLive()` 之后）。
- **定位**：`desktop/bridge/main.go:3600` 一带（`case CmdAsk:` 与 `case CmdAskBatch:` 两个分支）。
- **定位**：`desktop/bridge/main.go:3250`（`case CmdAsk:`）与 `3273`（`case CmdAskBatch:`）。

### 来自 `patch17.py` —— 目标小节 `#### E-ArtifactsBlock.tsx-1：`bundles` prop`

- **定位**
- **定位**

### 来自 `patch2.py` —— 目标小节 `#### E-agents/agent_loop.go-5：收尾合并快照 + 声明 + 分类`

- -- 以上不动 --
- // 收敛结果（BR-15）：在 updater 里算出、setState 之后写诊断
- // …（中间是既有的助手块冻结/工具行收尾/子卡片收敛，一行不动）…
- // 位置：useAppStore.ts 的事件分发 switch（`case 'command_response':` 同层，末尾 default 之前）。
- // 与既有 case 平级，不嵌进任何分支。
- // 回包：bridge 侧等待器按 id 唤醒；失败也回（否则调用方要等超时）
- // 位置：desktop/app/electron/main.ts，`embed:open-target` 注册之后（同一注册区）。
- // 全部是「隐藏窗口加载 URL → 取结果 → 销毁」，不复用主窗口视图池。

### 来自 `patch3.py`

- +	// selectionScope 当前选区任务的作用域（nil = 无选区任务；见 selection_guard.go）。
- +	// selectionScope 选区写范围守卫（nil = 未启用）；并发安全由它自己的 mu 负责。
- // selectionScope 当前选区任务的作用域（nil = 无选区任务）。emit 的 AgentEnd
- // selectionScope 选区写范围守卫（nil = 未启用）。emit 的 AgentEnd 与 Ask 入口
- selectionScope *selectionScope // 并发安全由 selectionScope.mu 负责
- **同一文件、同一函数**，在构造 `RunOptions` 的地方（不是 loop）加系统提示：
- >           void openArtifactBundle(first.id, first.version!, sid)
- >           void getState().openArtifactPreview(first, { activate: false, quiet: true })
- > `openArtifactBundle` 是 `useAppStore.ts:5700` 一带新增的 action（§11.2 已给全文），
- > `openArtifactPreview` 是 §11.2 已给全文的 store action（`useAppStore.ts:5826` 一带），
- 组合函数见 §6.9。
- 组合函数见 §6.9。

### 来自 `patch4.py`

- +	// 产物契约：只在产物工具真的注册成功时注入 —— 否则模型会被告知用不存在的工具。
- +	// 固定前缀（不随轮次/时间变化），prompt caching 不被打穿。
- +	// 产物契约：只在产物工具真的注册成功时注入 —— 否则模型会被告知用不存在的工具。
- +	// 固定前缀（不随轮次/时间变化），prompt caching 不被打穿。
- -- 以上不动 --
- +- 以上三条不动（完整文本见 §6.9）

### 来自 `patch8.py`

- > 状态：**方案定稿，待评审**（2026-11-19）
- > 状态：**方案定稿，待评审**（2026-11-19；同日 v2 增 §21「代码变更全集」）
- 2. 每一处改动的**完整代码**（新文件全文 + 旧文件逐 hunk diff）；
- 2. 每一处改动的**完整代码**（新文件全文；旧文件逐处「定位 / 原文 / 变更后」，见 §21）；
- - 标「全文」的代码块是可直接落盘的完整文件（新文件为主）；
- - 标「核心代码/核心算法」的是关键函数与骨架，接口、结构体、错误路径已给全，样板代码（import 之外的空行/日志）按仓库风格补；
- - `diff` 的行号是 2026-11-19 基线，实施时以 hunk 里的函数名/相邻代码为准；
- - 所有新工具/新命令都配了「失败语义 + 测试用例」；没有测试的改动不应合并（§16.2）。
- - 标「全文」的代码块是可直接落盘的完整文件（新文件为主）；
- - **§21 是实施清单**：每个修改点给出定位（函数 + 基线行号）/ 原文 / 变更后 / 落盘顺序。
- 前面各节的 diff 是设计意图的摘要，**与 §21 冲突时以 §21 为准**（已知修正：钩子路径归一、
- `filterNoiseArtifacts` 的死代码、`WithArtifactRun` 的真实签名、选区守卫的安装时机）；
- - `diff` 的行号是 2026-11-19 基线，实施时以函数名/相邻代码为准（§21 已核对过一轮真实锚点）；
- - 所有新工具/新命令都配了「失败语义 + 测试用例」；没有测试的改动不应合并（§16.2）；
- - §21.15 是编译关卡表：每一步都必须独立可编译，回滚粒度 = 一步。

### 来自 `patch9.py`

- // 产物工具：与办公工具同一层（loop 建一次、kit/开关不可用就不注册）。
- // 注册失败不阻断：模型只是看不到这些工具，其余能力照常。
- // 产物工具：与办公工具同一层（每次 buildLoop 注册一次；开关关掉就不注册）。
- // 需要 sid：声明表与指标都要按会话归属（见 artifact_tools.go 的签名）。

### 来自 `patch_chip.py`

- label: string // 例：button「立即购买」
- label: string // 例：button「立即购买」/ styles.css:40-60 / 整页
- // chip 上的类型标记：文案短、可翻译；图标复用既有（不新增一套 SVG）。
- element: '元素', elements: '多元素', text: '文字', comment: '批注',
- region: '区域', page: '整页', artifact: '产物', file: '文件',

### 来自 `patch_doc.py`

- +	// 选区守卫：**每会话一个**（内部有锁 + active 标志，见 selection_guard.go）。
- +	// 为什么创建时就装好、而不是任务开始时再 SetToolGuard：selection.commit 可能在
- +	// run goroutine 已经启动之后才到（用户先发普通消息、再点选区），装晚一步，
- +	// 那道窗口里 bash 是放行的。常驻 + active=false 时零成本放行，没有窗口。
- // 这一行是**单行字面量**（main.go 现长度 ~600 字符）：按字段名插入，不要重排。
- +	// selectionScope 选区改稿的写范围守卫（nil = 未启用）。与 Session 共享同一对象：
- +	// selectionScope 选区改稿的写范围守卫（nil = 未启用）。与 Session 共享同一对象：
- +	// Session.Run 里读它做 PreToolUse，bridge 在 selection.commit/AgentEnd 里改它。
- -- 以上不动 --
- // 收敛结果（BR-15）：在 updater 里算出、setState 之后写诊断
- // …（中间是既有的助手块冻结/工具行收尾/子卡片收敛，一行不动）…
- // 位置：useAppStore.ts 的事件分发 switch（`case 'command_response':` 同层，末尾 default 之前）。
- // 与既有 case 平级，不嵌进任何分支。
- // 回包：bridge 侧等待器按 id 唤醒；失败也回（否则调用方要等超时）
- // 位置：desktop/app/electron/main.ts，`embed:open-target` 注册之后（同一注册区）。
- // 两个 handler 都是「隐藏窗口加载 URL → 取结果 → 销毁」，不复用主窗口视图池。

### 来自 `patch_drag.py`

- // 区域拖拽：mousedown 记起点，mousemove 画框，mouseup 收口。
- // 用 pointer 事件而不是 mouse：触控板/触屏一致。
- // 拖拽：元素模式 = 框选（→ elements），区域模式 = 区域（→ region）。
- // 用 pointer 事件而不是 mouse：触控板/触屏一致；阈值 8px 用来区分「点击」与「拖拽」。
- suppressClick = true // 拖完的这一次 click 要丢掉，否则会把框选覆盖成单选
- // 元素模式：框选 = 区域内的节点，按文档序取前 20（LCA 只用于展示成组提示）
- let dragRect = null      // 区域拖拽中的矩形（视口坐标）
- let dragRect = null      // 区域拖拽中的矩形（视口坐标）
- let suppressClick = false // 拖拽结束后的那次 click 要丢弃
- **明确不引用**（避免把上下文撑爆，也避免把不可信内容当指令）：
- > **分期**：Phase B1 支持 `element / elements / text / region / page / artifact / file` 七种；
- > `comment`（批注 pin）放 B2 —— 它需要 pin 的放置/编辑 UI 与「多条意见一次提交」的交互，
- > 与只读引用不是同一档工作量。
- **明确不引用**（避免把上下文撑爆，也避免把不可信内容当指令）：

### 来自 `patch_final.py`

- // Sweep 周期清理（挂在 revisit 清扫器上，与 artifact_sweep 同一个 tick）。
- // Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。
- // Sweep 周期清理（挂在 revisit 清扫器上，与 artifact_sweep 同一个 tick）。
- // artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：
- // 内容寻址去重后版本不变时不重复 reload（避免每次都闪一下）。
- // artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：
- // 内容寻址去重后版本不变时不重复 reload（避免每次都闪一下）。
- // selectionRefs 选区引用表（页面选中 → Composer chip；§23.2）。

### 来自 `patch_guard.py`

- // 三条**守卫覆盖不到**的编排路径，必须在入口封死（不是「顺便」）：
- //   - codemode：脚本内 tools.x(...) 走 Engine.ExecuteOne，**不经过 PreToolUse**
- //     （tools/nested.go 的负向测试钉住了这条降级），bash/写文件全都能绕；
- //   - agent_spawn/subagent/explore：新 Run 的 RunOptions 由工具自己组装，
- //     只继承 ToolContext.Hooks，不继承 Session 的 ToolGuard。
- // 不加这一条，选区守卫等于一块写着「禁止入内」的布帘。
- // 入口在工作区根时，「入口目录」= 整个工作区。按目录放行等于允许改任意文件
- //（上面已判），新增文件一律拒绝：根目录的新文件与普通工作区文件无法区分。

### 来自 `patch_kind2.py`

- // 版本必须真实存在：页面是模型生成的，它报的 version 不可信；这也是「过期引用」
- // 唯一能确定性拒绝的地方（版本被清理 → 这里 404，而不是发出去让模型猜）。
- kind = selElement // 兼容旧 inspector（只发元素）
- // file 可以脱离 artifact（源码面板引用工作区文件）；其余类型必须有版本 ——
- // 没有版本的选区无法回答「你说的是哪一版」，source_get 也无从定位。
- // 版本必须真实存在：页面是模型生成的，它报的 version 不可信；这也是「过期引用」
- // 唯一能确定性拒绝的地方（版本被清理 → 这里 404，而不是发出去让模型猜）。

### 来自 `patch_meta.py`

- > 状态：**方案定稿，待评审**（2026-11-19；同日 v2 增 §21「代码变更全集」）
- > 状态：**方案定稿，待评审**（2026-11-19；v2 增 §21 代码变更全集；v3 增 §22 CodeMode、§23 选区引用、§24 Review 修正）
- > 计数口径：**只算生产文件**（不含 `_test.go` / e2e / 评估脚本，那些在 §17 单列）。
- > 计数口径：**只算生产文件**（不含 `_test.go` / e2e / 评估脚本，那些在 §17 单列）。
- > `CodeMode 支持`（§22）不新增独立文件（并入工具实现），但新增 `artifact_toolbase.go`；
- > `选区引用`（§23）新增 `selection_refs.go`。
- > 本表只列**已有文件的改动**。两章新增的行为契约不含独立文件改动、但必须一起落：
- > **§22（CodeMode：命名/结构化/档位/并发）** 与 **§23（选区引用：ref store + Composer chip）**。

### 来自 `patch_pub.py`

- // **必须写回事件**：AgentEnd 紧接着进 reducer → checkpoint → 事件日志，
- // 不写回的话三处都拿不到 Version（卡片有入口却打不开）。
- // 这是 v1 稿的真实缺陷：函数返回了带版本的副本，调用方却丢弃了返回值。
- // 已打开的预览页要跟着刷新。常规路径是「write_file → AgentEnd 归档」，
- // artifact_edit 自己那次 Notify 只覆盖工具改稿这一条支路。
- // 版本没变（内容寻址去重）就不推，避免同一页无意义 reload 闪一下。
- // artifactSweeper 版本库周期清扫（启动 + 每小时；见 artifact_sweep.go）。
- // artifactSweeper 版本库周期清扫（启动 + 每小时；见 artifact_sweep.go）。
- // artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：
- // 内容寻址去重后版本不变时不重复 reload（避免每次都闪一下）。

### 来自 `patch_ref2.py`

- // Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。
- // Resolve 取一组 refId（发送时调用）：逐条校验属主/过期，并保持用户点选的顺序。
- // 空 ids 返回 nil（不启用选区，行为与改造前一致）。
- // Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。

### 来自 `patch_review2.py`

- AgentEnd 清 scope（`bs.emit` diff）：
- 激活前必须确认会话**空闲**（否则会把正在跑的那条 run 一起拦掉）：
- // onArtifactPageMessage 的 selection.commit(mode=edit) 分支
- if bs.running != nil { // 有 run 在跑：不要改 scope，否则当前 run 的工具立刻被拦
- bs.selectionScope.set(ref.ArtifactID, v.Manifest.Entry, files) // 先激活
- > `bs.running` 是 Session 的当前运行句柄（`session.Session.running`，有锁）。用「忙就拒绝」
- > 而不是排队：排队意味着用户点完没反应，10 分钟 TTL 可能先到期。
- AgentEnd 清 scope（`bs.emit` diff）：

### 来自 `patch_sweep.py`

- // 同一个 tick 顺带清三张**内存表**：它们的键都含版本/会话，磁盘清扫不会替它们收尸。
- m.selectionRefs.Sweep(now) // 过期引用（30 分钟）
- m.artifactPreview.SweepRefs(now) // 预览索引：指向已删版本的 ref 留着就会 404
- m.artifactMetrics.RotateIfLarge() // jsonl > 8MiB 时轮转（保留 1 份）

### 来自 `patch_ws.py`

- // 路径里必须带 wsKey：artifact id 与版本 id 都**不是全局唯一**的（两个工作区可以有
- // 同名的 artifacts/index.html，内容相同时版本 id 也相同）。只按 id+version 建索引
- // 会让后打开的 workspace 覆盖前一个 —— External 大文件会从**另一个工作区**读。
- // wsKey 工作区的公开短键（8 hex）：只用于 URL 命名空间与签名，不暴露路径。
- // refKey 预览页索引键：wsKey + artifact + version，三者缺一都会串工作区。
- // 否则嵌套入口的 inspector 会请求到 /p/id/ver/sig/sub/~hai/... 而 404。
- // 否则嵌套入口的 inspector 会请求到 /p/k/id/ver/sig/sub/~hai/... 而 404。

---

## 四、§22 CodeMode 支持（整节逐字，581 行）

见同目录 `ARTIFACT_part22_CodeMode.md`（由 `/private/tmp/part22.md` 原样复制）。

## 五、§21 改动清单要点（从表格与标题还原）

- 新增文件（最终版）：**24 项**（含测试与脚本）；修改文件总表：**10 个 Go + 7 个前端/Electron**
- `desktop/bridge/`：`artifact_embed.go`(~16 行)、`artifact_commands.go`(~230 行)、
  `selection_refs.go`、`selection_guard.go`、`view_reducer.go`
- `desktop/app/electron/`：`artifact-verify.ts`(~120 行)；`src/lib/bridge.ts` +12 行
- `scripts/artifact_eval.py`（~40 行，§18.3 的指标评估）
- `agents/agent_loop.go`（收尾合并快照 + 声明 + 分类）
- 依赖决策：**唯一新模块** `golang.org/x/net/html` tokenizer（正则方案被否，理由见表格）

## 六、代码块

见同目录 `ARTIFACT_CODE_FRAGMENTS.md`（177 段，61 KB）。
