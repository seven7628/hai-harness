# 创造模式 V0.1 —— 文档评审：隐式状态与歧义裁决

> 评审对象：`01-design.md`（设计）、`02-pr-plan.md`（排期）、`03-code-plan.md`（代码）
> 方法：把三份文档里的**同一概念**逐条对照（类型、命名、状态归属、时序、数量），凡是"文档没写但实现必须选一个"的地方，都记为隐式状态并要求给出唯一答案。
> 结论：**3 处阻断级、6 处高、19 处中低**，已全部裁决并回写文档。

---

## 一、隐式状态登记表（唯一权威）

> 判据：每一行都必须能回答「谁写、谁读、活多久、存在哪、失效后怎么办」。答不出来的，就是下一个 bug。

| # | 状态 | 归属 | 持久化 | **唯一写者** | 读者 | 不变量 / 失效处理 |
|---|---|---|---|---|---|---|
| S1 | `rightLayout {open,bias,ratio}` | store | localStorage（窗口级） | `expand/collapse/setBias/afterDrag` | `App`、`panelSizes` | 只存 ratio（百分比），不存像素；resize 重算 |
| S2 | `leftPinned` | store | localStorage | 用户点 pin | `panelSizes` | 只在 `open` 时有意义；收起右栏后保留 pin |
| S3 | `tabsByScope[scope] = {tabs, activeId, closed, restored}` | store | localStorage（结构版本 v2） | **只有 `applyTabs()`** | `RightPanel`、`TabBar`、e2e | 同一 `dedupeKey` 只有一个实例；`activeId ∈ tabs ∪ {null}` |
| S4 | `tab.dirty` | 标签实例 | ❌ 内存 | Editor 的 `setTabDirty` | 关标签确认、退出拦截 | 关标签/保存即清；切 scope 保留 |
| S5 | `editorDirtyCount` | **selector 现算** | ❌ | （不可写） | App 根 → `window.desktop.setDirty` | 禁止存成字段，禁止组件各自上报 |
| S6 | `editorOpenSet` | store | ❌ 内存 | Editor mount/unmount（**全量上报**） | bridge `editors` | 关闭即移除；bridge 以最后一次上报为真值 |
| S7 | `fileChanges[path]` | store | ❌ 内存 | bridge `ui{file_changed}` | `Editor` | 保存/静默 reload/关标签后 `clearFileChange`；上限 32 LRU |
| S8 | `rightTab` | store（兼容字段） | 随 TabSet | **只有 `applyTabs()` 写穿** | e2e、`SessionView` | 恒等于 `kindOf(active)`；PR-11 删除 |
| S9 | `surfaces` 注册表 | 模块单例 | ❌ 内存 | `registerSurface/unregisterSurface` | `RightPanel`、空态面板、快照 | 只装"活着的"；重复 kind 抛错；插件无 `write` |
| S10 | `surfaces/index.json` | 磁盘 | ✅ | **只有安装器** | 启动加载、插件页 | 变更前 `.bak` 快照；失败不写 index |
| S11 | `enabled` 位 | index 内 | ✅ | 安装器 / 插件页 / 3 次崩溃自动禁用 | 启动 `syncPlugins` | 禁用 ≠ 删除；禁用项不进注册表 |
| S12 | `surface.disabled`（3 次崩溃） | localStorage | ✅ | `recordMountFailure` | loader | 与 index 的 `disabledReason` 双向同步（以 index 为准，启动时回写） |
| S13 | `uiCache.snapshot` | bridge 内存 | ❌ | `ui_snapshot` | `ui_inspect` | **只含当前会话**；`ageMs` 单调；>5s 标 stale |
| S14 | `uiCache.status[kind]` | bridge 内存 | ❌ | `surface_status` | `ui_inspect` | `surfaces_changed` 时清掉不在 index 的项 |
| S15 | `loaded`（插件模块表） | renderer 内存 | ❌ | `applyManifest/unloadPlugin` | 卸载、热更 | 卸载必须连带把该 kind 的标签标 `unavailable` |
| S16 | `audit.jsonl` | 磁盘追加 | ✅ | `ctx.host.command` 代理 | 插件页 | 只追加，不轮转（V0.1）；单行 JSON |
| S17 | `previewRenderers` 注册表 | 模块单例 | ❌ | `registerRenderer` | `file` 预览 | `match` 优先级确定；同 id 重复注册抛错 |
| S18 | 转换缓存 + 输出目录 | bridge 内存/磁盘 | 输出 ✅ | **只有转换服务** | 预览、`office_*` 工具 | key = 源 SHA-256 + 操作 + 参数 + 转换器版本；失败不缓存 |
| S19 | 转码任务队列 | bridge 内存 | ❌ | `media_transcode` | 媒体面板 | 有并发上限、可取消；产物 24h + LRU |
| S20 | office-kit 安装状态（版本/平台/sha256/可用性） | 磁盘 `KIT.json` + 进程内缓存 | ✅ | **只有 kit 管理器** | 转换服务、设置页 | 探测结果按进程缓存；卸载即失效；sha256 不匹配视为未安装 |

**四条总原则**

1. **一个状态只有一个写者**；派生值要么现算（S5），要么单点写穿（S8），不允许两处都写。
2. **派生状态不持久化**（S4/S5/S6/S7/S9/S13-S15）。
3. **"活着的"与"装过的"分离**：S9 vs S10/S11。
4. **跨会话只给计数**（S13），不越 ADR-0。

---

## 二、发现的歧义与裁决

### 阻断级（不裁决就无法开工）

**F01｜`rightTab` 到底是 selector 派生还是写穿？**
- 设计稿 §3.3 写"改为 selector 派生，store 里不再写入"；PR-02 写"selector 派生"；代码方案 §2.4 写"`applyTabs` 单点写穿"。三种说法。
- **裁决（D-D）**：**写穿**。store 保留 `rightTab` 字段，只有 `applyTabs()` 写它。理由：50+ 读取点里混着 `getState()` 快照读与组件订阅读，selector 现算会在同一帧里给出不同值；e2e 的 `setState({rightTab})` 复位也全部失效。
- 回写：设计 §3.3、PR-02、代码 §2.4 三处统一。

**F02｜dirty 是组件 state 还是标签 state？**
- 设计 §4.5 只说"dirty 标记随标签走"；代码骨架里每个 Editor 各自 `window.desktop.setDirty(dirty)`。
- **后果**：两个编辑器，一个保存后 `setDirty(false)`，另一个未保存的改动被一起"洗白"，退出时不拦截 → **丢用户数据**。
- **裁决**：`dirty` 挂 `tab.dirty`；`editorDirtyCount` 是 selector；`window.desktop.setDirty(count>0)` **只在 App 根部一处调用**。

**F03｜`editor_open_set` 是增量还是全量？与 `file_open` 的 track 什么关系？**
- 代码方案原文写"二者取并集"。
- **后果**：关掉的编辑器永远留在 bridge 的跟踪表里（LRU 64 也救不了），`file_changed` 会推给不存在的面板；更糟的是"取并集"意味着集合只增不减。
- **裁决**：`editor_open_set` 上报**全量集合**，是唯一真值；`file_open` **不再 track**。

### 高

**F04｜`fileChanges` 没有生命周期。**
- 裁决：保存/静默 reload/关标签三处 `clearFileChange(path)`；上限 32 条 LRU。否则同一文件第二次被外部改动时，`hash === base.hash` 的旧记录会吞掉新事件。

**F05｜`ui_snapshot` 包含**所有会话**的标签。**
- 后果：agent 用 `ui_inspect` 能读到"另一个会话打开了 `财务.xlsx`"，等于绕过 ADR-0。
- **裁决（D-H）**：只给当前会话；其它会话只给 `{otherScopes: n}`；`ui_inspect` 回执带 `scope` 字段。

**F06｜锁的机制写错了。**
- 代码方案原文说"提到 `internal/filelock`（`sync.Map[path]*sync.Mutex`）"，但 `tools/builtin/lock.go` 实际是**文件 fd 上的 flock**（跨进程 POSIX 建议锁）。
- **后果**：按文档实现会得到两把互不相识的锁，`file_save` 的「读 hash → rename」窗口被 agent 的工具写入穿透。
- **裁决（D-G）**：提取为 `internal/atomicfile`（`Lock/Replace/Append`，保留 flock 语义），`builtin` 留薄壳转发；新增 **PR-05a0**。

**F07｜禁用插件的"可见性"三处不一致。**
- 设计说"禁用的插件仍可见（墓碑）"，代码说 `listSurfaces()` 过滤 disabled，插件页又从 index 读。
- **裁决**：注册表只装"活着的"（禁用=不 import、不注册）；墓碑来自"标签还在但 provider 没了"；插件页列 index 全部（含禁用）；空态能力面板**不列**禁用项。

**F08｜`rightTab` 何时删除含糊。**
- **裁决**：PR-02 起写穿保留；PR-11 删字段。中间任何 PR 不得新增对它的读依赖。

### 中

**F09｜PR 数量自相矛盾。**
- 原文"18 个（M0=14，M1=4）"，但表格实际 17 项 + 缓冲，且未含 05a0。
- **裁决**：当时 19 个 / 22.5 人日；**后续并入文档/媒体（PR-08/08a~08e）后为 24 个 / 34.5 人日**（以 `02-pr-plan.md` §1 为准）。

**F10｜工具名与仓库实际不符。**
- 文档写 `ask_user_question` / `exit_plan_mode`；仓库实际是 **`ask_user`**、**`plan_submit`**（`tools/question/question.go:44`、`tools/builtin/plan.go:24`）。
- 裁决：以仓库为准；后续 D-N 取消模式，工具名清单改由标准工具集注册处维护。

**F11｜`office` provider 与已拍板的 D6 冲突。**
- `RIGHT_PANEL_TABS_PLAN.md` D6：视觉类文件落**浏览器标签**（标题显示源文件名）。新方案让 pptx/docx/xlsx 落 `office` provider。
- **裁决**：M0 起 D6 **对 office 家族失效**（pptx/docx/xlsx → `office`），其它视觉类（pdf/图片/3D）仍走 web；已持久化的 web 标签按原样恢复，不迁移。
- 回写：设计 §4.3 补一句，避免下一个人按 D6 把 PPT 塞回 web。

**F12｜空态"能力面板"与"插件页"职责重叠。**
- 裁决：空态只列**已启用的 kind**（含内置）；插件页只管安装/禁用/卸载/审计。两者读不同数据源（S9 vs S10）。

**F13｜`leftPinned` 与 `ratio` 的基准。**
- 三栏并存时 ratio 以谁为分母没写。
- 裁决：`ratio` 恒以「**除左栏外的可用宽度**」为分母，与左栏是否展开无关；代码里 `panelSizes` 已按此实现。

**F14｜窗口 resize 时 ratio 与像素谁优先。**
- 裁决：只存 `ratio`；resize 按占比重算。用户拖拽即改写 ratio（bias 变 `custom`）。这意味着"像素级手工微调"在窗口变化后不保留 —— 这是有意的（比例优先），写进文档避免被当 bug。

**F18｜"自动展开"被写进了 `applyTabs()`。**
- 代码方案原稿在 `applyTabs` 里做 `rightLayout = expand()`，而 `applyTabs` 是**所有**标签变更的唯一入口——关闭标签、切会话恢复、同步 web 标签都会触发，结果是"关掉一个标签，右栏自己又弹开"。
- **裁决（D-A 精确化）**：自动展开只属于 `openTab` 的「打开/激活」路径；`applyTabs` 不碰布局。

**F19｜`office` 应不应该是一种 kind？**
- 设计稿原写 `office` provider；调研 DSH 后发现它是"一个预览子系统 + 渲染器"，不是面板。
- **裁决（D-I）**：取消 `office` kind，改 `file{mode:'preview'}` 内部的 `PreviewRenderer` 注册表；**不新增 kind**（保持 4 个）；已持久化的 office 标签迁移为 file。
- 理由：页码/sheet/播放进度都是**渲染器内部状态**，做成 kind 会让三套状态各自演化。

**F20｜音视频的字节通道必须是 Range，且是一个新的文件读取面。**
- DSH 没有媒体实现（`attachment` README 明写"音频和视频暂无专门处理"），无法照抄。
- **裁决（D-L）**：Electron 自定义协议 `hai-media://`，支持 `206 Partial Content`；**路径必须规范化并校验在工作区内**，否则这个协议就是任意文件读取后门。

**F21｜转换必须有准入与限额。**
- 现有三个 previewer 各自起 Python、各自清临时目录；没有并发上限，100 MiB 的 pptx 能把 bridge 拖死。
- **裁决（D-M）**：统一 `conversion.Service`：并发 2、60s 超时、50/100 MiB 上下限、内容寻址缓存、输出 24h + 512 MiB LRU、**失败不缓存**。

**F22｜"降级"必须显式。**
- 没有 LibreOffice 时，pptx 只有近似重建；如果 UI 不说明，用户会认为"预览坏了"。
- **裁决（D-J / F25）**：docx/pptx 渲染器顶部固定能力提示；转换失败给"用系统程序打开"的真实出口，不转圈到死。

**F23｜`soffice` 并发会撞 profile 锁。**
- 所有封装 LibreOffice 的项目都会踩：共用默认 UserInstallation 时，第二个转换报 "user installation is locked"，或者静默串行到超时。
- **裁决**：每个 job 独立 `-env:UserInstallation=file://<tmp>`；`Setpgid` + 超时杀**进程组**（`soffice.bin` 是 fork 出来的）；安装完成后跑一次空转换做预热。

**F24｜kit 分发不是"顺手打包"。**
- 体积 700 MB、平台 4 种、MPL/LGPL 许可、sha256/版本 pin 都必须在设计里写死。
- **裁决**：不内置；CDN 下载 + `KIT.json`（版本/平台/sha256/许可）；Linux 优先复用系统 `soffice`；缺失时明确 `unavailable`，不静默。

**F25｜近似渲染器的定位要明确。**
- 有了 kit 之后，现有 python-pptx 近似还有没有价值？如果不写清楚，会出现"两条路径各修一半"。
- **裁决**：保留为 **fallback**（零安装体验的兜底 + 主题/占位符改进的落点）；kit 可用时默认走 PDF；UI 用横幅区分，**不允许**在没有横幅的情况下走近似。

**F26｜"创造"要不要做成一个模式？**
- DSH 用 preset（切模式才挂工具）；OpenCode v2 没有模式，靠"插件 API + skill"。
- **裁决（D-N）**：取消 `CreatorMode`，4 个工具常驻 + skill 路由 + 安装审批。理由：模式要求用户知道它的存在，而"顺手扩展"是更常见的意图；内核（内省 + 受管安装 + 知识包）与模式无关。
- 代价：工具 schema 常驻（600–900 token/请求）→ 用 F27 的瘦身纪律约束。

**F27｜常驻工具的 schema 成本必须写进纪律。**
- 没有约束的话，"再加一个工具"会变成默认动作，上下文悄悄涨。
- **裁决**：≤4 个工具、每个描述 ≤120 字、参数 ≤3 字段；教程只放 skill 正文；V1 可用 `load_skill onLoad` 做"技能触发解锁"。

**F28｜新工具没写进 CodeMode 契约。**
- 仓库已有明确的脚本调用契约（`docs/CODEMODE_CALL_CONTRACT_2026-10-28.md`）：脚本路径要**对象**不要字符串、失败要 reject、嵌套调用逐次审批、写类保序。
- **裁决**：4 个 surface 工具 + 2 个 office 工具全部实现 `OutputSchemaProvider` + `StructuredSink`（仅 `IsScriptCall`）；失败返回 error；`ui_inspect/list` 可并行，`install/remove` 不可；`install` 幂等（同 id 同 hash = noop）。
- 补一条 UI 语义：脚本等待审批时界面要显示"脚本正在等待批准"，否则看起来像卡死。

### 低

**F15｜`closed` 栈里重开一个已卸载插件的标签。** → 重开为墓碑，不进渲染路径。
**F16｜`uiCache.status` 只增不减。** → `surfaces_changed` 时按 index 清理。
**F17｜术语混用**：右栏/右侧栏/right panel、能力/Surface/面板。→ 统一为：**右栏**（区域）、**Surface**（一种能力）、**面板**（Surface 的渲染实例）、**能力面板**（空态选择器）、**插件页**（管理页）。

---

## 三、已知取舍（不是缺陷，不再反复讨论）

| 取舍 | 代价 | 为什么不改 |
|---|---|---|
| 插件同源可信、可调 `window.desktop` | 无法真正隔离 | Electron 单渲染进程；iframe 会丢主题/locale/作用域（DSH 同样拒绝） |
| 编辑器不做崩溃恢复 | 断电/崩溃丢未保存内容 | 落盘草稿要引入第二套真相，V0.1 不值 |
| `rightTab` 兼容字段保留一个版本 | 一份冗余状态 | 一次性改 50+ 读取点风险高于收益 |
| 不做行级自动合并 | 冲突时用户自己选 | 合并算法是编辑器厂商领域 |
| 标签按 session 分域 | 用户可能觉得"切会话标签没了" | ADR-0；`workspace` 作用域留给看板类 |

---

## 四、本次回写清单

| 文档 | 改动 |
|---|---|
| 设计稿 | §3.3 写穿口径；§4.5 dirty 归属 + 打开集合 + 生命周期表；§5.3 快照限当前会话；§5.5 禁用/卸载三处语义；新增 §11 决策登记（D-A…D-H）+ 文档索引 |
| PR 计划 | 总数 24（08e 可选）；新增 PR-05a0（`internal/atomicfile`）与 PR-08/08a~08e（文档/媒体）；PR-02 改"写穿"；合计 34.5 人日 |
| 代码方案 | §2.7 锁改 flock + `atomicfile` API + 保存时序；Editor 改 `setTabDirty`/`setEditorOpen`；补三处状态归属表；§3.2/3.3 快照限当前会话 + scope 字段；§3.6 `syncPlugins` 与卸载关标签；§3.7 工具名改 `ask_user`/`plan_submit`；`applyTabs` 不再负责自动展开 |
| 本文 | 新增：状态登记表 + 28 条裁决 |
| `05-preview-media-research.md` | 新增：DSH 文档/Office 实现拆解 + 差距矩阵 + D-I…D-M + PR-08a~08e；补充 §8 LibreOffice kit 落地设计 |

> 后续任何设计变更，先改**本文第二节 + 设计稿 §11 决策登记**，再改实现文档。否则三份文档会再次分叉。

---

## 五、实施期裁决（F29…）

> 开工后新增的裁决；编号接着 §二 的 F01-F28。

**F29｜R01 的拖拽帧预算（P04/P05）推迟到 PR-15 测量。**

- 问题：PR-01 的验收写着「拖拽 60fps」，但本机无人值守环境无法驱动 Electron 的真实指针拖拽（`react-resizable-panels` 的 `isUserInteraction` 只在真实指针/键盘事件里为真）。
- 裁决：**推迟到 PR-15 的 e2e 收口**统一测（那里已有 playwright 起应用的完整链路），R01 不因此阻塞。
- 替代证据（本轮已验）：`panelSizes`/`afterDrag` 是 O(1) 纯函数（11 例单测）；拖拽路径每次只写一次 store（`setRightLayout` 唯一写者）；面板库自身按 rAF 合批，React 侧只重渲 App 壳与标题栏。
- 风险：若 PR-15 实测掉帧，回滚开关是 `surfaces.layout=off`（走旧 `rightExpanded` 行为），成本约 0.5 人日。
- 同类：P05（折叠不重排整树）同批测量。

**F30｜R01 不设回滚开关，回滚 = revert 单 commit。**

- 问题：00 §1.10 要求「可能伤到现有用户」的 PR 留一个开关；R01 删掉了「右栏拖过 50% 自动收左栏」这条用户可见规则。
- 裁决：**不留开关**，回滚方式 = revert 本次 commit。理由：①被删的规则本身是「左栏莫名消失」缺陷的来源，留开关等于留缺陷路径；②本 PR 无持久化 schema 变更（`rightLayout` 缺省时按 `rightExpanded` 归一化，旧数据可读）；③开关会分叉两条布局路径，测试成本翻倍。
- 代价：回滚粒度是整轮（1.5 人日），不是运行时开关。允许，因为 R01 的失败模式是「立刻可见」而不是「悄悄错」。

**F31｜R01 的真渲染行为断言（`e2e/verify_layout_states.mjs`）推迟到 PR-15。**

- 问题：02 原本要求 R01 交付一个 6 行验收的 e2e，但本机无人值守环境起不了「真实指针拖拽 + 三档窗口」的完整应用；且 `desktop/app/e2e/` 在 `.gitignore:64` 里，主 worktree 里没有可复现的 e2e 文件。
- 裁决：R01 的行为证据由 `scripts/test-right-panel-tabs.mjs`（220 项，含切会话恢复、预览不收左栏、rightSize 语义）+ `npm run test:layout`（7 例）承担；**真渲染 e2e 与 P04/P05 帧率一起在 PR-15 补齐**。
- 与 F29 的关系：F29 只推迟了帧率，F31 把行为断言也纳入同一批收口。

**F32｜编辑器走「零依赖受控编辑器」，CodeMirror 只作 V1 可选增强（OP-1 结案）。**

- 问题：D-F 写了「选 CodeMirror 6」，但 `desktop/app/package.json` 里没有它，而「不加新 npm 依赖」是硬约束（00 §1.1 MUST）。这两条不可能同时成立。
- 裁决：**V0.1 不引入 CodeMirror**。编辑器 = 受控 `<textarea>` + 独立行号槽 + 只读态用现有 shiki 高亮（只读时不进编辑路径，避免两套渲染打架）。
- 够用的理由：V0.1 的编辑需求是「改几行 + dirty 提示 + 冲突条三选一」，不是 IDE；`textarea` 天然支持输入法、撤销栈、系统级查找、辅助功能，反而比自绘编辑器少踩坑。
- 代价（明确接受）：没有语法高亮编辑、没有括号匹配、没有多光标；大文件（>1MB）在 PR-08 的预览器里走只读分页，不进编辑器。
- 升级路径：V1 若确有需求，按 00 §1.1 四步走例外（04 裁决 + 新增 D-* + 独立 commit + 可整体摘除），届时可只引 `@codemirror/state`+`view`+`commands` 三包而非全家桶。
- 影响：`02` PR-05b 的标题与验收改成「受控编辑器」；`00` §7 OP-1 标为已结案。

**F33｜错误信封沿用 `{ok:false, error:"…", code:"…"}`，不改成嵌套 `error:{code,message}`。**

- 03 §2.10 的样例是嵌套形态，但 bridge 既有 20+ 命令（`file_preview` 等）都走 `respCode` 的扁平形态，前端也读 `r.code`。
- 裁决：**保持扁平**，只把 code 值标准化（`conflict` / `not_found` / `too_large` / `denied`）。改嵌套要同时动前端与既有命令，单独立项。

**F34｜打开集合的 API 是 `file_open` / `file_close`；`editor_open_set` 取消；补 64 条 LRU 上限。**

- 03 写的是 `editor_open_set`，实现用了更自然的 open/close 配对。裁决：**以 open/close 为准**（打开就登记、关闭就退订），03 与 02 回写。
- 附带要求：打开集合必须有 **64 条 LRU**（编辑器崩溃/漏发 close 时不至于无限增长）。
- **淘汰口径（R05a 实现后澄清）**：表里只有 `refs>=1` 的条目（归零即移除并退订），所以淘汰条件是
  「`refs==1` 且 `used` 最小」；`refs>=2` 一律不淘汰；若候选为空则允许临时超限。
- 已知副作用（接受）：被淘汰的单引用路径失去写授权，`file_save` 回 `denied`，需前端重新 `file_open`；
  V0.1 把「64 个编辑器」当前端应该守住的上限，而不是静默丢数据（`baseHash` 冲突检测仍有效）。
- `file_preview` 不受影响（只读预览不登记）。

**F35｜外部变更用 fsnotify 目录监听，`by` 在 V0.1 恒为 `"external"`。**

- 02/03 原方案是「工具写完比对 hash → 推 `by:"agent"`」，覆盖不到 agent 用 `bash` / `run_python` 直接改文件；fsnotify 能覆盖。
- 裁决：**用 fsnotify**；V0.1 不区分 agent 与外部（编辑器统一显示「文件已被修改」横幅）。`by:"agent"` 留到有明确写入意图标记时再加（PR-06 起评估），并在 03 记「已知限制」。

**F36｜`allowNew` 是显式入参；`baseHash` 为空 + 已登记路径 = 无条件写，由调用方自律。**

- 「不许覆盖未打开的路径」与「新建/另存为」天然冲突，必须有一个显式开关。
- 裁决：`allowNew:true` 才允许创建**不存在**的文件；`baseHash:""` 且路径已登记 = 无条件覆盖（另存为语义）。前端必须在覆盖前给确认，Go 侧不猜。冲突检测只在 `baseHash` 非空时生效。
- **R05a 评审补充（接受，不改代码）**：`allowNew:true` + `baseHash:""` 打到**已存在但未打开**的文件会静默覆盖。V0.1 把它当前端责任（另存为必须先确认目标文件），不收紧为 `conflict`——否则「另存为」在正常路径上永远失败。若后续出现误覆盖反馈，再按 00 §0.3 L3 改。

### F37（R05a1 仓库卫生）—— 测试脚手架从未入库，Go 门禁一直是假绿

**发现（实现者自查推翻了我的简报假设）**：`desktop/bridge` 下被 `.git/info/exclude` 排除且从未提交的 `_test.go` **有 68 个（17006 行）**，不是 1 个。此前每轮 `go vet` / `go test` 能过，纯粹因为它们在本地磁盘上——干净检出直接编译失败（14 个未定义符号）。**这意味着 R00-R05a 的 Go 门禁全部建立在未跟踪文件上。**

**裁决**：
1. 补齐「最小可编译集」7 个文件（`bench/checkpoint/interrupt_task/mcp_projects/mcp/metrics/view_reducer`，3611 行、66 个 Test），与本轮 `main_test.go` 合成**一个** commit（`--amend`），语义单元 = 「测试脚手架转正」。不另开轮。
2. 剩余 61 个未入库测试文件（约 13395 行）**不影响编译**，但它们的测试在任何环境都不会跑——单开 R05a3 清理轮，逐文件判断「入库 / 删除 / 重写」，不阻塞主线。
3. `.git/info/exclude` 里 17 条指向已删文件的陈旧条目，随 R05a3 一并清理。
4. **门禁口径修正**：此后 Go 门禁一律在 `git worktree add` 出的干净检出里跑，不再信本地工作区。这条写进 00 §4.1。

**F37 追补（R05b 期间发现）**：前端测试资产是同一类问题的更大版本——`desktop/app/e2e/`（53 个脚本）被 `.gitignore:66` 整目录忽略、`*.test.ts`（7 个 electron 测试）被 `:62` 忽略，`git ls-files` 全为 0。干净检出下 `npm test` 连第一步都跑不了。R05a3 改名「测试资产转正」，范围 = 61 个 Go 文件 + 53 个 e2e + 7 个 electron 测试 + MCP testdata fixture（R-10），逐项跑通再入库，跑不通的明示删除，不留模糊地带。

### F38（R05b 内嵌编辑器）—— 首评 CHANGES，5 条必修

**通过项**：零 Go 改动、零新依赖、双入口、i18n 28 键双语、状态机三态、`blocked` 后不重发、`baseHash` 取自响应 hash、`saved` 保留保存期间的新 dirty、`allowQuit`+`isDestroyed` 双放行。

**必修**：
1. `send()` 的 `onData` 与 `dispatchEvent:4703` 就地 return —— **最危险的主干改动落在测试盲区**（`test:editor` 用假 send，真覆盖它的冒烟脚本是 gitignored 的）。补真 store 用例，删掉 return 必须红。
2. 回滚开关提到 `openFileForEdit` 单点 —— `fileSurface.tsx` / `FilePreview.tsx` 的入口未确认受控。
3. **CRLF 静默改行尾**（真缺陷）：textarea 把 `\r\n` 归一成 `\n`，CRLF 文件一打开即显脏，保存后整份文件变 LF。记 `eol` 并在 `savePayload` 还原。
4. 批量关闭（关其它/关右侧）的 dirty 守卫未取证。
5. `file_*` 的失败被 return 挡住，不参与 `looksBridgeDown` → bridge 半死时存盘失败不标红。

**加判**：`savePayload` 在响应缺 `hash` 时把 `baseHash` 写成 `""` → 下次保存变成 F36 的「无条件覆盖」，必须保留旧值。

**已知限制（不修，写进 03）**：`abs` 未 `EvalSymlinks`/未大小写归一，Windows 或 symlink 的两种字符串写法指向同一文件时外部变更会漏匹。

### F39（R05b 复评）—— PASS，合并 `2ad74e2`

六条必修全部闭合，且都是「断言指向可静态推理的变异点」而非凑数：B1 的 `bridgeError===undefined`、`[5b]` 的 `=== crlf` 逐字节、B5 的「一个都没关 + 队列 2 + 只剩未确认那个」、B4 的「0 个标签」。

**评审额外确认的两处正确性**：
1. `editorDocs.save` 回传 reducer 的是**编辑器空间**内容（若传写盘空间的 payload，CRLF 文件保存后立刻又脏）——最容易写反的一步。
2. `EditorConfirm` 从 `closeNow+cancel` 改为 `closeNext`：旧写法在队列下会让「cancel 清空整队」与「只关了一个」错配，剩余 dirty 标签被静默遗忘。

**转 R16 的四条加固建议（不阻塞）**：
- B3 缺反例：`permission denied` 不该把 bridge 标红，现在只钉了「该红则红」
- `looksBridgeDown` 可能撞路径名（`/ws/stdin_client.go` 的 denied 文案内嵌绝对路径 → 误判掉线）
- 队列里的失效 id（标签在确认前因切会话消失 → 「另有 1 个」指向不存在的标签）
- `eolOf` 的注释对真混行不成立（行为已由 03 的已知限制覆盖）

### F40（R05a3a）—— 「环境相关基线红」是遮羞布，6 条成本失败其实是测试隔离缺陷

**被推翻的判断**：连续三轮报告里写着「10 条环境相关基线失败，与本方案无关」，我接受了。实际：

- 这 6 条**单独跑全绿**，按文件序跑到第 80 个测试 `TestParseCheckpointFromRecordWindow` 之后才变红；
- 触发链：`checkpoint_test.go:25 newManagerWithAppData → loadProviderConfig() → providers.go:677 setUserPrice` 把本机 `~/.go-code/settings.json` 里的 `deepseek-v4-flash 0.14/0.014/0.28` 写进**进程级** `userPriceTable`，且**不恢复**；
- 数值反解精确吻合：`(40×0.14 + 900×0.014 + 100×0.28)/1e6 = 0.0000462` = 实测值，期望 `0.0006687` 用的是 0.15/0.003/0.6；
- `memory_baseline_test.go:744` 的注释早写明了这个现象，还留了现成修复件 `isolateUserPrices(t)` —— **说明前人知道这个坑，只是 `newManagerWithAppData` 没调用**。

**结论**：这 6 条不是「环境相关」，是**测试污染**；剩下 1 条（`TestP0GuidanceCausesModelToWriteMemory`，`400 Model is unavailable`）才是真的需要网络。

**规则（写进 00 §4.6）**：
> 「环境相关」「基线红」「本机才红」**不是合法的归因**。任何红必须在干净检出里定位到具体机制（隔离缺陷 / 真实网络 / 时钟 / 端口 / fixture），能修就修，不能修就加显式环境门**并在默认链里绿**。允许存在的只有「显式 skip」——跳过的测试会出现在输出里，而红的测试会被当成噪音忽略掉。

**同轮其它事实**：`.git/info/exclude` 删 83 条 / 保留 208 条（其中 191 条是本机仍在用、未跟踪的文件，删了会让主工作区多出 191 个未跟踪项）；MCP fixture 的真实路径是**仓库根** `mcp/testdata/server`，不是我简报里写的 `desktop/bridge/mcp/testdata`。

### F41（R05a3a 收尾）—— 干净检出 `go test ./desktop/bridge/` 首次全绿：646 PASS / 15 SKIP / 0 FAIL

**关键数字**：本轮前 371 个顶层 Test、9 条「基线红」；本轮后 661 个 Test、0 红。多出的 290 个测试是**三年来第一次真正被 CI 看见**。

**两个污染源，不是一个**：修掉 `newManagerWithAppData` 后仍剩 `TestReducerLLMEndDedupAndUsage` 一条红，数值仍精确等于污染价。第二处在 `skill_capability_wiring_test.go` 的 **5 个** `loadProviderConfig()` 调用点（能力面断言与价格无关，但写全局表是无条件副作用）。修法：`pcForTest(t)` 小助手统一入口，而不是逐处塞一行。

**方法局限（实现者自己写进 commit 正文）**：上一轮按文件序二分只找到「第一个」污染源，因为当时的目标测试在文件序里早于第二个源。**单目标二分能定位「为什么这条红」，找不到「还有哪里会污染」** —— 要发现全部污染源得用「全量红名单 × 每个 `loadProviderConfig` 调用点」的交叉视角。

**16 条 SKIP 的构成**（我逐条看过了）：
- 3 条显式环境门（联网 eval / 真实数据 trace / replay 验证）—— 开门后仍响亮失败，`HAI_EVAL_NETWORK=1` 下实测 `FAIL: 400 Model is unavailable`
- 8 条评测/lab 类（需 `HAI_EVAL_TOOLCHOICE` / `HAI_LAB_*`）
- 4 条基线装置（产品树已接线 → 空状态前提不成立，故意 skip，注释写明理由）

**新发现（同 R-10 一类）**：`desktop/bridge/testdata/trace_fixture.json`（3 KB）从未入库，被全局 `testdata/` 吞掉 → `TestTraceProjectionCrossImpl` **永久静默 skip**。这比红更糟：红会被看见，skip 不会。已派最后一小轮收掉。

### F42（R05a3c）—— 3 KB fixture 把一个「永久静默 skip」变成真跑

`TestTraceProjectionCrossImpl` 缺 `desktop/bridge/testdata/trace_fixture.json` 时 `t.Skipf` 兜底 → 干净检出里它永远不跑，而 skip 不会像红那样被注意。入库后：**PASS 647 / SKIP 14 / FAIL 0**（663 个顶层 Test），唯一变化是这一条 SKIP → PASS。

`.gitignore` 例外写成三条而不是一条 `!.../**`，理由是实测的：该测试每次运行会写一份 `.go.golden.json`，若用 `!**` 通配，每跑一次工作区就多一个未跟踪文件。三条收窄后，跑完全量 `git status -uall` 仍为空。

**合并时的第二个坑**：R05a3a 删掉了 `.git/info/exclude` 里的 61 条 → 主工作区立刻把这 61 个文件视为未跟踪，合并时全部拦下。处理方式：逐文件 `git hash-object` 与目标 commit 的 blob 比对（60 个相同、1 个不同是因为分支上加了 skip 门），备份到 `/tmp/mc-bak2` 后删除再合并。**这正是 exclude 清理的必然副作用，下一轮别再当成意外。**

### F43（R05c）—— 默认皮肤下用户消息整段不渲染：一次「删事件卡片时顺手删了对话本体」

**缺陷**：`narrative_skin` 未设置时默认 `'current'`，而 `Narrative.renderBlock` 里唯一的 user 分支带 `skin === 'runtime'` 门。于是**默认皮肤下用户自己发的消息完全不渲染**——转录直接从助手回复开始，一条回答对着不存在的提问。函数末尾是 `if (b.kind !== 'assistant') return null`，少一条分支就是静默消失。

**引入点**：`6d28d60 runtime A2: events that used to be cards are now one line each`，diff 里整段删掉 46 行 `.msg.user` JSX，且**没有留下非 runtime 分支**。

**判定「误删」而非「有意」的三条证据**（实现者给，我认可）：
1. 该提交主题是「**不属于对话**的九种事件」（memory saved / hook / run ended / task came back）——用户发言是对话本体；
2. 提交正文写着「Both other skins keep the cards they were verified with」，删除直接违背这句承诺；
3. 同一提交里 `task_result` 被**加闸保住**（`if (skin !== 'runtime') return <TaskResultBlock/>`）、`system` 也在新分支后**保留**老渲染 —— **只有 user 没有兜底**。

**修法**：恢复旧 `.msg.user` JSX（与 `6d28d60` 删除的 46 行逐字节一致），**不复用 `UserBlock`** —— 因为 `user-block.css` 282 行 100% 锚在 `html[data-skin='runtime']`，搬过去在 current/v3 下是裸的（没有竖轨、没有 pre-wrap 换行兜底），等于顺手改 runtime 美术。这一条判据我认同：**修 bug 时不要夹带设计迁移**。

**回归网**：`e2e/verify_user_blocks.mjs`，54 断言。红态实测 **24 红 / 24 绿**（current/v3 全红、runtime 全绿），绿态 54/54。断言里有 `elementFromPoint` 命中块内 —— 专治「渲染了但看不见」。已接进 `npm test`（69 → 70 步）。

**波及面（同一 `renderBlock`，一处修复四处受益）**：主对话 `CenterPanel`、子会话 `AgentSessionView`、子 agent 卡片 `TaskCard`、cron 运行详情 `CronRunDetailPage`。后三者数据来自 `lib/agentSession.ts:70`，那里的注释本来就写着「映射成主会话的 user 块 —— 这样**我对它说的**和主会话里一样」。

**只列不改的既有差异**：
1. current/v3 的用户块走 `InlineCode` 不解析 Markdown（换行有 `pre-wrap`，是好的）；runtime 走 Markdown。
2. `b.injected`（运行中插话）在 current/v3 没有标记 —— `6d28d60` 之前就没有。
3. `.user-imgs` 的 CSS 注释写「点击原图新窗口打开」，但 `6d28d60` 之前的 JSX 里就没有 `onClick` —— 注释与实现不符。

**新记一条探针陷阱（R-13）**：探针若只 `import 'index.css'`，在 **vite dev** 下末尾那三行 `@import`（narrative-skin / runtime-palette / runtime-skin）**不下发**（`vite build` 产物里有），皮肤只生效一半 —— `.rt-head` 的 `display` 会是 `block` 而非 `flex`。写新探针必须显式 import 那三份。

### F44（R06a）—— 路径归一化有两份同构实现，只差一次 `normalizeFilePath`

`samePreviewTarget` 原本调 store 私有 `relIfInside`，而 `lib/filePaths.ts` 里的 `toRelIfInside` 是 PR-04 从 RightPanel 抽出的同口径版本（store 那份的注释自己就写着「与 RightPanel.toRelIfInside 同口径」）。R06a 搬函数时必须二选一：

- 原样搬 → 再造**第三份**；
- 改调 `toRelIfInside` → 口径从「手抄一份」变成「用同一份」，代价是它多做一次 `normalizeFilePath(ws)`（多剥 ws 开头的 `./`）。

**裁决**：选后者，并用四条测试钉住「工作区内绝对/相对互认、工作区外不归一化」。这是 R06a 唯一偏离「逐字不变」的地方（一个标识符）。

**遗留**：store 里那份 `relIfInside` 仍在服务 `fileKeyOf` / `autoOpenVisualFile` 等 4 处，**双份口径没有消除**。让 store 也改调 `toRelIfInside` 是一次独立的口径去重，属 R4/R1 地盘，不在 R06 里做。**这是技术债，不是本轮缺陷**。

### F45（R06b）—— 截图「全一致」差点成为伪证：探针缺了副作用 import

R06b 的验收是「注册 provider 后界面零变化」，唯一手段是截图逐字节比对。实现者先证明了确定性（同码两跑 9/9 IDENTICAL，无需容差），然后发现了一件更要紧的事：

> **探针页缺 `import 'src/surfaces/builtin'`** → `getSurface()` 全部 `undefined` → 9 张截图证明的一直是 `??` 兜底分支，而不是 provider。

如果没发现这条，R06b 的「零变化」是**伪证**；更糟的是 R06c 换掉 render 时，同一个坏仪器会继续静默通过。

修法：探针补副作用 import，再用**老代码**重跑 9 张 → 零变化（证明修仪器本身不改变像素），最后才做 A/B。判据是 web 标签图标：兜底路径给 `kindIcon('web')`=Globe，实测渲染出的是 provider 的 FileIcon → 委派确实被执行。

**写进规范（00 §4.4 追补）**：任何「用探针证明没变」的验收，必须先证明**探针自己走的是被测那条路**（打点/断言 provider 参与、或故意改坏 provider 看像素是否变）。否则「一致」只说明两条没被测的路都没变。

### F46（R06c）—— 最高风险一步没踩雷，但踩出了更贵的东西：一个红了一整轮的 e2e

**渲染委派本身**：`BrowserTab` 404 行原块平移，逐行 diff 只剩 3 处（注释改名、签名、12 行 prop 来源），根 DOM 逐字不动、**没加 key**（加 key = 每次切 web 标签销毁重建 `WebContentsView`）。`kind === 'web'` 13 → 12，RP 1103 → 730 行。

**它自己披露的两件事比代码更值钱**：

1. **`e2e:session-tabs` 在 main 上已经红了一整轮**。原因：R06b 把 `title` 委派给 provider 后，这个探针缺 `import 'src/surfaces/builtin'` → `getSurface()` 全 undefined → 标签名退回兜底「网页」。它不在 `npm test` 链里，**没人发现**。R06b 当时用 9 张截图证明「零变化」是对的，但那 9 张照的是 `??` 兜底路径 —— 两个缺陷叠在一起，正好互相掩护。
2. **本机 5199 端口上挂着 5 天前的 vite（另一棵 worktree）和一个 `http.server`**。`waitPort` 只证明「有人在听」，探针连上的是**别人的模块图**。基线跑出 `waitForFunction 超时`，看起来像代码坏了。

**仪器修正三条**（都单独 commit，可只回退仪器）：
- 两个探针补 builtin import，并给 `provider.render` 包计数落成**自检断言**（`renderCalls > 0`）—— 以后再坏会当场变红；
- 端口归属自检（取回探针页自己的 `<title>` 比对）；
- `e2e:container` T16 采样竞态：`waitFor` 只等「某个 target visible」，而 activate 建视图**先以默认矩形可见、url 还是空串**，随后才导航 → 抓到那一帧就是假红。判据改成「可见且 url 含本文件名」，几何断言一字未改。

**内存基线（R-5 的硬要求）**：12 个活渲染进程（`targets=21 / live=12 / sleeping=9`）下 P09=151.4MB（限 150）、P10=1207.7MB（限 800）。**A/B 证明不是 R06c 的增量**：退回 `35149f9` 同负载 4 次，P09 144.5–162.3、P10 1159–1214；背靠背两次相差 4MB，差异随机器状态走。
**结论：预算口径本身不成立** —— 12 × ~76MB ≈ 912MB，光这一项就吃光 800MB，**任何实现都过不了**。已改口径（见 03 §2.3 与看板 P09/P10）：改为「每视图预算 + 主窗口」，并要求在**生产构建**上采基线（dev server + HMR 把主渲染进程抬到 150–216MB）。

**既有缺陷（不属本轮）**：`e2e:pdf` T3 视图比容器右移 3px（x 差 3–4，y/宽/高全等），A/B 同红，怀疑窗口宽度在 `embedShow` 与测量之间变了 3px。

### F47（R06d）—— 把门禁修诚实：链路补上三个探针，T3 的 3px 是采样竞态不是产品缺陷

F46 留下的两笔账，这一轮结清（两个独立 commit，可各自 revert）。

**① 三个纯 Chrome 探针进链（`e730b78`）**。`e2e:session-tabs` 红了一轮没人知道，根因就一句：它不在 `npm test` 里。现在 `e2e:session-tabs` / `e2e:auto` / `e2e:artifacts` 挂进链（**71 → 74 步**，实测 +25s：12s/3s/10s）。
真 Electron 的三个（`e2e:pdf` / `container` / `scoping`：`go build` + 真窗口，40–90s）**不进默认链**，另立 `npm run test:electron` 每轮手跑 —— 进链会让「跑一次全量」从 12 分钟变 16 分钟，退出代价远大于收益。

**② 就绪判据从「有人在听」升级为「这张页面是我的」**。F46 的 5199 事故里，`waitPort` 只能证明端口有人监听；`_serve.mjs` 新增 `waitProbePage`：取回探针页、比对自己的 `<title>` 标记，三种失败都抛 `ProbePrecondition` 并**退出码 2**（= 前置不满足，不是测试失败），断言失败仍是 1，两者都不许静默 0 或 skip（00 §4.6）。实测两条失败分支：
- 端口被占（IPv4+IPv6 都占）→ `⛔ 前置不满足（退出码 2）：探针服务器已退出（code 1）——端口被别的进程占着`，`exit=2`；
- 端口上跑着别人、且对任意路径都回 200 → 判据要求页面里带本脚本标记，缺标记即判「别人的服务器」。
三个探针原先各抄一份 `waitPort`（三份副本，其中一份还抛普通 Error 落成退出码 1，与真失败混在一起）；现在统一走一处。

**③ 又一例 F45（缺副作用 import 的仪器）**。`grep -L "surfaces/builtin" e2e/*.mjs` 查出**两个在链里**的探针也在裸奔：`e2e:artifacts`（渲染真 RightPanel 做预览）与 `e2e:agent-tree`（渲染三栏）。缺 `import 'src/surfaces/builtin'` 时 `getSurface('file')` 恒 undefined，面板静默走 `??` 兜底 —— 它们一直在验兜底组件。补上 import 后：
- `e2e:artifacts` 87 → **88 项**：新增一条仪器自检「file/git/web 三个 provider 已注册」，坏仪器当场变红；
- `e2e:agent-tree` 105 条断言**一字未改仍全绿** —— 说明补仪器本身不改行为，这条才能当证据。

**④ T3 的 3px = 采样竞态（`a8acaaf`）**。F46 记为「既有缺陷、怀疑窗口宽度变了」。本轮复现 + main 侧插桩证死，**不是**窗口宽度、不是 DPR：
- 宽度 877 与窗口 1440 全程相等、`zoom=1`、`toDip` 直通 → 差的是**纯平移**；
- `.right-body` 的入场动画 `slideRight`（translateX 18px → 0）把 `.browser-shot` 从 x+18 移到 x，而原生视图只在 ResizeObserver（只报尺寸）与 `animationend` 补的那一枪拿到新矩形；
- E2E 窗口在后台，Chromium 把 180ms 的动画**节流到 ~3.4s**；红的跑次采样在「最后一次动画中途同步」（x=555）之后 2ms、纠正（x=552）之前；
- 同一份代码反复连跑（十几轮，带打点）：绿红交替落在 50% 附近，红的几次停在动画残值 3/4/5/6px 不等 —— 与「固定 3px」的现象不符，这一点本身就是判据。

产品是收敛的（`animationend → applyRect → embed:resize` 已把它补正），所以**没动产品代码**。处置与 R06c 修 T16 同款：**判据一字不改（±2px），先等被测对象落定再量**，并把「等了多少 ms、等在哪些动画上」打进输出。判据没被稀释的证明：把 `embedRectOf` 的 x 硬加 5px，T3 仍红（557 vs 552）；还原后 5 连跑全绿。

**⑤ 跑 `test:electron` 顺带暴露：默认 PDF 夹具不在版本库里**。`desktop/百工2026年上半年经营分析.pdf` 从来没收进 git（`git ls-files` 无 pdf）—— 干净检出下这条新命令第一步就停。它原来抛普通 Error 退 1，读起来像「PDF 预览坏了」；现按 00 §1.9 的退出码三档改判**前置不满足 → 退 2** 并打印下一步。资产入库另记一条（看板 R-17）。

**仍未进链的探针（下一步的债，不是本轮缺陷）**：见看板 R-15 的清单 —— 链外还有 30+ 个 `verify_*.mjs`，其中一半以上是 Chrome 探针（轻量），另有 8 个真 Electron 探针连 `test:electron` 也没覆盖。

### F48（R07）—— 「像素一致」在 provider 委派里证明不了任何东西

ActivityTab 229 行平移（`RightPanel` 730 → 480），provider 注册，`renderCalls` 仪器断言。**逐字证明**是脚本比对：`c66922e` 的 `RightPanel.tsx:45-293` 与新文件 249 行差异 0。

**三个变异，是本轮最有价值的产出**：

| 变异 | 期望 | 实测 |
|---|---|---|
| 删掉 `builtin/index.ts` 的 `import './activitySurface'` | 仪器红 | `registered:false, calls:0` 红；但 **DOM/像素完全不变**（兜底渲染的是同一个组件）—— F45 的原样复现 |
| `render` 返回 `<div data-r07-mutant/>` | 内容跟着变 | `rail:0, wf:0` → 面板消失的断言红 |
| `render: () => null` **且保留兜底** | 应当全绿 | **109 条全绿** —— 反证「像素一致」这类验收在委派场景下什么都证明不了 |

**实现者顶回了我一条硬约束，它是对的**：我要求 `kind === 'activity'` 计数归 0，但按字面写 `activeTab?.kind === 'activity' && (render ?? fallback)` 会踩 `null ?? <ActivityTab/>` 的坑 —— provider 对别的 kind 返回 null → **git/web 标签会连带渲染出整块链路面板**。计数归 0 的正确形态是「三个非 file 面板收口成一条委派」，单独一轮做（已派 R07b）。

**顺手挖出的两笔旧账**：
1. `verify_md_file_modes` 的 [9] 在补 import 之前就红：`seedActivity` 清空 file 标签后靠 `syncTabsToRightTab()` 建空标签，而 PR-02 起该函数**只在「该 scope 还没有标签集」时**建默认集 —— 标签集还在、只是空了，于是永远建不出来，[9]~[11] 自那时起从未执行。改成显式 `openTab` 后 63 项全过。
2. `e2e:trace` 在 `c66922e` 基线就红（⑧ `实时链路含 edit_file 行`），且它**不在链里** —— 又一个「红了几轮没人知道」。已派 R07b 查根因（按 §4.6 不许以「既有红」结案）。

### F49（R07c/R07d）—— 收口成一条委派；`e2e:trace` 的红是选择器 drift，不是产品缺陷

**R07c**：`{activeTab && kind !== 'file' && (getSurface(kind)?.render(...) ?? panelFallback(...))}`。计数：`kind === 'activity'` 1→0、`git` 1→0、`web` 12→11、`file` 5→5（file 的 `Fragment key={activeTab.id}` 一字未动，PR-11 再收）。
变异：强制走 fallback → `{"calls":0,"rail":1,"wf":1}` → 仪器断言红而**轨迹面板照旧**（兜底渲染同一组件）—— 又一次证明「像素不变」不能当委派证据。

**实现者的一处自查值得记**：它第一版注释里写了 `` `grep -c "kind === '"` ``，结果**注释自己把计数抬成 17**；改成不带引号的写法后 amend。这种「测量工具污染被测对象」的细节，正是这一轮反复踩的东西。

**R07d（`e2e:trace` ⑧）**：根因 = 探针还在用 `[role=dialog][aria-modal=true]` 去答 `ask_user`，而 `AskUserQuestionPanel` 早已搬进 `CenterPanel` 的 `.dock`（`role="region"`，`.q-item` / `.q-option`）→ 选择器恒 null → 没人答题 → mock 卡在 ask_user → `edit_file` 永不执行 → ⑧ 干等 18s。三条旁证：`grep -rln "role=dialog" e2e/*.mjs` 只剩它一个；组件源码无 `aria-modal`；改按 `.dock` 答题后抽屉断言**第一次真的执行**。
修后 52 项全过，且「先红」是用历史缺陷（`(unified as unknown as string[]).map(...)`）复现的 —— `页面异常: s.map is not a function`。**修的是失配，没把断言改软。**

顺手堵的两个坑：答题 helper 收敛成一份；端口改 `freePort()`（固定 4183 会撞没退干净的 preview）；`e2e:trace` 自带 `vite build`（它验 `dist/`）。

**链路现状**：**75 步 / 1206 断言 / 0 失败**；`test:electron` 185 断言。`RightPanel.tsx` 2303 → **499** 行。

### F50（R08a）—— 注册表落地，以及「不在链里的探针才抓得到真回归」

**交付**：`surfaces/preview/{types,registry,loader,PreviewHost,PreviewPanel}` + `renderers/{text,image,csv}`，`file{mode:'preview'}` 分发，`data-renderer` 可观测，注册表 12 例 + 渲染器接线 5 例单测，探针 31 项入链（75 → 78 步）。

**三个变异**（都先红后绿）：
1. 清空注册表 → `ids=["unavailable"]`，且**旧 source 标签同样失效**（证明旧分支确实搬空，不是硬编码兜底）；
2. 删 `PreviewHost` 的 `import './renderers'` → 仪器红 `{"file":true,"renderers":[]}`（F45 的标准动作）；
3. csv 的 `weight 60→5` → 表格被 text 抢走，`waitForSelector('data-renderer="csv"')` 超时。
正面断言同时盯**容器与内容**（30 个 `.fp-ln` 且内容确是 `package auth`；图片 `naturalWidth×Height=120×40`；csv 6 行逐格对上）。

**接口偏离（我认）**：03 草案写 `weight: 'light'|'heavy'`，实现改成 `weight: number`（`match` 只判接不接，数字不参与排序）。理由：「同 weight 按注册顺序」要求可比值，而 `heavy` 那层语义（转换服务准入队列）等 R08b 有真队列时再独立成字段。现在造一个恒为 `'light'` 的枚举没有可观测差别。

**`office` 持久化数据 = 0，不需要迁移**（证据而非推断）：代码史上 `RightTab` 从未有 `office`；实机解码 381 个 Electron profile / 415 个 leveldb 文件，两个在用档案的 kind 集合是 `{activity,file,web,git,agent}` —— **反而挖到真历史 kind `agent`（1 条标签）**，它已在 `ea8f8ba` 退役、`sanitizePersistedTab` 整条丢弃。也就是「退役 kind 丢弃而非迁移」这条路已被真实数据走过一次。

**本轮唯一真回归**：把两条分支搬进注册表后，`e2e:md-file-modes` 的「切回来仍在原视图」红（scroll 140 → 0）。根因：恢复滚动的 effect 在渲染器**挂载之前**跑完（`querySelector` 为 null），之后依赖不再变化。修法：文件面板预热模块 + `PreviewHost` 初始 state 吃 loader 同步缓存 + 恢复/定位改有界重试 1.5s。
**而它不在链里、`e2e:csv` 也不在** —— 又一次「链外的探针才抓得到真回归」。已派 R08b 的 Commit C 收掉。

**已知临时态（记账）**：`.md` 在 preview 模式走 `text`（源码视图），`markdown` 渲染器未做；`image` 在 FilePreview 的共享分支没拆（pdf/xlsx/docx 都在里面，文案受 `e2e:auto` 守）；`mode='preview'` 本轮**无产品入口**（R08b Commit B）。

**布局坑**：`PreviewPanel` 根必须是 `.file-activity-panel`（`flex:1` 只挂在它上面）。直接塞 `.file-source` 会被按内容宽度收缩成 350px —— **截图才看得出来，断言全绿**（`.fp-code` 存在、行号也在）。

### F51（R08b）—— 四个渲染器 + 产品入口；PDF 只能走原生视图是硬事实

**交付**：`renderers/{pdf,office}.tsx`（weight 100）、`deps.ts`（conversion/embed 的真值来自能力面）、`PreviewHost.onUnavailable`（`data-renderer` 只有一个写者）、三处复用而非重写（`lib/embedRect.ts` / `lib/uiOverlay.ts` 从 WebTab 平移 / `previewEmbed.ts`）。链 78 → **80 步**；`test:electron` 新增 `e2e:preview-native`（43 项）。

**三条实现者自行裁决（我认可）**：
1. **pdf 面板只能托管现有原生视图**：渲染进程拿不到 PDF 字节，主窗口没开 `plugins`、dev 是 http 源，`file://` 的 iframe/embed 全被拦 —— 唯一真实通路就是 `e2e:pdf` 已钉住的那条。所以「内容真的在」用「插件尺寸 843×787 + capturePage 亮像素 93.3% + CDP contentType」证，**不用页数假装**（页数要等 PR-08d 的 pdfium 通道）。
2. **docx/pptx/xlsx 走转换产物回读 + `srcDoc` iframe**（产物在临时目录，跨 scheme 进不去）。代价：`file_preview` 的 1 MiB 上限 → 超了如实标 truncated 并给「内嵌浏览器打开完整版」。
3. **验收分两层**：Chrome 层只能验「落点指名谁 + 写明缺什么」，原生层才验内容。**硬说 Chrome 能看到 PDF 页数就是伪证**。

**两个变异**：`pdf` weight 100→5 → 被别的渲染器抢走（`实得 text`）；删 `unavailable` 分支 → 依赖假装可用时白屏超时。都先红后绿。

**真回归（产品级，先红后绿）**：pdf 面板空白 —— React 双挂/切标签的 `cleanup` 与下一次 `attach` 撞车，`close` 关掉的正是刚复用的那条。探针先红在「池里只剩 `about:blank` 自举视图」。修法：`previewEmbed.ts` **引用计数 + 600ms 关闭宽限**（宽限期内被再次持有则取消关闭）。

**结构版本没动（v:1）**：新增的只是 `mode:"preview"`，`toPersistedTab`/`sanitizePersistedTab` 双向都认；探针真跑了 localStorage 往返。

**实机数据复核**：383 个 profile / 1925 个 leveldb / 解出 162 份标签集，`office` 0 条、`preview` 0 条（与「R08b 之前没有入口」一致）、`agent` 8 条（旧数据，读取即丢）。

**已知边界（记账）**：pdf 不做页数/文字层（等 PR-08d）；切走 pdf 预览标签会关视图（下次重建约百毫秒）—— 要「切回秒开」需把视图属主从「面板」上移到「标签」；`.md` 仍走 text。

### F52（PR-08a 评审）—— 收口做得干净，但放行名单给缓存开了「可写」的口子

**评审结论**：9 项里 7 PASS、1 FAIL（安全面）、1 选择性 FAIL（构建标识）。干净检出 `vet` / `-race` / bridge 全量 0 FAIL，共享原语无漂移（`atomicfile` 测试一行未改全绿），锁顺序无环（`pythonRuntime()` 在 `previewService` 之前顺序调用，不嵌套）。

**FAIL 1：`~/.go-code/previews` 读写都放行 → 缓存投毒面成立。**
`denyHarnessConfigPath` 的放行循环对读与写**无区分**，所以 `write_file` / `edit_file` / bash 都能改缓存里的 `index.html`；而 `Lookup` 只 `Stat` 大小、无完整性校验。危害边界被 iframe 挡住一半（`office.tsx:124` 是 `sandbox="allow-same-origin"`，**没有 `allow-scripts`**）：

- 不能执行脚本、不能碰 parent DOM/storage/凭据；
- **能做「假内容 + 外联告警」**：面板里带 app 外壳的伪造文档（钓鱼/假数据）、`<img>`/`@import` 打 beacon、`<a>` 帧内导航到任意站点。

→ 修法已派：把 `previews` 降为**只读放行**（读名单与写名单分开），功能不受影响（`file_preview` 读回仍成功）。

**FAIL 2（选择性）：构建标识用 vcs revision 是错取舍。**
**既过度**（每次 rebuild 全量失效，缓存寿命≈一次 `go build`）**又不足**（`go run` / `buildvcs=false` / 本地改脚本未提交时 `vcs.revision` 不变 → 正好命中它声称要防的「改了脚本静默命中旧产物」）。改法：三个脚本是编译期常量，`init()` 里算一次 sha256。

**其余必修**：`preview_convert.go:41` 传 `context.Background()` → 取消分支是**死码**，且排队无上界（5 个 60s 大文件，第 5 个等 120s、前端无法中止）；产物权限 0644 → 应 0600（memory 是 0600）；补「排队超过 timeout 仍成功」的测试（本 PR 唯一没有真值保护的核心语义）。

**评审抓到的两个细节，我记下来当方法论**：
1. `previews` 的软链接逃逸有断言、**硬链接覆盖是靠 memory 的测试传递过去的** —— 按「看断言不看注释」的口径，这不算覆盖。已要求补一条硬链断言。
2. 超时测试用「与 sandbox 同语义的假转换器」，`kill(-pgid)` 断言**落在测试替身上**（等价于断言 OS 语义），对 `sandbox.runExec` 零覆盖。评审判定「断言与实现边界一致，不是验了另一个东西」，但指出**测试名 `…KillsProcessGroup` 会让人误读为覆盖了 sandbox 的 kill** —— 命名撒谎也是一种假绿。

**文档对齐（我改）**：`01-design.md` §4.7 的「输入/输出上限 50 MiB / 100 MiB」改为「输入 100 MiB 硬闸，50 MiB 仅容量规划」；错误码统一为代码实际拼法 `too_large`（下划线），全文 4 处 `too-large` 一并改掉。

### F53（PR-08a 复评）—— 第四次「测试里另建了一个等价物」

**sweep 的锁是空操作**：`sweepPreviewCache()` 里 `conversion.NewCache(...)` 新建了一份 Cache，而 `Cache.mu` 是**实例级**锁 → 与 `Service.cache` 是两个 mu。更讽刺的是 `TestSweepDoesNotRunInsidePut` 之所以绿，是因为它**复用同一个 `cache` 变量** —— 测试通过恰恰因为它没模拟生产的实例关系。

**另一处同类**：commit message 声称「删工作区会取消在飞转换」，但 `deleteWorkspace` 既不调 `shutdownWorkspace` 也不调 `cancelWorkspace`；而取消测试是直接调注册表的，所以这个缺口测试全绿。

**归纳成 `00 §4.7` 硬规则**（v0.8）：测试/探针必须从真实入口进入（`dispatch(cmd)` / `shutdownWorkspace` / DOM 点击），不许直接调内部函数「代表」它；需要替身时必须同签名同语义并写明「这一层没被覆盖」。**「我在测试里另建了一个等价物」＝这个测试不算证据。**

**实现者的一处顶回（我认）**：评审建议 `m.previewService(nil)` 复用实例，但 previewer 是**单例**，用 nil 建出来会把 `py` 永久钉成 nil → `Convert` 永远回「运行时未装配」，启动清扫会把预览功能整个打死。改成 `m.previewService(m.pythonRuntime())`，注释写在 `sweepPreviewCache` 里。

**最终交互事实（我认可这是本轮的验收标准）**：每条修法都配一条**能打死对应变异**的测试 —— 另建 Cache、去掉 deleteWorkspace 的 cancel、去掉 shutdownWorkspace 的 cancel、时限从入队开始算、指纹写死常量、meta.json 回 0644，六条变异各自红在对应测试上。

**沙箱侧实测（不只是策略字符串）**：`cat previews/index.html` 可读；`touch` / `echo >` 均 `Operation not permitted`；同一策略下 `runtime/` 仍可写（受管运行时无回归）。

**干净检出**：`go vet` 0；`conversion` 2.8s；`desktop/bridge` 58.2s / 0 FAIL；中间提交可独立通过（历史里没有红点）。

### F54（PR-08b 表格增强）—— 增强做完，但暴露门禁是「概率性成立」

**交付**：`tools/builtin/xlsx_preview.go` +544 行（sheet 导航 sticky 锚点 / 冻结窗格 / 数字格式 / 合并列宽 / 截断如实 / 注入转义），单测六条走生产入口 `XlsxPreviewer.Convert`，e2e 11 条**全用 `getBoundingClientRect` 比对**。

**两处写测试时才撞出来的「空断言」，值得记**：
1. 第一版夹具只有 15 行 → 容器根本不滚 → `scrollTop/scrollLeft` 是空操作 → **「吸顶断言恒真」**。改成 30 行 × 12 列，并先断言「必须真的滚动了」。
2. 冻结断言里补了「非冻结列已滚出左边（left=-205）」—— 否则「冻结列贴边」在没滚动时也成立。

**注入测试的做法**：openpyxl 拒绝 `<script>` 当表名，所以它去改 zip 里的 `workbook.xml` + inline string，同时打**表名与单元格**两处；断言产物里没有 `<script` / `<img`、实体正确、`data-sheet="><` 不存在（属性未被提前闭合）。为此把 `clean()` 拆成 `attr_clean()` —— 属性里插 `<br>` 就是坏字面量。

**⚠️ 真正的问题：`TestReducerLLMEndDedupAndUsage` 在 5 次全量里红了 2 次。**
数值 `0.000046` 与本机 `~/.go-code/settings.json` 的 `deepseek-v4-flash {0.14,0.014,0.28}` 逐位吻合 —— 就是 F40 的价表泄漏，只是这次漏在**另一个用例**上（`memory_baseline_test.go` 有 `isolateUserPrices`，`view_reducer_test.go` 没有）。

**这意味着「干净检出 0 FAIL」是概率性成立** —— 比表格增强重要得多。已派修复，并要求加 `assertNoUserPrices(t)` 辅助：**泄漏时红在源头用例，而不是红在下一个受害用例**；验证方式 `-count=3` + 定向 `-count=3 -run 'TestReducer|TestMetrics|TestCostUsd|TestListProviderPresets'`。

**规矩补一条（写进 §4.6）**：同一测试「全量红、单跑绿」= **顺序依赖**，必须修到顺序无关，并用 `-count=3` 证明；「单跑是绿的」不构成结案理由。

### F55（R08f）—— 账目 bug 既不是「消费者读旧值」也不是「测试等早了」，是终值本身写错

**实现者先判断后动手**（我要求的）：`grep` 账目字段 → 只在 `session/session.go` 写；`SessionClosed` 消费者四个（`view_reducer` 只 flush、`checkpoint` 提交边界、`im` 空实现、`useAppStore:7390` 读三个数成一行小信号）；**没有人在 AgentEnd 那一刻读账**。反证「测试等早了」：`Shutdown` 读账前已等到 `inRun == false`，而 `inRun` 是 `Run` 的 defer 置的 → 读到的必然是终值。

**真机制（两件事各自对、合起来错）**：记账点在 `RunStream` 返回之后，判据是 `ac.Err == nil && !ac.IsAborted()`，而 `aborted` 是随时能翻的 atomic。AgentEnd 已宣告 `stop` → 事件出门 → 消费者（用例）在事件后 `Abort` → 记账才执行，读到 `IsAborted()==true`，而 `ToolAborted()` 是 false → **两个数都不加**。

**修法（事件即事实）**：`wrapRunLedger` 拦 AgentEnd，在 `next(...)` **之前**记账，只认本次 Run 的顶层 ac（子 agent 的 AgentEnd 同路转发但 `RunId` 不同，不记也不摘凭据），记完 `CompareAndSwap`；判据换成 `end.FinishReason`（与旧判据逐项等价）。新增只读 `RunLedger()`，`Shutdown` 改用它。

**验证强度**：修复前 200 次红 2 次 → 修复后 200 次 0 FAIL；定向 50 次 0 FAIL；全量连跑 2 次 0 FAIL；变异回旧实现 → 新用例 100% 打死（`[1 0 0], want [1 1 0]`）。另加一条**反例** `TestRunLedgerEarlyInterruptStaysUnconsumed`（收尾前打断仍 `[1 0 0]`），防止「一律算交付」的过度修正。

**同类问题（只报告未修，记账）**：
1. `session.go:864` 用 `ac.Err != nil && !ac.IsAborted()` 决定发不发 `SessionRunError` —— 同款事后读，错误收场后紧接 Abort 会吞掉会话级报错（真值应是 `FinishReason == error`）。
2. `s.absorb(ac)` 在 AgentEnd 之后 → 事件时刻读 `Usage()/Cost()` 是上一轮值。
3. `toolTaskSink` 先 `emit(TaskEnd)` 再累加成本 → TaskEnd 时刻少算一个任务。
4. `s.running = nil` 同样在事件之后（本 bug 的手柄；记账已不依赖它）。

### F56（R08g 触发）—— 第三个「测试从未入库」事故：`go test ./session/` 在干净检出编不过

`session/store_record_test.go` 定义 `testSessionRecord`，被 `.git/info/exclude:154` 藏住、从未入库；`store_record_backup_test.go` 等依赖它 → **干净检出里 session 包连编译都过不去**。实现者这次如实标注了「session 侧证据不是纯干净检出」——**这个诚实标注本身就是本轮最有价值的产出**：它把一个我们一直在自我欺骗的口径差异（本机能跑 vs 干净检出能跑）钉到了台面上。

至此同类事故三处：F37（bridge 68 个文件）、R-15（3 个探针缺 import）、F56（session helper）。已派 R08g：先做**全仓 `go vet ./...` 扫描**，把「还有哪些包在干净检出里编不过」一次列清，再逐包修。

### F57（R08g）—— 「干净检出编不过」不止 session：6 个包，19 个文件，7273 行

全仓 `go vet ./...` 扫描（迭代到收敛，因为首错只报每包第一个缺失符号）：

| 包 | 缺失符号 | 定义文件 |
|---|---|---|
| `session` | `testSessionRecord` | `store_record_test.go` |
| `agents` | `skillRegistryWith` / `collectEvents` / `compressProvider` / `failingRounds` / `reqCaptureProvider` | 5 个 `_test.go` |
| `mcp` | `newTestMCPServer` | `manager_test.go` |
| `provider` | `sseServer` | `openai_test.go` |
| `tools` | `mockTool` | `engine_test.go` |
| `subagent` | `waitDone` / `mockProvider` / `spinTool` / `safeEvents` | 4 个 `_test.go` |

另有 `skills/testdata/`（6 个夹具）被 `.gitignore:74` 的 `testdata/` 吞掉 → `agents` 的两条技能用例在干净检出必红（与 F37 追补、R-10 同形状）。

**修复后**：6 个编译失败包 → 0；`session` 116 个 PASS / 0 FAIL；其余 5 个包全绿；干净检出 `go vet ./...` 只剩 1 条**真 vet 告警**（`memory_dream/main.go:201` 的 `fmt.Println` 带尾换行，Go 1.25 报 redundant newline）——已并入 PR-08c 的 Commit A 清掉。

**两处版本分歧，处理方式值得记**：
1. `agents/agent_loop_test.go`：11 个副本里 10 个一致，`agent-hub` 分支多 48 行（`keepAllCompressor` + 一条新用例）。实现者**实测那条新用例在本 HEAD 上 FAIL**（`compact + text 应继续调用一次模型，calls=0`）—— 那是另一分支在研的生产改动。**用主仓副本，分歧如实报告，不硬合。**
2. `subagent/nested_loop_e2e_test.go`：12 个副本 0 个一致，主仓版本是 11 个 worktree 版本的**严格超集**（多孙运行归属锚断言）→ 用主仓版，并建议其余 worktree 合并时丢弃本机旧副本。

**合并侧提醒（我处理时验证过）**：主仓这 13 份未跟踪副本与入库 blob **逐字节相同**（13/13），备份到 `/tmp/mc-bak3` 后删除再合并；`agent-hub` 的 `agent_loop_test.go` 与 11 份 `nested_loop_e2e_test.go` 是**不同版本**，不能无脑删。

### F58（PR-08c 评审）—— 媒体协议把「工作区」当成了渲染进程说了算的参数

**安全洞（必须改 1）**：`workspace` 由渲染进程自由传入，主进程只校验「绝对路径 + 存在 + 目录 + 非敏感」，然后**直接登记为媒体根**。传 `workspace=/Users/admin` 或 `~/Documents` 即可 → 之后该目录下**任何媒体扩展名文件**都能被 `hai-media://` 流式读出。协议从「工作区内的媒体通道」退化成「任意目录的媒体文件读取通道」。

- 唯一现有约束是**巧合**：`workspace=/` 因 `real.startsWith('/' + sep)` 变成 `'//'` 才失效 —— 换 `/Users` 就通。**这不是防护，是边界 bug 碰巧 fail-closed。**
- `media:probe` 是同一个洞的第二扇门（`ws` 同样来自渲染进程）。
- `MAX_ROOTS` 是插入序 FIFO 且命中不刷新 → 活跃根会被 8 次别的登记**挤掉**（正播的视频断流，可主动触发），且关闭工作区后根**永久驻留到进程退出**。

**修法（已派）**：主进程持有「真的被打开过的 workspace」集合（打开时写入 realpath、关闭时删除），`media:url` / `media:probe` 只接受集合内成员，`allowMediaRoot` 降为模块私有；另加 `senderFrame` 校验。

**第二处（必须改 2，1 行修法）**：扩展名白名单判的是 `abs`，读字节用 `real` —— 软链 `ws/x.mp4 -> ~/foo.json` 能过白名单，**字节还是送出去了**。修法：realpath 之后重判白名单。

**第三处（必须改 3）**：8 条逃逸测试**只断言状态码**，`statusOf` 还把 body destroy 掉。这类测试能抓「放行了」，抓不住「回了 403 但 reason 里回显路径」。唯一做了内容断言的是非媒体扩展名那条。

**第四处（必须改 4）**：`dispose()` 是**死代码**（`src/` 零调用者）→ 关标签只 `detach` 不释放解码器；且 `e.attached` 是**池级共享标志**而 `attachedTo` 是句柄级 → 同 URL 双标签时 A detach 会让 B 正在显示的 `<video>` 被 `destroyEntry` 从活 DOM 拔掉。

**评审确认达标的部分**（不返工）：
- **Range 语义**逐条核对边界全对（闭区间 `end-start+1`、`bytes=-512` 后缀、越界 clamp、`start==size → 416`、空文件两种、`1e3` 被正则挡、`bytes=100-50` 按语法错忽略）；超长数字用 `isSafeInteger` 挡住精度截断，**比 05 §4 D-L 的伪代码更严格**。
- **416/HEAD/405** 三个都带对了头（`Content-Range: bytes */size`、HEAD 有头无体、405 带 `Allow`）。
- **TOCTOU 可接受**：`createReadStream` 用的是已解的 `real` 字符串，原位换软链无效。
- `isSensitiveFsPath` 的顺手收紧**方向正确且有回归实测**（原实现拿未 realpath 的 `os.homedir()` 比，macOS `/var → /private/var` 下 `~/.ssh` 漏判）。
- 多段 Range 回 200：RFC 9110 §14.2 允许，demuxer 只发单段，口径保留。
- **不回头拆 PR**：协议与渲染器是同一个失败域，拆开只剩「协议先上线、无人从 UI 验证」的空窗。

**评审点名的两处方法论**：
1. **测试自证**：统计断言验的是生产代码自己 `+=` 的计数器（且还在**规划时**累加，客户端 abort 也照算全额），而 e2e 拿它当「不重下」的唯一依据 —— 自报数字，不是实测字节。
2. **没走生产入口**：33 条测试全部直接调 `handleMediaRequest`，**绕过 `protocol.handle` 翻译层**。若哪天 `headers.get('range')` 返回 null，生产里 200 会静默顶替 206、拖动条失效，而 33 条全绿。与 F52/F53 同形状，只是上移一层。

### F59（PR-08c 复评）—— 两条窄修，其中一条是「commit message 声称删了，代码没动」

**真修的部分**（评审逐条核过，不是复述）：
- 信任锚：`media:url` / `media:probe` 只认清单成员、`allowMediaRoot` 降为模块私有且**先判信任再登记**、`senderFrame` 只认主窗口 mainFrame、`deleteWorkspace → workspace:forget` 真删；
- realpath 后重判扩展名：`逃逸 5b`（`ws/hidden.mp4 -> notes.txt`）断言 403 **且** body 干净，还额外断言 `mediaUrlForFile` 这条路也拒；
- 内容断言：`assertClean` **17 处调用点**，8 条逃逸一条不落（不是抽样）；
- 真入口用例：注入假 `protocol` 捕获 handler、用**真 `Request`**、断言 Range→206 与 `if-none-match`→304 —— 上一轮「翻译层没人验」的洞关掉了；
- `bytesServed` 真挂到流 `'data'`（不是改注释）；`If-None-Match`/`If-Range` 真实现。

**两处必须改**：
1. **`MAX_ROOTS` 的 FIFO 没删**：commit message 写「顺手删掉 MAX_ROOTS 的 FIFO」，而 `media-protocol.ts:138` 的常量与 `:167-168` 的 `slice(-MAX_ROOTS)` **一个字没动**。后果：9+ 工作区用户登记第 9 个根后第 1 个根 403 → 正在播的视频断流；**且零测试覆盖** —— 这既是它没被删的原因，也是没人红的原因。`known-workspaces.ts:93` 还新增了同一个 bug 的第二份（512 条 FIFO）。
2. **`pendingDispose` 过杀**：同文件双标签 A/B，关 A 后 B 只是**切走**就被销毁 → 切回空白/重下，**正撞这条 PR 的验收点**。修法：销毁判据改用 owner 计数（`acquire` +1 / `dispose` −1），`detach` 不参与。

**方法论（写进 §3）**：commit message 里的每个「删掉了 / 改成了 / 新增了」**必须能在 diff 里指到行**。指不到＝虚假声明，按缺陷处理。这轮的 FIFO 就是活例：声明与代码分叉，而测试因为零覆盖也不红。

### F60（PR-08c 收尾）—— 两条窄修落地，我用自己的复核代替了第三轮评审

**代码事实**（不是 commit message）：
- FIFO：`media-protocol.ts` 改 `roots = [real, ...roots.filter(r => r !== real)]`；`known-workspaces.ts` 同写法；`MAX_ROOTS` / `MAX_KNOWN` / `slice(-N)` 全仓已无（我 grep 过，剩余命中都是无关的日志截断/输入历史）。
- 元素池：条目上是 `owners: Set<string>`（标签 id），`acquire` add、`disposeMediaElementsForPath` delete，**销毁只发生在 `owners.size === 0`**；`attachedCount` 彻底退出销毁决策，`detach` 只暂停与摘除。

**变异各自打死对应用例**：
- 把 FIFO 加回去 → `登记第 9/10 个根之后，最早那个根仍然能 200` 红（这条用例正是上一轮「零覆盖」的补丁）；
- 把销毁判据改回 attachment 计数 → `A/B 双标签：关 A 之后 B 切走再切回，元素不能被销毁` 红。

**我复核的三条**：`test:media-protocol` 39/39、`test:known-workspaces` 7/7、`test:media-stage` 7/7；干净检出 `go vet ./...` 为空；`go test ./desktop/bridge/` 54.8s 0 FAIL；`test:electron` 281 断言。

**没有做第三轮独立评审** —— 这两条是机械修改且各有一条能打死它的变异，我自己 grep + 跑测已能确认；再把评审开一轮只会重复同样的三段 grep。**判断依据记在这里，免得以后被当成「省略了评审」。**

**它对「怎么确认 FIFO 删了」的回答值得存档**：
> 上一轮的根因是**用计划里的动作写正文**；这一轮改成**用 diff 里的行写正文** —— 先 grep 出代码形态，再照着行写动词，正文里每条「删掉/改成/新增」都能在 diff 里指到行才发。

这正好是 `00 §3` 新规则的执行形态。

### F61（环境）—— LibreOffice 装上了，但本机渲染不出中文

`brew install --cask libreoffice` 失败（portable-ruby 从 ghcr.io 下载断流），改从官网下 dmg 手动装：**LibreOffice 26.8.3.1 可用**。

真转换实测（python-docx 造 docx → `soffice --headless --convert-to pdf` → pypdfium2 渲染）：

| 文档字体 | PDF 里有字符 | 渲染出的字形 |
|---|---|---|
| 默认 | ✅ `extract_text` 取到中文 | ❌ 空白 |
| PingFang SC | ✅ | ❌ 空白 |
| Songti SC | ✅ | ❌ 空白 |
| Arial Unicode MS（并拷进 `~/Library/Fonts` 重建 profile） | ✅ | ❌ 空白 |

**结论：这台 macOS 上 LO 26.8 的 CJK 字体发现/嵌入是坏的**，与我们的代码无关。

**处置**：
- 真转换的「中文正确性」标为**未验证（环境阻塞）**，记 R-18；
- 只用真 soffice 验「ASCII 文档能转出 PDF 且能进内嵌浏览器」；
- 不改产品代码去绕（不是代码问题）；
- 需要另一台机器复验 —— 若普遍复现，就得考虑锁 LO 版本或改成「docx → 图片」路线。

**另一条已确认的好消息**：`pypdfium2` 在受管运行时里本来就有（`~/.go-code/runtime/python/venv`），`renderImages` 的 15 个测试全 PASS、**0 SKIP** —— 之前简报里说「本机没有 pypdfium2」是我看错了系统 python，不是运行时。

### F62（PR-08d 复评）—— P0 的根因是**起跑竞态**，不是「fixture 不写」；顺带抓出「一个字段两个消费者」

复评给了四条 must-fix。四条全落地，但**第一条的根因和评审判断不一样**，值得记下来。

**P0 误判**：评审判断是「fixture 没写 `profiles.log`」。实现者加探针实测 —— 压满 ncpu 个 `yes` 跑 20 轮，**8 轮的假 soffice 一行都没执行就被 SIGKILL**（plog 根本不存在），另 1 轮合法路径 elapsed 到了 3.5s。所以 `convert_test.go:193` 红，而它后面两条断言（`kill(-pgid)=ESRCH`、profile 清理）**从未执行过** —— 一条测试红了三轮，真正被验的却是零条断言。

修法三处，每处都有一个「不这么改就会掷硬币」的理由：

| 改动 | 为什么必须 |
|---|---|
| 预建 `profiles.log`（`os.WriteFile(nil, 0o600)`），假 soffice 只追加 | 文件缺失不再能伪装成「断言没跑」 |
| `Timeout` 500ms→3s、`sleep` 5s→60s、elapsed 上界→10s | 3s 对实测起跑 ≤0.5s 有 6x 余量；10s 仍远小于 60s |
| 长睡改由**重定向了 stdio 的孙进程**承担再 `wait` | 原先孙进程握着继承的 stdout 管道，「只杀单进程」的变异会让 `cmd.Wait` 卡到 `WaitDelay(3s)`，红/绿变成掷硬币（实测到过 3.5s）。让开管道后，唯一存活证据就是进程组本身 |

变异证据（`killProcGroup` → `cmd.Process.Kill()`，连跑 3 次）：

```
--- FAIL  convert_test.go:233: 进程组 18109 仍存活（kill(-pgid,0) = <nil>）：超时没有杀进程组
（3/3 都红；已回退，git diff sandbox/sandbox.go 为空）
```

**P1 状态自失效**：`officeKitGone(kit)` 只判 `os.Stat(bin)`，而 `Kit()` 是缓存、不会自己变 nil。改成 `prober.Refresh()` + 主动推 `office_kit_status` 事件，前端同名分支更新 `officeKit`。测试走真入口 `m.dispatch(docx_preview)`；变异 `if false && officeKitGone(kit)` → `timeout waiting office_kit_status 事件` 红。

**P1b 工具快照：选了「改文案」而不是「补注册」**。注册点是 per-session 的 `*tools.Engine`，补注册等于**在会话跑到一半时改工具集**（模型 schema、CodeMode surface 都会变），而工具集变化只允许发生在 `rebuildLoop` 那个边界上。代价是一条 i18n + 设置页一行小字：「重新探测只刷新状态与预览；工具需重启应用才出现在会话里」。**这是产品决策，不是偷懒** —— 记在这里备查。

**P2 超时按路径**：`previewTimeoutMs()` = kit 已知可用 ? 90s : 30s，首次未知时补问一次。

**额外真 bug（不在这四条里，装 LO 才暴露）**：`e2e:preview-native` 的 docx 一条红在「等待超时: docx 产物内容（最后：null）」。根因是**一个字段两个消费者**：

```
bridge 产出 html_path
  ├─ 内嵌浏览器：能吃 PDF ✔（Chromium PDFium 原生渲染）
  └─ 右侧栏面板：把 html_path 当**文本**读回塞 iframe → 读到 PDF 二进制 → 空白 ✘
```

修法是让消费者自己声明要什么：面板侧加 `approximate=true`（显式要近似 HTML、跳过 kit），原生视图不传、照旧拿 PDF。

> **教训**：一个字段有两个消费者时，语义不能由生产者单方面决定 —— 谁要什么，自己在请求里说。`html_path` 这个名字本身就在骗人：它有时是 HTML，有时是 PDF。

**CJK 仍是 R-18**（环境阻塞）。ASCII 链路已打通：docx → soffice 26.8.3.1 → PDF 22568 字节 / 2.56s → 内嵌浏览器原生渲染，纸张占比 61.8%，`e2e:pdf` ALL PASS。
### F63（PR-09）—— 派单里写错的一条口径，被原样写进了代码注释

PR-09 的派单里我写了「`⌘1..9` 是切 `rightTab`」。实现者照这句话在 `⌃⇥` 处理里写下注释：

```ts
// 与 ⌘1..9（切 rightTab）是两回事，所以显式排除 metaKey。
```

**这句话是错的**。`App.tsx:139-158` 的 ⌘1..9 / ⌘0 切的是**最近会话**（`recentSessions[idx]` → `setActiveSession`），全仓没有任何 digit → `rightTab` 的绑定。`grep -rn "rightTab" desktop/app/src` 里也没有。

发现方式是评审时去 `App.tsx` 核了一眼按键分支 —— 而不是读注释。**如果我只读注释，这条会一路混进 main**，下一个人还会照着它推断快捷键语义。

> **教训（F59 的上游）**：F59 管的是「commit message 声明要能在 diff 里指到行」；F63 是它前面一环 —— **派单里的事实陈述同样要能指到代码行**。实现者没有动力去核对上级给的背景，他会把它当成既定事实，然后写进注释、测试名、甚至产品文案。派单里每一句「现状是 X」，作者（我）都有举证的义务。
>
> 具体做法：派单里写现状时带上 `file:line`；写不出来就写「我没核，你自己核」。

同轮另外两处修正（都是主会话做、各一行）：

1. 注释改成「⌘1..9 在 `App.tsx` 切的是最近会话，不是 `rightTab`」；
2. 探针补 npm 入口 `e2e:tabbar` —— 新探针没有 script 就是链外孤儿，F50 已经记过一次「不在链里的探针抓不到回归」。

实现者的两处判断经核为**正确**，记录备查：
- `关闭全部` 用 `store.closeTabs(ids)`：store 里它先关干净的、把 dirty 的**整批**塞进 `closeConfirm` 队列逐个问；逐 id 调 `closeTab` 只会问到第一个。
- `⌃⇥` 读 `useAppStore.getState().tabsByScope[scope]` 而不是闭包里的 `tabs` —— 键盘事件比 React 状态更新先到，用闭包值会慢一拍。

### F64（PR-10）—— 缓存就是第二种真相：两个模块实例，两份 cache，一边写一边不认

`surfaces/health.ts` 第一版是「模块级内存 store + 每次改动 write-through 到 localStorage」。看着很稳：
写一半内存一半持久化，读走内存（省一次 JSON.parse）。

探针直接把它打穿了：`disableSurface('git')` 之后 `localStorage` 里有记录、探针这一侧的
`isSurfaceDisabled('git') === true`，但 `listSurfaces()` 仍然把 `git` 列在菜单里。两边读的是**同一份
文件、同一个 localStorage**，却给出相反答案 —— 因为探针用 `import('../../src/surfaces/health.ts')`
动态引入，和 RightPanel 那条 `../surfaces/health` 被解析成了**两个模块实例**，两份 `cache` 各自为政。

修法不是「让探针别动态 import」（那只是把症状挪走），而是**删掉 cache**：真相源只留 localStorage，
每次读它。一条记录的 `getItem + JSON.parse` 是微秒级，用不上第二种真相。

> **教训（F53 的同族）**：F53 是「测试里新建了一个 Cache 实例，生产用的是 Service.cache，锁在测试路径上
> 是空操作」；F64 是同一件事的另一面 —— **只要有两份可变状态，就迟早会分叉**，而分叉现场通常长得
> 像「某个 UI 分支没刷新」。判据很简单：这个函数能不能被单独 import 两遍还表现一致？不能就别加缓存。

### F65（PR-10）—— React 18 dev 会把出错的那次 render 跑第二遍，「只崩一次」的夹具因此永远不崩

探针需要一个「第一次挂载崩、第二次成功」的标签来验「切走再切回能重新渲染」（boundary 按 tab.id 重置）。
第一版写成计数器：`n = hits+1; hits.set(id, n); if (n <= 1) throw`。

结果：那个标签一次都没崩，探针拿到的是正常的 `.probe-ok`。原因是 React 18 在**开发模式**下，出错后
会把同一次 render 再执行一遍（用来重建组件栈）；第二遍 `n=2` 不再抛 → 错误消失 → 边界根本没被触发。

改成显式闸门（`__probe.setBoom(true/false)`）后语义才可控：崩不崩是**夹具的输入**，不是渲染次数的函数。

> **教训**：任何「按调用次数改变行为」的夹具，在 React dev 双跑、StrictMode 双挂载、并发重放面前都不成立。
> 要么用显式开关，要么用「输入决定输出」的纯函数夹具 —— 别让夹具依赖框架内部的重试策略。

### F66（PR-10 复评）—— 三条「我以为是取舍」的，两条其实是错的；还有一条是 React 的 commit 边界

复评推翻了 PR-10 的两处实现，并留下了本 PR 最值钱的一个坑。

**① `key={tab.id}` 换隔离：错。** 第一版为了「A 标签崩了切到 B 不能看到 A 的错误页」给 boundary
挂 `key={tab.id}`，并且在注释里如实记了代价（children 重挂）。复评的判断是：**代价本身就不该付** ——
R06c 的「切 web 标签原地复用」是硬约束（原生视图与页面状态靠 DOM 容器不重挂），而「按标签重置
错误态」根本不需要 key。改法：boundary 收 `tabId`，变化时 `setState({err:null})`，children 原地更新。
证据从「隔离用例」升级成「实例号用例」（T2b）：同一 provider 组件在 a→b→a 之后，`useState` 惰性初值
领到的实例号必须不变。实测：key 版本 `1 → 2 → 3`，tabId 复位版本 `1 → 1 → 1`。

> **教训**：写「代价：……」不等于尽到了告知义务。**如果那个代价破坏了另一条硬约束，它就是缺陷**，
> 不是取舍。判断方法：把代价写成「它会破坏 X」，再问「X 是谁的硬约束」。这里 X=R06c，早就写在
> RightPanel 的注释里（「**不要**给这个元素加 key」），我却先加了 key、再在注释里解释为什么加。

**② `addableKinds()` 和 `listSurfaces()` 取交集：错。** 第一版的理由是「禁用只有注册表知道」，
于是菜单 = `ADDABLE_KINDS ∩ listSurfaces()`。复评指出：菜单是「这个应用能开什么」的静态能力清单，
不是注册表镜像；取交集会让「注册表空 / 某个 provider 漏了副作用注册」变成**菜单静默变空**，
而那正是 `panelFallback` 兜得住的场景（F45）。少一项没人报障，比渲染出错更难查。
改法：`ADDABLE_KINDS.filter(k => !isSurfaceDisabled(k))`。探针补了反向用例：`unregisterSurface('git')`
之后 ＋ 菜单里**仍然有** git，只有 `disableSurface` 才让它消失。

**③ 墓碑的「移除标签」用 `closeTabNow`：错（数据安全）。** 派单当时写的是「这是墓碑，不是有未保存
内容的编辑器」，所以跳过 dirty 守卫。复评指出：`file{mode:'edit'}` 标签崩到被禁用时，`dirtyTabs`
里可能真的有未保存内容，`closeTabNow` 会**静默丢掉**它。改 `closeTabs([id])`：干净的立刻关、
脏的进确认队列 —— 交互洁癖不该压在数据安全上。探针拆成 T7a（干净：立刻消失、不弹框）/ T7b
（脏：标签仍在 + 弹确认框 + 取消后脏标记还在）。

**④ 顺带踩到 React 的 commit 边界（本 PR 最贵的一条）。** 改成 tabId 复位后，连崩 3 次的用例
在第 1 次就数到 2，直接误禁用。原因：复位判据写成 `this.state.err` 时，**「切到 b4a」和
「b4a 在这一帧崩了」是同一个 commit** —— 复位逻辑把刚崩出来的新错误当成上一个标签的残留清掉，
children 再渲染一次、再崩一次，一次崩溃记两笔。判据必须是 `prevState.err`（这次更新**之前**
就已经在错误态）。

> **教训**：`componentDidUpdate` 里判「状态是不是旧的」，要看 `prevState`/`prevProps`，
> 不能用 `this.state`/`this.props` —— 后者描述的是**这次更新之后**的世界，而你想问的是它之前。
> 这类 bug 只在「同一次更新里既换输入又出错」时出现，平时跑一万次也碰不到；能抓到它的是
> 「连续 3 次」这种**计数型断言**，不是「有没有崩溃卡」。计数型用例值得为每一个阈值都写一条。

