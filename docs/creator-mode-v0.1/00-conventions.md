# 创造模式 V0.1 —— 前置规范与 PR 约束

> 状态：**活文档 v0.4（实施期持续修订）**　日期：2026-11-11
> 定位：01 说「为什么」、02 说「做什么/什么顺序」、03 说「写成什么样」、**00 说「施工时不许违反什么、PR 怎么算做完」**。
> 一句话：**规范只把不可回退的那几条钉死；其余条款允许边做边改，但改动必须留痕。**

---

## 0. 地位、约束力与修订

### 0.1 约束力三档

| 档 | 含义 | 违反后果 |
|---|---|---|
| **MUST** | 硬约束。改了它就要动决策或架构 | PR 不许合；必须先走 §0.3 的 L3 |
| **SHOULD** | 默认这样做，有理由可偏离 | PR 描述里写「偏离 + 理由」，评审接受即可 |
| **MAY** | 建议/示例，照抄省事 | 无 |

每条规则的档位直接写在标题或表格里（`[MUST]` / `[SHOULD]`），**没有标注的按 SHOULD**。

### 0.2 冲突时的优先级

```
01 §11 决策登记（D-A…D-N）
  > 00 §2 CodeMode 契约 与 §1.1 依赖约束（硬约束）
    > 02 PR 条款（顺序与验收）
      > 03 代码方案（参考实现）
        > 00 其余条款
```

03 是**参考实现**：实现时发现更好的写法，改 03 即可，不需要裁决；但如果改动碰到上面三层，按 §0.3 走。

### 0.3 修订分三档（这条是本文最重要的条款）

| 档 | 典型改动 | 流程 | 版本 |
|---|---|---|---|
| **L1 澄清** | 错字、补例子、把含糊的话说清楚，语义不变 | 直接改，§8 记一行 | v0.x.**y** |
| **L2 条款调整** | 行数上限、测试门槛、检查表增删、命名细则 | 改本文 + `grep -rn` 修交叉引用 + §8 记录 | v0.**x** |
| **L3 硬约束变更** | 依赖例外、CodeMode 契约、工具接线方式、24 个 PR 的拆分、kind 数量 | **先写 `04-review.md` 裁决**，再同步 01/02/03/00 | v**1**.0 |

**谁可以改**：实施者有权直接改 L1/L2（不需要问）；L3 需要一次裁决留痕（裁决可以很短，一两句话+理由即可，不需要开会）。

**过期即删**：任何条款如果在连续 2 个 PR 里都没被用到、也没被引用，就在该 PR 的收尾里删掉并记一行。规范的价值在「还在被读」，不在「写得全」。

### 0.4 本文不重复别处的内容

| 想知道 | 去哪 |
|---|---|
| 为什么取消 CreatorMode | 01 §5、06 §5、04 F26 |
| 24 个 PR 的依赖与估时 | 02 §1 |
| 具体文件里写什么代码 | 03 对应小节 |
| 某条设计的取舍理由 | 04 裁决表 |
| 文档口径数字 | `README.md` 口径表 |

---

## 1. 前置规范

### 1.1 依赖 [MUST]

**不加新 npm 依赖**是硬约束（用户明确要求）。已装依赖见 `desktop/app/package.json`：

```
react react-dom zustand react-resizable-panels react-markdown remark-* rehype-* katex
mermaid shiki github-slugger hast-util-*  ← 运行时
vite typescript eslint playwright electron electron-builder  ← 工具链
```

**例外流程**（四步，缺一不可）：

1. 在 04 写一条裁决：为什么现有依赖/自研做不到、体积与维护成本、卸载路径；
2. 结论必须是**新增一条 D-* 决策**（例：`D-O 允许 CodeMirror 6`），不能只在 PR 里说一句；
3. PR 里单独一个 commit 只做 `package.json` + lock，便于 revert；
4. 该依赖必须能在一个 PR 内整体摘除（不散落在业务代码里）。

> **已结案（F32）**：V0.1 不引入 CodeMirror，编辑器走零依赖受控 `textarea` + 行号 + 只读 shiki 高亮。CodeMirror 只作 V1 可选增强，届时按本节四步走例外。

### 1.2 目录与命名 [MUST]

```
desktop/app/src/surfaces/
  types.ts            契约：SurfaceKind / SurfaceProvider / SurfaceRenderContext
  registry.ts         注册表（模块级单例，唯一写者）
  Boundary.tsx        每面板错误边界
  builtin/            activitySurface.tsx gitSurface.tsx fileSurface.tsx webSurface.tsx
  preview/            types.ts registry.ts renderers/*.tsx
  plugin/             loader.ts（动态 import + 注册 + 卸载）
```

| 对象 | 规则 |
|---|---|
| Surface 组件文件 | `<kind>Surface.tsx`，默认导出组件 |
| 渲染器 | `renderers/<format>.tsx`，命名与 `PreviewFormat` 一一对应 |
| 工具名 | `动词_名词`，全小写下划线：`ui_inspect` / `surface_install` |
| 插件 id | 必须以 `plugin:` 开头，内置注册表拒收（01 §5） |
| 内置 kind | 只允许 4 个（activity/git/file/web）；注册期拒收未知 kind（含历史名 `office`） |
| 测试文件 | 纯函数 `src/lib/<name>-test.ts`；e2e `e2e/verify_<topic>.mjs` |
| barrel（`index.ts` 转导出） | **禁止**新建：会制造循环依赖，也会让 tree-shaking 失效 |

### 1.3 TypeScript [MUST]

- `strict: true` 已开（`tsconfig.json`），不许 `// @ts-ignore`；确需绕过用 `@ts-expect-error` + 一行原因。
- 禁 `any`：外部边界（IPC、JSON、DOM 事件）用 `unknown` 收，进来立刻窄化。
- 类型导入用 `import type`（`isolatedModules` 已开）。
- 联合类型去字段**不许**直接 `Omit<Union, K>`（`keyof` 取交集，会只剩公共字段）：一律用 `DistributiveOmit`，并加一条编译期用例（R00 评审踩过）。
- 依赖方向单向：`registry.ts` 不 import `builtin/`；`builtin/*` 只 import `types.ts` + `registry.ts` 的注册函数。**注册发生在 `builtin/index.ts` 的副作用里，由应用入口 import 一次。**
- 跨进程数据（bridge ↔ renderer）必须显式校验/窄化后再用，不直接 `as`（返回形态约束见 §1.6）。

### 1.4 Go [MUST]

| 主题 | 规则 |
|---|---|
| 包边界 | `desktop/bridge` 只做通道；`internal/officekit` 只做转换；业务不落 bridge |
| 错误 | 返回带码错误（`ErrNotFound` / `ErrTooLarge` / `ErrUnavailable`），bridge 转成 `{ok:false, error:{code,message}}`；不用裸 `errors.New` 拼串给前端 |
| context | 第一参，所有外部命令（soffice/ffprobe）必须 `exec.CommandContext` |
| 写文件 | **只能**走 `internal/atomicfile`（flock + 原子替换）；不许直接 `os.Rename` 或自己加锁 |
| 日志 | 结构化，字段见 §1.8；不打印文件内容、不打印绝对路径的用户名段 |

### 1.5 状态纪律 [MUST]

三条来自 04 的 S1-S20，实施时最容易破：

1. **注册表只装「活着的」**：运行时注册；卸载即注销；持久化在 `index.json`（装过什么），不在注册表。
2. **派生值不落库**：`rightTab` 由 `applyTabs()` 单点写穿（D-D）；dirty 属于标签，聚合值现算。
3. **渲染态不跨面板共享**：视图池/休眠状态归各自 Surface；`ui` 快照只给当前会话（D-H）。

### 1.6 工具 schema 与返回 [MUST]

| 约束 | 值 | 依据 |
|---|---|---|
| 常驻工具数 | ≤ 4（`ui_inspect` / `surface_install` / `surface_list` / `surface_remove`） | F26 |
| 描述长度 | ≤ 120 字，只写「何时用」，不写教程（教程进 skill） | F27 |
| 参数字段 | ≤ 3 个 | F27 |
| 只读工具 | 声明 `readOnly: true`，且**不许**有副作用 | 审批策略按此判定 |
| 返回空态 | 集合类字段返回 `[]`，**不返回 `null`** | 脚本侧 `Array.isArray` 断言 |

### 1.7 安全 [MUST]

- **路径**：任何来自渲染层的路径（`hai-media://`、`file_open`）都要 `filepath.Abs` + `EvalSymlinks` 后判断是否在允许根内，拒绝前缀混淆（`/a/bc` 不算在 `/a/b` 内）。
- **插件包**：安装前 `sha256` 校验（与 index 记录一致），校验失败即拒；manifest 里出现 `write` 能力直接拒（D-E）。
- **审批点固定三处**：安装、卸载、脚本内首次调用写类工具。不许新增静默路径。
- **禁止** `eval` / `new Function` 执行插件代码；插件走 `import()` + 契约类型。
- **CSP 不放宽**：`hai-media://` 通过 protocol handler 提供，不改 `webSecurity`。

### 1.8 可观测性 [SHOULD]

统一字段（日志与 e2e 断言共用）：

```
surface=<kind|plugin:id> session=<id> pluginId=<id?> op=<open|install|convert|save>
durationMs=<n> outcome=<ok|error|rejected|timeout> bytes=<n?>
```

- 不记录文件内容、不记录完整用户路径；
- 转换类调用额外记 `cache=hit|miss`、`kit=<version|none>`。

### 1.9 测试三层 [MUST]

| 层 | 位置 | 命令 | 适用 |
|---|---|---|---|
| 单测（纯函数） | `src/lib/<name>-test.ts` | `esbuild --bundle --platform=node --format=cjs --outfile=/tmp/x.test.cjs && node --test` | 布局换算、注册表、schema 校验 |
| 集成（通道） | `desktop/bridge/*_test.go` | `go test ./desktop/bridge/...` | file_open/save、转换服务限额、安装器 |
| e2e（真渲染） | `e2e/verify_<topic>.mjs` | `npm run build && node e2e/verify_<topic>.mjs` | 三态布局、多标签、编辑器、CodeMode 脚本 |

新增测试必须同时加到 `package.json`：

```json
"test:surface-registry": "esbuild src/surfaces/registry-test.ts --bundle --platform=node --format=cjs --outfile=/tmp/surface-registry.test.cjs && node --test /tmp/surface-registry.test.cjs"
```

并被 `npm test` 的链条引用——**没进链条的测试等于不存在**。

e2e 约定（对齐现有 39 个 `verify_*.mjs` / 57 个 `e2e/*.mjs`）：
- 头注释写清「覆盖 / 用法 / 退出码」；
- 每个文件用**独立端口**（避免并行冲突）；
- 失败累加 `fail` 并以非 0 退出，不中途 `process.exit(0)`；
- **就绪判据必须是「这张页面是我的」**：取回探针页并比对自己生成的 `<title>` 标记（`e2e/_serve.mjs` 的 `waitProbePage`）。`waitPort` 只证明「有人在听」，证明不了「听的是本脚本刚起的那个」（F46：5199 上别人的 vite 让探针连上了另一张模块图，报出来却是 `waitForFunction` 超时）；
- **退出码三档**：`0` 全过 / `1` 断言失败 / **`2` 前置不满足**（端口被占、探针服务没起来）。前置不满足要打印「为什么」并退 2 —— 不许静默 skip，也不许混进 1 当成测试失败（F47）；
- 探针若渲染 `RightPanel` / `App` 等走 `getSurface()` 的组件，必须 `import 'src/surfaces/builtin'`，并加一条「provider 已注册」的自检断言（F45：缺这行时验的是 `??` 兜底分支）。

### 1.10 特性开关 [MUST]

每个可能伤到现有用户的 PR 都要留一个**一个 PR 内可用**的回滚开关：

| 开关 | 默认 | 作用 |
|---|---|---|
| `settings.features.surfaceTools` | `true`（PR-14 起） | 关掉 = 4 个工具不进工具集 |
| `surfaces.plugin.enabled` | `false`（PR-13 起） | 关掉 = 不加载任何第三方面板 |
| `surfaces.preview.kit` | `auto` | `off` 强制走近似渲染 |

开关必须：默认值写进 `01` 的决策、改动不需要重启（或明确写出需要重启）。

### 1.12 性能 [MUST]

> 看板：`progress.html` 的「性能」页（P01–P15）。**没有数字的 PR 不算通过。**

| 规则 | 内容 |
|---|---|
| 预算看 p95 | 不看均值；同一样本、同一机器、跑 3 次取 p95 |
| 先建基线 | 首次触达 heavy 路径（web/编辑器/Office/媒体）时必须先测一次基线 |
| 回归阈值 | 相对基线 **> 10% 阻断合并**，不做「感觉快了」的优化 |
| 测量位置 | 渲染侧用 `performance.now()` 打点；Go 侧用 `time.Since`；内存用 Electron `getProcessMemoryInfo` |
| 记录位置 | 实测值写回 `progress.html` 对应 P 行的 `last`，并写进 PR 描述 |

关键预算（全表见看板）：面板打开 120ms、标签切换 50ms、编辑器开 1MB 文件 300ms、ui 快照 20ms、Office 冷转换 60s、单 web 视图内存 150MB、工具 schema ≤ 900 token。

**禁止**：用「逻辑上不会慢」代替测量；用采样一次的均值当 p95；在没有基线时做大规模预优化。

### 1.11 提交与分支 [SHOULD]

```
feat(surface): 注册表骨架 —— 重复 kind 抛错，plugin: 前缀拒收
fix(surface): 关标签误触发自动展开 —— 展开只属于 openTab
refactor(atomicfile): 提取 flock 写文件 —— 零行为变化
test(surface): CodeMode 脚本断言 —— 对象返回 + 审批拒绝
docs(creator-mode): 00 前置规范 v0.1
```

- 提交信息用中文，`scope` 用英文小写；
- 一个 commit 一件事；**禁止**把 `gofmt`/`prettier` 全量重排混进功能 commit；
- 分支名 `feat/pr-00-surface-registry`，与 PR 编号对齐。

---

## 2. CodeMode 契约 [MUST]（工具类 PR 的硬门禁）

四个工具**全部**要能在 `run_code` 脚本里被调用并拿到结构化结果。没做到就不算完成——PR-12/13 的验收里已绑定。

### 2.1 七条

| # | 要求 | 实现点 |
|---|---|---|
| C1 | 声明输出 schema | 实现 `tools.OutputSchemaProvider`：`OutputSchema() any` 返回 JSON Schema（`nil` = 不声明，本工具不许为 nil） |
| C2 | 写结构化结果 | `tools.StructuredSinkFrom(ctx).Set(raw)`，**必须判 nil**（引擎可能没注入） |
| C3 | 只在脚本调用时写 | 外面套 `if tools.IsScriptCall(ctx) { … }`——模型直调走文本路径，避免同一份数据占两份 token |
| C4 | 失败必须 reject | 工具返回 `error`（Go error / MCP `isError`），**不许**返回 `{"error": "..."}` 当正常结果——脚本的 `catch` 才抓得到 |
| C5 | 审批失败的错误串固定 | 用户拒绝 → 含 `rejected`；超时 → 含 `timed out`（`tools/nested_approval_test.go` 按子串断言） |
| C6 | 写类保序 + 幂等 | 同脚本内多次 `surface_install` 按调用顺序生效；同 `sha256` 重复安装返回**同一 id**，不产生第二份 |
| C7 | 顶层必须是对象 | 返回 `{tabs:[…]}`，不是 `[…]`——脚本侧 `s.value.tabs` 取值 |

补充两条「不要做」：

- **不要**实现 `SkipTruncateProvider`：引擎 20 KB 截断（`tools/engine.go:22 defaultMaxResponseSize`）是刻意设计，只给 codemode 自己用；
- **不要**把结果存进工具实例字段：同一 engine 会被主 agent 与子 agent 并发使用，会串味（`tools/exposure.go` 顶部注释记录了这次实测）。

### 2.2 四个工具的契约表

| 工具 | 参数（≤3） | 成功返回（对象） | 空态 | 失败 |
|---|---|---|---|---|
| `ui_inspect` | `what` (`layout`/`tabs`/`editors`/`surfaces`) | `{tabs:[{id,kind,title,dirty,active}], activeId, layout:{mode,bias,leftPinned}}` | `tabs: []` | 会话不存在 → `rejected`/`not_found` |
| `surface_list` | 无 | `{installed:[{id,version,enabled,sha256}], active:[...]}` | 两个数组都 `[]` | — |
| `surface_install` | `path` / `manifest` | `{id, sha256, version, created:true/false}` | — | 校验失败 reject；未批准 `rejected`；等审批超 `timed out`；重复安装 `created:false` |
| `surface_remove` | `id` | `{id, removed:true}` | — | 不存在 → `not_found`；未批准 `rejected` |

### 2.3 脚本侧断言模板

```js
// e2e/verify_codemode_surfaces.mjs 的核心三段
const r = await run_code(`
  const s = await tools.ui_inspect({ what: "tabs" })
  if (Array.isArray(s.value.tabs) === false) throw new Error("tabs 不是数组")
  return { n: s.value.tabs.length, active: s.value.activeId }
`)
// 1) 返回是对象、不是字符串
// 2) 空会话时 tabs 是 [] 而不是 null
// 3) tabs 里每项都有 id/kind/title
```

审批两段：

```js
try { await tools.surface_install({ path: "/tmp/evil" }) }
catch (e) { assert(/rejected|timed out/.test(String(e.message))) }
```

**为什么这两段必须有**：C4 一旦破，脚本会把失败当成功继续跑；C5 一旦破，模型无法区分「用户不同意」和「工具坏了」，会重复请求审批。

### 2.4 验收口径

工具类 PR 合入前，`e2e/verify_codemode_surfaces.mjs` 必须包含**该 PR 新增工具**的：

- [ ] 正常路径（断言对象与字段）
- [ ] 空态路径（`[]` 不是 `null`）
- [ ] 失败路径（`catch` 到错误串）
- [ ] （写类）审批拒绝 + 超时两条

---

## 3. PR 约束

> **工作树纪律（用户 2026-11-13 明确）**：**一轮 = 一个独立 worktree**。
> - 每轮开工：`git worktree add .go-code/worktrees/<轮次> -b feat/<轮次> main`（**从最新的 main 拉**，不叠在上一轮的 worktree 上）；
> - **只能从 main 拉，绝不能从一条还没合并的分支拉**：2026-11-13 我从 `feat/creator-mode-v0.1`（未合并、含待修的 R08d）拉了 `html-preview-fix`，合并时把 R08d 一起带进了 main，main 当场红了（officekit 那条 P0 测试）。后来靠 cherry-pick 修回来。
> - **`docs/` 在 `.gitignore` 第 18 行**（整个目录被忽略，只有历史文件是已跟踪的）：新写的文档必须 `git add -f` 才进得了版本库，否则**只存在于主工作区** —— 从 main 拉的 worktree 里看不到，子智能体会读不到规范。2026-11-13 PR-09 开工时踩到：worktree 里没有 `docs/creator-mode-v0.1/`。
> - 该轮所有 commit 落在这条分支上；
> - 轮次结束：评审 PASS → `git merge --no-ff feat/<轮次>` 进 main → **删除该 worktree**；
> - 下一轮重新从 main 拉新 worktree。
>
> 为什么：长期复用同一个 worktree 会与 main 越差越远，最后演变成「合并 395 个文件、2 处冲突」那种局面（2026-11-13 实际发生过一次）。main 是唯一同步点，worktree 是一次性的。

> **commit message 纪律（F59）**：正文里每个「删掉了 / 改成了 / 新增了」必须能在 `git show` 的 diff 里指到行。指不到＝虚假声明，按缺陷处理——声明与代码分叉时，测试往往也没覆盖到，于是没人红。

### 3.1 八条硬规则

| # | 规则 | 判据 | 违反信号 |
|---|---|---|---|
| R1 | **一个 PR 只做一次语义变更** | 能一句话说清「这个 PR 让什么变了」 | 描述里出现「顺便」 |
| R2 | **可独立 revert** | `git revert` 后能编译、能跑 | 「必须配合下一个 PR 才编译得过」 |
| R3 | **指定 PR 红灯先行** | PR-02 / 05b / 06 / 12 / 13 先提交失败测试 | 测试和实现同一个 commit |
| R4 | **不跨 PR 重命名** | 重命名单独成 PR，或与删除同 PR（PR-11） | 功能 PR 里改文件名 |
| R5 | **不带无关格式化** | diff 里非目标行改动 ≤ 5% | 整文件重排 |
| R6 | **依赖不倒挂** | 只依赖 02 §1 表中前置项 | 声称「先做 13 也能跑」 |
| R7 | **收尾回写三处** | 02 状态行、03 实现偏差、04 新发现 | 改完代码就想合 |
| R8 | **表外 PR 先裁决** | 24 个之外的 PR 必须有 04 条目 | 临时插入重构 PR |

### 3.2 规模上限 [SHOULD]

| 度量 | 上限 | 超了怎么办 |
|---|---|---|
| 非测试 diff | 600 行 | 按「kind」拆（一个 provider 一个 PR）、按「通道」拆（Go 先/UI 后）、按「先加后删」拆 |
| 改动文件 | 15 个 | 同上；跨 3 个目录以上通常说明 PR 不纯 |
| 新增公共类型 | 10 个 | 契约先落 PR-00，别在迁移 PR 里现造 |

> 600 行是**经验值**，前 3 个 PR 实测后按 §0.3 L2 调整（见 OP-3）。

### 3.3 并行窗口

可以并行（不同文件）：

```
05a0（internal/atomicfile）
   ├─ 03 / 04（git/file provider）
   └─ 05a（bridge 通道）
09 / 10（标签条与健壮性）  ∥  06-08（web/activity/预览）
```

**禁止**并行：PR-02 与任何改 `useAppStore.ts` 的 PR；PR-11 与任何改 `TAB_KINDS` 的 PR。

### 3.4 验证门槛（每个 PR 至少跑）

| 改动类型 | 必跑 |
|---|---|
| 纯 TS/前端 | `npx tsc --noEmit && npm run lint && npm run test:<本 PR 新增>` |
| 碰 Go | `gofmt -l .` + `go vet ./...` + `go test ./desktop/bridge/...`（或 `scripts/check.sh --skip-frontend`） |
| 碰渲染 | 上两行 + `npm run build && node e2e/verify_<topic>.mjs` |
| 工具类 | 上面 + `node e2e/verify_codemode_surfaces.mjs` |
| 收尾（PR-15） | `scripts/check.sh` 全绿 + `npm run check` |

### 3.5 STOP 条件（遇到就停，先裁决）

1. 需要新增依赖 → §1.1 四步；
2. 需要改 ADR-0（会话隔离）→ 不是本方案的范围；
3. 需要改内置 kind 数量或 `file{mode}` 语义 → 01 §11；
4. 需要让插件拿到比 `file`/`web` 更多的能力 → D-E；
5. 需要跨 PR 重命名 → R4。

停下不是失败：写一条 04 裁决 + 一个 TODO，比硬做进去便宜得多。

---

## 4. DoD 检查表（贴进 PR 描述，逐项打勾）

### 4.1 通用（每个 PR）

- [ ] 一句话说清语义变更；`git revert` 后仍能构建
- [ ] `npx tsc --noEmit` 与 `npm run lint` 无新增告警
- [ ] 新测试已进 `npm test` 链条
- [ ] 无新增 `any` / `@ts-ignore` / `console.log` 残留
- [ ] 无新依赖（或已按 §1.1 走完四步）
- [ ] 回滚开关可用（若本 PR 影响现有用户）
- [ ] 02 状态行、03 偏差、04 新发现已回写
- [ ] 中文提交信息 + `scope` 小写

### 4.2 工具类（+ §2）

- [ ] `OutputSchema()` 非 nil，Schema 覆盖所有字段
- [ ] `StructuredSinkFrom(ctx)` 判了 nil，且包在 `IsScriptCall` 里
- [ ] 失败返回 error，不是 `{error}` 结果
- [ ] 空态是 `[]`，顶层是对象
- [ ] 写类：保序 + 幂等（重复安装同 id）
- [ ] 审批：`rejected` / `timed out` 两条 e2e 断言
- [ ] 描述 ≤ 120 字、参数 ≤ 3、总数仍 ≤ 4

### 4.3 UI 类

- [ ] 空态有引导（不是空白面板）
- [ ] 错误边界包住（Boundary）
- [ ] 键盘可达：`Tab`/`Esc`/`⌘W` 行为与现有标签一致
- [ ] 焦点不丢：切标签/切会话后回到原元素
- [ ] 暗色/亮色都过一遍截图

### 4.4 迁移类（provider 迁移）

- [ ] **用探针证明「零变化」时，先证明探针走的是被测那条路**（F45：缺 `import surfaces/builtin` 时 `getSurface()` 全 undefined，9 张截图证明的是兜底分支；判据可以是「故意改坏 provider，像素必须变」）

- [ ] 行为等价证明：迁移前录一组操作 → 迁移后重放，结果一致
- [ ] 旧代码同 PR 删除（不是留着等下个 PR）
- [ ] 墓碑：崩溃/卸载后面板显示占位而非白屏
- [ ] 回滚开关能退回旧实现（PR-11 之前）

### 4.5 Go 通道类

- [ ] 错误带码，前端能分支处理
- [ ] 超时/取消：`exec.CommandContext` + 进程组 kill
- [ ] 写文件走 `internal/atomicfile`
- [ ] 大文件不整读进内存（流式或 `io.Copy`）
- [ ] 日志字段符合 §1.8，无内容泄漏
- [ ] **在 `git worktree add` 出的干净检出里跑过 `go vet` + 定向 `go test`**（本地工作区有未跟踪的 `_test.go`，本地绿≠干净检出绿；F37）

---

### 4.6 红的归因（硬规则）

- 「环境相关」「基线红」「本机才红」**不是合法归因**。任何红必须在干净检出里定位到具体机制。
- 能修就修；修不了就加**显式环境门**（`t.Skip` 带理由 + 环境变量），让默认链里是「跳过」而不是「失败」。
- 跳过会出现在输出里，失败会被当成噪音忽略 —— 这是两者的本质区别（F40：6 条成本失败被当「环境相关」忽略了三轮）。
- **同一测试「全量红、单跑绿」＝顺序依赖**，必须修到顺序无关，并用 `-count=3` 证明。「单跑是绿的」不构成结案理由（F54：`TestReducerLLMEndDedupAndUsage` 5 次全量红 2 次，价表泄漏漏在了另一个用例上）。

### 4.7 测试必须走生产入口（硬规则，F45/F46/F47/F52/F53 的共同形状）

这一周反复出现同一类假绿，五次形态不同、本质相同：**测试/探针验证的对象不是生产里跑的那个**。

| 事故 | 测试验的 | 生产跑的 |
|---|---|---|
| F45 | `getSurface()` 兜底分支 | provider（缺 `import 'surfaces/builtin'`） |
| F46 | 另一个 worktree 的 vite（端口被别人占着） | 被测服务器 |
| F47 | 固定容差掩盖动画残值 | `animationend` 之后才收敛的布局 |
| F52 | 测试替身自己 `kill(-pgid)` | `sandbox.runExec` 的 kill |
| F53 | 测试里新建的 `Cache` 实例 | `Service.cache`（**两个 mu**，锁在生产里是空操作） |

**规则**：
1. 测试/探针必须从**真实入口**进入（`dispatch(cmd)`、`shutdownWorkspace`、DOM 点击），不许直接调内部函数「代表」它。
2. 需要替身时，替身必须与生产实现**同签名、同语义**，并在报告里写明「这一层没有被覆盖」。
3. 「我在测试里另建了一个等价物」＝这个测试不算证据。发现即打回。

---

## 5. PR 模板

```md
## 语义变更
（一句话：这个 PR 让什么变了）

## 改动
- 文件 → 做了什么

## 验收
- [ ] 命令与输出（贴关键行）
- [ ] 工具类：e2e CodeMode 四段（正常/空态/失败/审批）

## 回滚
（revert 后行为；开关名）

## 风险与偏离
（偏离了哪条 SHOULD，为什么；或「无」）

## 口径影响
（是否改了 PR 数/估时/决策/裁决；无则写「无」）
```

---

## 6. 命令速查

| 目的 | 命令 | 备注 |
|---|---|---|
| 全量 Go 门禁 | `scripts/check.sh` | gofmt / tidy / vet / test / build |
| 前端门禁 | `cd desktop/app && npm run check` | build:electron + tsc + lint + 全量 test |
| 单个纯函数测试 | `npm run test:<name>` | 走 esbuild + `node --test` |
| e2e | `npm run build && node e2e/verify_<topic>.mjs` | 独立端口，非 0 退出 |
| CodeMode 面板 | `node e2e/verify_codemode_surfaces.mjs` | PR-12 起必跑 |

---

## 7. 观察项（预期会改 / 待裁）

> 这一节存在的意义：**承认规范会变**。每条写清「改的信号」和「预定动作」，避免实施中反复讨论同一件事。

| # | 条款 | 现状 | 变更信号 | 预定动作 |
|---|---|---|---|---|
| ~~OP-1~~ | ~~编辑器依赖~~ | **已结案（F32）**：零依赖受控编辑器；CodeMirror 留 V1 | — | — |
| OP-2 | 工具是否常驻 | V0.1 常驻 4 个（600–900 token/请求） | PR-14 实测 token 占比 > 2% | 评估 `load_skill.onLoad` 解锁（`desktop/bridge/skill_capability.go:96` 已有钩子），需处理 loop 重建后的重放 |
| OP-3 | 600 行 diff 上限 | 经验值 | 前 3 个 PR 实测 | 按 §0.3 L2 调到实测中位数 ×1.5 |
| OP-4 | e2e 是否统一 runner | 39 个独立 `verify_*.mjs`（含截图/调试脚本共 57 个 `e2e/*.mjs`），各自起服务 | 新增 e2e 超过 5 个 | 引入 `e2e/run-all.mjs`（复用 playwright 实例）；不许改成 jest |
| OP-5 | 插件隔离 | 同源 `import()`，契约类型约束 | 出现第一个第三方面板 | 评估 preload + `contextBridge` 白名单；代价是 manifest 要声明 capability |
| OP-6 | kit 分发 | 可下载 + sha256（D-J） | 首个真实用户环境缺 LibreOffice | 退化为近似渲染 + 横幅（已有三档降级） |

---

## 8. 修订记录

| 版本 | 日期 | 变更 | 触发 |
|---|---|---|---|
| v0.12 | 2026-11-13 | §3 再补：`docs/` 是忽略目录，新文档必须 `git add -f`，否则 worktree 里没有 | 实际踩到：PR-09 开工时 worktree 无规范文档 |
| v0.11 | 2026-11-13 | §3 工作树纪律补一条：只能从 main 拉（从待修分支拉会把半成品一起合进 main） | 实际踩到：main 红了约 6 分钟 |
| v0.10 | 2026-11-13 | §3 增「工作树纪律」：一轮一个独立 worktree，从 main 拉、合回 main 即删 | 用户明确要求；此前长期复用同一 worktree 导致 395 文件合并 |
| v0.9 | 2026-11-13 | §3 增「commit message 纪律」：声明必须能在 diff 里指到行 | F59（声称删 FIFO 但没删，且零覆盖） |
| v0.8 | 2026-11-12 | §4.7 新规则：测试必须走生产入口（五次同形状假绿的归纳） | F45/F46/F47/F52/F53 |
| v0.7 | 2026-11-12 | §1.9 e2e 约定补三条：端口归属自检（比对自己探针页的 `<title>`）、退出码三档（2 = 前置不满足）、surface 探针必须自带 builtin import 自检 | R06d 把三个探针挂进 `npm test` 时暴露（F45/F46/F47） |
| v0.6 | 2026-11-12 | §4.6 红归因硬规则：禁止「环境相关」说法；失败要么修、要么显式 skip | F40 揭穿 6 条假基线红 |
| v0.5 | 2026-11-12 | R05a1 发现 68 个测试文件从未入库 → §4.5 增「干净检出才算门禁」；§4.1 同步 | 门禁假绿 |
| v0.1 | 2026-11-11 | 初版：§1 前置规范 11 条、§2 CodeMode 契约 7 条、§3 PR 规则 8 条、§4 DoD 5 类、§7 观察项 6 条 | 开工前对齐（用户要求先立规范、再开 PR） |
| v0.2 | 2026-11-11 | 新增 §1.12 性能（预算/基线/回归阈值/记录位置）；进度改由 `progress.html` 看板承载 | 用户要求「有一个模块注意性能」「进度用 HTML 管理」 |
| v0.3 | 2026-11-11 | §1.3 增加「联合类型禁用裸 Omit」；§1.2 增加内置 kind 只许 4 个 | R00 第一次评审 CHANGES（`Omit<SurfaceTab,'id'>` 收缩） |
| v0.4 | 2026-11-12 | §1.1 的 CodeMirror 冲突结案（F32：零依赖编辑器）；§7 OP-1 关闭 | R05b 开工前裁决 |
