# 创造模式 V0.1（Surface 版）—— 与右侧栏容器化改版合并设计

> 状态：设计稿（未实施）　作者：调研 + 代码勘察　日期：2026-11-11
> 前置文档：`docs/RIGHT_PANEL_TABS_PLAN.md`（标签模型，阶段 1-5 已交付）、`docs/DESKTOP.md` §5.7（布局与聚焦模式）
> 本文不改动已拍板的 7 条决策（D1-D7）与三条硬规则（快捷键不重叠 / 生命周期不共享 / 可见性判定权在渲染层）。

---

## 0. 一页摘要

**右栏改版的交付物不是一个"更好看的标签条"，而是一张 `Surface` 注册表；创造模式 V0.1 就是这张注册表的第一个外部消费者。**

- **右栏是唯一的外部能力工作台**：三态布局（收起 → 中间占满 / 各半 → 左栏自动收缩成图标条 / 放大中间或右侧）+ 多标签并存（浏览器、文件、Git、PPT 可同时开着，用户逐个关）+ 内嵌编辑；左栏只做导航、中间只做会话；
- 右栏从「4 个硬编码 kind」变成「能力容器」：内置能力（执行链路 / Git / 文件 / 浏览器 / PPT）与插件能力走**同一套 provider 契约**，谁都不特殊；
- "能创造"不靠切模式，只新增三件事：**能看见**（`ui_inspect` 读运行中的 surface/tab 快照）、**能装**（`surface_install` 受管安装 + 结构化回执）、**知道怎么装才对**（`hai-surface-dev` skill）；四个工具常驻，skill 负责路由（D-N）；
- 边界先划死：V0.1 只做**右栏 Surface**、只接受**同源可信 ESM**、**不做 Host 侧动态工具**、**不做 iframe**、**不做插件市场**。

验收线（可执行）：

1. `grep -c "kind === '" RightPanel.tsx` 只剩 0 处（新增一种能力 = 一个 provider 文件 + 一行注册）；
2. 一句 prompt 装出一个「成本看板」Tab，**不重启**出现，重启后仍在，卸载后标签变墓碑且无残留；
3. 插件面板写坏时，**只黑自己那块**，连续 3 次挂载失败自动禁用并给出原因；
4. 卸载一个内置能力（如 git）后，右栏其它能力与已装插件不受影响；
5. 布局三态可复现：默认中间占满 → 展开右栏自动收左栏、中右各半 → 一键放大中间/放大右侧；
6. 右栏里改一个文件能保存；agent 同时改同一文件时**不会静默覆盖**（冲突条出现，三条出路都在）；
7. 全程不改 `agents`/`tools`/`session` 的任何现有语义。

---

## 1. 为什么必须合并做

| 做法 | 后果 |
|---|---|
| 先改版，后做创造模式 | 改版时没有"外部 provider"这个约束，四个内置 kind 会被写得更深；等做创造模式时，注册表要再动一次刀 |
| 先做创造模式，后改版 | 插件直接挂在 `'git' \| 'file' \| 'web' \| 'activity'` 上，改版 = 全部插件失效 |
| **合并（本文）** | 改版的第一行代码就是注册表；创造模式只是"往注册表里写第 N 个 provider 的那只手" |

一句话：**注册表既是重构目标，也是扩展点**。这也正是 DSH 那边成立的原因——它的 slot 注册表既是自己 UI 的装配方式，也是插件唯一的接入方式。

---

## 2. 现状盘点

### 2.1 八个硬编码接触点（改版必须全部清掉）

| # | 位置 | 现状 | 迁移后 |
|---|---|---|---|
| 1 | `store/useAppStore.ts:26` | `RightTab = 'activity'\|'git'\|'file'\|'web'` | 保留为**内置 kind 别名**，新增 `SurfaceKind = BuiltinKind \| \`plugin:${string}\`` |
| 2 | `store:38` | `TabKind = RightTab`（别名） | `TabKind = SurfaceKind`（由注册表收窄校验） |
| 3 | `store:49` | `TabInstance` 四路联合 | 增加 `PluginTabInstance`，其余不动 |
| 4 | `store:1508` | `TAB_KINDS` 白名单 | 改为 `registry.has(kind)`，未知 kind → 墓碑 |
| 5 | `store:1395` | `makeTab(kind, s)` 逐 kind 分支 | 交给 `provider.defaultSpec(state)` |
| 6 | `store:1473/:1497` | `toPersistedTab` / `fromPersistedTab` | 交给 `provider.persist? / restore?`，无实现则用通用字段 |
| 7 | `RightPanel.tsx:1946` | `kindIcon` / `kindLabel` / `tabTitle` 的 switch | 交给 `provider.icon/title` |
| 8 | `RightPanel.tsx:2289` | 四个 `activeTab?.kind === 'x' && <XTab/>` | 交给 `provider.render(ctx)` + 每 surface 一个 ErrorBoundary |

外加分组：`RightPanel.tsx:35/:41` 的 `primaryTabs/secondaryTabs` —— 改为 `provider.group` 排序。

### 2.2 四项必须保留的资产

- **`TabSet` + `scopeKey`**（`store:37/:54`）：会话级标签集、最近关闭、`restored` 标记；
- **持久化**（`store:1584` `persistTabsNow`，localStorage + 结构版本）：插件标签要跟着走；
- **视图池 LRU + 休眠**（web 标签的 `viewId/pageId`、`isPendingWebTab`、`rebuildWebTab`）：这是"标签 ≠ 视图"的成熟解法，插件不得绕过；
- **`syncTabsToRightTab` 的"单一真值源"原则**：保留"只允许一处推另一处"，但**方向反转**——`activeId`（实例）为权威，`rightTab` 降为派生值。理由：多标签并存后同 kind 可有多个实例，kind 无法定位"当前是哪一个"（见 §3.3）。

> 结论：**`TabSet` 结构一字不改**，只改两件事——kind 的解释权交给注册表、`rightTab` 从权威降为派生。除此之外不动数据模型，这是能在一个改动窗口内落地的根本原因。

---

## 3. 目标架构

### 3.1 Surface 契约（前端唯一扩展面）

```ts
// desktop/app/src/surfaces/types.ts（新增）
export type SurfaceKind = 'activity' | 'git' | 'file' | 'web' | `plugin:${string}`

export interface SurfaceProvider<T extends TabInstance = TabInstance> {
  kind: SurfaceKind
  namespace: 'builtin' | 'plugin'
  group: 'workflow' | 'content' | 'plugin'   // 对应标签条三段
  order?: number                              // 同组内排序
  title: (tab: T, t: TFn) => string
  icon?: (tab: T) => ReactNode
  badge?: (tab: T, s: AppState) => number     // 未读/待办角标（沿用 unreadViews 口径）
  singleton?: boolean                          // true = 同 scope 只允许一个实例
  allowDuplicate?: boolean                     // 去重键相同是否允许第二份（默认 false = 激活已有）
  scope?: 'session' | 'workspace'              // 默认 session；workspace 仅限无进程级视图的能力
  weight?: 'light' | 'heavy'                   // heavy 进资源池（web / 文档转换 / 插件重面板）
  capabilities?: ('read' | 'view' | 'write')[]  // write 仅内置；插件恒为 read + view
  defaultSpec?: (s: AppState) => Omit<T, 'id'>
  persist?: (tab: T) => unknown                // 不实现 = 该标签不持久化
  restore?: (raw: unknown) => Omit<T, 'id'> | null
  render: (ctx: SurfaceRenderContext<T>) => ReactNode
  onOpen?: (tab: T) => void
  onClose?: (tab: T) => void
}

export interface SurfaceRenderContext<T> {
  tab: T
  scope: ScopeKey
  host: HostApi                 // 见 §5.2，插件唯一可用的宿主能力
}
```

注册表（模块级单例，**声明即授权**）：

```ts
registerSurface(p: SurfaceProvider): void   // kind 重复 → 直接抛错（加载期暴露，不等渲染）
getSurface(kind): SurfaceProvider | undefined
listSurfaces(): SurfaceProvider[]           // 按 group/order 排序
```

### 3.2 四层职责，不要互相越界

| 层 | 负责 | 不负责 |
|---|---|---|
| **容器**（RightPanel） | 标签条、分组、激活、拖排、溢出、墓碑、高度链 | 任何业务渲染 |
| **provider** | 一个 kind 的标题/图标/默认 spec/渲染/持久化 | 访问别的 kind 的 DOM/状态 |
| **数据**（store） | `tabsByScope`、`rightTab`、视图池引用 | 知道有哪些 kind（只问注册表） |
| **外壳**（Electron/bridge） | 视图池合成、休眠、文件系统 | 渲染决策（硬规则 3 不变） |

### 3.3 命名空间与迁移

- 内置 kind 保持不变（`git`/`file`/`web`/`activity`）；**文档与媒体不新增 kind**，它们是 `file{mode:'preview'}` 内部的渲染器（见 §4.7）；
- 插件 kind 统一 `plugin:<id>`，与内置命名空间隔离，**注册表拒绝内置前缀**；
- localStorage 结构版本 +1：旧标签按原 kind 恢复；未知 kind → `PluginTabInstance` 的 `unavailable` 形态（墓碑：显示"插件已移除/未加载"，可关闭、不可激活内容）。

⚠️ 一处必须纠正的旧假设：`rightTab` 现在是"当前显示哪类内容"的**权威**字段，e2e 直接读写它（`store:31-33` 点名了 `verify_auto_open_visual.mjs:114`）。**"多标签并存"会打破这条**——同一个 kind 可以有多个实例（两个文件、两个网页），kind 就不再能标识"当前是哪一个"。

结论：`activeId`（实例）成为权威，`rightTab` 降级为**派生值**（= 当前标签的 kind），保留一个版本供 e2e 与 `SessionView` 读取，随后删除。`syncTabsToRightTab` 的单向收敛方向随之反转（标签集→rightTab）。

**实现形态（唯一口径，避免两份文档各说一套）**：store 里**保留 `rightTab` 字段**，但只有 `applyTabs()` 一处写它（写穿）；所有既有读取点（50+ 处）不改。**不要**改成"组件里用 selector 现算"——那会让不同渲染时机读到不同值，且 e2e 的 `setState` 复位全部失效。PR-11 再删除字段本身。这一步单独一个 commit，先改测试再改实现。

### 3.4 实例、作用域、资源（三张表决定一切）

| 能力 | singleton | 实例身份（去重键） | 作用域 | 资源 |
|---|---|---|---|---|
| activity | ✓ | — | session | 轻 |
| git | ✓ | — | session（diff 基线本就是会话级） | 轻 |
| file | ✗ | `(path, mode)` | session | 中（>2 MiB 只读） |
| web | ✗ | 池视图 `viewId` / `pageId` | **只允许 session** | 重（LRU + 休眠） |
| 文档/媒体预览 | ✗ | `(path)`（在 `file` 内） | session | 中~重（走转换服务与缓存） |
| `plugin:*` | manifest 决定 | `(kind, params)` | 默认 session；无进程级视图的可选 workspace | manifest 声明 `light`/`heavy` |

三条不变量：

1. **去重键相同 = 激活已有实例**，不新开（想开第二份用 `allowDuplicate`，V0.1 只有 file 的 diff/source 天然区分）；
2. **有进程级视图的能力（web）必须 session 作用域**——ADR-0 不动，`scope:'workspace'` 只对无视图的看板类开放；
3. **标签 ≠ 视图**：标签是壳，视图是资源。这条是后面"多标签并存"能撑住的前提。

### 3.5 产品原则：所有外部能力只在右侧

| 区域 | 只做 | 违规信号 |
|---|---|---|
| 左栏 | 导航：会话 / 工作区 / 技能 / 定时 / 设置 | 左栏出现任何内容预览 |
| 中间 | 会话：叙述 + Composer + 审批 | 中间出现文件树 / 浏览器 / Git 面板 |
| 右栏 | 一切"外部能力"：浏览器、文件（含编辑）、Git、PPT、插件面板 | 右栏出现会话消息流 |

这条原则的价值不在排版，而在**它把"能力"和"导航"彻底分开**：新增能力只有一个去处（右栏一个 provider），新增导航只有一个去处（左栏一个入口）。创造模式因此只需要管一件事——往右栏加 provider。

---

## 4. 右栏改版（M0）

### 4.1 布局（按 Claude Code Desktop 的三态模型重写）

**目标形态**：默认中间占满；右上角一个按钮展开右栏；展开时左栏自动收成图标条、中/右各半；展开态下用户可再选"放大中间"或"放大右侧"。

```
 默认（right=closed）
┌────────┬──────────────────────────────────────────────┐
│ 左栏   │  会话主页面（占满）                    [⧉ 展开]│
│ 236    │                                              │
└────────┴──────────────────────────────────────────────┘

 展开（right=split，左栏自动 rail）
┌──┬───────────────────────┬───────────────────────────┐
│▣ │  会话主页面 50%        │  右栏 50%  [50/50|◀|▶][✕] │
│56│                       │  标签条 / 面板 / 状态条     │
└──┴───────────────────────┴───────────────────────────┘
   ▲ logo + 图标（peek 滑出、pin 钉住）
```

**状态机只有一个真值源**（写进 store，**会话级**——切会话各自恢复，与 ADR-0 / D-B 一致）：

```ts
type RightLayout = { open: false }
                 | { open: true; bias: 'even' | 'center' | 'right'; ratio: number }
// 左栏宽度只有 leftExpanded 一个真值源（rail / 展开），rightLayout 不推导左栏；
// leftPinned 只表达「pin 住不被其它规则收起」，不参与右栏比例换算
```

| 事件 | 结果 |
|---|---|
| 启动（有会话） | `open:false`；左栏 expanded；中间占满 |
| 点右上角「展开右栏」/ `⌘\` | `open:true, bias:'even'`；**左栏不动**（2026-09 修复：展开/预览右栏不收左栏） |
| 内容触发（点 diff / 文件树 / 打开网页 / 打开 PPT） | 自动 `open:true`（沿用现状），bias 不变 |
| 点「放大中间」 | `bias:'center'`（中 64 / 右 36） |
| 点「放大右侧」 | `bias:'right'`（中 36 / 右 64） |
| 收起右栏 | `open:false`；左栏不动 |
| 手动拖分隔条 | 只改像素并把 bias 标为 `custom`（按钮组显示"自定义"），不改 open |
| 左栏手动展开（pin） | 只影响左栏自身；右栏比例换算始终用**左栏真实像素**（44 或 236） |

**必须删掉的旧规则**：`App.tsx:160` 的「右栏拖过 50% 就自动收起左栏」（`rightOverHalf`）。它和上面的显式状态机冲突，也是"左栏莫名其妙消失"的来源。有了 `open` 之后这条判据不再需要。

**宽度与最小尺寸**：左 rail **44**（`index.css --rail-w` 与 `collapsedSize` 的实测值，不是 56）/ expanded 236；中 min 300；右 min 200。比例换算在 `react-resizable-panels` 的百分比里做，`minSize` 兜底防压死。

**按钮位置**：右栏头部左侧 = 标签条；右侧 = `[平分] [居中] [右重] [✕ 收起]`（M0 落地为文字按钮，custom 时不亮任何预设）。四个按钮都是 `aria-label` + tooltip，不占数字快捷键（硬规则 1）。

**不做**：右栏内部再并排两个面板（`Ctrl+O` 聚焦循环在 `SettingsModal.tsx:731` 里标记 `done:false`，从未实现；本方案不复活它，改用上面三个显式按钮——可发现性更好，也不必给 `react-resizable-panels` 加嵌套 Group）。

### 4.2 内置能力迁移顺序（每步一个可验证的"删除线"）

| 步 | 迁移 | 完成判据 |
|---|---|---|
| 1 | git | `RightPanel` 里 `case 'git'` 消失，标签行为与像素不变 |
| 2 | file | 多实例身份 `(path, mode)` 仍成立，`openSeq` 语义不变 |
| 3 | web | 视图池/休眠/恢复全绿（跑 `test:right-tabs` + `verify_pdf_preview.mjs`） |
| 4 | activity | trace 选择器、Span 抽屉、Alt+←/→ 不变 |
| 5 | 文档渲染器注册表 | docx/pptx/xlsx/pdf/csv/图片/音视频全部走 `file{mode:'preview'}`，由渲染器按扩展名分派；不再借 web 标签 |

> 顺序理由：先迁"最独立"的（git 只读 store），最后迁"最纠缠"的（activity 绑 trace 投影），新能力放最后以免一上来就双线作战。

### 4.3 新能力落位

- **浏览器**：沿用 `web` provider + 视图池，不改 ADR-0 的会话隔离；`MCP` 页与原生页继续用 `viewId/pageId` 两种身份；
- **文件编辑**：`file` 增加 `mode='edit'`，内嵌编辑器（CodeMirror 6），**M0 必做**——它是右栏从"预览抽屉"变成"工作区"的分水岭，详见 §4.5；
- **PPT / 文档 / 媒体**：统一进 `file{mode:'preview'}` 的渲染器注册表，**不单独设 office kind**（决策 D-I，见 §4.7）。这取代 `RIGHT_PANEL_TABS_PLAN.md` 的 D6（视觉类文件落浏览器标签）：pdf/图片仍可用 web 标签打开，docx/pptx/xlsx/csv/音视频走文档渲染器；已持久化的 web 标签按原样恢复、不迁移（评审 F11 / F19）。
- **Git**：`ChangesTab` 整体平移，diff 聚合与回滚逻辑零改动。

### 4.4 错误与恢复（M0 必须有）

- 每个 `provider.render` 外包 `SurfaceBoundary`：捕获异常 → 面板内显示"这个面板崩了 + 原因 + 重新加载 + 禁用"；
- 挂载失败计数落 `localStorage`（按 kind+hash）：连续 3 次 → 自动禁用并写 `disabled_reason`，**不阻塞启动**；
- 禁用的插件在标签条上仍可见（墓碑），点击只显示原因，不尝试渲染。

### 4.5 内嵌编辑器（M0 必做）

**形态**：`file` 标签新增 `mode='edit'`（与 `'diff'` / `'source'` 并列，沿用 D5 的多实例身份 `(path, mode)`）。双击文件树、点 diff 里的「编辑」、或 `openTab({kind:'file', mode:'edit', path})` 都落到同一个实例。

**为什么不能用 `file_preview`**：它产出的是**预览 HTML**（xlsx/docx/pptx 转换链），不是字节；编辑器要的是原文 + hash + 编码。需要一对新命令：

| 命令 | 入参 | 出参 |
|---|---|---|
| `file_open` | `{ path }` | `{ content, hash, mtime, size, encoding, readonly? }`；二进制/超 2 MiB → `readonly:'binary'|'too_large'` |
| `file_save` | `{ path, content, baseHash }` | `{ ok, hash, mtime }` 或 `{ ok:false, code:'conflict'|'locked'|'permission'|'too_large', currentHash? }` |

**三个必须一起解决的硬问题**：

1. **写锁**：复用 Go 侧现有的 per-path 锁（agent 的 `edit_file`/`write_file` 已用它）。`file_save` 拿同一把锁，人和 agent 不交叉写。
2. **冲突**：保存带 `baseHash`；不匹配 → 拒绝并弹冲突条，不静默覆盖。冲突条三个动作：`查看差异` / `用磁盘覆盖`（丢弃我的）/ `另存为`。**不做行级自动合并**——那是编辑器厂商的活，我们只保证不丢数据。
3. **外部变更推送**：编辑器和 agent 是同一个文件的两个写者。bridge 需要知道"编辑器开着哪些文件"（渲染进程推 `editor_open_set`），在任一工具写文件后对比 hash：
   - 编辑器 **clean** → 静默 reload（保留光标/滚动锚点）；
   - 编辑器 **dirty** → 顶部黄条「Agent 改了此文件，你的改动未保存」。
   实现上只 watch 打开的文件（fsnotify 按文件），不要 watch 整个工作区。

**编辑器选型：CodeMirror 6，不用 Monaco**。要的是"改一改配置/几十行代码"，不是 IDE：CM6 约 200 KB gzip、按需加载语言包、无 worker；Monaco 约 3 MB + worker + 主题桥接，收益全是 V0.1 用不到的（多文件、LSP、重构）。语言包按扩展名懒加载，未知类型降级为纯文本。

**⌘S 的既有决策要改**：`docs/DESKTOP.md` §5.7 写的是「⌘S 保留给 macOS Save，不占用」。有内嵌编辑器后这条必须细化：**只在编辑器聚焦时拦截 `⌘S` 并 `preventDefault`**；全局仍不注册 ⌘S，其它面板里它依然是系统 Save。这条要回写进 DESKTOP.md，否则下一个人会按旧口径把快捷键改回去。

**脏状态归属**（避免"多个编辑器互相覆盖"）：

- dirty 属于**标签实例**（`tab.dirty`），不是组件内部 state —— 组件卸载、切 scope、切 bias 都不能丢；
- `editorDirtyCount` 是 **selector 现算**（`tabs.filter(t => t.dirty).length`），不是另一个字段；
- `window.desktop.setDirty(count > 0)` 只在聚合值变化时调用；**禁止**每个 Editor 组件各自 `setDirty(true/false)`——两个编辑器会互相清掉对方的标志（这是最容易埋的隐式状态）；
- 退出拦截问的是"有没有 dirty 标签"，不依赖任何单例布尔。

**编辑器打开集合的归属**：`editor_open_set` 上报**全量集合**（不是增量），bridge 以最后一次上报为真值；`file_open` 只负责读，**不再自己 track**——否则关掉的编辑器会永久占住跟踪表（64 条 LRU 也救不了漏）。

**脏状态生命周期**（漏一个就是丢用户数据）：

| 事件 | 行为 |
|---|---|
| 关闭标签 / 关闭全部 | 有 dirty → 确认框（保存 / 丢弃 / 取消） |
| 切会话（换 scope） | 标签集按 scope 保留，不弹框；dirty 随标签走 |
| 退出应用 | Electron `before-quit` 拦截 → 读聚合 `editorDirtyCount` → 有则取消退出并聚焦该标签 |
| 崩溃 | 不做恢复（V0.1）；dirty 只在内存，**未保存内容不落盘**，这是明确取舍 |

**agent 的可见性边界**：编辑器 buffer **不是**会话状态，agent 的 `read_file` 读的是磁盘。所以：

- 未保存的改动对 agent **不可见**，可能给出基于旧内容的建议 —— 这是可接受的，但必须让 agent 知道；
- `ui_inspect({what:'editors'})` 返回 `[{path, dirty, hash, mtime}]`，skill 里要求「改一个用户正打开的文件前先查 editors」；
- 反过来，agent 写盘后必须能触发上面的"外部变更推送"，否则用户会在无提示下被覆盖 —— 这是本设计里**最不能省**的一条。

**不做**：LSP/补全/重构/多光标/内嵌终端。V0.1 的编辑器 = 语法高亮 + 保存 + 冲突处理。

### 4.6 多标签并存（浏览器 + 文件 + Git + PPT 同时在）

这是右栏从"抽屉"变成"工作区"的最后一块拼图，也几乎全部复用已交付的标签模型（阶段 1-5），只改三处语义。

**行为**

- **打开新标签不挤掉旧的**：`setRightTab(kind)` 的"单选组"语义彻底退场，`openTab(spec)` 是唯一入口；
- **逐个关闭**：`closed` 栈支持重开（已有，上限 `TAB_CLOSED_MAX=10`）；右键菜单补齐 `关闭 / 关闭其他 / 关闭右侧 / 全部关闭 / 固定`；
- **溢出**：可见 5 个 + `+N` 下拉（沿用 `TABS_MAX_VISIBLE`）；**超限不拒绝打开**，只收进溢出列表——拒绝打开会让 agent 的动作"看起来失败"；
- **空态**：右栏展开但没有标签 → 显示"能力面板"选择器（浏览器 / 文件 / Git / PPT / 已装插件），而不是空白；
- **切相邻标签**：`⌃⇥ / ⌃⇧⇥`（不占 `⌘1..9`——硬规则 1）；中键关闭；双击标签条空白 = 打开能力面板。

**资源（撑住"同时存在"的关键）**

| 类型 | 策略 |
|---|---|
| web | 视图池 LRU + 休眠（已有）；**休眠不是关闭**，URL/滚动位置不丢，点击唤醒 |
| 文档转换（docx/pptx/xlsx）/ PDF | 转换产物落缓存，命中即秒开；转换中显示进度而不是空白 |
| 媒体（视频/音频） | 原生解码；Range 支持拖动；转码任务进队、可取消 |
| file | 编辑 buffer 只在内存；大文件降级只读；同一 `(path,mode)` 不重复开 |
| 插件 | manifest 声明 `weight`，`heavy` 的进入同一个池；未声明的按 `light` 计 |

**会话切换**

- 标签集按 `scope` 恢复；web 走 `rebuildWebTab`（待重建态，点开才建视图）；
- 编辑器 buffer 随 scope 保留（不卸载），切回来光标/滚动位置仍在；
- **不要**"切会话就重置右栏"——那是旧 `rightTab` 单向收敛的副作用，`activeId` 权威后自然消失。

**验收（M0 结束必须手测一遍）**

> 同时开：1 个浏览器 + 2 个文件（一个 diff、一个 edit）+ 1 个 Git + 1 个 PPT → 任意切换、各关各的、关错了能重开、切会话再切回来都还在；重启后除 web 变"待重建"外全部恢复；把右栏拖到最宽/最窄，标签条走溢出而不是挤压变形。

---

### 4.7 文档与媒体预览（`file` 内的渲染器注册表）

右栏要承接的不只是"面板"，还有"各种文件怎么被看见"。结论：**不新增 office / media 两种 kind**，而是在 `file{mode:'preview'}` 内部做一层渲染器注册表（决策 D-I，调研见 `05-preview-media-research.md`）。

```ts
// surfaces/preview/types.ts
export interface PreviewRenderer {
  id: string                       // 'pdf' | 'docx' | 'pptx' | 'sheet' | 'csv' | 'image' | 'media' | 'text' | 'html'
  match: (f: FileRef) => number     // 0 = 不接；多个命中取最大
  weight: 'light' | 'heavy'         // heavy 走转换服务准入队列
  load: () => Promise<React.ComponentType<PreviewProps>>   // 懒加载，不进启动 bundle
}
```

**三条来源**

| 渲染器 | 来源 | 备注 |
|---|---|---|
| text / markdown / code / image / csv / html / pdf | 直接渲染或复用内嵌浏览器 | pdf 继续用 Chromium viewer（不自研 pdf.js，避免引依赖） |
| docx / pptx / xls / xlsx / ppt | **转换服务**：kit 可用 → `soffice→PDF`；否则受管 Python 近似（沙箱 + 临时目录） | 统一收进 `desktop/bridge/conversion/` + `internal/officekit`，照抄 DSH 的限额 |
| media（音视频） | **自定义协议 + Range** | DSH 没有这块，自研；详见 05 文档 |

**转换服务的限额（照抄 DSH，不照抄依赖）**

| 项 | 值 | 理由 |
|---|---|---|
| 并发 | 2 | 转换吃 CPU，桌面端不能把自己跑满 |
| 单次超时 | 60s | 超时给 `timeout`，不是无限转圈 |
| 输入 / 输出上限 | **输入 100 MiB 硬闸**（50 MiB 仅作容量规划口径，不设软档）；输出受 512 MiB 缓存总量约束 | 超限明确 `too_large`（R08a 落地：**代码里的错误码拼法是下划线**，`too_large`） |
| 缓存键 | `sha256(源字节)+操作+参数+转换器版本`（R08a 落地再拼**源文件名**：产物 `<title>/<h1>` 写着它，改名必须换键） | 同一文件反复打开秒开 |
| 输出目录 | `~/.go-code/previews/<key>/index.html`（R08a 落地；原定 `<session>/<uuid>/` 会把同一内容复制 N 份并切断跨会话命中） | **不落工作区** |
| 保留 | 24h + 512 MiB LRU | 启动时清理过期目录 |
| 失败 | **不缓存**，返回稳定错误码 | 避免"一次失败永久失败" |

**三档降级（必须显式告知，不静默）**

| 条件 | 路径 | 保真 |
|---|---|---|
| kit 可用（下载的 LibreOffice，或系统已有 `soffice`） | Office → PDF → pdf 渲染器；`office_render` 用 **pypdfium2** 逐页出 PNG | 高（DSH 同级） |
| kit 缺失 | 现有 mammoth / python-pptx 近似 / openpyxl + 顶部「轻量预览」横幅 | 中 |
| 失败/超时 | 稳定错误码 + 「用系统程序打开」 | — |

**kit 的形态**：不内置（安装包 +700 MB 不可接受），按平台下载、sha256 + 版本 pin、`-env:UserInstallation` 每 job 独立（否则撞 profile 锁）。它与转换服务共用限额与缓存。完整落地设计见 `05-preview-media-research.md` §8。

pptx/docx 渲染器顶部固定一条能力提示（"当前为本机近似预览"），延续 `pptx_preview.go` 既有的"诚实比假装像更重要"。

**媒体（自研）**：Electron 自定义协议 `hai-media://` 支持 `Range`（206）——没有 Range 就不能拖进度条；`media_probe` 走 `ffprobe` 拿时长/码率/编码；按编解码矩阵判断能否直放，不能则给「系统播放器打开」或（有 ffmpeg 时）「转码为 MP4」。详见 05 文档 §4 D-L。

**验收**：docx/pptx/xlsx/pdf/csv/png/mp4/mp3 各开一次全部可看；同一文件二次打开命中缓存；100 MiB pptx 给 `too_large` 不卡死；转换器崩溃只影响该面板。

---

## 5. 创造能力 V0.1（M1，无模式）

> 修订（2026-11-11，决策 D-N）：**取消 `CreatorMode`**，改为"常驻工具 + skill + 审批门"。依据是 OpenCode v2 的做法——它没有 creator 模式，靠"应用本身支持插件 + 一个 skill 教怎么写"。调研见 `06-opencode-skills-research.md`。

### 5.1 无模式：4 个常驻工具 + 1 个 skill

**不再有"切换模式"这一步。** 用户说"给右栏加个成本看板"，普通会话里的 agent 就能做——它看到工具、看到 skill 描述、按 skill 正文执行。

```go
// desktop/bridge/tools_registry.go —— 与 bash/edit_file 同处注册，不再挂 mode
eng.RegisterTool(ctx, &builtin.UIInspectTool{Cache: m.uiCache})
eng.RegisterTool(ctx, &builtin.SurfaceInstallTool{Reg: m.surfaces})   // RequiresApproval: true
eng.RegisterTool(ctx, &builtin.SurfaceListTool{Reg: m.surfaces})
eng.RegisterTool(ctx, &builtin.SurfaceRemoveTool{Reg: m.surfaces})    // RequiresApproval: true
```

三个必须守住的约束：

| 约束 | 为什么 |
|---|---|
| **最多 4 个工具** | 常驻 schema 每次请求都付 token，加一个就永久加成本 |
| **每个描述 ≤ 120 字** | 只写"什么时候用 + 一句语义"，教程放 skill 正文（渐进披露） |
| **参数 ≤ 3 个字段** | 枚举不展开长列表，避免 schema 膨胀 |

预估成本：**600–900 token/请求**，换来"任何时候都能扩展"，比"让用户先学会切模式"划算。

**skill 是唯一路由信号**：

```md
---
name: hai-surface-dev
description: 当用户要求给桌面端右栏加面板、改界面、做看板或工具页，或调试已装 Surface 插件时使用。
requires:
  tools: [ui_inspect, surface_install]
---
（正文：六步交付纪律 + 禁止清单 + 验收话术）
```

- `requires.tools` 走已有的**技能能力面过滤**（`skills/capabilities.go`）：没有 surface 工具的会话（如 Explore 子 agent）看不到这个 skill；
- 系统提示词只注入 description 一行；正文按需加载；
- 用户可 `@hai-surface-dev` 手动点名。

**审批不变**：`surface_install` / `surface_remove` 每次都要批准（`RequiresApproval → true`），与模式无关。

**V1 优化（不阻塞）**：若上下文成本敏感，用 `load_skill` 的 `onLoad` 钩子做"加载 skill 才解锁工具"（`desktop/bridge/skill_capability.go:96` 已有该钩子）；代价是工具 schema 中途出现会有一次 prefix cache miss，且需在 loop 重建后重放"已加载技能"。

### 5.2 工具面（4 个工具：inspect / install / list / remove）

| 工具 | 输入 | 输出（回执） |
|---|---|---|
| `ui_inspect` | `{ what: 'surfaces'\|'tabs'\|'theme'\|'commands'\|'settings'\|'viewport' }` | 只读快照（JSON） |
| `surface_install` | `{ target: 绝对目录 \| npm spec, enable?: bool }` | `{ status, reason?, id, version, hash, nextStep? }` |
| `surface_list` / `surface_remove` | `{ id }` | 同构 `plugin.State` |

`surface_install` 的语义对齐现有 `plugin.Registry`：**幂等**（同 id 同 hash = no-op）、**先快照后写**、**失败不写 index**。回执字段直接复用 `plugin.State{status, reason, installed, pinned}` 的形状，UI 与工具共用一套文案。

### 5.2b `HostApi`：插件唯一可用的宿主能力

契约要窄到"能写出一个好面板，但写不出一个能拖垮应用的插件"：

```ts
export interface HostApi {
  React: typeof import('react')                 // 共享模块表，插件不打包 React
  Icon: (p: { name: string; size?: number }) => ReactNode
  t: (key: string, vars?: Record<string, unknown>) => string
  theme(): { mode: 'light' | 'dark'; tokens: Record<string, string> }
  useSession<T>(sel: (s: SessionProjection) => T): T   // 最小订阅，只读
  command(name: string, payload?: unknown): Promise<unknown>  // 受 manifest.commands 白名单约束
  events: { on(type: string, fn: (e: unknown) => void): () => void }  // 只允许白名单事件类型
  openTab(spec: TabSpec): void
  toast(msg: string): void
}

// 只读投影（不是 store）：字段少、语义稳定、可按需扩
export interface SessionProjection {
  id: string; workspace: string; model: string; status: 'idle' | 'running' | 'waiting'
  cost: { usd: number; calls: number; cachePct: number }
  ctx: { used: number; max: number }
}
```

**不给**的东西（比给什么更重要）：原始 `useAppStore`、`blocks`/`traceEvents` 全量、`window.desktop` 直连、文件系统、其它面板的 DOM。V0.1 的面板是"只读看板 + 少量命令"，不是第二个前端框架。

### 5.2c CodeMode 兼容（新工具必须满足的 5 条）

这些工具在脚本里（`run_code` / `tools.ui_inspect(...)`）同样可用，且必须满足仓库既有的脚本调用契约（`docs/CODEMODE_CALL_CONTRACT_2026-10-28.md`）：

| # | 规则 | 依据 |
|---|---|---|
| 1 | **脚本路径返回对象，不返回 JSON 字符串**：实现 `OutputSchemaProvider`，在 `Call` 内当 `tools.IsScriptCall(ctx)` 为真时 `StructuredSinkFrom(ctx).Set(json)` | MCP 工具曾只回字符串，脚本 `r.count` 恒 `undefined` 且不报错 |
| 2 | **失败即 reject**（返回 `error`），不返回 `{ok:false}` | 脚本用 `try/catch` 判失败；`IsError + 结构化` 会被当成 resolve |
| 3 | **审批对嵌套调用逐次生效**：拒绝 → `rejected`，超时 → `timed out`，脚本要能区分 | `ExecuteOne` 已走完整审批（`tools/nested_approval_test.go`） |
| 4 | **写类保序**：`surface_install/remove` 在脚本里按帧号串行，`Promise.all` 也不会乱序 | `tools/builtin/codemode/serial.go` |
| 5 | **幂等**：同 id 同 hash 的 `surface_install` = `noop`，脚本重试安全 | 与 DSH 的 plugin_manager 同语义 |

`office_convert` / `office_render` 同样适用这 5 条（它们是工具，也会被脚本调用）。

另外三条工程约束：

- `ui_inspect` / `surface_list` 标 `CanParallel: true`，`surface_install/remove` 为 `false`；
- **不要**实现 `SkipTruncateProvider`（普通工具的 20 KB 截断是刻意设计；脚本路径本身有 1 MiB 零损耗额度）；
- 工具名必须是合法 JS 标识符（下划线命名已满足），脚本里就是 `tools.ui_inspect({...})`。

```go
// 脚本路径给对象（结构化槽），模型路径仍给文本 —— 与 bash / MCP 同款
if tools.IsScriptCall(ctx) {
    if sink := tools.StructuredSinkFrom(ctx); sink != nil { sink.Set(out) }
}
```

### 5.3 数据通道（本方案最关键的一处工程设计）

问题：surface 是**渲染进程**的知识（有哪些 provider、开着哪些标签、主题 token、视口尺寸），但工具跑在 **Go** 侧。

不做反向 RPC（渲染进程被 Go 主动调用），改成 **单向推送 + 缓存读取**：

```
renderer ──(debounce 500ms)──> bridge: ui_snapshot {layout, surfaces, tabs[currentScope], otherScopes:{count}, editors, theme, viewport}
renderer ──(挂载/崩溃/卸载)──> bridge: surface_status {id, mounted|error, message}
bridge   ──> 内存缓存 + 最近 N 条事件（不落盘、不进 28 个 agent 事件类型）
Go tool  ──> ui_inspect 读缓存（含 ageMs，超 5s 标 stale）
bridge   ──(安装/卸装完成)──> renderer: 信封 {type:'ui', payload:{op:'surfaces_changed'}}
```

三条纪律：

1. `ui` 信封**不进** `events.Event` 封闭集合、**不落盘、不重放**——它是 UI 通道，不是会话历史；
2. 快照是**派生数据**，丢了就再推一次，不做一致性保证（与 `docs/architecture` 里"会话日志是唯一真相"同构：UI 状态本来就不是真相）；
3. `ui_inspect` 结果必须带 `ageMs`，让模型知道"你看到的是 0.3 秒前还是 30 秒前"；
4. **快照只含当前会话的标签**：`tabs` 按 `scopeKey` 过滤，其它会话只给 `{count}`。否则 agent 能读到别的会话开了哪些文件/网页，等于绕过 ADR-0 的会话隔离（这条在评审里被单独拎出来过，别删）。

### 5.4 安装、持久化、回滚

```
~/.go-code/surfaces/
  index.json                 # 有序列表：[{id, version, hash, source, enabled, installedAt, disabledReason?}]
  index.json.bak             # 每次变更前的快照（只留 1 份）
  <id>/surface.json          # manifest
  <id>/panel.js              # 单个 ESM 文件（无构建步骤）
  <id>/icon.svg
  audit.jsonl                # 每次 ctx.host.command 一行（id, cmd, ts, ok）
```

- 安装 = 校验 manifest → 计算 `sha256(panel.js)` → 快照 index → 原子写（目录 0700 / 文件 0600，复用 `config_patch.go` 的"临时文件 + rename"套路）→ 推 `surfaces_changed`；
- 卸载 = 从 index 移除 → 关闭该 kind 的所有标签 → 推 `surfaces_changed` → 目录移到 `~/.go-code/surfaces/.trash/<id>-<ts>`（不直接删，给"手滑"一次后悔的机会）；
- 回滚 = `index.json.bak` 覆盖 + 重新推送（V0.1 只做手动回滚，不做自动）。

### 5.5 权限：诚实地"软约束"

V0.1 的插件是**同源可信代码**，跑在渲染进程里、能拿到 `window.desktop`。所以：

- **不做假沙箱**：不声称隔离，文档里明说"装插件 = 信任它"；
- **做能力白名单**：`ctx.host.command(name, payload)` 由宿主代理，只放行 manifest `commands` 里列出的命令；未列出的调用直接拒绝并写 audit；
- **做可见审计**：插件页展示 manifest 声明的 `commands`/`data`，与 audit 的调用记录对照；
- **审批边界**：`surface_install` / `surface_remove` 在 creator 模式下**逐次要批准**（不因模式放行），与 DSH 的 `plugin_manager` 同一条纪律。

**"禁用 / 卸载"的三处语义必须一致**（评审时发现的歧义点）：

| 位置 | 禁用（`enabled:false`） | 卸载 |
|---|---|---|
| 注册表 | 启动时**不 import、不注册**，`listSurfaces()` 里没有它 | 同禁用 + 从 index 删除 |
| 已存在的标签 | 变墓碑（显示原因，不可渲染） | 同左，但重启后墓碑仍在（index 里留 `lastKnown` 一条） |
| 插件页 / 空态能力面板 | 插件页**列出**（数据来自 index）；空态面板**不列** | 插件页不列；空态面板不列 |

一句话：**注册表只装"活着的"，index 记录"装过的"**。不要把两件事混成一个 list，否则墓碑会变成可点击的幽灵。

manifest：

```json
{
  "id": "cost-board",
  "name": { "zh": "成本看板", "en": "Cost board" },
  "version": "0.1.0",
  "api": 1,
  "kind": "plugin:cost-board",
  "group": "plugin",
  "singleton": true,
  "entry": "panel.js",
  "icon": "icon.svg",
  "commands": ["context_breakdown", "provider_billing"],
  "data": ["session.cost", "session.ctx"]
}
```

### 5.6 panel.js 模板（无构建步骤，模型可直接写）

```js
// 与 DSH 的 client.js 同构：注册一个惰性 factory，React 由宿主提供。
export const surface = {
  kind: 'plugin:cost-board',
  group: 'plugin',
  singleton: true,
  async apply(ctx) {
    const React = ctx.host.React            // 宿主共享模块表，插件不打包 React
    const h = React.createElement
    function CostBoard() {
      const s = ctx.host.useSession((x) => x.cost)   // 只读投影，最小订阅
      return h('div', { className: 'sb-root' },
        h('div', { className: 'sb-kpi' }, '本次会话 $' + s.usd.toFixed(3)),
        h('div', { className: 'sb-sub' }, s.calls + ' 次调用 · 缓存命中 ' + s.cachePct + '%'))
    }
    ctx.surfaces.register({
      kind: surface.kind, group: surface.group, singleton: surface.singleton,
      title: () => ctx.host.t('cost-board.title'),
      icon: () => h(ctx.host.Icon, { name: 'coins', size: 12 }),
      render: () => h(CostBoard),
    })
  },
}
```

> 为什么不用 JSX：V0.1 拒绝引入构建步骤（`esbuild` 目前只是 Vite 的传递依赖，没有作为直接依赖锁定版本；把它提成正式依赖并维护一条编译链，是 M2 的成本，不是 V0.1 的）。`createElement` 是唯一"模型写什么、运行时就是什么"的形态；等插件复杂到需要 TSX 时，M2 再加安装期编译。

### 5.7 Skill：`hai-surface-dev`（知识包，不是文档）

照 DSH `dsh-doc` 的形状写，五段：

1. **事实顺序**：先 `ui_inspect`（surfaces/tabs/theme/commands）→ 再读 `docs/` 与 `desktop/app/src/surfaces/*` → 都不行就明说未验证；
2. **授权边界**："加载本 skill 不授予任何安装/改文件权限；用户要求的是成果，不是插件时，交付普通产物"；
3. **六步交付**：定位（右栏哪一段）→ 选 kind/group/singleton → 拷模板 → `surface_install` → 读回执 + `ui_inspect` 验证 → 在同一插件里改；
4. **禁止清单**：不做 mock HTML 预览、不装到 `activity`/`git` 组、不读别的面板 DOM、不替换根节点、不往 `document.body` 追加元素、不在面板里写全局 CSS；
5. **验收话术**：装完必须报"装在哪、回执是什么、有没有 restart 需要、哪些没验证"。

### 5.8 验收用例（M1 结束必须全绿）

| # | 场景 | 判据 |
|---|---|---|
| A | "在右栏加一个成本看板" | Tab 出现，数字与会话实际一致，重启后仍在 |
| B | 面板里 `throw` | 只黑该面板，其它可用；3 次后自动禁用并给出原因 |
| C | 卸载 | 标签变墓碑，重启不复活，`.trash` 里有备份 |
| D | 越权：调用未声明命令 | 被拒 + `audit.jsonl` 有记录 |
| E | 内置能力回退：删 git provider | 右栏其它能力正常，注册表仍能启动（缺 kind 只影响该 kind） |
| F | 浏览器 + 文件 + Git 同时开，关掉中间那个 | 各自独立，重开可用，切会话再切回来仍在 |
| G | 在右栏"编辑"里改一行并保存 | 落盘；agent 再读是改后内容；agent 同时改 → 冲突条（§4.5） |
| H | `run_code` 里 `tools.ui_inspect({what:"tabs"})` | 返回**对象**；`Array.isArray(s.value.tabs)` 为真 |
| I | 脚本里 `surface_install` 未批准 | `catch` 到 `rejected`；超时为 `timed out`；界面显示"脚本等待批准" |

---

## 6. 分期与工作量（估算，不是承诺）

| 期 | 内容 | 估时 | 主要风险 |
|---|---|---|---|
| **M0** 容器化 | 契约 + 注册表 + 三态布局 + 迁移 5 个内置 kind + **内嵌编辑器** + 多标签并存 + 墓碑 + 错误边界 | 3 周 | web 视图池迁移（休眠/恢复/双 id）；编辑器的写锁与冲突路径 |
| **M1** 创造模式 | `ui` 通道 + 3 工具 + creator Mode + skill + 模板 + 审计 | 1 周 | 快照时序（装完→渲染进程还没加载→工具读到 stale） |
| **M2** 增强 | 右栏内并排、安装期 TSX 编译、出进程插件、插件市场、编辑器 LSP | 3-4 周 | 嵌套 Group 动高度链 |

M0 内部顺序（每步可独立回滚）：**布局三态 → 注册表 + 迁 git/file → 编辑器 → 迁 web → 迁 activity → 文档渲染器 + 转换服务**。编辑器放在 web 之前，因为它只依赖 file 一个 provider，能最早暴露"写路径"的问题。

M0 与 M1 可以并行两周：M0 先在 `web` 之前插一个"假 provider"验证契约，M1 同时把 Go 侧工具与通道打通。

---

## 7. 明确不做（V0.1）

1. **不做 Host 侧动态工具**——`plugin.Capability` 仍是编译期注册；Go 没有可卸载模块，硬做只能出进程，那是 M2；
2. **不做 HMR / 热重载**——装完把新 provider 挂上就够；改代码重装一次，成本远低于造重组引擎；
3. **不做 iframe 面板**——拿不到主题/语言/会话作用域，等于自断"长得像原生"；
4. **不做插件市场**——先把 `surface_install(绝对目录)` 跑稳；
5. **不做 YAML preset 体系**——模式和工具集的关系用 `Mode` 表达足够，声明式装配等有第二个消费者再说；
6. **不碰 ADR-0 会话隔离**——插件标签照样按 `scopeKey` 分域，禁止跨会话读数据。

---

## 8. 风险清单

| 风险 | 触发场景 | 缓解 |
|---|---|---|
| 注册表变成公共 API | 第三方插件大量出现后不能改契约 | `api: 1` 版本号 + 只在 `docs/surfaces/` 下写契约；内部实现（store 字段）不进契约 |
| localStorage 结构升级 | 用户在旧版本有 20 个标签 | 版本 +1 时保留旧字段读取路径一个版本；未知 kind 落墓碑而不是丢弃 |
| 插件崩面板 | 组件 throw / 异步未捕获 | ErrorBoundary + 3 次自动禁用 + `disabledReason` 落 index |
| 视图池压力 | 插件开一堆 web 标签 | 插件默认**不允许多实例**，manifest 显式开；池上限沿用 LRU |
| 权限形同虚设 | 插件直接 `window.desktop.command` 绕过代理 | 代理只覆盖 `ctx.host.command`；文档明说不是沙箱；安装审批 + audit 是唯一边界 |
| 模型写出"假页面" | 面板渲染成截图/mock | skill 第 4 条禁止 + 验收只用"装好的面板" |
| 标签条被挤爆 | 右栏 50% 只有 ~700px，开 6 个标签 | `TABS_MAX_VISIBLE=5` + `+N` 溢出列表；插件组默认折叠；右侧变窄时优先收窄而非换行 |
| "切会话标签没了"的预期落差 | 用户把浏览器当工作区用 | 标签集按 scope 恢复且记住 `activeId`；在能力面板里显式标注"按会话"；workspace 作用域留给看板类 |
| 用户改完没保存就切走 | 切会话/关标签/退出 | 关标签与退出必确认；切会话保留 dirty 标签；`before-quit` 拦截 |
| 编辑器与 diff 基线打架 | 用户手改 + agent 改同一文件 | baseHash 拒绝保存 + 冲突条；写锁串行；不做自动合并 |

---

## 9. 与 DSH 的对照：抄四条，不抄四条

**抄**：① 注册表即扩展点（内置也走同一契约）；② 安装前先探查（`ui_inspect` 对应 `cordis_inspect_*`）；③ 结构化回执 + `applied/restart-required` 语义（V0.1 只有 `applied|disabled`）；④ 知识包写纪律、审批写边界。

**不抄**：① bundle + 多层 patch（V0.1 一个 `index.json` 够用）；② HMR 重组引擎；③ 双半侧插件（Host+Client 两个包）；④ profile 概念（go-code 是单 profile + 单进程）。

---

## 10. 附录

### 10.1 要改的文件清单（24 个）

```
desktop/app/src/surfaces/types.ts          新增：契约（含 scope/weight/capabilities）
desktop/app/src/surfaces/registry.ts       新增：注册表（模块级单例）
desktop/app/src/surfaces/builtin/gitSurface.tsx
desktop/app/src/surfaces/builtin/fileSurface.tsx
desktop/app/src/surfaces/builtin/webSurface.tsx
desktop/app/src/surfaces/builtin/activitySurface.tsx
desktop/app/src/surfaces/preview/{types,registry}.ts 新增：文档渲染器注册表
  desktop/app/src/surfaces/preview/renderers/*.tsx   新增：pdf/docx/pptx/xlsx/csv/image/media/text
desktop/app/src/surfaces/Boundary.tsx      新增：每面板错误边界
desktop/app/src/surfaces/plugin/loader.ts  新增：import() + 注册 + 卸载
desktop/app/src/components/RightPanel.tsx  改：删 4 个 switch，接注册表 + 溢出/右键菜单
desktop/app/src/components/Editor.tsx      新增：CodeMirror 6 + 冲突条 + dirty 提示
desktop/app/src/lib/layout.ts              新增：三态布局（open/bias/leftPinned）+ 百分比换算
desktop/app/src/store/useAppStore.ts       改：activeId 权威、rightTab 派生、TabKind 走注册表
desktop/bridge/surfaces.go                 新增：index.json / 安装 / 快照缓存
desktop/bridge/file_edit.go                新增：file_open / file_save / 外部变更推送
desktop/bridge/command_registry.go         改：+ui_snapshot / +surface_status / +surface_*
                                           +file_open / +file_save / +editor_open_set
desktop/bridge/conversion/service.go       新增：转换服务的准入/限额/缓存/输出保留
internal/officekit/{kit,render}.go         新增：kit 探测 / soffice→PDF / pypdfium2 出图
desktop/app/electron/media-protocol.ts     新增：hai-media:// + Range
tools/builtin/ui_inspect.go                新增：只读工具（含 editors）
tools/builtin/office_tools.go              新增：office_convert / office_render
skills/hai-surface-dev/SKILL.md            新增：六步交付纪律（requires.tools）
  templates/surface/*                        新增：surface.json / panel.js / icon.svg
desktop/app/src/components/PluginsPage.tsx 新增（或并入 Settings）：列出/禁用/卸载
```

### 10.2 最小数据流（装一个插件，端到端）

```
用户: 「右栏加一个成本看板」
  └ agent(creator) → ui_inspect(surfaces)        读快照：content 组有 file/web，plugin 组为空
  └ agent → write_file ~/ws/cost-board/{surface.json,panel.js,icon.svg}
  └ agent → surface_install(target=~/ws/cost-board)
        bridge: 校验 manifest → hash → 快照 index → 写 index.json → 推 ui{surfaces_changed}
  └ renderer: 收到 ui → import('file://…/panel.js?v=<hash>') → surface.apply(ctx) → register
  └ 标签条“插件”段出现「成本看板」；renderer 推 ui_snapshot
  └ agent → ui_inspect(surfaces) 读到 {id, mounted:true, tabs:1}
  └ agent 报告：装在哪、回执是什么、需要重启吗、哪些没验证
```

### 10.3 与现有计划的衔接

- 标签条交互（拖排、溢出、⌄ 重开、`TABS_MAX_VISIBLE=5`）**全部沿用**，只把"段的来源"从常量数组换成 `provider.group`；
- `docs/RIGHT_PANEL_TABS_PLAN.md` §7「未决」里的关闭快捷键、视觉类文件双标签等问题不因本方案改变；
- 聚焦模式（Chat/Preview/Chain）不改：新 provider 只是多了几个"预览类"目标。

---

## 11. 决策登记与文档索引

### 11.1 已拍板决策

| 编号 | 决策 | 落在哪 |
|---|---|---|
| **D-A** | 内容触发（点 diff / 文件 / 网页 / PPT / agent 打开文件）时自动展开右栏；手动收起不加锁 | §4.1、PR-01 |
| **D-B** | 默认 **session 作用域**；`workspace` 只开放给无进程级视图的看板类 | §3.4 |
| **D-C** | `⌘S` 只在编辑器聚焦时拦截，全局不绑定 | §4.5 |
| **D-D** | `activeId` 权威；`rightTab` 保留字段、由 `rightTabPatch()` **写穿派生**（标签动作与 sync 时成立；`viewFields` 的会话恢复镜像与外部 `setState` 是例外），PR-11 删除 | §3.3 |
| **D-E** | 插件不可声明 `write`；注册期断言，不是文档约定 | §3.1、§5.5 |
| **D-F** | 编辑器选 CodeMirror 6（不是 Monaco），不做 LSP/多光标 | §4.5 |
| **D-G** | 文件写锁提取为 `internal/atomicfile`（与 `tools/builtin` 共用 flock 实现，不复制） | 代码方案 §2.7 |
| **D-H** | `ui` 快照只含当前会话标签，其它会话只给计数 | §5.3 |
| **D-N** | **取消 `CreatorMode`**：4 个工具常驻 + `hai-surface-dev` skill 路由 + 安装审批；模式不是创造的前提 | §5.1、06 |
| **D-I** | **取消 `office` kind**：文档/媒体是 `file{mode:'preview'}` 的渲染器 | §4.7 |
| **D-J** | Office 保真**沿用 DSH**：LibreOffice kit（可下载、不内置）→ PDF；缺失则近似 + 明确横幅。**不采用** Keynote/PowerPoint 脚本 | §4.7、05 §8 |
| **D-K** | 表格不引前端依赖，增强服务端 HTML（sheet 导航/冻结/数字格式） | §4.7、05 |
| **D-L** | 音视频自研：`hai-media://` 支持 Range + ffprobe 探测 + 可选转码 | §4.7、05 |
| **D-M** | 转换服务统一限额（并发 2 / 60s / 50-100 MiB / 内容寻址缓存 / 输出保留 24h） | §4.7、05 |

### 11.2 四份文档的分工

| 文档 | 回答 | 状态 |
|---|---|---|
| `01-design.md`（本文） | 为什么这么做、边界在哪 | 定稿，待实施 |
| `02-pr-plan.md` | 按什么顺序、每步验收与回滚 | 定稿，可直接开 PR |
| `03-code-plan.md` | 具体写成什么样（类型/函数/协议） | 参考实现，随 PR 更新 |
| `04-review.md` | 隐式状态登记 + 歧义裁决记录 | 评审留痕 |

> 冲突时以**本文的决策登记**为准；`REVIEW` 文档记录每次裁决的理由，改设计先改登记表。

### 11.3 一句话记住状态归属

**注册表只装"活着的"，index 只记"装过的"；dirty 属于标签，聚合值现算；`ui` 快照只给当前会话；`rightTab` 只有一个写者。**

