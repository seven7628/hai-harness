# 创造模式 V0.1 —— M0/M1 可执行 PR 序列

> 配套：`docs/creator-mode-v0.1/01-design.md`（设计稿）
> 日期：2026-11-11　状态：待执行
> 原则：**一个 PR 只做一次语义变更**；每个 PR 都能单独 revert；先把测试改绿，再改实现（PR-02 / PR-05b / PR-06 / PR-12 / PR-13 强制）。
> 规章：`00-conventions.md`——开工前读 §3.1（八条硬规则）与 §4（DoD 检查表）；工具类 PR 另过 §2（CodeMode 契约）。

---

## 0. 已拍板（本次补充）

| # | 决策 | 影响 |
|---|---|---|
| D-A | **内容触发时自动展开右栏**（点 diff / 文件树 / 网页 / PPT / agent 打开文件） | 手动收起不加锁，下一次内容触发仍会展开 |
| D-B | **默认 session 作用域**，`workspace` 只留给无进程级视图的看板类 | ADR-0 不动；切会话切标签集，切回来要恢复 `activeId` |
| D-C | **`⌘S` 只在编辑器聚焦时拦截**，全局不绑定 | `DESKTOP.md §5.7` 的旧措辞要改，否则会被改回去 |

---

## 1. PR 总览（24 个，08e 可选；另 2 个为收口与缓冲）

| # | 标题 | 类型 | 估时 | 依赖 | 风险 |
|---|---|---|---|---|---|
| 00 | 契约与注册表骨架（无 UI 接入） | 纯新增 | 0.5d | — | 低 |
| 01 | 三态布局（open/bias/leftPinned）+ 删 `rightOverHalf` | 行为 | 1.5d | 00 | 中 |
| 02 | `activeId` 权威、`rightTab` 派生（零行为变化） | 语义 | 1.5d | 01 | **高** |
| 03 | git → SurfaceProvider | 迁移 | 0.5d | 02 | 低 |
| 04 | file → SurfaceProvider（多实例身份） | 迁移 | 1d | 03 | 中 |
| 05a0 | 提取 `internal/atomicfile`（flock + 原子替换，零行为变化） | 重构 | 0.5d | — | 低 |
| 05a | Go：`file_open` / `file_save` / 外部变更推送 | 新增 | 1.5d | 05a0 | 中 |
| 05b | 内嵌编辑器（CodeMirror 6 + 冲突条 + dirty） | 新增 | 2d | 04,05a | **高** |
| 06 | web → SurfaceProvider（视图池 / 休眠 / 重建） | 迁移 | 2d | 05b | **高** |
| 07 | activity → SurfaceProvider | 迁移 | 1d | 06 | 中 |
| 08 | 文档渲染器注册表（docx/pptx/xlsx/pdf/csv/图片/文本接入） | 新增 | 1.5d | 04 | 中 |
| 08a | 转换服务（准入/限额/内容寻址缓存/输出保留） | 新增 | 2d | 08 | 中 |
| 08b | 表格增强（sheet 导航 / 冻结 / 数字格式 / CSV 双视图） | 增强 | 1.5d | 08a | 低 |
| 08c | 媒体渲染器（`hai-media://` Range + ffprobe + 播放器） | 新增 | 2d | 08 | **高** |
| 08d | **LibreOffice kit**：下载/探测/校验 + profile 隔离 + `soffice→PDF` + `renderImages`(pypdfium2) | 新增 | 3d | 08a | 中 |
| 08e | （可选）ffmpeg 转码 + 缩略图 | 可选 | 2d | 08c | 中 |
| 09 | 标签条：溢出 / 右键菜单 / `⌃⇥` / 空态能力面板 | 交互 | 1d | 04 | 低 |
| 10 | Tombstone + Boundary + 3 次自动禁用 | 健壮性 | 1d | 09 | 低 |
| 11 | 删除 legacy：`TAB_KINDS` / `makeTab` switch / `rightTab` | 清理 | 0.5d | 10 | 中 |
| 12 | `ui` 通道 + `ui_inspect` 工具 | 新增 | 1.5d | 11 | 中 |
| 13 | 安装器（index/manifest/install/remove/audit） | 新增 | 2d | 12 | 中 |
| 14 | 常驻工具接线 + `hai-surface-dev` skill + 模板 + 插件页（**无 mode**） | 新增 | 1.5d | 13 | 低 |
| 15 | 验收收口：e2e 补全 + 文档回写 | 收尾 | 1d | 14 | 低 |
| 16 | （缓冲）修复与打磨 | 缓冲 | 2d | — | — |

**合计 ≈ 34.5 人日**（含缓冲 2 人日；08e 转码可选 2 人日，不做则 32.5）；若 05a0→05a 与 03/04 并行、09/10 与 06-08 并行，日历时间约 4 周。

---

## 2. 每个 PR 的条款

> 每条 PR 的完成标准 = 本节「验收」+ `00-conventions.md` §4 对应类别的检查表。
> 工具类（PR-12/13/14）额外要求 §2 的 C1-C7 与 `e2e/verify_codemode_surfaces.mjs` 四段断言。

### PR-00 契约与注册表骨架

**目标**：先把"扩展点长什么样"钉死，不接 UI——这样后面每个 provider 迁移都只是填空题。

**改动**
- 新增 `desktop/app/src/surfaces/types.ts`（`SurfaceKind` / `SurfaceProvider` / `SurfaceRenderContext`，含 `scope/weight/singleton/allowDuplicate/capabilities`）
- 新增 `desktop/app/src/surfaces/registry.ts`（`registerSurface/getSurface/listSurfaces`）
- 新增 `desktop/app/src/surfaces/registry-test.ts`（`node --test` + esbuild，见 00 §1.9）：
  - 重复 kind 抛错；内置前缀 `plugin:` 被拒；`listSurfaces()` 按 `group/order` 稳定排序

**验收**
- `npm run test:surface-registry` 绿（已进 `npm test` 链条）
- 应用行为零变化（不 import 到 RightPanel）

**回滚**：删两个文件，无副作用。文档回写（03/00/看板）随 PR-15 收口统一提交——`docs/` 未被 git 跟踪，代码可 revert、口径不可 revert。

---

### PR-01 三态布局

**目标**：把 `App.tsx` 的隐式规则换成显式状态机。

**改动**
- 新增 `desktop/app/src/lib/layout.ts`：`expand/collapse/afterDrag/biasOfRatio/normalizeLayout`
  （`ratio` = 右栏占「除左栏外」的百分比；入参是库的 layout 百分比，不用 `leftPinned` 推）
- `store`：会话级 `rightLayout: RightLayout`；`setRightLayout` 是唯一写者，`rightExpanded` 是它的镜像
  （`layoutMirror()` 在读出时归一化，避免漂移被持久化）
- `App.tsx`：删 `rightOverHalf`；Group 级 `onLayoutChanged` 只在右栏已展开时记比例；
  预设 bias 时按比例 `resize`（`rightSize` 非空时让位，避免两个 effect 抢写）
- `RightPanel.tsx`：头部 `[平分][居中][右重][✕]` + `aria-label`；点预设会清 `rightSize`
- `scripts/test-right-panel-tabs.mjs`：夹具补 `rightLayout`（否则 `rightExpanded` 会造出漂移态）——属本轮必要改动
- i18n：`rp.bias.even/center/right`（+ `rp.bias.label`）

**验收**
| 用例 | 断言 |
|---|---|
| 启动有会话 | 右栏 `isCollapsed()`；左栏 236 |
| 点展开 / `⌘\` | 右栏展开；**左栏不收**（2026-09 修复）；预设 even |
| 点"居中" / "右重" | 右栏占比 36% / 64%（±3）；`rightSize` 被清空 |
| 手动拖到 55% | 左栏**不**自动收起，右栏 bias 标 `custom`，松手不跳宽度 |
| 手动拖到 20% 宽（窄拖拽） | 仍记 `custom`（守卫不能把窄拖拽当抖动丢掉） |
| 收起后拖左/中分隔条 | 右栏**不被弹出**（D-A：自动展开只属于 `openTab`） |
| 右栏收起 | 中占满；左栏不动 |
| 1024 / 1440 / 1920 三档 | 无面板低于 minSize，无横向溢出 |

**验收方式（F29/F30）**：行为断言用 `scripts/test-right-panel-tabs.mjs`（220 项）+ `npm run test:layout`（8 例）
兜住；**真渲染的 `e2e/verify_layout_states.mjs` 与 P04/P05 帧率推迟到 PR-15 收口**（本机无人值守无法驱动真实指针拖拽）。

**回滚**：revert `f03f420` 之后的合并提交；无数据迁移（`rightLayout` 缺省按 `rightExpanded` 归一化）。
不回滚开关：旧规则（>50% 收左栏）本身是「左栏莫名消失」的缺陷来源，且本 PR 无持久化 schema 变更（F30）。

---

### PR-02 `activeId` 权威（语义反转，零行为变化）

**目标**：为"同 kind 多实例"扫清地基。**这是全程风险最高的一个 PR**，单独一个 commit，先改测试。

**改动**
- 新增 `scripts/test-activeid-authority.mjs`（17 项不变量：动作后 `rightTab`=激活 kind、外部改 `rightTab` 不换标签、标签动作不碰布局、关光为空态、切会话恢复 activeId）
- `syncTabsToRightTab` 删掉反向收敛分支②③，只保留「无标签集时按 `rightTab` 建默认集」；`restored` 改为「先补派生字段，再清标记」
- 抽出 `rightTabPatch` 作为 `rightTab` 的**唯一写点**（`tabPatch` 走它）
- `store`：`activeId` 成为唯一真值；`rightTab` **保留字段**，改由 `applyTabs()` 一处写穿（`rightTab = kindOf(active)`）——**不要**改成组件里 selector 现算
- `applyTabs(state, scope, nextTabSet)` 成为标签变更的唯一入口（8 条路径全部改调它）；仓库里它的现名是 `tabPatch`
- `syncTabsToRightTab` **不删**，退化为「无标签集时建默认集 / 恢复态补派生字段并清标记 / 派生字段落后时纠正」（实现偏差见 03 §2.4）
- 8 条改写路径（`openTab/activateTab/closeTab/reopenTab/syncWebTabs/syncTabsToRightTab/deleteSession/deleteWorkspace`）逐个核对
- e2e 先改：`verify_auto_open_visual.mjs:114` 的 `setState({rightTab})` → `activateTab(id)`；`verify_pdf_preview.mjs:284` 的订阅改 `activeId`

**验收**
- 全部既有 e2e 绿（这是唯一判据，**不允许出现像素变化**）
- 新增断言：同一 kind 连续开两个标签，`rightTab` 相同但 `activeId` 不同（PR-04 之后才真正用到）

**回滚**：单独 revert；若已产生数据，`rightTab` 派生值可随时重算，无脏数据。

---

### PR-03 git → provider

**改动**
- 把 `ChangesTab` 从 `RightPanel.tsx`（原 341-711 行）整体平移到 `components/ChangesTab.tsx`，**行为零改动**
- 顺带抽出两个纯工具：`lib/fileStatus.ts`（`FileActivityStatus` + `fileStatusLabel`，原本在 RightPanel 里被两处用）、`lib/gitStatus.ts`（`EMPTY_GIT`）——否则新文件会因「组件文件里导出常量」吃 react-refresh 告警
- `surfaces/builtin/gitSurface.tsx`：provider 元数据（workflow/order 20/singleton/session/light/read+view）+ 注册
- `surfaces/builtin/index.ts`：**副作用 import** 的注册入口，`main.tsx` 只 import 一次（不要改成遍历数组，否则 tree-shaking 会把没人引用的 provider 删掉）
- `RightPanel` 的 git 分支：`getSurface('git')?.render(...) ?? <ChangesTab />`（provider 未注册时退回直接渲染，保证 e2e/单测不依赖注册顺序）
- `src/surfaces/builtin/index-test.ts`：注册冒烟测试（证明副作用 import 真的注册进去了）

**验收**：`npm run test:builtin-surfaces`（1 例）+ `npm run test:right-tabs`（220 项）+ `test:activeid`（26 项）+ `test:layout`（8 例）全绿；`tsc`/`eslint src` 干净；标签条位置不变。

**回滚**：provider 文件删除 + 恢复一个 case（单 commit revert）。

---

### PR-04 file → provider

**改动**
- `FilePreviewTab` 与文件工具（`OperationDiff`/滚动记忆/路径工具/活动聚合）从 `RightPanel.tsx` 平移到
  `components/FilePreview.tsx` + `lib/{filePaths,fileActivity,format}.ts`（净增约 86 行，其余是移动）
- `surfaces/builtin/fileSurface.tsx`：`singleton:false` / `allowDuplicate:true` / `capabilities:['read','view','write']`；
  `(path, mode)` 去重仍留在 store 的 `tabKeyOf`（跨 provider 通用，provider 不碰身份）
- `RightPanel` 的 file 分支走 `getSurface('file')?.render(...)`，未注册退回直接渲染；`Fragment key={activeId}` 保持整体重挂
- **D-A 接线**：自动展开收口到 `openTab`（`!activate` 与已展开都不展开）；`toggleRight`/`setRightExpanded` 仍是用户手势路径
- web 判据（embedHide / 待重建 / toolbar）改读 `activeTab.kind`；effect 依赖一起改（同类多实例切换时 `rightTab` 不变）

**验收**：`test:activeid` 32 项（含 F 段同 kind 双实例 + C 段 D-A 无锁）、`test:right-tabs` 220 项、
`test:builtin-surfaces` 2 例、`test:layout` 8 例全绿；`tsc`/`eslint` 干净；`verify_auto_open_visual.mjs` 归 PR-15（F31）。

---

### PR-05a0 提取 `internal/atomicfile`

**目标**：让 bridge 的 `file_save` 与 `tools/builtin` 的 `write_file`/`edit_file` 用**同一把锁**（flock），而不是各写一套。

**改动**
- 搬移 `tools/builtin/lock.go`（116 行）→ `internal/atomicfile`：`Lock(path) (*os.File, error)` / `Replace(lock, path, content)` / `Append(path, content)`，保留原文的全部注释与错误文案
- `tools/builtin` 改为薄壳调用（`lockPath`/`replaceLocked` 保留同签名转发，避免一次改太多调用点）
- **零行为变化**：`go test ./tools/...` 全绿；补一个跨包并发测试（两个 goroutine 交替 `Replace` 同一文件，无丢更新）

**为什么不能各写一套**：`lockPath` 是 **文件 fd 上的 flock**（POSIX 建议锁、跨进程），不是内存 mutex。复制一份实现等于两个互不相识的锁；bridge 与工具交替写同一文件时，`file_save` 的"读 hash → rename"窗口会被工具的一次写入穿透，用户保存的内容静默覆盖 agent 的改动。内存 mutex 更糟：只在本进程有效，且与 flock 不互斥。

**回滚**：`internal/atomicfile` 删除 + `builtin` 恢复原实现（单 commit revert，无数据影响）。

---

### PR-05a Go：编辑写路径

**改动**
- `desktop/bridge/file_edit.go`：`file_open`（读+登记）/ `file_save`（持锁比 hash → 原子写）/ `file_close`（退订）
- 写入走 `internal/atomicfile`（R05a0）；per-path flock + 锁内重读比对，锁被占用直接 `conflict`
- 大小/二进制：沿用 `maxPreviewBytes`（1 MiB）与 NUL 判定，超限/二进制 → 只读（`truncated`/`binary`）
- 打开集合：64 条 LRU + 引用计数（同一路径多标签不误退订）；watcher 按目录挂、引用计数归零才 `Remove`
- 外部变更：**fsnotify** 目录监听 + 300ms 去抖 → `file_changed{path, hash, by:"external"}`；自家保存 2s 同 hash 丢弃
- Go 单测 9 例：open/hash、越界拒绝、save 落盘、baseHash 冲突不改文件、flock 冲突、未授权 denied、外部推送、自家不推、close 退订

**验收**：`go test ./desktop/bridge/ -run 'TestFileOpen|TestFileSave|TestFileChange'`；
**偏差**：错误信封扁平（F33）、`editor_open_set` 取消（F34）、`by` 恒 external（F35）、`allowNew`（F36）；真渲染 e2e 归 PR-15（F31）。

---

### PR-05b 内嵌编辑器（零依赖受控 textarea + 行号 + 只读 shiki 高亮；F32）

**改动**：`components/Editor.tsx`（CM6 懒加载 + 语言包按扩展名）；`file` 的 `mode='edit'`；冲突条三动作；dirty 标记进 tab；关标签/退出确认；`window.desktop.setDirty` 镜像。

**验收（新增 `e2e/verify_editor.mjs`）**
| 用例 | 断言 |
|---|---|
| 打开 → 改 → `⌘S` | 磁盘内容更新；tab 去掉 dirty 点 |
| dirty 时外部写同文件 | 黄条出现；三条动作都可执行且结果正确 |
| clean 时外部写 | 静默 reload，光标行号保持 |
| 关闭 dirty 标签 | 出现确认；取消后内容仍在 |
| 退出应用 | `before-quit` 被拦截一次 |

**回滚**：`mode='edit'` 入口加 feature 开关（`settings.editor = false` 时不出现"编辑"动作），可一键关掉。

---

### PR-06 web → provider（风险最高）—— **拆三轮执行（R06a/b/c，见 03 §2.3）**

> 拆轮依据（只读分析定稿）：三条最危险的风险（原生 overlay 挂载时机、`menuOpen/immersive` 决定 `embedHide`、身份键两套 id 空间）只出现在**渲染委派**这一步；C1/C2 的失败模式是 `tsc`/单测秒级红，C3 的失败模式只有真 Electron e2e（3-5 分钟/轮）才抓得到。合一等于用快反馈环保守两类不同失败，且 bad-PR 时无法只回退渲染层。
>
> - **R06a** 身份判据下沉 `lib/webTab.ts`（零 store 运行时依赖）：`60ae0b5`
> - **R06b** 注册 `webSurface` + `title/icon` 委派，render 仍由 RP 兜底
> - **R06c** `WebTab.tsx` 平移 + `panelMenuOpen` 上移 + render 走 provider 带回退；**合并前必须手跑** `e2e:pdf/container/scoping/session-tabs/auto` 并采 P09/P10 内存基线（R-5）

**改动**：`webSurface.tsx` 承接 `WebTab`；`isPendingWebTab / rebuildWebTab / webTabKey` 移入 provider 或保持 store 但由 provider 调用；视图池 LRU/休眠/上限不变；`TABS_MAX_VISIBLE` 溢出在 PR-09 处理。

**验收**：`node e2e/verify_pdf_preview.mjs`、`desktop_browser_e2e.mjs` 全绿；休眠标签点开能唤醒且 URL 不丢；重启后标签为"待重建"且点开才建视图。

**回滚**：与 PR-03 同法；但**不要**与 PR-09 合并。

---

### PR-07 activity → provider

`ActivityTab` 平移，trace 选择器 / Span 抽屉 / `Alt+←→` 不动；验收 `trace` 相关 e2e。

### PR-08 文档渲染器注册表（**不是** office provider）

**目标**：取消 `office` kind，把 docx/pptx/xlsx/pdf/csv/图片/文本统一进 `file{mode:'preview'}`。见设计 §4.7、调研 `05-preview-media-research.md`。

**改动**
- `surfaces/preview/types.ts` + `registry.ts`：`PreviewRenderer{match, weight, load}`，按扩展名优先级选渲染器
- `renderers/`：`text`、`image`、`csv`（复用 `CsvPreview`）、`pdf`（内嵌浏览器）、`docx`、`pptx`、`xlsx`（复用现有转换产物）
- `file` 标签新增 `mode='preview'`；`visualFile.ts` 继续当单一事实源
- 已持久化的 `office` 标签（若有）迁移为 `file{mode:'preview'}`，结构版本 +1

**验收**：8 种格式各开一次全部可见；渲染器命中可观测（`data-renderer="pptx"`）；未安装运行时给 `unavailable` 而不是空白。

**回滚**：渲染器注册表是新增路径，旧 `previewOfficeFile` 保留一个版本作开关。

### PR-08a 转换服务

**目标**：把三个 previewer 收进 `desktop/bridge/conversion/`，照 DSH 的限额（并发 2 / 60s / 50-100 MiB / 内容寻址缓存 / 输出 24h + 512 MiB LRU）。**行为零变化**，只是收口 + 加缓存与限额。

**验收**：同一文件二次打开命中缓存（<50ms）；100 MiB 输入返回 `too-large`；Python 异常返回 `failed` 且不缓存；启动清理过期目录。

**落地口径（R08a 实施记录；评审 CHANGES 后的最终形态）**
- 缓存键 = `sha256(口径版本 | 脚本指纹 | kind | 源文件名 | 参数指纹 | 文件字节)`。
  - 源文件名是**必须**的一位：三个脚本都把 `basename(src)` 写进产物标题，只按字节做键会让「另存为」后的预览标题停在旧名字。
  - **脚本指纹**（`builtin.PreviewScriptFingerprint`，三个脚本常量的 sha256）取代了「构建标识」：vcs revision 在 `go run` / `buildvcs=false` / 改了脚本没提交时都不变，恰好漏掉要防的场景；脚本指纹只随产物生成规则变。
- 输入上限只留一道硬闸（100 MiB → **`too_large`**，下划线拼法，与既有 `file_edit` 的码一致）。不设「50 MiB 软上限」档：软上限只有两种落地，成功带警告字段（改响应形状）或在 50 MiB 就拒（与验收口径冲突、且 99 MiB 的演示文稿本可转）。50 MiB 继续作为**容量规划口径**（512 MiB LRU ≈ 10 份大输入），不做运行时判定。
- 错误码（进 `respCode` 信封）：`too_large` / `timeout` / **`queue_timeout`** / `canceled` / `failed` / `not_found` / `invalid_argument`。
  - `timeout` = 一次转换超过 60s（已 kill 进程组）；`queue_timeout` = **还没开始转**就等太久（默认 2×60s，`Options.QueueTimeout` 可调）；`canceled` = 工作区关闭/退出时主动取消。
- `~/.go-code/previews` 是**只读放行**（`readOnlyConfigSubtrees` + `sandbox.defaultReadOnlyOpenSubpaths`）：前端要经 `file_preview` 读回 html_path（不读就是「转换成功但读不回」），但 `write_file` / `edit_file` / `file_save` / 沙箱内写一律拒绝（能写 = 把 index.html 换成伪造文档，用户看到的是「带 app 外壳的假内容」；缓存键是内容哈希、模型算得出来，所以「难猜」不是防线）。软链接/硬链接逃逸仍由 `denyHarnessConfigPath` 既有规则拦。
- 产物权限 **0600**、条目目录 0700（`internal/atomicfile.ReplaceReader(..., mode)`）：缓存里是用户文档内容，与 memory 子树同口径。

### PR-08b 表格增强

多 sheet 导航（sticky 锚点）、冻结窗格（`ws.freeze_panes` → `position:sticky`）、数字格式/合并单元格/列宽、CSV「表格 / 文本」双视图。**不加前端依赖**。

**验收**：20 个 sheet 的工作簿可跳转；冻结表头滚动时固定；截断仍如实告知。

### PR-08c 媒体渲染器（新能力）

**改动**
- `electron/media-protocol.ts`：`hai-media://` 支持 `Range` → 206；路径规范化 + 仅允许 workspace 内
- `media_probe` 命令：`ffprobe -print_format json`，按 `(path,size,mtime)` 缓存
- `renderers/media.tsx`：`<video>`/`<audio>` + 元信息 + 编解码判断 + 「系统播放器打开」
- 编解码矩阵见 05 文档 §4 D-L：mp4/h264、webm/vp9、mp3/wav/ogg/flac 直放；mkv/avi/ts 给转码入口

**验收**：1 小时 mp4 拖动即时；切走切回不重下；mkv 明确提示；无 ffmpeg 时只降级不报错。

**风险**：媒体协议成为任意文件读取通道 → 必须路径校验；编解码差异 → 以 ffprobe 为准。

### PR-08d LibreOffice kit（参考 DSH，**核心项**）

**目标**：pptx/docx/xls/xlsx/ppt 走 `soffice → PDF → 现有 PDF 渲染器`；kit 缺失时降级近似 + 横幅。

**改动**
- `internal/officekit`：探测（系统 `soffice` 优先，其次 `~/.go-code/office-kit/<ver>/<platform>`）、`KIT.json` sha256 校验、平台矩阵
- 转换：每 job 独立 `-env:UserInstallation`、`--headless --norestore --nolockcheck`、进程组 kill、60s 超时、安装期预热一次
- `renderImages`：PDF → **pypdfium2**（运行时已装 5.9.0）逐页 PNG，`dpi` 可配，写缓存
- 工具：`office_convert` / `office_render`（给 agent"看图验收"），`recalculate` 留到有真实需求
- 设置页：安装/卸载/版本/体积/许可说明

**验收**：见 `05-preview-media-research.md` §8.6（并发不撞 profile 锁、超时清理进程组、卸载即降级、`office_render` 指定页）。

**风险**：kit 分发（体积/签名/许可）与首启冷启动。

### PR-08e（可选）ffmpeg 转码 + 缩略图

转码队列（进度/取消/保留策略）+ 视频 poster。只在真有需求时做。

---

### PR-09 标签条交互 / PR-10 健壮性 / PR-11 清理

- **09**：`+N` 溢出下拉、右键 `关闭/其他/右侧/全部/固定`、`⌃⇥`、空态能力面板、中键关闭。
- **10**：`SurfaceBoundary`（每面板 ErrorBoundary）、挂载失败计数（localStorage，按 kind+hash）、3 次自动禁用、墓碑标签（`unavailable`）。
  - **落地偏差（R10，F64–F66）**：计数键只用 **kind**，不带 hash —— 派单给的记录结构是
    `kind/count/lastError/at/disabled`，而「provider 内容哈希」这一层现在拿不到（插件要等
    PR-12 的 loader 才有可哈希的产物）。**已裁决：同意这个取舍，不造假 hash**，风险记为 **R-20**。
    PR-12 把键改成 `kind@hash` 时的**迁移策略**：旧键一律视为「无 hash 版本」，首次遇到新 hash
    就按新键重新计数（不删旧键，它只是不再被这条插件命中）；在此之前把「发版后可能要手动
    重新启用一次」写进插件安装文档。
  - **复评推翻的两处**（三轮改动都在同一分支上，见 04 F66）：① 隔离**不用** `key={tab.id}`
    （会重挂 children、破坏 R06c 的原地复用），改成 boundary 内 `tabId` 变化时复位错误态；
    ② 墓碑的「移除标签」不用 `closeTabNow`（会静默丢未保存内容），改 `closeTabs([id])`。
- **11**：删 `TAB_KINDS`、`makeTab` 的 kind switch、`kindIcon/kindLabel` 的 switch、`rightTab` 字段本身（如果 e2e 已全部迁移）。**判据：`grep -c "kind === '" RightPanel.tsx` = 0，`grep -rn "rightTab" desktop/app/src` 只剩兼容注释。**
  - **落地偏差（R11，F67/F68）——三件做了、一件推迟，判据换了口径**：
    开工前核实两条前置：`grep -c "kind === '" RightPanel.tsx` 实得 **18**（不是 0）；`rightTab` 在
    `desktop/app/e2e/*.mjs` 有 **55 处 / 15 个文件**。按本条自己的措辞「**如果** e2e 已全部迁移」，
    `rightTab` 字段这一轮**不删** → 拆出 **R11b**。字段今天仍被两头用着，两条都得先迁：
    e2e **55 处 / 15 个文件**（`window.__store` 直写 `rightTab` 复位标签条）；
    `desktop/app/src` **47 行 / 4 个文件** —— 真代码是 RightPanel 的 rail 高亮
    （`rightTab === tab.key`）与 sync effect 的依赖，加 store 里 `tabPatch`/`rightTabPatch`/
    `viewFields` 的写穿，其余是注释里的口径说明。原判据「`grep -rn rightTab desktop/app/src`
    只剩兼容注释」今天**也不成立**（41 行在 store），一并记在这里，免得 R11b 再踩一次。
    其余三件已落地，且都是行为保持：`TAB_KINDS` → 注册表 `BUILTIN_KINDS`（+ `isBuiltinKind` 谓词）；
    `makeTab` 的 kind switch → provider 的 `defaultSpec`（file/web 各一份实现，`focusSpec`/`activePageIdOf`
    仍只有一份、在 store）；`kindIcon` 的 switch → provider 的 `icon`（`icon` 入参改为**可选** ——
    ＋ 菜单与空态能力面板没有标签实例）。
  - **判据改口径**：「`kind === '` 归零」这一轮**不成立，也不该为它硬搬**。剩下的 **16 行 / 17 处**逐条
    分了三类 —— web 的 embed 语义（7）、file 的 mode 与 path（5）、探针契约 `data-mode`/`data-view`（2）、
    原生宿主判定（2 处 1 行）、provider 兜底（1）。每条为什么留、什么时候才会掉，见 `03-code-plan.md` §5.4。
    **可守的数：RightPanel 的 `grep -c "kind === '"` = 16，且只许降不许升**（新增一条 `kind === '`
    必须先回答「provider 能不能给」—— 能，就进 provider；不能，是标签形状的固有语义）。

---

### PR-12 `ui` 通道 + `ui_inspect`

**改动**
- 协议：新增 `{"type":"ui","payload":{op, data}}` 信封（**不进** `events.Event` 28 类、不落盘、不重放）
- renderer → bridge：`ui_snapshot`（debounce 500ms，含 `layout/surfaces/tabs/editors/theme/viewport`）、`surface_status`
- bridge → renderer：`surfaces_changed`（安装/卸载后）
- Go 侧缓存 + `tools/builtin/ui_inspect.go`（只读，返回 `ageMs`，>5s 标 stale）

**验收**：装一个 dummy provider（不落盘，直接 register）→ `ui_inspect` 能读到它的 kind 与 tab；故意不推快照 → `ageMs` 增长并被标 stale。

**CodeMode 验收（必过）**：`run_code` 里 `const s = tools.ui_inspect({what:"tabs"})` 拿到**对象**（`Array.isArray(s.value.tabs)` 为真），不是 JSON 字符串；`ui_inspect` 标 `CanParallel: true`。

---

### PR-13 安装器 / PR-14 常驻工具 + skill + 模板 + 插件页（无 mode）

- **13**：`desktop/bridge/surfaces.go`：`~/.go-code/surfaces/index.json`（有序 + hash + source + enabled）、`.bak` 快照、`.trash`、`audit.jsonl`；manifest 校验；`surface_install/list/remove` 三个工具；失败不写 index。
- **14**：把 4 个工具注册进标准工具集（`settings.features.surfaceTools` 开关，**不改 `mode.go`**）；`skills/hai-surface-dev/SKILL.md`（`requires.tools` 走能力面过滤）；`templates/surface/{surface.json,panel.js,icon.svg}`；`PluginsPage`（列表 / 禁用 / 卸载 / 权限与 audit 对照）。
- 工具描述瘦身纪律：每个 ≤120 字、参数 ≤3 字段（常驻 schema 的成本约束，见设计稿 §5.1）。

**验收**：设计稿 §5.8 的 A–G 七条全绿。

**CodeMode 验收（必过）**：脚本内 `tools.surface_install({target})` 未批准时 reject，`catch` 到 `rejected`；审批超时 reject 到 `timed out`（两者措辞不同）；两个 install 并发（`Promise.all`）按帧号保序，不出现 index 写乱。

---

### PR-15 收口

- 回写 `docs/DESKTOP.md` §5.7（⌘S、右栏三态、多标签）；
- 更新 `docs/RIGHT_PANEL_TABS_PLAN.md` 的状态（阶段 6）；
- 截图（`capture_readme_longshot.mjs`）与 README 一句话；
- 全量 e2e 一遍。

---

## 3. 红线（PR review 必查）

1. **一个 PR 只迁一个内置 provider**；迁移 PR 里不允许顺手改布局或标签条。
2. **PR-02 / PR-06 必须先改测试**；测试没绿之前不 merge 实现。
3. **不允许 skip 既有 e2e**。要改断言，在 PR 描述里逐条写"为什么旧断言是错的"。
4. **每个 PR 都留一条"可观测证据"**：e2e 名称 / 截图 / `ui_inspect` 输出。没有证据不算完成。
5. **插件永远不能拿到 `write`**：`capabilities` 里 `write` 只允许 `namespace:'builtin'`，注册表在 `registerSurface` 时断言。

---

## 4. 门禁（M0 结束）

- [ ] `grep -c "kind === '" RightPanel.tsx` = 0
- [ ] 同时开 浏览器 + 2 文件（diff/edit）+ Git + PPT，逐个关、重开、切会话、重启全部正确
- [ ] 编辑器：改→存→agent 再读为改后；dirty 时 agent 写同文件必出冲突条
- [ ] 布局：三档宽度 × 三种 bias 手测矩阵通过
- [ ] 删掉任一内置 provider，其余能力与插件不受影响
- [ ] `go test ./agents/... ./tools/... ./desktop/bridge/...` 绿
- [ ] 全部 `desktop/app/e2e/*.mjs` 绿

## 5. 门禁（M1 结束）

- [ ] 一句 prompt 装出「成本看板」Tab，不重启出现、重启仍在、卸载后墓碑无残留
- [ ] 面板 throw 只黑自己；3 次自动禁用并给原因
- [ ] 越权调用未声明命令被拒 + `audit.jsonl` 有记录
- [ ] `ui_inspect` 的 `ageMs` 与 stale 标记符合预期
