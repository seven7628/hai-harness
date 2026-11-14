# 09 · 插件 ↔ Agent 双向互通（技术方案 v2）

> 状态：设计定稿（2026-11-14 重写）
> 适用：Creator Mode V0.1 之后的第一块平台能力
> 本文替代此前所有 patch 版本；冲突以本文为准。

---

## 0. 决策摘要

| # | 决策 | 内容 |
|---|---|---|
| D1 | 卸载后的插件会话 | **保留**：左栏「已卸载插件」分组，只读、可手动删 |
| D2 | 会话归属 | **虚拟 Project**：每插件一个，不绑真实项目，不进 `workspace_list` |
| D3 | 模型 | 与普通会话一致：建会话继承全局 active；用户可切 Provider/Model（现有 `switch_model` 会更新全局 active）；插件不能指定 |
| D4 | 会话 ID | **随机**；`agent.run` 不传 `session_id` = 新建，传了 = 复用；插件自己记 id |
| D5 | 插件对外 API | **只有 `command` + `onEvent` 两个函数**；`React` / `t` / `theme()` 作为无权限本地工具 |
| D6 | `window.desktop` | **插件世界里不存在**：preload 只暴露一次性交接函数，宿主启动时领走并存入模块私有变量 |
| D7 | 引用 | 只插草稿，绝不自动发送 |
| D8 | 工具面 | 内置 + 全局 MCP + 全局 subAgents；**denylist** 见 §7；允许 `memory_dream`；插件只能**新增**自己的工具，不能改既有集合 |
| D9 | Agent 事件 | 独立 IPC 通道，按插件/run 过滤；不复用全局 `onEvent` |
| D10 | 插件工具范围 | **只在它自己的虚拟 Project/会话内生效**；不注册到任何真实 Project 会话，也不跨插件 |
| D11 | 旧只读命令 | `workspace_list` / `session_list` / `git_snapshot` / `ui_inspect` **全部禁用**（防碰 Project）；surface 相关命令/工具全禁 |

---

## 1. 目标与非目标

### 目标
1. 插件面板能发起 Agent 任务（翻译、总结、查询），流式拿结果并渲染回自己的面板。
2. 插件能把选中文本放进当前会话草稿（用户手动发送）。
3. 插件能向 Agent 注册工具，让 Agent 在**该插件自己的虚拟 Project/会话**里调用它（D10：不进真实 Project 会话）。
4. 插件会话出现在左栏，用户可查看、停止、切换模型，与普通会话同构。
5. 上述能力都建立在「插件只能做声明过的事」之上，而不是「装插件 = 交出整个应用」。

### 非目标（V1）
- 插件读用户其它会话正文；订阅全局事件流。
- 插件代点审批、自动发送引用、自动重试扣额度。
- 插件指定 provider / model / API key。
- 插件装卸其它插件、操作真实 git 仓库、向其它会话发消息。
- 真正的进程级沙箱（iframe / 独立 WebContents）——列为 V2 加固。

---

## 2. 现状（设计依据）

### 2.1 两种已有 Session

| 形态 | 运行时 | 存储 | 工具面 |
|---|---|---|---|
| Project + Session | `runtimeProject`，`m.workspaces[path]` | `~/.go-code/sessions/<workspaceKey>/` | 内置 + 项目/用户 MCP + 项目/全局技能与子代理 |
| 智能体 / 群组（Hub） | `runtimeHubAgent`，`m.hubRuntimes[agentID]`，**不进 workspaces** | `~/.go-code/hub/homes/<agentID>/{sessions,events}` | 全局层：内置 + 用户级 MCP + 全局技能/子代理；无项目上下文 |

关键代码：`desktop/bridge/main.go:1659`（`runtimeKind`）、`hub_session.go:62`（hubRuntime 装配）、`sessionRecordKey()`。

### 2.2 工具装配

`newSessionEngine()`（`main.go:5803`）在会话创建时把工具注册进 `tools.Engine`：内置（file/bash/todo/run_python/subagent）、MCP（`mcpm.Tools`）、business 工具（office/UI/surface）、mesh（开关门控）。`Engine.RegisterTool/HideTool` 支持热插拔，`HideTool` 粘性。

### 2.3 界面与桥

- renderer 只有**一个**全局 `state.workspace`；`send()` 把它注入每条命令（`useAppStore.ts:3974`、`4024`）。
- `switch_model` 按 `session_id` 解析 runtime，非 Hub 会话会更新全局 active（`main.go:3426`）。
- preload 暴露 `window.desktop` 91 个方法（`preload.ts:107`），含 `providerKey` / `fs*` / `command` / `onEvent`。
- 插件 `panel.js` 被 `import()` 进**同一页面**执行（`surfaces/plugin/loader.ts`）。

---

## 3. 总体架构

```
┌─────────────────────────────────────────────────────────────┐
│ Renderer（同一页面）                                          │
│                                                             │
│  宿主代码 ── import { bridge } from './lib/bridge'           │
│              （启动时一次性 claim，模块私有）                  │
│                                                             │
│  插件 A ── apply({ host: { command, onEvent }, React })      │
│  插件 B ── 同上；看不到 window.desktop                        │
│                                                             │
│  loader：token 绑定 / 命令白名单 / 事件按 run 过滤            │
└───────────────┬─────────────────────────────────────────────┘
                │ IPC（main 校验 token → 注入 plugin_id）
┌───────────────▼─────────────────────────────────────────────┐
│ Main（Electron 主进程）                                       │
│  - claim 交接对象；不向页面暴露完整 API                        │
│  - surface:agent 路由 / 事件转发 / 工具调用转发                │
└───────────────┬─────────────────────────────────────────────┘
                │ stdio
┌───────────────▼─────────────────────────────────────────────┐
│ Bridge（Go）                                                 │
│  - plugin runtime（虚拟 Project）                             │
│  - agent loop / 工具集 / 审批 / 配额 / 审计                    │
└─────────────────────────────────────────────────────────────┘
```

三条不变式：

1. **插件只拿到两个函数**（`command`/`onEvent`），拿不到承载它们的对象。
2. **插件世界没有 `window.desktop`**（不是隐藏，是不存在）。
3. **所有跨边界数据都有 schema 与归属**（plugin_id、run_id、call_id、session_id）。

---

## 4. 虚拟 Project（第三种会话容器）

### 4.1 定义

```
插件 = 一个虚拟 Project
{ virtual: true, plugin_id, name, icon,
  path: ~/.go-code/plugin-sessions/<id>/home,
  sessions: [ 普通 Session ] }
```

- Session 仍是普通 Session：`new_session` / `list` / `ask` / `interrupt` / `approve` / `switch_model` **签名不变**。
- 运行时：`runtimePlugin`（`runtimeKind` 第三值），装配在 `m.pluginRuntimes[path]`，**不进 `m.workspaces`**。
- 存储：`~/.go-code/plugin-sessions/<id>/{home,sessions,events}`（0700）。`home` 是 bash/file 工具的根；卸载不删。
- 工具面：照 `runtimeHubAgent` 装配**全局层**；不叠项目层；再叠加该插件自己的工具（§7）。
- 模型：普通会话语义（D3）。

### 4.2 生命周期

- 插件安装并启用 → `plugin_projects_list` 返回虚拟 Project（无需用户操作）。
- 会话创建：`agent.run` 不带 `session_id` → 新建随机 id 会话（D4）；带 → 复用。插件把 id 存自己的 localStorage。
- 插件停用：虚拟 Project 仍显示、会话可读；`ask` 在 dispatch 前置拒绝 `denied`。
- 插件卸载：停止 runtime、取消在跑的 run；会话保留为「已卸载插件」，只读、可手动删。
- 插件升级：runtime 重启；虚拟路径与会话不变。

### 4.3 runtime 分流表（hub 的安全性分散在 6 个 `kind ==` 判定里，不能只写"照 hub 装配"）

| 位置 | `runtimePlugin` 行为 | 漏掉的后果 |
|---|---|---|
| `createIn` hooks（`main.go:2314`） | `hooksOptionUser`（与 hub 同规） | 读插件 home 的 `.go-code/hooks.json` → 插件写 hooks、宿主执行命令 |
| `sessionRecordKey()` | `plugin-<id>` | 改 id/迁目录后历史会话丢失 |
| `newSessionEngine` sessions root（`main.go:5877`、`4708`、`codemode.go:232`） | 由 runtime 提供 `sessionsRoot` | journal/codemode 落错目录，删除删一半 |
| `deleteSession`（`main.go:4634`） | `resolveRuntime` + `storeDir/eventsDir` | 停不掉 run、删错目录、会话"删不掉" |
| `shutdownWorkspace/All`、`deleteWorkspace` | 遍历含插件 runtime | runtime 不优雅关闭 |
| mesh 注册（`main.go:2452`）、hub 队列泵（`1009`）、`autoEnablePlugins`（`2107`） | **跳过** | 插件会话进 mesh、被当项目泵队列 |
| `m.runtime(path)` 未命中插件表 | 若 `isPluginHomePath(path)` → 返回 nil，**绝不**走项目装配 | 造出项目 runtime、写进 `m.workspaces`、nil 解引用 |

### 4.4 负向守卫（后端不变式）

新增 `isPluginHomePath(p)`（前缀 `~/.go-code/plugin-sessions/`），在四个入口硬拒绝：

1. `m.runtime` / `createOpts`（不造项目 runtime）；
2. `workspaceSync` / `workspaceAdd`（`workspace_registry.sync` 不校验目录，必须显式拦）；
3. `hubWorkspaceOps.ListWorkspaces` / `knownWorkspace`；
4. `plugin_session_*` 之外任何按 workspace 解析的入口。

**「虚拟路径永不进 `workspaces.json`」是后端不变式，不是前端过滤。**

### 4.5 前端：owner path

renderer 只有一个 `state.workspace`，插件会话必须补一层归属：

- store 增加 `sessionOwner: Record<sid, ownerPath>`；来源：`plugin_projects_list` 水合、`new_session` 回执新增的 `workspace`、`list` 按 project 回填。
- `send()` 取值：`payload.workspace || sessionOwner[sid] || state.workspace`（`useAppStore.ts:3974`）；`pending` / prefs / 响应校验同源。
- `setActiveWorkspace` **不用于**虚拟 Project；打开插件会话走 `openSession(sid, {ownerPath})`，只切 `activeSessionId` 与 owner，不触发 git/skills/browser/resetView。
- 左栏「插件分组 → 新建会话」→ `new_session { workspace: 虚拟路径 }`。

### 4.6 前端过滤清单（11 处，评审实测）

1. `syncWorkspacesToBridge` / `persistProjects` 的 `LS_PROJECTS.paths`；
2. 启动水合 `for (p of projects) send({type:'list'})`（虚拟走 `plugin_projects_list`）；
3. workspace 选择器 / 最近项目 / 置顶；
4. `setActiveWorkspace` 的 `workspace_add`、skills、git、browser、`resetView`；
5. `loadWsGit` / git 历史 / 文件树刷新；
6. 左栏 per-project 菜单：pin、files、新会话、删除工作区；
7. `resolveActiveOwner(sid, ws)` 的 owner 注入（hooks 信任 / IM 路由 / 插件启停）；
8. 项目层 MCP/技能状态展示；
9. 手机端 `mobileAllowedCommands` 加 `plugin_projects_list`；`switch_model` 在手机上原本不支持；
10. hub `ListWorkspaces` / `knownWorkspace`；
11. `workspaceSync` / `workspaceAdd` 入口。

---

## 5. 插件 API：两个口子

```ts
// apply({ host, React }) 里
host.command(name: string, payload?: unknown): Promise<{ ok: boolean; [k: string]: unknown }>
host.onEvent(type: string, fn: (ev: SurfaceEvent) => void): () => void
```

- `React` / `t` / `theme()` 作为**无宿主权限的本地工具**直接给。
- `openTab` / `toast` / `quote` 全部收进 `command`（`ui.*` / `composer.quote`），不另开 API。
- 没有 `host.agent` / `host.tools` / `host.composer` 这些具名对象。

### 5.1 `command` 清单

| 命令 | 载荷 | 回执 | 说明 |
|---|---|---|---|
| `agent.session` | `{ title? }` | `{ok, session_id}` | 建插件会话（随机 id） |
| `agent.run` | `{ run_id?, session_id?, input, instructions?, schema?, tools }` | `{ok, run_id, session_id}` | 立即回；流式走事件 |
| `agent.cancel` | `{ run_id }` | `{ok}` | 取消 |
| `tool.result` | `{ call_id, ok, result?, error? }` | `{ok}` | 回工具调用 |
| `composer.quote` | `{ text, source? }` | `{ok, reason?}` | 只插草稿 |
| `ui.open_tab` | `{ kind, params? }` | `{ok}` | 只能开自己的 kind |
| `ui.toast` | `{ message }` | `{ok}` | 一行提示 |

未在 manifest `commands[]` 声明 → `not_declared`；声明了但不在宿主表 → `denied`。

**旧只读命令（`workspace_list` / `session_list` / `git_snapshot` / `ui_inspect`）已移除**（D11）：它们能读到真实项目/会话元数据，与「插件不碰 Project」冲突。manifest 里声明这些名字 → 安装时按未知命令拒绝（给出明确 reason），运行时即使绕过也 `denied` 并审计。P1 同步更新模板与 SKILL。

### 5.2 `onEvent` 清单

| 事件 | data | 说明 |
|---|---|---|
| `agent.delta` | `{text}` | 文本增量（50ms 合并） |
| `agent.tool` | `{name, status, ok?}` | 只给工具名与状态，不给参数/结果 |
| `agent.usage` | `{input_tokens, output_tokens}` | 结束时一次 |
| `agent.done` | `{text, json?}` | 终态；`json` 仅当 schema 通过 |
| `agent.error` | `{reason, message?}` | 终态 |
| `tool.call` | `{call_id, tool, args, deadline_ms}` | Agent 调插件工具；用 `tool.result` 回 |
| `surfaces.changed` | `{}` | 插件装卸/启停（安全的全量生命周期） |

信封：`{type, seq, run_id?, session_id?, data}`；`seq` 单调递增，终态后停投。

### 5.3 manifest

```json
{
  "commands": ["agent.run", "agent.session", "agent.cancel", "tool.result", "composer.quote", "ui.toast"],
  "data": ["agent.delta", "agent.tool", "agent.done", "agent.error", "tool.call", "surfaces.changed"],
  "tools": [
    {
      "name": "course_week",
      "description": "查询某周的课程安排",
      "input": {"type":"object","properties":{"week":{"type":"integer"}},"required":["week"]},
      "read_only": true,
      "requires_panel": false
    }
  ]
}
```

- 安装页自然语言展示：**可发起 AI 请求（消耗额度）/ 可插草稿 / 可提供 N 个工具 / 可接收 N 类事件**。
- 设置页可撤销；撤销后命令 `denied`、事件停投、工具 Hide。
- 工具模型侧名：`plugin__<id>__<name>`；与内置/MCP/subagent 重名 → 安装拒绝。

---

## 6. 协议

### 6.1 L0：一次性交接（D6）

```js
// preload.ts（唯一暴露给页面的东西）
let claimed = false
const api = { /* 91 个方法 + 事件订阅 + token 申请 */ }
contextBridge.exposeInMainWorld('__haiClaimHostApi', () => {
  if (claimed) return null
  claimed = true
  return api
})

// src/lib/bridge.ts（宿主内部唯一入口；任何插件加载之前）
const bridge = (window.__haiClaimHostApi?.() ?? null)
```

- 插件代码里 `window.desktop === undefined`；`__haiClaimHostApi()` 返回 null。
- 宿主内部 105 处 `window.desktop.xxx` 全部改为 `bridge.xxx`（模块私有，插件不可达）。
- 宿主代码不准把 `bridge` 挂回 `window`，也不准把它作为参数传给插件。
- 门禁：探针插件断言 `window.desktop === undefined`、`__haiClaimHostApi()` 为空、`host.command` 可用。

### 6.2 命令通道

```
host.command(name, payload)
  → loader：manifest.commands 白名单 + 形状校验
  → bridge.surfaceCommand({ token, name, payload })
  → main：校验 token ↔ plugin_id，注入 plugin_id
  → bridge：按 plugin_id 解析虚拟 Project / 会话 / run
  → 回执（恒为对象，不抛）
```

`composer.quote` / `ui.*` 在 renderer 内完成，不经过桥：

- `quote` → `uiBus.emit('composer_quote', payload)` → `Composer.tsx` 插入草稿（带来源 chip，≤8 KiB，不自动发送）。
- `ui.open_tab` → 只能 `kind` 属于本插件；`ui.toast` → 宿主 toast。

### 6.3 事件通道

```
bridge --surface_agent_event--> main --IPC surface:agent-event--> preload(claimed api) --> loader
loader：按 manifest.data + run 归属过滤 → host.onEvent(type, fn)
```

- 不复用全局 `onEvent`（它广播所有项目/Hub/IM 消息）。
- `agent.*` 只投递给发起该 run 的插件；`tool.call` 只投递给该工具所属插件；`surfaces.changed` 全量。
- 与 transcript 的关系：同一 run 的 `content_chunk` 是权威（进 record/checkpoint），`agent.delta` 是派生副本、不落盘；取消时一起停。

### 6.4 工具调用

```js
host.onEvent('tool.call', async (ev) => {
  const result = await handle(ev.data.tool, ev.data.args)
  await host.command('tool.result', { call_id: ev.data.call_id, ok: true, result })
})
```

- 默认超时 10s（manifest ≤60s）；超时 → `{ok:false, reason:'timeout'}`，不中断 run。
- 插件卸载/崩溃 → 等待中的 call 立即失败 `plugin_unloaded`。
- 结果 ≤64 KiB JSON，超出截断并标记 `truncated`。
- `requires_panel:true` 且面板未开 → `panel_not_open`（不偷偷挂隐藏实例）。
- 审批：`read_only:false` 走现有工具审批；卡片显示「插件名 · 工具名」，插件不能代点。

### 6.5 错误码

`not_declared` / `denied` / `quota` / `no_session` / `panel_not_open` / `plugin_unloaded` / `timeout` / `invalid_schema` / `cancelled` / `bridge_error`。

语义与处置见 §8.4。

---

## 7. 工具集

### 7.1 允许（插件会话）

内置 file / bash / edit / glob / grep / run_python / todo / ask_user / agent_spawn 系列 / codemode / office / artifact_read / **memory_dream**；全局 MCP；全局 subAgents；插件自己的工具。**所有这些只在插件自己的虚拟 Project 内可用**（D10）。

### 7.2 denylist（能操作宿主/其它会话/其它插件的一律不给）

| 工具 | 是什么 | 为什么剔除 |
|---|---|---|
| `surface_install` / `surface_remove` / `surface_list` | 面板插件安装/卸载/清单 | 插件能装卸插件 = 权限升级；prompt injection 可装恶意面板 |
| `session_send` / `session_list` | Session Mesh：给其它会话/节点发消息 | 跨会话注入、内容外发 |
| `workspace_create` | 创建真实工作区 | 插件能批量建项目污染 `workspaces.json` |
| `session_create` / `assign_task` / `handoff` / `handoff_ack` / `group_*` / `task_*` / `agent_status` | Hub 多智能体/任务板 | 插件不该编排 Hub、改写任务状态 |
| `promote_task` / cron 类 | 后台持久/定时任务 | 用户不看着时持续消耗 token 与写盘 |
| `git_worktree` | 真实仓库操作 | 插件 home 不是仓库；用户明确不需要 |
| `workspace_list` / `session_list` / `git_snapshot` / `ui_inspect` | 旧只读命令（已从 command 表移除） | 能读真实项目/会话元数据，与「不碰 Project」冲突（D11） |

> `workspace_list` / `session_list` / `git_snapshot` / `ui_inspect` 既不是工具、也从 command 表移除（D11）——它们能读真实项目/会话元数据。

### 7.3 插件工具

- manifest `tools[]` 声明即注册；模型侧名 `plugin__<id>__<name>`。
- 冲突（与任何已有工具同名）→ 安装即拒绝。
- 调用走 `tool.call` / `tool.result`；插件侧无 register API。
- 子 agent 继承主引擎 → 插件工具对子 agent 可用。
- **只在插件自己的虚拟 Project/会话内注册**（D10）：不进入任何真实 Project 会话，不跨插件；没有"授权到项目会话"这个开关。
- 插件工具不能修改工具集、注册 MCP、定义 subagent、读别的工具的 schema/结果。

---

## 8. 安全模型

### 8.1 边界（诚实表述）

L0 之后：插件拿不到 `window.desktop`、拿不到 token、拿不到其它插件/会话的事件；只能调声明过的命令、收自己的事件。

**但仍是同一 realm**：插件能读同源 `localStorage`（项目路径、会话标题、prefs），能改共享全局对象。所以：

- L0 是**降低误用与顺手作恶**，不是进程级沙箱；
- 真正的隔离（iframe / 独立 WebContents）列为 V2；届时插件 API 不变，只换承载。

### 8.2 身份

- main 在插件加载前签发 per-plugin token；loader 闭包持有；payload 不带 `plugin_id`。
- main 校验 token ↔ plugin_id 后注入；bridge 只信任 main 注入的值。
- `agent.run` 的 `session_id` 必须属于同一 plugin_id，否则 `denied`（防跨插件劫持）。

### 8.3 额度（V1 默认）

并发 run 1/插件；20 run/小时；input ≤32 KiB；instructions ≤8 KiB；schema ≤16 KiB；单 run ≤10 min；工具结果 ≤64 KiB；取消即停。超限 `quota`，不排队。

### 8.4 失败语义

| reason | 触发 | 插件应做什么 |
|---|---|---|
| `not_declared` | manifest 未声明 | 提示作者改 manifest |
| `denied` | 用户撤销授权 / 跨插件访问 | 提示去设置启用 |
| `quota` | 超频率/并发/大小 | 退避或提示 |
| `no_session` | quote 时没有活动会话 | 提示先开会话 |
| `panel_not_open` | requires_panel 工具被调但面板没开 | 提示打开面板 |
| `plugin_unloaded` | 工具执行中插件被卸载/崩溃 | 可重试一次 |
| `timeout` | 工具超时 | 缩小任务 |
| `invalid_schema` | 模型输出未过 schema | 展示原文或重试 |
| `cancelled` | 用户/插件取消 | 静默 |
| `bridge_error` | 桥内部错误 | 提示重试 |

### 8.5 审计

`surfaces/audit.jsonl` 追加：`{id, action:"agent_run"|"tool_call"|"quote", ts, ok, session?, run?, tool?, bytes?, reason?}`。写失败不影响主流程。

---

## 9. UI 变更

1. 左栏：插件分组（虚拟 Project），图标 + 「插件」徽标；空则隐藏；「新建会话」「删除会话」「停止」。
2. 已卸载插件分组：只读、可删。
3. 会话视图：插件徽标；Provider/Model 选择器与普通会话一致（D3）；空态说明。
4. 设置页插件行：命令/事件/工具授权开关、会话数、审计入口。
5. 安装页/回执：能力摘要（自然语言）。
6. Composer：引用 chip（可删），不自动发送。

---

## 10. 实施分阶段

### L0（1–2 轮）
- preload 一次性交接 + `src/lib/bridge.ts` + 105 处调用点迁移。
- 门禁探针：插件里 `window.desktop` 不存在、claim 不可二次领取、`host.command/onEvent` 可用。

### P1（5–6 轮）
- Go：`runtimePlugin` + 7 点分流 + 4 道负向守卫 + `plugin_projects_list` + `plugin_session_open/delete` + `agent.session/run/cancel` + 配额/审计。
- Renderer：`sessionOwner` + 11 处过滤 + 左栏插件分组 + loader 的 `command/onEvent` + `composer.quote`。
- 模板/SKILL：移除旧只读命令示例；manifest 校验拒绝已移除的命令名。
- 测试：会话归属、模型切换、卸载保留、跨插件拒绝、quota。

### P2（2–3 轮）
- manifest `tools[]` + 代理工具 + `tool.call/result` + 审批 + denylist 断言（工具只在插件自身会话注册，D10）。

### P3（可选，2 轮）
- 当前会话只读订阅；通用 channel；iframe 隔离预研。

**P1 完成**即可跑通 PDF 阅读器：引用草稿 + 翻译流式回面板。
**P2 完成**才有「在插件自己的会话里让 Agent 调插件工具（如 PDF 面板自己更新）」；真实 Project 会话永远不加载插件工具（D10）。

---

## 11. Skill 同步（`skills/hai-surface-dev/SKILL.md` 新增一节）

1. 两个口子：`command` / `onEvent`；一张「什么时候用哪个」表；**旧只读命令已移除**（D11）。
2. manifest 声明示例 + 安装页会展示什么。
3. `agent.run` recipe：新建/复用 `session_id`、schema、`agent.delta/done/error`、取消；id 存插件自己的 localStorage。
4. `tool.call` / `tool.result` recipe：manifest 对齐、`read_only`、`requires_panel`、超时、结果上限。
5. `composer.quote` recipe：来源字段、上限、不自动发送。
6. 错误码 + 额度 + denylist（明确插件拿不到哪些工具）。
7. 反例：不要试图读 `window.desktop`、不要假装宿主会持久化、不要改全局工具集。

---

## 12. 风险与后续

| 风险 | 现状 | 处置 |
|---|---|---|
| 同 realm 残留（localStorage / 全局篡改） | 已知 | V2 iframe/独立 WebContents；V1 明说不构成安全边界 |
| dev HMR 二次 claim | 页面 reload 会重跑 preload；模块热替换极端场景可能拿不到 | dev 用注入 fake；生产以门禁为准 |
| 插件工具误入项目会话 | 设计上不存在（D10） | 装配点只对 `runtimePlugin` 注册；门禁断言项目会话看不到 `plugin__*` |
| 旧插件使用已移除的只读命令 | 会 `not_declared` / 安装拒绝 | P1 更新模板与 SKILL；安装回执给出明确 reason |
| `surface_list` 是否放行 | 暂禁 | 若需要只读清单，单独评审 |
| 工具 denylist 随版本漂移 | 新工具默认拒绝（白名单式装配） | 装配时用 allowlist，新增工具需显式放行 |

---

## 13. 评审 blocker 处置记录（2026-11-14）

1. **命令拿不到虚拟路径** → §4.5 `sessionOwner[sid]` + `send()` 优先取。
2. **runtime 分支不全** → §4.3 七点分流表。
3. **缺负向守卫** → §4.4 四道硬拒绝；虚拟路径永不进 `workspaces.json` 为后端不变式。
4. **D3 语义** → 与普通会话一致（切模型更新全局 active）。
5. **工具越权** → §7.2 denylist；Hub/mesh/surface/cron/git_worktree 全部剔除，`memory_dream` 保留。
6. **事件/身份** → §6.1–6.3 独立通道、token 派生、payload 不带 plugin_id。
7. **API 收敛** → 只有 `command` + `onEvent`（D5）；具名 API 全部取消。
