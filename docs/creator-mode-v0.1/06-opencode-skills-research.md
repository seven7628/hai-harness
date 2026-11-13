# OpenCode v2 的"创造者"方案 —— 调研与并轨

> 日期：2026-11-11　状态：调研 + 决策（修订 DSH 式"创造模式"）
> 结论：**OpenCode 没有 creator 模式**。它把"能创造"拆成两件事——**稳定的插件 API** + **一个可被路由的 skill**，智能体在普通会话里就能扩展应用。
> 这与我们已有的 skills 框架天然契合；建议**取消 `CreatorMode`**，改为"常驻工具 + skill + 审批门"。

---

## 1. OpenCode v2 的机制

### 1.1 插件 = 一个包，四个部件

```ts
// .opencode/plugins/example/index.ts
import { Plugin } from "@opencode/plugin"
export default Plugin.define({
  id: "example",
  async setup(ctx) { await ctx.storage.set("loaded", true) },
})
```

`package.json` 的 `exports` 决定有哪些部件：

| 部件 | 运行位置 | 能做什么 |
|---|---|---|
| `.`（server） | OpenCode 服务端 | 加工具、hooks、集成、命令、agent |
| `./tui` | 终端 UI | 加 TUI 元素 |
| `./gui-experimental` | 桌面每个窗口 + 主进程 | 加 GUI 能力 |
| `./rpc` | 类型化契约 | server 实现、其它部件调用 |

**要点**：插件不是"一种模式"，而是**应用的正常装配方式**——和服务端、UI 同等的扩展面。

### 1.2 加载与治理（这部分设计很成熟）

- **发现**：`.opencode/plugins/`（项目）、`~/.config/opencode/plugins/`（全局）自动加载；也支持 npm 包、git、绝对路径、`file://`；
- **配置**：`opencode.json(c)` 的 `plugins` 数组，**多层配置按优先级叠加**（全局 < 项目 < 项目 `.opencode/`）；
- **启停**：`-` 前缀禁用单项、`*` 全选、`.*` 前缀匹配；`opencode.config.policy` 与 `opencode.provider.opencode` **不允许被仓库关掉**（策略插件有豁免权）；
- **管理**：`opencode plugin add/list/check/update/remove`，支持版本、标签、范围、git；
- **失败隔离**：一个插件坏掉不拖垮启动（否则用户会去删整个目录）。

### 1.3 Skills = 渐进披露的"知识包"

```md
---
name: Git Release
description: Prepare release notes, version bumps, and GitHub releases
---
## Workflow
1. Read `references/release-policy.md`.
2. …
```

- 启动时只把 **name + description** 放进上下文，agent 判断相关后用 `skill` 工具加载正文；
- 目录形式（`skills/<id>/SKILL.md` + `references/` + `scripts/`）是推荐形态，路径相对 `SKILL.md`；
- **发现路径**：`~/.config/opencode/skills`、`~/.claude/skills`、`~/.agents/skills`、`.opencode/skills`、`.claude/skills`、`.agents/skills`（兼容既有生态）；
- 支持 **HTTP catalog**（`index.json` + `version` 缓存失效）；
- 用户可以用 `@skill-id` 手动点名加载。

### 1.4 "创造"在 OpenCode 里是怎么发生的

没有开关。链路是：

```
用户：「给这个应用加个面板 / 写个插件」
  └ agent 用**普通文件工具**写一个插件包（package.json + 入口 + TUI/GUI 部件）
  └ 用一个 skill 提供的**约定与坑**（目录结构、Plugin.define、部件导出、本地加载路径）
  └ 用户或 agent 把它加进 opencode.json（或放进 .opencode/plugins/）
  └ 重启/重载后生效
```

所以"创造者模式"的真正含义是：**应用本身支持插件 + 有一份写插件的知识**。前者是架构，后者是 skill；**都不是模式**。

---

## 2. 三方对比

| 维度 | DSH（Creator 模式） | OpenCode v2（无模式） | 我们当前方案 |
|---|---|---|---|
| 工具可见性 | 只有 `cordis` preset 挂 `tool-cordis` + `plugin_manager` | 插件工具常驻（server 部件提供） | `CreatorMode.AllowTool` 才挂 |
| 路由信号 | 用户切模式 | skill 的 `description` | 模式 + skill |
| 提示词 | persona 加一句 | 不加，靠 skill 正文 | 加 `SystemHint` |
| 安装审批 | 每次要批准 | 加进配置文件即可（信任本地） | 每次要批准 |
| 热生效 | HMR | 重启 / 重载 | 前端 surface 可热装 |
| 用户心智 | "我要改造应用，先切模式" | "我就让它加个东西" | 同 DSH |
| 上下文成本 | 切模式后常驻 | 工具常驻（schema 常驻） | 切模式后常驻 |

**关键洞察**：是否"能创造"，取决于**基础框架**（插件 API、发现、加载、失败隔离），不取决于模式。模式只是**限制工具可见性**的一种手段——而它的代价是"用户必须知道有这个模式"。

---

## 3. 我们能抄什么、不能抄什么

**抄**

1. **无模式**：普通会话里就能扩展，用户不需要知道"模式"这个词；
2. **skill 负责路由**：`description` 写得让模型能判断"这是要改应用"，正文只在加载时进上下文；
3. **插件包多部件**：我们对应 Host 半侧（Go 工具/服务）+ Client 半侧（面板），已经规划了；
4. **治理面**：目录自动发现、配置数组、`-` 前缀禁用、CLI/设置页管理、失败隔离——我们已经有 `index.json` + 插件页，补齐"禁用/启用"即可；
5. **发现路径兼容**：我们已经读 `.agents/skills`，与 OpenCode/Claude 同源，白捡生态。

**不抄**

1. **JS 插件直接进服务端**：我们是 Go 内核，工具与服务的动态加载走进程边界（M2），不把 JS 跑进核心；
2. **"加进配置就生效"**：我们的插件能调 `window.desktop`，必须保留**安装审批**（这是与 OpenCode 的刻意差异）；
3. **无失败隔离**：我们已有 3 次崩溃自动禁用，保留。

---

## 4. 修订后的设计（决策 D-N）

### 4.1 取消 `CreatorMode`

- 删除 `agents/mode.go` 里的 `CreatorMode`（PR-14 不再改 mode）；
- `ui_inspect` / `surface_install` / `surface_list` / `surface_remove` **进入标准工具集**（与 `bash`/`edit_file` 同级），受 `settings.features.surfaceTools` 开关控制（默认开）；
- **审批不变**：`surface_install` / `surface_remove` 仍每次要批准（`RequiresApproval → true`）。

### 4.2 工具描述的"瘦身"纪律

常驻工具的代价是**每次请求都带 schema**。三条约束把它压到最小：

| 约束 | 具体 |
|---|---|
| 数量 | 最多 4 个（inspect / install / list / remove），不再加 |
| 描述 | 每个 ≤ 120 字，只写"什么时候用 + 一句话语义"，**不写教程**（教程在 skill 里） |
| 参数 | 最多 3 个字段，枚举不展开长列表 |

预估成本：4 个工具 ≈ 600–900 token/请求，可接受。

### 4.3 Skill 作为唯一路由信号

```md
---
name: hai-surface-dev
description: 当用户要求给桌面端右栏加面板、改界面、做看板/工具页，或调试已装 Surface 插件时使用。
requires:
  tools: [ui_inspect, surface_install]
---
（正文：六步交付纪律 + 禁止清单 + 验收话术）
```

- `requires.tools` 用我们已有的 **技能能力面过滤**（`skills/capabilities.go`）：没有 surface 工具的会话（例如 Explore 子 agent）不会看到这个 skill；
- 用户可在输入框 `@hai-surface-dev` 手动点名；
- 系统提示词只注入**一行描述**，正文按需加载。

### 4.4 备选优化：技能触发解锁（如果上下文成本在意）

我们已有 `load_skill` 的 `onLoad` 钩子（`desktop/bridge/skill_capability.go:96`）：**加载该 skill 时再把 surface 工具注册进本会话引擎**。

- 优点：常态零成本；缺点：工具 schema 中途出现 → 一次 prefix cache miss，且需要"会话级已加载技能"在 rebuild 后重放；
- 结论：**V0.1 先常驻**（简单、可预期），把技能触发解锁记为 V1 优化点，不阻塞。

### 4.5 与 DSH 方案的最终取舍

| 保留 DSH 的 | 放弃 DSH 的 |
|---|---|
| 只读运行时内省（`ui_inspect`） | "创造模式"这个模式 |
| 安装回执 + 审批 + 快照回滚 | persona 里加"你可以改造自己" |
| skill 里的六步交付纪律 | 模式级工具可见性 |
| 内容寻址缓存/限额（转换服务） | — |

一句话：**DSH 的"创造模式"内核是"内省 + 受管安装 + 知识包"，模式只是它的包装；我们保留内核，去掉包装。**

---

## 5. 对现有文档的改动

| 文档 | 改动 |
|---|---|
| `01-design.md` | §5.1 由 `CreatorMode` 改为"无模式：4 个常驻工具 + skill + 审批"；§0 摘要去掉"切模式"表述；§11 增 D-N |
| `02-pr-plan.md` | PR-14 改为"常驻工具 + skill + 模板 + 插件页"（不再改 `mode.go`）；估时 1.5d 不变 |
| `03-code-plan.md` | 删 `CreatorMode` 代码块，改为工具注册 + skill frontmatter；补 `settings.features.surfaceTools` |
| `04-review.md` | 新增 F26（模式 vs 技能）、F27（常驻工具的上下文成本与 schema 瘦身纪律） |
| `README.md` | 增 06 行 + D-N |

---

## 6. 风险

| 风险 | 缓解 |
|---|---|
| 常驻工具让普通会话"手贱装插件" | 安装/卸载逐次审批；安装目标必须是绝对目录；3 次崩溃自动禁用 |
| skill 描述不准 → 不触发 / 乱触发 | description 里写"用户要求给桌面端加面板"这类**用户语言**，不写内部术语 |
| 工具 schema 变化破坏 prefix cache | 工具集在会话内稳定（除非用户开关 feature），不在运行中增删 |
| 用户不知道这能力存在 | 空态能力面板 + 插件页"从对话创建"入口，把能力做成可见的 UI |
