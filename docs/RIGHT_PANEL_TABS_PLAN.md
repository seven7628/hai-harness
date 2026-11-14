# 右侧栏统一标签管理方案（浏览器式标签）

> 状态：**方案定稿，待实施**（2026-09-20）
> 前置文档：`docs/RIGHT_PANEL_AND_PREVIEW_PLAN.md`（R0–R4：视图池会话隔离 / 预览多标签 / 标签条重做）
> 本文回答：右栏 5 个固定 tab 如何变成浏览器式标签管理、**作用域定在哪一层**、对现有功能有哪些影响、分几期做。
> 与前置文档的关系：本文**取代** §0.2 的「右栏 tab 是固定单选组」前提，并对 R4 的标签条做**上层化**（从 web tab 内提到面板级）；ADR-0（会话隔离）**继续有效**（见 §0.3）。

---

## 0. 一页摘要

### 0.1 已拍板的 7 条决策（2026-09-20，用户口径）

| # | 决策 | 取值 | 理由 / 代价 |
|---|---|---|---|
| **D1** | 终端标签 | **不做**（本次） | 应用内无 PTY：`TerminalIcon` 只用于 bash 工具卡（`Narrative.tsx:606`）与设置页快捷键图标（`SettingsModal.tsx:24`），`package.json` 无 node-pty/xterm。终端是**新能力**（PTY + 沙箱边界），与本次重构正交 |
| **D2** | 链路 / 任务标签的作用域 | **session 维度**（标签带会话锚点） | 两者数据本就是会话级（`traceEvents`/`traceList`、`blocks`/`runNodes` 全在 `SessionView`；bridge `trace` 命令按会话） |
| **D3** | 标签条作用域 | **scopeKey 抽象，首期 = session id** | 见 §2.2。先按会话落地（行为等价、ADR-0 不动），将来若要让「文件/浏览器/变更」跨会话共享，**只换 scopeKey 取值**，标签条与组件不改 |
| **D4** | 视图池超限 | **LRU + 休眠**，替代当前「抛错」 | 用户口径：超限报错会阻塞 Agent 运行；LRU 只影响展示。**前置缺口见 §5.2** |
| **D5** | 文件标签 | 区分 **diff 模式** 与 **完整预览（source）模式** | 现有 `fileFocus.toolId`（操作/diff）与 `mode='source'`（`RightPanel.tsx:1236-1243`）正好映射 |
| **D6** | 视觉类文件的标签归属 | 落**浏览器标签**，标题显示源文件名 | 现状（R4 的 `tabLabel()`）；备选「文件标签内嵌原生视图」需把全局单 rect 与标签条联动，复杂度高 |
| **D7** | 无会话工作区 | 用**工作区路径作临时 scope**，文件/变更/浏览器都可用 | 否则右栏在「加了工作区但没建会话」时无归属（今天 `setRightTab` 无 sid 只写顶层，`:6215`，切会话即被 `resetView()` 冲掉） |

### 0.2 三条硬规则（沿用前置文档 §0.2）

1. **快捷键不重叠**：`⌘1..9 / ⌘0` 归顶部会话标签（`App.tsx:73`），右栏标签**不得**占用数字键；`⌘W` 是窗口级 → 关闭标签另选键位（§7 未决 3）。
2. **生命周期不共享**：关会话标签 ≠ 关内容标签；关工作区 → 该 scope 的标签随之销毁。
3. **可见性判定权在渲染层**：主进程只做合成计算（`embed.ts:303-310`），前端 `RightPanel` 只 hide、`BrowserTab` 负责 show（`RightPanel.tsx:2143-2146`、`:1589-1603`）。多标签后这条不变。

### 0.3 为什么作用域先选 session（而非工作区）

| 维度 | session 级（选定） | 工作区级（备选） |
|---|---|---|
| 链路 / 任务 | 天然一致 | 需锚点（工作区可有 N 个会话） |
| **ADR-0 会话隔离** | **完全不动** | **反转**：同工作区多会话共享视图池（AI 在后台会话开的页会出现在前台标签条） |
| 视图池压力 | 更大：同一文件在两个会话 = 2 个视图（去重键 `sid + srcPath`，`embed.ts:391-412`） | 更小 |
| 无会话工作区 | 需兜底 scope（= D7） | 天然成立 |
| 持久化 key | `titleKey(path, sid)`（先例 `LS_SESSION_PREFS`，`:1372-1386`） | 工作区路径 |

ADR-0 的四处实现因此**一行不改**：可见性合成（`embed.ts:305-306`）、`pickSessionView`（`:314-321`）、跨会话激活拒绝（`:522-523`）、前端按 sid 过滤（`RightPanel.tsx:1489`）。

---

## 1. 现状事实（实施前必须知道的）

### 1.1 标签条现状 = 单选组，不是标签

| 事实 | 位置 |
|---|---|
| `RightTab` 是 5 值枚举 `'activity' \| 'git' \| 'agent' \| 'file' \| 'web'` | `useAppStore.ts:13` |
| 分主段（链路·变更）/ 辅段（任务·文件·网页）渲染，`tab-sep` 分隔 | `RightPanel.tsx:31-43`、`:2205-2221` |
| 切换 = 改一个字段（`setRightTab`），无「实例」概念 | `useAppStore.ts:6207-6218` |
| 折叠态 rail 复用同一个 `rightTab` 高亮 | `RightPanel.tsx:2182-2195` |

### 1.2 状态归属：会话级 + 仅进程内

- `rightTab` / `rightExpanded` 是 **`SessionView` 字段**：`useAppStore.ts:496-497`；真相源 `views: Record<sid, SessionView>`（`:1540`），顶层只是**活跃会话的镜像**（`:625-626` 出、`:678-679` 回、`:692-696` `loadViewToTop`、`:699-701` `resetView`）。
- 默认值 `rightExpanded:false, rightTab:'activity'`（`emptyView()` `:579-580`）→ **新建会话 = 右栏收起 + 回到链路**。
- **完全不持久化**：checkpoint schema 无这两个字段（`store/checkpoint.ts` 头注释「瞬时交互态不在此 schema 中」），localStorage 只有 `go-code.projects/titles/prefs/sessionPrefs`（`:1330-1331`、`:1372-1373`）→ **重启 = 默认值**。
- **LRU 会吃掉它**：`VIEW_CACHE_MAX = 10`（`:4930`），`evictViews` 按 `viewOrder` 淘汰非活跃/非 busy 会话（`:4937-4954`）→ 被淘汰的会话切回时 `resetView()` 归零（`:5381-5383`）。**所以标签集不能继续寄生在 `SessionView` 里**（见 §2.2）。

### 1.3 内容状态有三种作用域（本方案的核心难点）

| 标签 | 内容数据 | 现有作用域 | 证据 |
|---|---|---|---|
| 变更 / 审查 | `git/gitFiles` **或** `wsGit/wsGitFiles` 双轨 | 会话级 + 工作区级双轨（无会话时用工作区级） | `RightPanel.tsx:341-347`；`loadWsGit` 不依赖会话 `:5805-5809`；变更 tab 打开时同步工作区快照 `:1317-1318` |
| 文件 | `filePreview` / `fileFocus` | **全局单例**（既非会话级也非工作区级，只在 `closeFile` 清） | `:1574-1575`、`:5907` |
| 网页 | 视图池 entry（`sid` 字段） | **会话级**（进程级单例池 + 会话归属） | `embed.ts:39`、`:87-90`、`:305-306` |
| 链路 | `traceEvents` / `traceList` / `activeTraceId` / `traceDetail` | 会话级 | `useAppStore.ts:498-524` |
| 任务 | `blocks` / `runNodes` / `runningAgentCount` | 会话级 | `:476-489` |

→ **结论**：标签条可以整体按 scopeKey 走，但**「文件」标签的内容必须先拆掉全局单例**（否则两个文件标签必然串台：打开 B 覆盖 A），「变更」标签天然双轨可用，「网页」保持会话归属（D3）。

### 1.4 视图池模型（多标签地基已存在）

| 事实 | 位置 |
|---|---|
| `views: Map<'tN', EmbedEntry>`，key = 自增 target id（**不是** sid/页面 id）；`sid` 只是字段 | `embed.ts:99-112`、`:87-90` |
| 真开真关真枚举：`openEmbedTarget` / `closeEmbedTarget` / `activateEmbedTarget` | `:378-433`、`:487-516`、`:519-531` |
| 上限 `MAX_EMBED_VIEWS = 8`，超限**抛错**（无驱逐） | `:59-62`、`:413-418` |
| 去重键：同 `sid` + 同 `srcPath`（office 预览的临时 html_path 也命中同一标签） | `:391-412` |
| 可见性 = `visible && id === activeId && (sid === '' \|\| sid === currentSid)` | `:303-310` |
| 切会话**不销毁**任何视图（保活） | `setEmbedSession` `:657-663` |
| 几何：单 rect（全池同 bounds），由前端 `.browser-shot` 的 `getBoundingClientRect` 经 ResizeObserver → `embed:resize` 驱动 | `:538-544`、`RightPanel.tsx:1559-1579` |
| 删会话/删工作区按 sid 批量 close（防僵尸渲染进程） | `useAppStore.ts:5417-5428`、`:5461-5472`；验收 `scripts/test-store-scoping.mjs` |

### 1.5 自动激活的 4 处触发（⑤ 缺陷的根源）

| 触发 | 现状判据 | 位置 |
|---|---|---|
| 任何内嵌视图出现真实页面 | `nav.targets.some(real)` —— **不按会话过滤**；`embedAutoOpenedForSid` 是**单值**模块变量（只记住最后一个会话） | `:974-1005`、`:755` |
| 任何 AI 浏览器动作 | `focusWebTabIfClosed()` → `focusBrowser()`（**还会收起左栏**） | `:1006-1017`、`:770-776` |
| Agent 产出可视文件 | `activate:true` → 切 web tab（§6.10「产出即展示」） | `:872-926` |
| 用户点文件 | 切 file tab；视觉类再自动转 web tab | `:5884-5900` |

→ 后台会话/AI 在别处开的页也会把当前面板切走；切到新会话后 `embedAutoOpenedForSid !== curSid` → 下一次任何 nav 事件又自动打开。**这是现存缺陷，不是标签条引入的。**

---

## 2. 目标模型

### 2.1 数据结构

```ts
type ScopeKey = string            // 首期 = sessionId；无会话工作区 = workspace 路径（D7）
type TabKind = 'file' | 'browser' | 'changes' | 'trace' | 'tasks'

type TabInstance =
  | { id: string; kind: 'file';    path: string; mode: 'diff' | 'source'; toolId?: string; startLine?: number; unread?: boolean }
  | { id: string; kind: 'browser'; url: string; srcPath?: string; viewId?: string; unread?: boolean; dormant?: boolean }
  | { id: string; kind: 'changes'; filter?: 'unstaged' | 'staged' }
  | { id: string; kind: 'trace';   sessionId: string }   // D2：会话锚点
  | { id: string; kind: 'tasks';   sessionId: string }   // D2

// 容器与 SessionView 解耦（§1.2 的 LRU 会吃掉寄生字段）
tabsByScope: Record<ScopeKey, { tabs: TabInstance[]; active: string | null; closed: TabInstance[] }>
```

- `kind` 注册表决定**固定呈现**：`kind → { icon, label(tab), render(tab), singleton?, defaultScope }`（现在的 `primaryTabs/secondaryTabs` 演化为它，`RightPanel.tsx:31-43`）。
- 单例性：`changes` 每 scope 一个；`trace` / `tasks` 每**会话**一个；`file` / `browser` 多实例。

### 2.2 为什么 scopeKey 是参数而不是写死 sid

- 首期 `scopeKey = sessionId` → 行为等价重构（ADR-0 不动、视图池归属不动）。
- 将来若要「文件/浏览器/变更跨会话共享」→ 只改 `scopeKey` 的取值 + 视图池归属字段（`embed.ts` 的 `sid` → `scope`），**标签条/组件/交互零改动**。
- 无会话工作区（D7）= `scopeKey = workspace 路径`，天然落在同一容器里，不需要第二套机制。

### 2.3 交互面（对齐原型）

- 标签条：横向紧凑标签（复用 R4 的 `.browser-pages` / `.browser-page*`，`index.css:5993+`）+ hover ✕ + 溢出 `+N`（`PAGES_MAX_VISIBLE`）。
- `+` 菜单：**审查 / 浏览器**（终端不做 D1；文件由点击文件产生，不进 `+`）。
- `⌄` 下拉：标签总览 + **重开最近关闭**（`closed` 栈）。
- 右键菜单：复用 R4 已实现的「关闭 / 关闭其他 / 关闭右侧 / 复制路径 / 在访达中显示 / 用默认程序打开」（`RightPanel.tsx:1755-1815`）。
- ARIA：`role=tablist/tab` + `aria-selected`（R4 已有）。
- **沉浸模式冲突**：现在 web tab 会隐藏整条面板 tab 行（`tabsHidden`，`RightPanel.tsx:2152`、`:2205`）。统一标签条后必须改：要么取消，要么只在「该 scope 只有浏览器标签」时允许隐藏（否则切不走）。

---

## 3. 对现有功能的影响清单

| # | 模块 | 现状 | 改动 | 风险 |
|---|---|---|---|---|
| 1 | 标签状态容器 | `SessionView.rightTab` | 移到 `tabsByScope`（scopeKey），新增 `activeTabId` | 中：`views` LRU 与标签集解耦，需保证删会话/删工作区时清对应 scope |
| 2 | 切 tab 的调用点 | `setRightTab(kind)` 共 6 处（`AgentTranscript:173`、`Narrative:508/575`、`Statusbar:39`、`ApprovalPanel:27` 读、store 内部 4 处） | 语义从「切换」改为「**打开或激活**」（幂等键：kind + 目标路径/会话） | 中：`ApprovalPanel` 的 `hiddenInRail` 判据要按 **kind** 而非字符串 |
| 3 | 文件标签内容 | `filePreview`/`fileFocus` 全局单例（`:1574-1575`） | 每标签一份（`Record<tabId, FilePreview>` 或按需拉取） | **高**：不改必然串台 |
| 4 | 文件标签模式 | `toolId` 有值 = diff；`mode='source'` = 完整视图（`RightPanel.tsx:1236-1243`） | 落成 `mode` 字段（D5） | 低 |
| 5 | 视觉类文件 | `openFile` 自动转浏览器标签（R4） | 保持（D6），标签标题用源文件名 | 低 |
| 6 | 视图池归属 | entry `sid` + 可见性/激活/去重全按 sid | 首期**不动**（D3） | 低 |
| 7 | 视图池超限 | 抛错（`embed.ts:413-418`） | LRU + 休眠（§5） | **高**：需先补 Go 侧通知（§5.2） |
| 8 | 自动激活 | 4 处进程级判据（§1.5） | 改三档规则（§4） | 中：`activate:false` 要配未读点 + rail 角标，否则产出「看不见」 |
| 9 | 切 scope 的清理 | `clearBrowser()`（`:5220`、`:5369`） | 只清「回退模式页列表/截图」，**不清标签集** | 中 |
| 10 | 快捷键 | 无（右栏无快捷键） | 需新增关闭/切换键位（§7 未决 3） | 低 |
| 11 | 持久化 | 无 | `go-code.sessionTabs`（key = `titleKey(path, sid)`）或 bridge 每工作区文件（先例 `session_pins.go:10-15`） | 中：视图是运行时对象，只能持久化「打开什么」，重启按需重建 |

### 3.1 测试冲击面（必须同步改的验收脚本）

| 脚本 | 依赖点 |
|---|---|
| `e2e/verify_trace_tab.mjs` | `.right .tabs button` 文本匹配「链路」（`:99`、`:259`、`:298`） |
| `e2e/async_task_e2e.mjs`、`e2e/ego_browser_panel.mjs` | `.rbtn` 点击展开（`async_task_e2e:107`、`ego_browser_panel:77`） |
| `e2e/verify_preview_scoping.mjs` + `scripts/test-store-scoping.mjs` | 视图池**按会话隔离**与清理口径（本方案下继续成立，但断言需随容器变化复核） |
| `e2e/verify_pdf_preview.mjs` | T2 视图池+会话归属 / T5 标签条 / T10 关闭即销毁 |
| `e2e/verify_preview_container.mjs`、`verify_md_file_modes.mjs`、`verify_auto_open_visual.mjs`、`verify_artifacts_card.mjs`、`verify_csv_preview.mjs`、`verify_approval_panel.mjs`、`desktop_browser_e2e.mjs` | 读 `rightTab` / 标签条结构 |

对应 npm 脚本：`test:scoping`、`test:store:scoping`、`e2e:container`、`e2e:auto`、`e2e:pdf`、`e2e:artifacts`、`e2e:csv`、`e2e:md-file-modes`、`e2e:trace`。

---

## 4. 自动激活规则（三档）

| 触发 | 允许做什么 | 实现要点 |
|---|---|---|
| **用户显式动作**（点产出卡 / 文件树 / 消息里的「在浏览器打开」） | 展开右栏 + 激活对应标签 | 保持现状（`openInBrowserTab` 的 `activate:true` 路径，`:6269-6363`） |
| **AI 在当前 scope 操作浏览器** | 只「确保标签存在 + 标未读 + 徽标呼吸」；**不切标签、不展开、不收左栏** | 改 `onEmbedAiAction`（`:1006-1017`）去掉 `focusBrowser()`；`onEmbedNav` 的 `real` 判据按 scope 过滤，`embedAutoOpenedForSid` 单值换成 per-scope 集合 |
| **Agent 后台产出** | 只「打开或刷新标签 + 未读点」；不切、不展开 | **反转 §6.10「产出即展示」**：`autoOpenVisualFile`（`:872-926`）改 `activate:false`（D 已拍板） |
| **切会话 / 切工作区 / 新建会话** | **只恢复该 scope 的标签集与激活标签**；无标签 → 空态；**绝不新建、绝不激活** | 切 scope 不触发任何 focus* 路径；`clearBrowser()` 不清标签集 |

可见性兜底：折叠态 rail 已有角标位（`todo-badge` / `agent-badge`，`RightPanel.tsx:2191-2193`）→ 未读点复用它，保证「后台产出」不会变成看不见。

---

## 5. 视图池上限：LRU + 休眠（替代抛错）

### 5.1 三个前提

- **前提 A：只驱逐用户显式看过的标签**（`lastUsed > 0`，`embed.ts:88`、`:426` 已有该语义）；AI 刚开、还没操作的（`lastUsed === 0`）绝不驱逐。
- **前提 B：保护集** —— 当前激活视图、正在被 AI 驱动的 target（attach 中 / 最近 N 秒内有命令）、running 会话的视图、当前 scope 的最后一个视图。先例：`evictViews` 的 busy 会话保活（`useAppStore.ts:4940`）。
- **前提 C（必须先补的缺口）**：见 §5.2。

### 5.2 缺口：销毁视图时**没有**通知 Go 侧

- `closeEmbedTarget`（`embed.ts:487-516`）只 destroy + `pushEmbedNav`（发渲染层）；`broadcastLine` 仅出现在导航/事件路径（`:137`、`:610`），**没有 targetDestroyed**。
- Go 侧 `forwarder.RemoveTarget`（`plugin/browser/cdp/forwarder.go:207`）只由「AI 自己关页」（`:528`）与「re-attach 全量重同步」（`:243-257`）触发。
- 后果：**今天**用户手动关掉一个 AI 正在用的标签，Go 侧页表仍列着它 → AI 的 `list_pages` / `goto` 打到已死 target。LRU 会把它从「用户偶尔手动关」放大成「系统自动关」。
- **修法**：新增一条 `closed/destroyed` 通知（Electron → TCP → `forwarder.RemoveTarget` 广播 `Target.targetDestroyed`），让「标签消失」对 AI 可见且可恢复（AI 看到 target 没了，而不是命令超时）。

### 5.3 休眠语义（「只影响展示」的字面实现）

驱逐时销毁 `WebContentsView`（释放渲染进程）但**标签条上保留**（灰态 + 休眠标记），点回来重建。代价：页面状态（表单/滚动）丢失 → 必须显式标记。有保护集 + 休眠后，上限可从 8 提到 12~16。

---

## 6. 分期与验收

| 阶段 | 内容 | 验收 |
|---|---|---|
| **1. 标签模型** | scopeKey + kind 注册表 + 多实例 + 关闭/重开；scope = session；**行为等价重构** | 现有 e2e 全绿（`npm test` + `e2e:container` / `e2e:auto` / `e2e:pdf` / `e2e:artifacts` / `e2e:md-file-modes` / `e2e:trace`） |
| **2. 文件标签隔离** | 拆 `filePreview` 单例；diff / source 两种 mode（D5） | 新增 e2e：两个文件标签并存互不覆盖；diff 与完整预览各自正确 |
| **3. LRU + 休眠** | 保护集 + 休眠 + **Go 侧 targetDestroyed 通知**（§5.2） | 新增 e2e：连续打开 > 上限不报错；被驱逐标签可点回重建；**AI 在驱逐后 `list_pages` 不再列出死 target** |
| **4. 自动激活收口** | §4 三档 + 未读点/rail 角标 | 新增 e2e：切会话/切工作区/新建会话**不**自动打开任何标签；Agent 产出只加未读点不抢视线 |
| **5. 持久化** | `go-code.sessionTabs`（或 bridge 每工作区文件） | 新增 e2e：重启后标签集与激活标签恢复；视图按需重建 |

---

## 7. 未决

1. **`+` 菜单是否要「文件」入口**（打开一个不带路径的文件标签 = 需要先选文件）。倾向：不要，文件由点击产生。
2. **视觉类文件的双标签观感**：文件标签 → 自动转浏览器标签（D6）在统一标签条下是否可接受；若不可接受则回到「文件标签内嵌原生视图」（复杂度高）。
3. **关闭/切换快捷键**：`⌘1..9`、`⌘W` 均被占用 → 建议 `⌘⇧[` / `⌘⇧]` 切换、`⌘⇧W` 或中键关闭。
4. **持久化通道选型**：localStorage（照抄 `persistProjects`）vs bridge 每工作区文件（照抄 `session_pins.go`）。倾向 localStorage（标签是纯 UI 偏好，不参与 AI 路由）。
5. **上限新取值**：12 / 16（依赖休眠实现）。

---

## 8. 证据索引（本文引用到的关键位置）

| 主题 | 位置 |
|---|---|
| `RightTab` 枚举 | `desktop/app/src/store/useAppStore.ts:13` |
| `rightTab`/`rightExpanded` 字段 | `useAppStore.ts:496-497`；默认 `:579-580`；镜像 `:625-626`、`:678-679`、`:692-696`、`:699-701` |
| 视图缓存 LRU | `useAppStore.ts:4930`、`:4937-4954` |
| 持久化现状（无右栏） | `useAppStore.ts:1330-1331`、`:1372-1373`；`store/checkpoint.ts` 头注释 |
| `filePreview`/`fileFocus` 全局单例 | `useAppStore.ts:1574-1575`、`:5907` |
| 变更 tab 双轨数据源 | `RightPanel.tsx:341-347`；`useAppStore.ts:1317-1318`、`:5805-5809` |
| 文件标签 diff / source | `RightPanel.tsx:1236-1243` |
| 标签条渲染 | `RightPanel.tsx:31-43`、`:2120-2228`；样式 `index.css:4575-4625` |
| 浏览器标签条（R4，可复用） | `RightPanel.tsx:1755-1815`；样式 `index.css:5993+` |
| 沉浸模式 `tabsHidden` | `RightPanel.tsx:2152`、`:2205` |
| 视图池 / 上限 / 去重 / 可见性 / 几何 | `desktop/app/electron/embed.ts:99-112`、`:59-62`、`:413-418`、`:391-412`、`:303-310`、`:538-544` |
| 跨会话激活拒绝 | `embed.ts:522-523` |
| 切会话保活 | `embed.ts:657-663` |
| 自动激活 4 处 | `useAppStore.ts:755`、`:770-776`、`:872-926`、`:974-1005`、`:1006-1017` |
| 删会话/删工作区清视图 | `useAppStore.ts:5417-5428`、`:5461-5472` |
| Go 侧 target 表 | `plugin/browser/cdp/forwarder.go:158`、`:207`、`:243-257`、`:528`；`desktop/bridge/browser_embed.go:262-286` |
| 每工作区文件先例 | `desktop/bridge/session_pins.go:10-15`、`desktop/bridge/main.go:2775-2781` |

---

## 9. 阶段 0：实施前核实结论（2026-09-20）

补齐 §1 之外的实现细节，全部为代码事实核对结果。

### 9.1 布局与高度链（最高风险项，已定案）

现状 DOM 层级（`RightPanel.tsx:2203-2228` 的 flex 列）：

```
.right-body                       ← flex:1 / overflow-y:auto / padding:10px（index.css:4632）
  └─ BrowserTab
       └─ .file-preview           ← flex column（index.css:5970）
            ├─ .fp-hd             ← 浏览器自己的工具条（tag/状态/刷新；tabsHidden 时 display:none）
            ├─ .browser-pages     ← R4 标签条（role=tablist）
            └─ .browser-shot      ← **原生视图 rect 的唯一来源**（RightPanel.tsx:1828 + :1535-1543）
```

**结论**：统一标签条放**面板级**（`.tabs` 位置，在 `.right-body` **之外**），因此：
- `:has(.browser-shot)` 高度链（`index.css:4642-4648`）与 rect 测量**不受影响**——`.browser-shot` 位置不动即可。
- `.fp-hd` 保留，语义变为**浏览器标签自己的工具条**（与原型一致：截图里「审查」标签下方是自己的工具条）。
- **沉浸模式（`tabsHidden`）必须取消**：它隐藏的是面板级 tab 行（`RightPanel.tsx:2152`、`:2205`）——统一标签条后隐藏它就等于"切不走了"。☰ 按钮随之退役。

### 9.2 双 id 空间（回退 / window 模式）

`pickPage` / `closePage`（`RightPanel.tsx:1628-1645`）按 `embedLive` 分流：
- live（原生视图池）→ `embedActivate('tN')` / `embedCloseTarget('tN')`
- 回退（MCP 页列表）→ `setShotPage(number)` + `fetchBrowserShot(number)` / `closeBrowserPage(number)`

**结论**：浏览器标签实例必须**自带 id 空间**：`{ viewId?: string; pageId?: number }`，不得在渲染时按全局 `embedLive` 反推（否则一个标签在两种模式间会串 id）。

### 9.3 弹层清单（正交，不改）

`uiModalOpen = inSettings || inSkills || inCron || showNewWsModal || !!question || !!release`（`RightPanel.tsx:1406-1414`）——只用于「有 DOM 弹层时隐藏原生视图」（原生层级高于 DOM），与标签模型正交。

### 9.4 快捷键占用（内容标签不得使用）

`App.tsx:73-90`：`⌘B` 左栏、`⌘\` 右栏、`⌘1..5` 最近会话（注意是 1..5，非 1..9）。内容标签的关闭/切换键位推迟到阶段 4 决定。

### 9.5 mock 传输层

`transport/mock.ts` **不实现**任何 embed API（无 `embedShow/embedStatus/embedOpenTarget/embedSetSession`）；右栏全部经 `window.desktop?.` 可选链降级（20 处）。→ 单测可直接 shim `window.desktop`（先例 `scripts/test-store-scoping.mjs:40-70`）。

### 9.6 标签入口全清单（阶段 1 必须覆盖）

| 类别 | 入口 | 现状调用 |
|---|---|---|
| 用户点击 | `AgentTranscript.tsx:170-173` | `openFile(path, undefined,'source')` / `openFile(path, toolId)` / `setRightTab('git')` |
| 用户点击 | `AgentTranscript.tsx:218` | 产出卡 → `openFile(path, undefined, 'source', startLine)` |
| 用户点击 | `Narrative.tsx:409-418`、`:505-508`、`:557-575` | 同上三种模式 |
| 用户点击 | `RightPanel.tsx:1072`、`:1163` | 文件栏内部 → `openFile(item.path)` |
| 用户点击 | `Statusbar.tsx:39` | `setRightTab('git')` + `setRightExpanded(true)` |
| 用户点击 | `RightPanel.tsx:2187`、`:2207`、`:2213` | rail / 标签条 |
| 自动 | `useAppStore.ts:6223` | `focusBrowser()`（收左栏，Browser Use 接管语义） |
| 自动 | `useAppStore.ts:6232`、`:6288`、`:6386` | `focusBrowserForPreview()`（不收左栏） |
| 自动 | `useAppStore.ts:6242` | `focusBrowserForUse()` |
| 自动 | `useAppStore.ts:5885` | `openFile` → `setRightTab('file')` |
| 自动 | `useAppStore.ts:986`、`:996`、`:1014` | `focusWebTabIfClosed()` ← `onEmbedNav` / `onEmbedAiAction` |

### 9.7 右栏宽度的程序化 resize 边界

- 程序化 resize **不会**收左栏：`App.tsx:138-150` 用 `meta.isUserInteraction` 门控（2026-09-18 实测缺陷的修复）。
- 但 `setRightTab` 里的「切到文件标签给 60% 宽」规则（`useAppStore.ts:6210-6217`）挂在**切换动作**上 → 阶段 1 必须改成 **kind 感知且只在新建标签时触发**，否则每次激活文件标签都会重置宽度。

---

## 10. 实施记录

### 10.1 阶段 1 + 1.5 已交付（2026-09-20）

**做了什么**

| 项 | 内容 | 位置 |
|---|---|---|
| 标签模型 | `tabsByScope: Record<ScopeKey, TabSet>`（`tabs/activeId/closed`）+ `openTab/activateTab/closeTab/reopenTab/syncTabsToRightTab` | `store/useAppStore.ts` |
| 双轨兼容 | `rightTab` 仍是「显示哪一类」的权威字段（e2e 直接读写它、且经 SessionView 参与会话恢复）；标签集**单向收敛**到它（`syncTabsToRightTab`，已一致时不 set 以防 effect 自激） | 同上 |
| 面板级标签条 | `.rtabs`：⌄（重开已关闭）/ 标签实例（图标+名+✕）/ ＋（新建）/ 收起。折叠态 rail 保持 5 类固定入口（启动器语义，也是 e2e 的点击入口） | `RightPanel.tsx`、`index.css` |
| 浏览器页面 → 面板级标签（阶段 1.5） | 一页一标签：`syncWebTabs(source, items, activeKey)` 声明式收敛（native='tN' / mcp=数字 pageId 两套 id 空间）；**内层 `.browser-pages` 标签条退役** | `store/useAppStore.ts`、`RightPanel.tsx` |
| 右键菜单上移 | 关闭 / 关闭其他 / 关闭右侧 / 复制路径 / 在访达中显示 / 用默认程序打开 → 挂到面板级标签（`.rtab` 的 `data-kind`/`data-view` 供测试与 CSS 区分类型） | `RightPanel.tsx` |
| 浏览器工具条合并 | 原来两行（`.fp-hd` 状态行 + `.browser-toolbar` 导航行）合并为**一行**，且按预览/网页分形态：预览（`file://`/`data:`）→ 类型图标 + 解码文件名 + 状态点 + 刷新，**不摆 ←/→/地址栏**（用户口径：「如果是浏览器Tab，是不是就不需要这个头部了」）；真网页保留完整导航 | 同上 |
| 沉浸态语义修正 | M6 收的是「面板 tab 行」，阶段 1 起标签条常驻 → 改为只收**浏览器自己的工具条**，且**默认非沉浸**（☰/▴ 切换） | 同上 |
| 动画期 rect 竞态（顺带修） | `getBoundingClientRect()` **含 transform**，而 `.right-body` 有 180ms 入场动画（`slideRight: translateX(18px)→0`）→ 动画期间量到的是「动画中的位置」（实测随相位差 1~18px）。修法：`document.getAnimations()` 找作用于 **el 或其祖先** 的运行中动画，结束后补量；`animationend/start` 挂在真正被动画的祖先上 | 同上 |

**测试**

| 层 | 内容 | 结果 |
|---|---|---|
| L1 钉桩（新） | `scripts/test-right-panel-tabs.mjs`（`npm run test:right-tabs`）：A–J 段钉住**改动前**的既有语义（作用域/回落/openFile 三件套/focusBrowser 家族/rightSize 60% 规则/LRU 保活），K–L 段钉住标签模型与 web 标签同步 | **87 项全绿** |
| 回归 | `test:store`、`test:store:scoping`(12)、`e2e:pdf`、`e2e:container`、`e2e:auto`、`e2e:scoping`(32)、`e2e:md-file-modes`(29)、`e2e:artifacts`(87) | 全绿 |
| 视觉 | `e2e/shots_right_tabs.mjs` → `e2e/_rtabs_shots/*.png`（默认/五标签/＋菜单/重开菜单/空态/折叠 rail） | 人工复核通过 |

**顺带发现的两处既有缺陷（非本次引入）**

1. `e2e:trace` 的「实时链路含 edit_file 行」**在改动前就失败**（用 `git stash` 对照验证过）——mock 流程相关，与本方案无关，尚未修。
2. 上述动画期 rect 竞态：视图最多会偏 18px（已修）。

**未做（有意）**

- `file` 标签仍是**每 scope 一个实例**（多实例要等阶段 2 把 `filePreview`/`fileFocus` 全局单例拆掉）。
- 未做持久化（阶段 5）、未做视图池 LRU/休眠（阶段 3）、未做自动激活三档收口（阶段 4，但 `syncWebTabs` 已按「不抢视线」实现：用户不在 web 标签时后台页面只加标签不改激活）。

### 10.2 用户实测后的两处修正（2026-09-20 当天）

| 反馈 | 根因 | 修法 |
|---|---|---|
| 「如果是浏览器Tab，是不是就不需要这个头部了」 | 同一个东西出现三处：面板标签名 + 地址栏（还是百分号编码的临时路径）+ 内层标签条 | ① 内层标签条退役（页面提升为面板级标签，见 10.1）；② 工具条两行合并为一行；③ 预览（`file://`/`data:`）**整层不渲染**——文件名已在面板标签上，刷新收进标签右键菜单 |
| 「红线这一层完全没必要了」（预览的工具条行） | 同上：面板标签已写文件名，再来一行是重复 | 同上 ③：`{nativeViewMode && !toolbarHidden && !isPreviewUrl && ...}` |
| 「点击加号时…被下方预览区域给挡住了」 | **原生视图恒合成在窗口 DOM 之上**（原生层级 > DOM，CSS z-index 压不住）——菜单是 DOM，必然被预览画面盖住 | 把「面板级下拉菜单打开中」纳入原生视图的隐藏闸门（与设置/提问弹窗同一机制）：`menuOpen={!!menu || !!ctx}` → `embedHide()`。回归断言见 `e2e:pdf` 的 **T14**（菜单开→visible=false；关→恢复 true），并留截图 `04-add-menu.png` |

### 10.4 用户实测后的两处修正（2026-09-21）

| 反馈 | 根因 | 修法 |
|---|---|---|
| 「右侧侧边栏点击加号、选择网页无效」 | 新建的 web 标签 `makeTab` 认下了 `embedNav.activeId` —— 而池里**常驻**一个 `about:blank` 自举载体且它就是 activeId，`onEmbedNav` 喂给 `syncWebTabs` 的 items 却**把它过滤掉**（`url && url !== 'about:blank'`）→ 标签被当「这个页面已经关了」删掉，`activeId` 顺延回上一个标签。真机 trace：点击后 4ms 内 `tabs → [web:t1] → [activity]` + `rightTab` 顶回 `activity`（用户看到的就是「点了没反应」） | `makeTab` 只认**真实页面**（新增 `activePageIdOf`，判据与 items 过滤逐字同口径）；认不出就是**空 web 标签**（无 viewId）—— 它被 `syncWebTabs` 的收养分支接住，池里出现第一个真实页面时复用它的标签实例。回归断言见 `scripts/test-right-panel-tabs.mjs` 的 **Lb 组**（L16–L27）与 `e2e:pdf` 的 **T15**（先红后绿：旧实现下 L17/L19、T15 全红） |
| 「图片、网页等无法正常加载」（截图：整块面板只剩左上角一小块、页面底色铺满） | 两段叠加：① **制造**——收起右栏时 BrowserTab 卸载，ResizeObserver 对**已脱离文档**的元素还补发一次「尺寸 0」的通知，渲染层把这次「量不到」钳成 `Math.max(80, …)` 下发 → `embed:resize` 把**所有**原生视图缩到 80×80，`Emulation.setDeviceMetricsOverride` 把**当前显示那一页**的排版视口钉在 80×80（页面按 80px 宽排版 = 只剩左上角一小块）。真机实测：收起后该页 `innerWidth` = 80（面板宽 877）。② **不自愈**——视口对齐只在尺寸变化时下发、且只打给主进程当前 `activeId`：同尺寸切换标签既不重挂载组件也不触发 ResizeObserver → 错视口一直留着（点标签、刷新页面都回不来）。真机复现的关键是 ①→ 收起时把「量不到」下发成了 80×80 | ① 渲染层「量不到就什么都**不**下发」（新增 `embedRectOf`：脱离文档/零尺寸 → undefined；`sync()` 与可见性 effect 据此跳过，show 路径交给主进程的 `isValidEmbedRect` 兜底）② 对齐收口到主进程的「谁成为面板当前显示的页」两条路径：`activateEmbedTarget` 与 `showEmbed` 各调一次 `alignEntryViewport`（矩形用渲染层量测值 / `cssBoundsOfPanel()`，免时序竞态）。回归断言：**`e2e:container` 的 T16**（tracked + 进 CI；旧实现下两条红：视图 bounds=80×80、页面视口=80×80）与 `e2e:pdf` 的 T16a/T16b |

### 10.3 待拍板：视觉类文件的「双标签」

现状：点一个 PDF/PNG → **两个标签同名**（`file` 标签 + `web` 标签）。因为 R4 定了「可视类文件的内容只有内嵌浏览器能渲染」，而 `openFile` 又先切到 `file` 标签（它只剩一行状态「内容已在右侧预览中打开」）。

| 选项 | 效果 | 代价 |
|---|---|---|
| **A（建议）** 可视类文件**不建 file 标签**，直接开预览标签 | 一个文件一个标签，与「打开即见内容」一致 | `e2e:pdf` 的 T4b（「文件栏留一行状态」）要改；点文件树看 PDF 时不再进文件面板 |
| B 保留双标签，但给 file 标签加区分（前缀/图标） | 改动最小 | 同一个文件仍占两个标签 |
| C 维持现状 | — | 用户看到两个同名标签 |

---

### 10.4 阶段 4 已交付：自动激活收口（2026-09-20）

**问题（用户口径）**：「切会话、切换工作区、新建会话……也会自动打开右侧浏览器 Tab 很别扭」——
四处自动展开/切标签的判据都是**进程级**的「有没有页面 / 有没有动作」，而不是「这个页面属不属于
你当前看的会话」。

**做了什么**（三档规则落地，全部在 `src/store/useAppStore.ts`）

| 档位 | 改法 | 位置 |
|---|---|---|
| AI 在**当前 scope** 操作浏览器 | `onEmbedAiAction` **去掉** `focusWebTabIfClosed()`（旧行为走 `focusBrowser`：展开 + 切 web + **收左栏**）；只留 `embedAiActive` 呼吸徽标 + `embedLastAction` | `:1143-1162` |
| AI / 后台产出 的**首个真实页面** | `onEmbedNav` 的 `real` 判据**按 scope 过滤**（`!tp.sid \|\| tp.sid === navSid`，与 `syncWebTabs` 逐字同口径）；单值 `embedAutoOpenedForSid` → 按视图的 `embedAutoOpenedViews`；**只有** `previewOwnedViews` 命中的视图（用户显式预览）才展开 | `:1109-1141` |
| Agent 后台产出可视文件 | `autoOpenVisualFile` 改 `activate:false` + `quiet:true`（**不展开、不切标签**，只加/刷新标签 + 未读点）；函数改 async 以便未读点落地后再返回 | `:1016-1082` |
| 后台打开的视图 | `rememberPreviewView` 的 `activate:false` 分支**不再**把视图登记进 `previewOwnedViews`（登记 = 它的首个真实页面会展开面板，正是要修的缺陷），只标未读 | `:919-936` |
| 可见性兜底 | 折叠态 rail 的「网页」按钮加未读角标（`.unread-badge`，复用 `.todo-badge` 定位）——否则「不抢视线」会退化成「永远不知道有产出在等」 | `RightPanel.tsx:2166-2184`、`index.css:1029-1037` |

**语义反转（用户 2026-09-20 口径，明确推翻 §6.10 二次修正的「产出即展示」）**

- 二次修正的理由（「原实现没有任何一步会把它显示出来」）成立，但代价是**进程级抢视线**：
  不管用户在哪个会话、正在看什么，一次产出就把界面切走。
- 新口径：后台产出 = **标签 + 未读点**；看不看、什么时候看由用户决定。
  `e2e:auto` 的 A2 断言由「切到 web tab + 展开」改为**更强**的形态：
  「标签存在 + 有未读点 + **没有**抢激活 + **没有**展开 + **没有**收左栏」。
- 用户显式动作档**完全不变**：点产出卡 / 文件树 / 消息里的「在浏览器打开」→ 展开 + 激活
  （`e2e:pdf` T4b/T5 钉住；`e2e:auto` 新增 A4 覆盖 `activate:true` + nav 兜底路径）。

**测试**

| 层 | 内容 | 结果 |
|---|---|---|
| L1 钉桩 | `scripts/test-right-panel-tabs.mjs` 新增 **M 组（17 项）**：切会话/切工作区/新建会话三条路径上用**间谍**钉「一次 focus* 都不调」，且只恢复该 scope 的标签集与激活标签（新 scope 不预先建标签集） | **114 项全绿**（原 97 + 17） |
| e2e | `e2e/verify_auto_open_visual.mjs` 重写：探针注入**假视图池 + 可驱动的 nav/AI 回调**（在 store 模块加载前，否则 `subscribeEmbedEvents` 永不订阅 → 断言空转），A2/A3 钉新语义、A4 钉用户动作档、S2/S3 钉「别的会话的页面/AI 动作不改当前面板」 | ALL PASS |
| 回归 | `test:store`、`e2e:pdf`（含 T4b/T5/T14）、`e2e:scoping`(32)、`e2e:container`（含 T3c/T6/T13） | 全绿 |

**顺带修的一处既有缺陷**：`embedAutoOpenedForSid` 是**单值**（只记得最后触发的会话）→ 切到任何
别的会话后 `!== curSid` 立刻成立，下一次任何 nav 事件都会再展开一次当前面板。这正是用户
「切会话很别扭」的一半根因（另一半是 `onEmbedAiAction` 的 `focusBrowser`）。

### 10.5 阶段 5 已交付：标签集持久化（2026-09-20）

**做了什么**（全部在 `src/store/useAppStore.ts` + `RightPanel.tsx` 的渲染层）

| 项 | 内容 | 位置 |
|---|---|---|
| 通道 | localStorage `go-code.sessionTabs`（方案 §7 未决 4 定案：标签是纯 UI 偏好，不参与 AI 路由，不新增 bridge 文件通道） | `useAppStore.ts` 的「阶段 5」段 |
| 存什么 | 每个 scope：`tabs`（**剥掉运行时字段** `viewId`/`pageId`/`openSeq`）+ `activeId`；**web 标签额外存 `url` 快照**（+ `srcPath`）；`closed` 栈不存（跨重启没有意义） | 同上 |
| web 的 url 从哪来 | 持久化**当时**反查并快照：① 标签自带快照 → ② `embedNav.targets`（按 `viewId`）→ ③ `browserPages`（按 `pageId`）；**反查不到就丢弃该标签**（不恢复点不开的死标签） | `webUrlSnapshot` / `toPersistedTab` |
| 视图按需重建 | 恢复出来的 web 标签**不带池身份**（主进程 `tN` 是运行时 id，重启后从 t1 重来，存了会指向另一个页面）→ 落「待重建」态；`syncWebTabs` 对它**不判死**（重启后第一帧 targets 是空的）也不当作空标签收养；只有它成为「当前正在显示的内容」时才由 `rebuildWebTab` 建视图（有 `srcPath` 走 `openFile`＝预览路径，office 会重新转换；否则按 url 开），建出来的视图由 `syncWebTabs` 的**认领**逻辑接回原标签实例 | `isPendingWebTab` / `rebuildWebTab` / `syncWebTabs` / `RightPanel` 的 effect |
| 恢复 ≠ 激活 | 恢复只灌 `tabsByScope`（首帧就位）与 `rightTab` 的**反向收敛**（标签集比「会话 view 的默认 rightTab」权威，见 `TabSet.restored`）；不展开右栏、不收左栏、不调任何 `focus*`、不建任何视图 | `loadPersistedTabs` / `syncTabsToRightTab` |
| 上限 | 每 scope 20 条（丢最旧的，**激活标签必留**）；scope 数 40（最近会话优先）；单个 url > 2 KB 丢弃（`data:` 整段 HTML 的配额护栏） | `persistTabsNow` |
| 写入时机 | **store 订阅**（`tabsByScope` 引用变化）+ 300ms 防抖 —— 不逐个 action 插桩（八条改写路径）；关窗前只补**在途**的待写 | 文件末尾的 `subscribe` |

**测试**

| 层 | 内容 | 结果 |
|---|---|---|
| L1 钉桩 | `scripts/test-right-panel-tabs.mjs` 新增 **N 组（56 项）**：写入形态（无 openSeq/viewId/closed）、重启恢复（顺序/类型/diff-source 模式/激活标签/不展开不收左栏）、反查失败即丢弃、空 targets 不删恢复标签、认领不多开标签、坏数据四种形态、上限淘汰序与激活标签保留、跨 scope 不串、scope 数封顶。**「重启」= 清 require 缓存重新加载 store bundle**（模块加载时同步读 localStorage） | **202 项全绿**（原 146 + 56） |
| e2e（新） | `e2e/verify_session_tabs_persist.mjs`（`npm run e2e:session-tabs`）：探针页 + **`page.reload()`** 模拟重启（假视图池 reload 后归零、id 序号从 t1 重来）。P2 钉恢复且不展开/不收左栏；**P3 反向钉「视图池为空、空 targets 不删标签」**；P4/P5 钉用户要看它时才建视图（恰好一个、url = 快照、接回原标签、不多开）；P6 脏数据；P7 跨 scope | ALL PASS |
| 回归 | `test:store`、`e2e:container`、`e2e:pdf`（T4b/T5/T14）、`e2e:scoping`(41)、`e2e:auto`、`e2e:md-file-modes`(62)、`e2e:artifacts`(87) | 全绿 |

**为什么不用真 Electron 二次启动**：本阶段逻辑全在渲染层（主进程只被**调用**），`reload` 会重新
加载整个渲染层模块图 —— 在被测范围内等价，且能精确控制假视图池（真 Electron 里视图池由 Chromium
决定，反而验不到「恢复时一个视图都没建」这条反向断言）。

**已知取舍**：`TabSet.restored` 是**一次性**的「这个 scope 的标签集自本次启动起还没被用户碰过」
标记 —— 期间标签集比 rightTab 权威（否则「重启后切走再切回来」会被默认的 activity 顶掉）。任何
显式标签动作（含 `setRightTab`）都会清掉它，此后 rightTab 恢复权威。

---

## 11. 阶段 3 设计结论（2026-09-20，只读核实，未实施）

### 11.1 缺口 A：销毁视图不通知 Go 侧（必须先补）

- 现状：`embed.ts:487-516` `closeEmbedTarget` 只 `views.delete` + destroy + `pushEmbedNav()`（推渲染层），**无任何 `broadcastLine`**。`broadcastLine` 全仓仅两处：`:137`（adapter emit）、`:610-613`（导航 state）。
- `close` 消息**不能反向复用**：它是 Go→Electron 的请求/应答（带 rid，`multi_tcp.go:207-226` → Electron `:907-912` 回 `closed`），复用会与 `pendCtl` 的 rid 匹配打架。
- `state` 也**不能复用**：Go 侧收到走 `EnsureTarget + NotifyTargetChanged`（`browser_embed.go:266-271`）→ 死 target 被**复活**并广播 `targetInfoChanged`，语义相反。
- **建议做法**：`closeEmbedTarget` 里加一行 `broadcastLine({ type: 'destroyed', targetId: id })`；Go 侧 `multi_tcp.go` 的 `readLoop` switch 加 `case "destroyed"`（新增 `OnDestroyed func(string)` 字段），在 `browser_embed.go:266` 与 `OnState` 并列注入 `m.embed.fwd.RemoveTarget(id)`。
- 两端 switch 都**没有 default 分支** → 新消息类型天然向后兼容。
- ⚠️ `destroyEmbed()`（`:678-705`）**不要**广播：它在 `:699` 先 `stopDebuggerProxy()` 杀掉所有 socket，广播必然落空；且 `:698` 会把 `targetSeq` 复位（会出现新旧 tN 撞号窗口）。全量清理由下次 attach 的 `ResyncTargets` 兜住。

### 11.2 缺口 B（**真 bug**）：`RemoveTarget` 只发 `targetDestroyed` 不够

- `forwarder.go:207-238` 的 `RemoveTarget` 目前只广播 `Target.targetDestroyed`。
- 但 Puppeteer 的 `CdpTargetManager` 对 **page 类型**的 targetDestroyed **不做清理**（`vendor/chrome-devtools-mcp/build/src/third_party/index.js:64176` 注册 → `:64269-64279` 仅 `service_worker` 才 `targetGone`）。
- 真正触发清理链的是 **`Target.detachedFromTarget`（带 sessionId）**：`index.js:64224-64226` → `:64404-64414` `#onDetachedFromTarget` → `targetGone` → `:64662-64668` `_isClosedDeferred.resolve()` → `McpContext.js:94-111` 释放页面。
- 而 `Page.close()`（`index.js:62796-62800`）`await this.#tabTarget._isClosedDeferred` → **必然挂满 300s**。
  **这就是运行日志审计（HARNESS_AUDIT.md:385）记的 `browser_close_page` 3/3、p50=max=300s 的根因**（另有 `RAW_ANALYSIS.md:280-285` 的 `transport error: context deadline exceeded`）。
- **修法**（Go 侧约 15 行）：`RemoveTarget` 在 `targetDestroyed` 之后**再发**一条 `Level:"browser"` 的 `Target.detachedFromTarget{ sessionId, targetId }`；**sessionId 为空时不发**（无会话可解绑）；保持 `:223` 的幂等早退。
- 修复后 AI 侧的表现：`page.isClosed()` 为 true → `McpContext.getSelectedMcpPage()` 直接抛「The selected page has been closed. Call list_pages to see open pages.」——这才是「AI 看见 target 消失」的正确形态（对比：完全不通知时 `list_pages` 仍列幽灵页且 MCP 不自愈；只 RemoveTarget 时命令报 `sessionId 不存在`，可诊断但不解除 Puppeteer 的等待）。

### 11.3 LRU 保护集：Electron 侧字段够用，缺一个「最近命令时间」

- 现有：`attached`(:68)、`sid`(:87)、`lastUsed`(:88，0 = 从未显式看过，`pickSessionView` 已在用)、`shown`(:89)、`srcPath`(:90)。
- **缺**：`lastCmdAt`。埋点最省：`embed.ts:808-817` 的 `cmd` case 里 `e.lastCmdAt = Date.now()`（Electron 已能看到 100% 的 AI 命令，不需要 Go 反向推送）。
- Go 侧**没有**可用的「AI 在用」信号：`Target.Attached` 因 `emitAutoAttachEvents`(:680-693) 给所有 target 补发 attachedToTarget 而恒为 true；`targetState.sessionID` 同理。
- 白捡的信号：`Target.activateTarget` 现在是 no-op（`forwarder.go:419-427`），而 chrome-devtools-mcp 的 `select_page`+`bringToFront` 正好走它 → 在那里记时间 = 「AI 选中了这页」。

### 11.4 休眠：必须保住同一个 `tN` id

- 渲染层 web 标签的身份就是 `viewId`（`useAppStore.ts` 的 `makeWebTab`/`webTabKey`），而 `syncWebTabs` 会把「不在 nav targets 里的 key」当**关闭**删掉 → 休眠条目若从 `targets` 消失，标签会被同步逻辑删掉。
- 所以：休眠条目**仍要以原 id 出现在 `embed-nav` 的 targets 里**，带 `sleeping: true`；`activateEmbedTarget` / `reloadEmbedTarget` / `navigateEmbedTarget` 里先重建再动作。
- 实现形态建议：另开 `dormant: Map<string, DormantInfo>`（不要给 `EmbedEntry.view` 置空 —— 那会让 `targetInfos`/`applyPanelVisibility`/`applyViewBounds`/`embedStatus`/`navigateEmbed` 等 ~10 处都要加分支）；`targetInfos()` 合并输出，`views.size` 仍等于「活渲染进程数」，上限口径清晰。
- 重建用 `createEntry`（同步）+ 异步 `loadURL`；**不要**复用 `openEmbedTarget` 的默认去重路径（视图已销毁 → 新 id）。
- 代价：丢表单/滚动/JS 堆 + 一次完整加载（file:// 便宜、外网页贵）；标签上给灰态/「已休眠」标记。重建后不会重复抢焦点（`embedAutoOpenedForSid` 是每会话一次且已被消耗）。

### 11.5 风险清单（会被阶段 3 打破的 e2e）

| 脚本 | 会红的原因 |
|---|---|
| `e2e:container` T4/T7（`verify_preview_container.mjs:486-524`、`:526-546`） | **按设计**与 LRU 对立：T4 断言「超限抛错」且「第一个创建的视图未被驱逐」；T7 断言「撞上限时的静默/toast 口径」。必须改断言（新语义：驱逐 + 可点回重建 + 重建失败要 toast） |
| `e2e:container` T3/T3b/T11/T12/T13 | 依赖「标签 = 稳定 tN」：休眠换 id → 复用/刷新全红；休眠后「DOM 仍可达」必然失效 |
| `e2e:scoping` A4(`:193`)/A9(`:259-261`) | A4 钉「切会话不销毁视图」；A9 钉「关闭后 target 从 targets 消失」→ 休眠条目仍在 targets 里，计数断言必须区分 sleeping |
| `e2e:auto`(`:144-154`) | 驱逐刚自动打开的视图 → 「产出即展示」变「产出即消失」 |
| `e2e:pdf` T14 | 休眠重建若绕过 `embedShow/embedHide` 闸门 → 原生视图盖住菜单 |
| `trigger_browser_tool.mjs` / `desktop_browser_e2e.mjs` / `bridge_browser_e2e.py` | 若 LRU 驱逐 `lastUsed===0`（AI 刚开）的 target → `new_page` 返回的 targetId 立刻失效 |
| `test:right-panel-tabs` L 组 | 休眠条目不在 targets → 池快照断言红 |

**实施顺序建议**（TDD 先红）：① 先补 11.1 + 11.2（通知 + detach 广播，可独立验收）；② 再写 `e2e:container` T4/T7 与 `e2e:scoping` A9 的**新断言**并确认它们变红；③ 最后做 LRU + 休眠。

---

## 12. 阶段 6 已交付：多标签与面板插件（2026-11-13）

原计划里的「阶段 6」以**创造模式 V0.1** 的形式落地，看板与逐轮记录见 `docs/creator-mode-v0.1/`（`progress.html` 是权威状态）。与本文件直接相关的部分：

| 项 | 结果 |
|---|---|
| 标签条交互 | `TABS_MAX_VISIBLE = 5` 真生效（此前是死常量、实际 `overflow-x:auto`）；`+N` 溢出下拉，**活动标签始终可见**；右键补 `关闭全部` / `重开`；`⌃⇥` / `⌃⇧⇥` 回绕；中键关闭；空态改能力面板 |
| 每面板边界 | `SurfaceBoundary` 只包内容区；错误态按 `tabId` 复位（**不用 key**，保住 R06c 的原地复用：实例号 `1→1→1`）；连续 3 次失败自动禁用 + 墓碑（重新启用 / 移除标签） |
| 标签种类 | `TAB_KINDS` 删除，白名单回到注册表 `BUILTIN_KINDS`；`makeTab` 的 kind switch 改 `provider.defaultSpec`；标签图标改 `provider.icon` |
| 持久化 | 阶段 5 的 schema **未升版本**；新增 `plugin:<id>` 成员（`params` ≤ 4 KiB）；恢复先墓碑、插件注册后自动变回面板 |
| 插件面板 | `~/.go-code/surfaces/`（index / `.bak` / `.trash` / `audit.jsonl`）+ `hai-surface://` 字节通道 + `host.command` 四命令只读白名单 + 设置「面板插件」页 |
| 清理债 | `rightTab` 字段仍在（src 47 行 / e2e 55 处）→ R11b；`kind === '` 仍是 16 行（14 处是标签形状/探针契约）→ R11c |

**与阶段 3（LRU / 休眠）的关系不变**：本节交付没有动视图池语义，11.1–11.5 的缺口与风险清单仍然有效。
