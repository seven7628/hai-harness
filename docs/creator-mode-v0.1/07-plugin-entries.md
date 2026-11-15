# 07 插件入口与存储归属（决策记录）

> 2026-11-13。由产品讨论定下的两条边界，作为 R16 及后续插件能力的设计前提。
> 相关：`01-design.md` §5.4/§5.5、`03-code-plan.md` §3.6、R14/R14b/R14c 的落地记录。

## D-1 存储归插件，宿主不中介

**决定**：宿主**不**提供 `host.storage`，也不向插件开放文件读写。插件自己的业务数据（课程、课程资源、作业……）由插件自己负责：

- 同源 Web Storage / IndexedDB（插件私有命名空间，自行迁移与配额）；
- 或插件自带的 HTTP 后端（`panel.js` 里 `fetch`，受 CORS 约束）。

**宿主只保证**：加载、挂载点、入口、生命周期、崩溃隔离、能力白名单、审计。

**为什么**：课程管理是插件自己的领域；把它塞进核心等于让核心依赖一个业务域。宿主的职责是「把插件接进来、让它能安全地坏」，不是当它的数据库。

**代价（写清楚，别以后装糊涂）**：
- V0.1 插件是**同源可信代码**（`01 §5.5`），web storage 技术上任何同源代码都读得到 —— 那是信任模型的问题，不是存储 API 能解决的；
- 将来若上 iframe/worker 硬隔离，web storage 直接不可用，届时**必须**补一个 `host.storage`（或 postMessage 存储桥），并把迁移写进升级说明；
- `capabilities: ['write']` 仍然**禁止**（插件写自己的数据 ≠ 插件写应用数据，后者永远不开）。

## D-2 入口由 manifest 声明，宿主按规范渲染

**决定**：插件可以自定义**多个入口**，但只能通过 manifest 声明，形状由宿主规定；宿主负责把它们渲染成菜单项并打开对应标签。插件**不能**直接注入菜单、侧边栏或全局快捷键。

**manifest 扩展**（`surface.json`）：

```json
{
  "id": "course-manager",
  "version": "1.0.0",
  "entry": "panel.js",
  "kind": "plugin:course-manager",
  "entries": [
    { "id": "overview",  "title": "课程总览", "icon": "icon.svg",     "order": 10 },
    { "id": "schedule",  "title": "课表",     "icon": "calendar.svg", "order": 20 },
    { "id": "resources", "title": "课程资源", "icon": "folder.svg",   "order": 30 }
  ]
}
```

规则：
- 一个插件仍只注册**一个 kind**（`plugin:<id>`）；每个 entry 是同一 provider 的一个**标签实例**，`params.entry = <entry id>` 指明页面；页面内部路由（列表→详情→表单）由插件自理。
- `entries` ≤ 20 项；`id` 匹配 `^[a-z][a-z0-9-]{1,40}$` 且不重复；`title` ≤ 64 字符；`icon` 必须在插件目录内（同 manifest `entry` 的路径纪律）。
- **宿主渲染位置（V0.1）**：
  1. 右栏 `＋` 菜单的「面板插件」分组；
  2. 右栏**空态能力面板**（没有标签时那块）也列出来 —— 用户第一次看到的就是这里；
  3. 设置「面板插件」页每行一个「打开」按钮；
  4. 安装回执（工具返回值 / 设置页）写明「入口在右栏 ＋ 菜单 → 面板插件」。
  不注入左侧栏，不新增全局快捷键，不改 `ADDABLE_KINDS` 的内置语义。
- **禁用/卸载**：入口立即消失（`enabled:false` 或 index 无此项）；已打开的标签变墓碑（PR-10/14c 既有机制）。
- **持久化**：entry 随标签 `params` 持久化（R14c 已支持），重启后标签与页面都能恢复。
- **零入口 = 一个默认入口**（标题取 `name` / id）：任何已安装且 enabled 的插件**至少有一个可见入口** ——
  不允许出现「加载成功但界面上什么都看不见」的状态。这是入口闭环的硬约束，不是体验优化。

## D-3 校验与失败

| 情况 | 行为 |
|---|---|
| `entries` 形状非法 / id 重复 / 超 20 项 / icon 逃逸 | **安装 rejected**，index 一个字节不动（复用 PR-13 的「校验先于写盘」纪律） |
| 单个 entry 的 icon 缺失或读不到 | 该入口用 kind 图标兜底，**不阻塞安装** |
| 点击入口时 provider 尚未注册（插件加载失败） | 打开墓碑标签 + 可读原因；不影响其它入口与其它插件 |
| 插件被禁用 | 入口从菜单消失；已开标签保持墓碑，可从设置页重新启用 |

## R16 验收（入口闭环）

1. 装一个含 3 个 `entries` 的 `course-manager` 模板 → `＋` 菜单出现「课程总览 / 课表 / 课程资源」三项；
   空态能力面板里也能看到它们；设置页每行有「打开」；
2. 点任一项 → 标签打开、标题取自 manifest、`params.entry` 正确、面板渲染对应页面；
3. `reload` → 三个标签恢复，标题与页面正确；
4. 禁用插件 → 入口消失、已开标签变墓碑；重新启用 → 入口回来、墓碑可恢复；
5. 卸载 → 入口消失、`.trash` 有备份；
6. 非法 `entries`（重复 id / 21 项 / icon 逃逸）→ 安装 `rejected`，index 不变；
7. **零 entries 的插件**装完后也必然有一个默认入口（不是「加载成功但看不见」）；
8. 全程 `pageerror = 0`；变异至少覆盖：入口不按 enabled 过滤、点入口不传 `params.entry`、标题不取 manifest —— 各自必须红。

## R16 落地记录（入口闭环）

> 一句话：manifest 声明入口 → Go 在**写盘前**校验 → 渲染层从 `surface:index` 读 manifest →
> 宿主画三处入口 → `openTab({kind, params:{entry}})` 打开**一个 entry 一个标签**。

**A. manifest 与校验（Go）**

- `desktop/bridge/surfaces.go:133` `surfaceManifest.Entries []surfaceEntryDecl`（`{id,title,icon,order}`，形状见 `:139`）。
- 校验在 `validateSurfaceSource` 里、**hash 与拷贝之前**：`desktop/bridge/surfaces.go:706`；
  规则在 `:761` `validateSurfaceEntries`（≤20 项 / id 匹配插件 id 正则且唯一 / title 非空 ≤64 **字符**（rune）/ icon 走路径纪律），
  图标的路径纪律在 `:799` `validateSurfaceEntryIcon`（绝对路径 / `..` / 反斜杠 / 软链逃逸 = 拒绝；
  **文件缺失 = 合法**，渲染层用 kind 图标兜底）。任何一条不过 = `rejected`，index 一字节不动。
- **index.json 的 schema 不动**：entries 是 manifest 字段，读的时候顺带取（下一条）。

**B. 渲染层：索引 → 快照 → 三处入口**

- 索引读取：`desktop/app/electron/surface-index.ts:40`（`SurfaceEntryDecl`）、`:127` `parseEntries`（防御性：坏项丢自己，icon 坏只丢图标）、
  `:150` `displayName`（zh → en → 任意非空），`:217` 写进 `SurfaceIndexItem.entries`。
- 入口快照：`desktop/app/src/surfaces/plugin/entries.ts:128` `setSurfaceEntryIndex`（**唯一写者**是 loader 的 `syncOnce`，
  `desktop/app/src/surfaces/plugin/loader.ts:338`）；`:149` `listPluginEntries`；`:67` `entryItemsOf`（零 entries → 一个默认入口，
  id = `default`，标题 name → id）；`:99` `groupEntryItems`（三处共用一份分组口径）。
- 入口 1（右栏 ＋ 菜单）：`desktop/app/src/components/RightPanel.tsx:665`（分组「面板插件」+ 多插件时插件名小标），
  打开动作 `:240` `openPluginEntry`。
- 入口 2（空态能力面板）：`RightPanel.tsx:782`（`tabs.length === 0` 那块，与 ＋ 菜单同一份数据）。
- 入口 3（设置页）：`desktop/app/src/components/SurfacesPage.tsx:56` `onOpen`（第一个 entry）、`:244` 「打开」按钮、
  `:290` 每行的提示「入口在右栏 ＋ → 面板插件」。
- 标签标题：`RightPanel.tsx:437` —— **已声明**的 entry 用 manifest 的 title（菜单与标签同名）；
  默认入口与插件自己 `openTab` 的标签仍归 provider.title。
- 标签身份（R16 裁决后定稿，**PR-00 的 `dedupeKey` 契约**）：`desktop/app/src/store/useAppStore.ts:1501` `tabKeyOf` /
  `:1520` `pluginSpecOf` / `:1535` `stableStringify`：
  `provider.dedupeKey(tab)` 优先（`:1510`）；没声明时缺省 = `stableStringify(params ?? {})`（`:1511`）。
  `entry` 本身在 params 里，所以「课表」和「资源」是**两个标签**（同 kind），点第二个不会把第一个换掉；
  同时 `entry=resources, fileId=42` 与 `fileId=7` 是**两个**标签（同一入口的不同上下文），
  而声明了 `dedupeKey: tab => params.courseId` 的插件会把它们按 courseId 合并。
  `stableStringify` 是键排序的序列化 —— `{a:1,b:2}` 与 `{b:2,a:1}` 是同一份数据，用 `JSON.stringify` 会多出一个标签。
  纪律：`dedupeKey` 必须是**纯函数**（同一 params 恒定同一键），它每次 `openTab` 都会对已有标签重算。
- 图标：`desktop/app/src/surfaces/PluginEntryIcon.tsx`（entry.icon → `pluginAssetUrl`（`loader.ts:109`，与入口模块同一条 base），
  读不到退回 kind 图标）。

**C. 模板与技能**

- `templates/surface/surface.json`：3 个 entries（总览 10 / 课表 20 / 资源 30）。
- `templates/surface/panel.js`：一个 render 入口按 `tab.params.entry` 分发三页（零依赖、无 JSX）；
  没有 params 时落回总览 —— 老标签与「按 kind 直接打开」都不会空白。
- `desktop/vendor/create-mode-skills/create-mode/SKILL.md`：「Multiple entrances (`entries`)」一节（怎么写、`params` 怎么读、入口在哪三处、
  零入口的默认入口语义、内部路由归插件）。

**D. 测试与变异**

- Go：`desktop/bridge/surfaces_test.go` 的 `TestSurfaceEntriesRejectedBeforeWrite`（五连：重复 id / 21 项 / 非法 id /
  title 超 64 / icon 逃逸 → `rejected` + index 逐字节不变 + 无 `<id>` 目录 + 无暂存目录）、
  `TestSurfaceEntriesRoundTrip`（合法 entries 经 `List` + `Manifest` 原样拿回；缺省 / 空数组 / 图标缺失都合法）。
- 读侧单测：`desktop/app/electron/surface-index.test.ts`（IPC 被探针整条替掉，这一层没有别的覆盖）。
- e2e：`desktop/app/e2e/verify_plugin_entries.mjs`（端口 5231，`_serve.mjs` 端口归属自检，**58 项**；
  T8b 是合并后补的标签身份契约：同 entry 同 params = 1 个、不同 params = 2 个、键序不同 = 同一个、
  provider 的 `dedupeKey` 生效）。
- 变异（真跑 + FAIL + 复原，共 8 条）：入口不按 enabled 过滤 / 点入口不传 `params.entry` /
  标题不取 manifest（用 id）/ 零 entries 不给默认入口 / 设置页「打开」取最后一个 entry /
  插件标签不按 `(kind, entry)` 认身份（v1 的写法）/ 删掉 `provider.dedupeKey` 分支 /
  `stableStringify` 换成 `JSON.stringify`（键序敏感）—— 各自必红。

**门禁（2026-11-14，合并 main/R11b + dedupeKey 修复后全绿）**：`go vet ./...`、
`go test ./desktop/bridge -run TestSurface`、`tsc --noEmit`、`eslint src`（0 error / 59 warning）、
`vite build`、`test:surface-index`、`test:right-tabs`（238）、`test:rightpanel-select`（9）、
`verify_plugin_entries`（59）、`verify_surfaces_page`（51）、`verify_plugin_loader`（50）、
`verify_plugin_tab_persist`（33）、`e2e:tabbar`（89）、`e2e:surface-boundary`（82）、`e2e:session-tabs`（45）。

**R16b 收口（2026-11-14）**：上表三条「已知限制/已知不一致」（title 本地化、回执入口提示、
＋菜单键盘）全部落地；新增 `test:surface-install` 与 `e2e:external-surface` 两条可重复验收命令，
外部插件（干净子智能体只读 SKILL.md 写出的课程 MVP）已在真 Electron 里装得上、点得开。

**落地与裁决（2026-11-14 定稿；改之前先读）**

- **菜单分组**：接受现状 —— 一个分组「面板插件」；**多于一个** enabled 插件时，组内每个插件一行名字小标
  （单插件不画，少一行噪声）。**记为 V0.1 的 UX 债务**：20 入口 × N 插件时单层菜单会很长，
  届时再考虑折叠 / 搜索 / 二级菜单。
- **默认入口标题**：接受固定优先级 `manifest.name.zh → name.en → 任意非空 → 插件 id`，
  默认入口的 `params.entry` 固定 `default`。`entries[].title` **已支持** `string | { zh, en }`
  （R16b 落地：Go 逐语言校验 ≥1 非空 / 每个 ≤64 rune、electron 原样透传、渲染点按当前语言现选）；
  单字符串写法行为不变（两种语言都原样显示）。
- **设置页文案**：接受 ——「打开」+「入口在右栏 ＋ → 面板插件」；停用后按钮置灰、
  提示换成「插件已停用：启用后入口才会回到右栏」。
- **标签标题（契约）**：**已声明** entry 的标签标题由**宿主**用 manifest 的 `title` 覆盖
  （菜单与标签必须同名）；动态标题留给**插件自己 `openTab` 的标签**与默认入口（那些走 `provider.title`）。
  插件若需要「课表 · 周一」这类标题，应把它作为入口内部的页面内容表达，而不是改标签名（V0.1 口径）。
- **模板 id**：保持 `cost-board`（`verify_plugin_loader` T10 按这个 id 断言真模板渲染，改名会打红既有门禁）；
  三个 entry 的标题按本轮任务取「总览/课表/资源」，D-2 里的 `course-manager` 例子只示意形状。
- **安装回执的入口提示**：**已落地**（R16b）—— `surface_install` 回执带 `entries: <n>` 与
  `entryHint: "right panel ＋ → Panel plugins"`，`applied` 与 `already_installed` 都带；`uninstall` /
  `rejected` 不带（那两种状态下「入口在哪」没有意义）。
- **菜单键盘可达性**：**已落地**（R16b）—— ＋ 菜单打开即聚焦首项、↑↓ 在菜单项间移动并回绕
  （含插件入口分组边界）、Enter 走原生激活、Esc 关闭并把焦点还给 ＋；`aria-haspopup` / `aria-expanded`
  同步。与 `+N` 溢出门的 roving focus 口径一致。

## 明确不做（V0.1）

- 通用存储 API、文件读写、网络白名单；
- 左侧栏入口、命令面板注册、全局快捷键、自定义主题注入；
- 插件页面之外的 UI 注入（overlay / toast / 对话框由宿主提供，插件只能在自己的面板内画画）。
