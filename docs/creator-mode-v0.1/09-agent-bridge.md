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
| D11 | 旧只读命令 | `workspace_list` / `session_list` / `git_snapshot` / `ui_inspect` **全部禁用**（防碰 Project）；`surface_*` 命令/工具全禁（含只读 `surface_list`） |

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
- 插件工具进入任何项目会话或其它插件会话（D10）。
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
- 存储：`~/.go-code/plugin-sessions/<id>/{home,sessions,events}`（0700）。`home` 是 bash/file 工具的根；卸载不删。会话 record（`record.json` + block sidecar）也落在 `sessions/plugin-<id>/` 下——与 `delete_session` 删的 `storeDir` 同源，避免「删了 jsonl、留下 record，下次打开会话复活」。
- 工具面：照 `runtimeHubAgent` 装配**全局层**；不叠项目层；再叠加该插件自己的工具（§7）。
- 模型：普通会话语义（D3）。

### 4.2 生命周期

- 插件安装并启用 → `plugin_projects_list` 返回虚拟 Project（无需用户操作）。
- 会话创建：`agent.run` 不带 `session_id` → 新建随机 id 会话（D4）；带 → 复用。插件把 id 存自己的 localStorage。
- 插件停用/卸载：关闭 runtime（停 run、关 MCP/插件连接），但**会话目录不删**；`list` / 删除仍可用，打开时按需重新装配；`ask` 在 dispatch 前置拒绝 `denied`，`data.reason` 为 `plugin_disabled` / `plugin_uninstalled`。
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
| `agent.run` | `{ run_id?, session_id?, input, instructions? }` | `{ok, run_id, session_id}` | 立即回；流式走事件（`schema`/`tools` 参数未实现，见下） |
| `agent.cancel` | `{ run_id }` | `{ok}` | 取消 |
| `tool.result` | `{ call_id, ok, result?, error? }` | `{ok}` | 回工具调用 |
| `composer.quote` | `{ text, source? }` | `{ok, reason?}` | 只插草稿 |
| `ui.open_tab` | `{ kind, params? }` | `{ok}` | 只能开自己的 kind |
| `ui.toast` | `{ message }` | `{ok}` | 一行提示 |

未在 manifest `commands[]` 声明 → `not_declared`；声明了但不在宿主表 → `denied`。

**旧只读命令（`workspace_list` / `session_list` / `git_snapshot` / `ui_inspect`）已移除**（D11）：它们能读到真实项目/会话元数据，与「插件不碰 Project」冲突。manifest 里声明这些名字 → 安装时按未知命令拒绝（给出明确 reason，安装页标「需要更新」），运行时即使绕过也 `denied` 并审计，不做静默降级。P1 同步更新模板与 SKILL。

### 5.2 `onEvent` 清单

| 事件 | data | 说明 |
|---|---|---|
| `agent.delta` | `{text}` | 文本增量（50ms 合并） |
| `agent.tool` | `{name, status, ok?}` | 只给工具名与状态，不给参数/结果 |
| `agent.usage` | `{input_tokens, output_tokens}` | 结束时一次 |
| `agent.done` | `{text, json?}` | 终态；`json` 仅当 schema 通过 |
| `agent.error` | `{reason, message?}` | 终态 |
| `tool.call` | `{call_id, tool, args, deadline_ms, read_only, requires_panel}` | Agent 调插件工具；用 `tool.result` 回；`requires_panel:true` 且当前 scope 无该 kind 标签时宿主直接回 `panel_not_open`，不投给插件 |
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
- 工具模型侧名：`plugin__<id>__<name>`；同一插件内 `name` 重名 → 安装拒绝；跨插件/内置/MCP 因前缀天然隔离（安装期不扫描全局工具表）。
- 安装期硬校验：最多 16 条；`name` 匹配 `^[a-z][a-z0-9_]{0,40}$` 且不许含 `__`（模型侧名分隔符）；描述非空 ≤300 字符；`input` 必须是 `type:"object"` 的 JSON object（≤16 KiB，不收 `$ref`）；`timeout_ms` 缺省 10s、非零限 `[100ms, 60s]`；整名 `plugin__<id>__<name>` ≤64 字节（OpenAI function name 硬上限，超长是整轮 400，只能安装期拦）。有工具却没收 `tool.call` / 没声明 `tool.result` 也直接 reject（否则装上后每次调用都只能等超时）。

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
- `requires_panel:true` 且面板未开 → `panel_not_open`（不偷偷挂隐藏实例）。判据是**当前作用域**（`tabScopeKey`）的标签集里有该 kind，任意入口都算开；别的会话/工作区里同步的隐藏副本不算。门禁在 renderer 的 loader，插件回调不会被调用。
- 审批：`read_only:false` 实现 `MandatoryApproval`，先于 `events.ApprovalPolicy` 判定并剥掉 `modeApprover` 直等原始审批器——Auto/Full Access 不能同步放行；没有审批器时 fail-closed。卡片以模型侧名 `plugin__<id>__<name>` + 描述展示（名字已带插件 id；友好名映射留 P3），插件不能代点。

### 6.5 错误码

`not_declared` / `denied` / `quota` / `no_session` / `panel_not_open` / `plugin_unloaded` / `timeout` / `invalid_schema` / `cancelled` / `bridge_error`。

语义与处置见 §8.4。

### 6.6 Wire contract（P1b2 / P1c 共用，先钉死再各写一端）

**渲染层 → main（claimed API）**

```ts
surfaceAgentToken(id: string): Promise<{ ok: boolean; token?: string; reason?: string }>
surfaceAgent(p: { token: string; name: string; args?: unknown }): Promise<SurfaceCommandReply>
onSurfaceAgentEvent(cb: (p: { surface_id: string; event: SurfaceAgentEvent }) => void): () => void
```

- main 维护 `surface_id → token`；token 由 `crypto.randomUUID()` 生成，插件加载时领取。
- `surface:agent` 校验 token，注入 `surface_id`，再向 bridge 发 `{type:"surface_agent", payload:{id,name,args}}`。
- `surface_command` 仍存在，但 P1c 起宿主表为空：四条旧只读命令一律 `denied`（D11）。

**bridge stdout 事件（统一信封）**

```json
{"event_type":"surface_agent_event","data":{
  "surface_id":"demo",
  "event":{"type":"agent.delta","seq":1,"run_id":"sr-1","session_id":"s-1","data":{"text":"你"}}
}}
```

- main 把 `data` 原样转发到渲染层 `surface:agent-event`；loader 按 `surface_id === item.id` 过滤。
- `seq` 每个 run 单调递增；终态（`agent.done` / `agent.error`）之后停投。
- `tool.call` 同信封，`data:{call_id, tool, args, deadline_ms}`（P2 才启用）。

**命令参数/回执**

| name | args | 立即回执 data |
|---|---|---|
| `agent.session` | `{title?}` | `{session_id}` |
| `agent.run` | `{session_id?, input, instructions?}` | `{run_id, session_id}` |
| `agent.cancel` | `{run_id}` | `{}` |
| `tool.result` | `{call_id, ok, result?, error?}` | `{}`（P2；未知/已超时 call_id 幂等忽略并审计） |

- `agent.run` 不带 `session_id` = 新建插件会话（D4）；带 = 必须属于同一 `surface_id`，否则 `denied`。
- `agent.run` 先回 `command_response`，再流事件；事件失败不改变已发出的 ack。
- P1/P2 都不实现 `agent.run` 的 `schema` / `tools` 参数；manifest 里出现也只忽略，避免半套 JSON 校验。P2 启用的是 manifest `tools[]`（见 §5.3/§7.3），不是单次 run 的临时工具。

**事件 data**

| type | data |
|---|---|
| `agent.delta` | `{text}`（Go 侧 50ms 合并） |
| `agent.tool` | `{name, status, ok?}`（只给名字/状态） |
| `agent.usage` | `{input_tokens, output_tokens}` |
| `agent.done` | `{text}` |
| `agent.error` | `{reason, message?}`；reason ∈ `cancelled` / `quota` / `no_session` / `invalid_input` / `bridge_error` |
| `tool.call` | `{call_id, tool, args, deadline_ms}`（P2） |

**拒绝**

- `surface_agent` 的 `command_response`：`{ok:false, code, data:{ok:false, reason}}`，code/reason 用 §6.5 的封闭集合。
- 未声明命令：`not_declared`（不是 `denied`）；D11 四条：`denied`。

---

## 7. 工具集

### 7.1 允许（插件会话）

内置 file / bash / edit / glob / grep / run_python / todo / ask_user / agent_spawn 系列 / codemode / office / artifact_read / **memory_dream**；全局 MCP；全局 subAgents；**仅在本插件会话内**，插件自己的工具。**所有这些只在插件自己的虚拟 Project 内可用**（D10）。

### 7.2 denylist（能操作宿主/其它会话/其它插件的一律不给）

| 工具 | 是什么 | 为什么剔除 |
|---|---|---|
| `surface_install` / `surface_remove` / `surface_list` | 面板插件安装/卸载/清单 | 插件能装卸插件 = 权限升级；prompt injection 可装恶意面板；**含只读 `surface_list` 也全禁**（D11） |
| `session_send` / `session_list` | Session Mesh：给其它会话/节点发消息 | 跨会话注入、内容外发 |
| `workspace_create` | 创建真实工作区 | 插件能批量建项目污染 `workspaces.json` |
| `session_create` / `assign_task` / `handoff` / `handoff_ack` / `group_*` / `task_*` / `agent_status` | Hub 多智能体/任务板 | 插件不该编排 Hub、改写任务状态 |
| `promote_task` / cron 类 | 后台持久/定时任务 | 用户不看着时持续消耗 token 与写盘。注：`promote_task` 目前是宿主命令而非工具，denylist 仍保留该名字，防将来工具化后静默放行 |
| `git_worktree` | 真实仓库操作 | 插件 home 不是仓库；用户明确不需要 |
| `workspace_list` / `session_list` / `git_snapshot` / `ui_inspect` | 旧只读命令（已从 command 表移除） | 能读真实项目/会话元数据，与「不碰 Project」冲突（D11） |

> `workspace_list` / `session_list` / `git_snapshot` / `ui_inspect` 既不是工具、也从 command 表移除（D11）——它们能读真实项目/会话元数据。

### 7.3 插件工具

- manifest `tools[]` 声明即注册；模型侧名 `plugin__<id>__<name>`。
- 同一插件内 `name` 重名 → 安装即拒绝；模型侧全名带插件 id，跨插件/内置/MCP 不会撞名。
- 调用走 `tool.call` / `tool.result`；插件侧无 register API。call_id 是宿主生成的 `tc-*`，同一 run 内单调；工具结果 ≤64 KiB JSON，超出截断并标 `truncated`。
- 工具只在**启用且已加载**的插件上注册；卸载/禁用/`closePluginRuntime` 会让等待中的 call 立即失败 `plugin_unloaded`，不把 run 挂到超时。
- 子 agent 继承主引擎 → 插件工具对同一插件会话内的子 agent 可用；子 agent 若被派到别的会话，不继承。
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
4. 设置页插件行：命令/事件开关、工具清单（只读，显示"仅本插件会话"）、会话数、审计入口。
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

### P1 落地记录（2026-11-15）

| 块 | merge commit | 验收 |
|---|---|---|
| L0 一次性交接 | `34161e9` | host-boundary 29 项；生产 dist 无 `__haiTestBridge` |
| P1a 虚拟 Project（Go） | `e2cf98b` | 插件 TestPlugin 23 项；Go 全包曾 exit 0 |
| P1b1 owner path / 左栏（renderer） | `27ffe2d` | owner-path、plugin-projects、store、session-tabs 全绿 |
| P1b2 host API / quote | `ad61092` | host keys 恰五键；surface-host 86；composer-quote 33；creator-mode 606 |
| P1c Agent 协议（Go） | `c4f34f2` | SurfaceAgent/SurfaceCommand 19+11 全 PASS；D11 四条 denied |
| P1d 会话刷新 / 真 Electron | `c5bae12` | 真 Electron `agent.session` 22 项；左栏 0→1；host keys/`window.desktop` 断言 |
| appearance 探针回归 | `c8b0b17` | 79 项；dist 无 `__haiTestBridge` |
| P1e-A/B 空会话 + 安装期 D11 | `c461821`（`ab6f874`） | 空会话重启恢复 3 项；四条旧命令安装期 rejected + index/审计；Go 全包 196s |
| P1e-C `agent.run` 真机 | `f411edd`（`3c9334d`） | 本地 SSE 全链路；`e2e:agent-run` 接入 creator-mode；启动抖动加固 |
| P1e 联合门禁修复 | `614cea9` | 真安装器 D11 rejected；空会话 jsonl 必须存在（435B） |

**已验证的端到端**：临时插件 `host.command('agent.session')` → token → preload/main → Go `surface_agent` → `plugin-sessions/<id>/sessions` → 左栏插件分组；`workspace_list` 即使声明也 `denied`（D11）。

**P1e 收口（2026-11-15）**：

- **空会话持久化（P1e-A）**：`agent.session` 在 `createIn` 成功后立刻 `Session.Persist`（独立 5s 超时，不用请求 ctx，避免回执后 ctx 结束写半行）；`agent.run` 不带 `session_id` 时也在回执前落盘。落盘失败回 `bridge_error` + 审计，并撤回内存会话，不把「看起来成功、重启就丢」的 id 交出去。真 Electron：建完会话即有 435B `<sid>.jsonl`；`closePluginRuntimes()` 后同一 `appDataDir` 重起，`listSessions` 仍含该 sid 并可恢复。
- **安装期 D11（P1e-B）**：`validateSurfaceSource` 在 entry/hash 之前拒绝 `workspace_list` / `session_list` / `git_snapshot` / `ui_inspect`，理由点名命令并指到 `agent.*`；不写 index、不拷目录、无暂存残留，审计有失败记录。`agent.*` / `composer.quote` / `ui.*` 不受影响；运行期 `surfaceHostCommands` 空表保持不变（老已装插件仍走运行期 deny）。
- **真 Electron `agent.run`（P1e-C）**：本地 `127.0.0.1` 假上游 SSE → Go `agent.run` → `agent.delta/usage/done` → jsonl（history + state）→ 左栏插件分组。断言 run/session 归属、seq 单调、delta 拼接 = done、usage 在 done 前、零 pageerror；`npm run e2e:agent-run` 已接进 `test:creator-mode`。
- **联合门禁修复（`614cea9`）**：真安装器拒绝旧命令的 e2e 断言（外部安装器 `-expect=rejected`）；空会话 jsonl 从「允许尚未落盘」改成「必须存在且非空」。
- **启动抖动加固**：两个真 Electron 探针预建 `userData`（防 `DevToolsActivePort` 首启 ENOENT），假上游配置不放明文 `api_key`（防启动期 safeStorage/钥匙串迁移阻塞 180s）。两条都是测试环境真踩过的，不是产品行为变更。

**P1e 联合验收（2026-11-15，main `614cea9`）**：`npm run test:creator-mode` **exit 0 / 0 ❌**（含 T2b 真安装器 rejected、空会话 435B jsonl、`agent.run` 22.1s）；`go test ./desktop/bridge -count=1` **ok 244.9s**；`go test ./session/...` ok；`tsc` 0；eslint 0 error / 57 warning。`go test ./session/remote` 在全包并发跑时红过一次 `target node offline`，单独复跑 13.3s/14.9s 均绿，按既有负载敏感用例记录。

> 注：`link-check:embed-v2` 指向仓库根 `scripts/`（`.gitignore` 忽略的本地脚本），本轮为跑完整链在本地补了适配器新增的 `afterResponse()`，未入库；干净检出需先准备该脚本。

**P1e 遗留项（含 hardening 处置）**：
1. 真 Provider 未跑：P1e-C 用本地 OpenAI 兼容 SSE 假上游，覆盖协议/事件/jsonl/左栏；P2b 已把 `tool_calls` 多轮、`tool.call/result`、审批卡、`requires_panel` 门禁接到同一条真 Electron 链上。P2 hardening 补了 `TestSurfaceAgentRealProviderSmoke`（`HAI_E2E_SURFACE_REAL=1` 门控，走真实鉴权/HTTP/SSE），但默认环境没有凭据，未实际执行；真实 Electron UI + 真 Provider 仍待有 key 的联调。`agent.cancel` 已由 T14（真 Electron + 假上游延迟响应）覆盖。
2. 落盘失败路径的空壳已清：`dropSurfaceAgentSession` 删会话 jsonl、`<sid>/` 子树、空 record 目录；`DeleteSessionRecord` 同时收走 `.blocks.idx` 孤儿。撤回仍会发 `session_persist_error` / `session_closed`（渲染层会为未知 sid 建不可见视图条目）——这是可观察线索，保留。
3. 同一 realm 残留（localStorage/全局篡改）仍是 V1 已知边界，V2 iframe/独立 WebContents 解决。

### P2 落地记录（2026-11-15）

| 块 | merge commit | 验收 |
|---|---|---|
| P2a manifest `tools[]` + 代理工具（Go） | `4a8f36e`（`6e4c531`/`bcd2bd0`/`6c13c4a`） | 安装期校验 17 子用例；`plugin__*` 注册矩阵 / 回环 / 超时 / 跨插件 / 取消；bridge 全包 180s 绿 |
| P2c 强制审批（Go） | `38d1d59`（`9d64209`/`7969144`） | `read_only:false` 实现 `MandatoryApproval`；Auto/Full Access 不能静默放行；无审批器 fail-closed；`./tools` 全绿 |
| P2b 门禁 + 真 Electron e2e（renderer） | `2c72b96` + `ac688cd`（`3d435bb`） | `requires_panel` 当前-scope 判据；T11/T12/T13 真机绿；`test:creator-mode` exit 0 / 0 ❌ |

**已验证的端到端**（`e2e:agent-run`，约 11s）：

- **T11**：模型发 `tool_calls` → Go `plugin__*` → `tool.call` 到面板 → 面板 `tool.result` → 第二次上游请求带 tool 消息 → 终态 `done`。断言 call_id `tc-*`、args 原样、deadline/read_only/requires_panel 字段、恰好两轮请求。
- **T12**：关闭插件标签后，`requires_panel:true` 的工具调用由宿主直接回 `panel_not_open`，模型看到该错误、面板回调从未收到 `tool.call`。修复点：`tabsByScope` 跨 scope 会同步隐藏副本，判据必须是当前 scope 的标签集。
- **T13**：`read_only:false` 工具在 Auto 模式下也必须弹审批卡；批准前无第二轮请求、面板无回调；批准后才投递并回结果。审批卡标题显示「运行探针 · write_note」，不是 `plugin__...`。
- **T14**：假上游把响应按住 4s，插件 `agent.cancel` 回执 ok 后，终态是 `agent.error{cancelled}` 且**没有** `agent.done`，取消在延迟响应到点前生效。
- **真机耗时**：T11–T14 合计约 10s；整条 creator-mode 链 exit 0。

**P2 仍欠**：
1. `agent.run` 的 `schema` / `tools` 参数仍未实现（P1/P2 都只支持 manifest 静态工具）；模型输出 JSON Schema 校验仍无。
2. 工具 `input` 只做安装期形状校验（object / 上限 / 无 `$ref`），不做完整 JSON Schema 语义校验（required/type/enum）；参数不合 schema 时由插件自己回 `invalid_input`，宿主不拦。
3. `requires_panel` 粒度为「当前 scope 有该 kind 标签」，不区分 entry；多入口插件只要还有一个入口开着的标签就算在场。
4. 真 Provider 的 Electron UI 联调仍未跑（Go 门控 smoke 已有，但默认无凭据）；插件工具进子 agent 的继承尚未上真机覆盖。
5. `agent.cancel` 已覆盖「请求在跑」的取消；真实 Provider 的流式取消/服务端计费截断仍待联调。

### P2 hardening（2026-11-15，`a28cff8`）

| 项 | 实现 | 验收 |
|---|---|---|
| `agent.cancel` 真机 | Electron T14：假上游延迟 4s，面板取消按钮 → `agent.error{cancelled}` | 与 `agent.done` 互斥；取消早于延迟响应到点；回执 ok |
| 空壳清理 | `dropSurfaceAgentSession` 删 jsonl、`<sid>/agents`、空 record 目录；`removeBlockStore` 连带 `.blocks.idx` | `TestSurfaceAgentRunPersistFailureIsBridgeError` 断言磁盘无残留；`TestSidecarRemovedWithRecord` 断言索引删除 |
| 审批卡展示 | `pluginToolLabel`：`plugin__<id>__<name>` → `<插件名> · <工具名>`；协议 Name 不变 | T13 真实审批卡文本包含「运行探针 · write_note」 |
| 真 Provider | `TestSurfaceAgentRealProviderSmoke` 走 `agent.session` → 真实 HTTP/SSE → done/usage/jsonl | 默认 skip；`HAI_E2E_SURFACE_REAL=1` + provider/model/key 时执行 |

**真 Provider 运行方式**：

```bash
# 不要把 key 写进命令行、文档、commit 或 shell history。
# 先在当前终端用静默输入放进环境变量（read -s 不回显，也不会进 history）：
printf 'Provider API key: '; read -rs HAI_E2E_SURFACE_API_KEY; printf '\n'
export HAI_E2E_SURFACE_API_KEY

export HAI_E2E_SURFACE_REAL=1
export HAI_E2E_SURFACE_PROVIDER=opencode2
export HAI_E2E_SURFACE_MODEL=deepseek-v4.1-flash
go test ./desktop/bridge -run TestSurfaceAgentRealProviderSmoke -count=1 -v
unset HAI_E2E_SURFACE_API_KEY
```

> 文档只写变量名，**不写真实 key**；测试、审计、日志也不打印 key。真实 key 只应从密码管理器或上面的静默输入进入当前 shell；不要提交到仓库，不要写进 `settings.json` 或命令历史。key 只在测试进程内存里（临时 HOME 不写明文）；没有凭据时默认 skip，不让日常 `go test` 变慢或红。

**Hardening 联合验收（2026-11-15，`a28cff8`）**：`go test ./desktop/bridge` ok 172.8s；`go test ./session` ok 12.6s；`npm run test:creator-mode` exit 0 / 0 ❌；`e2e:agent-run` 连跑两次 10.7s / 10.6s；`tsc --noEmit` 0。真 Provider smoke 默认 skip（本机无凭据），需按上面的环境变量单独执行。

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

> **P2 已同步**：SKILL 新增 “Exposing tools to the agent” 一节（manifest/tool.result 对齐、`read_only`、`requires_panel`、超时/截断、D10）；`templates/surface/surface.json` 带一个只读 `template_echo` 样本与 `tool.call` 处理，`e2e:plugin-loader` 仍全绿。

---

## 12. 风险与后续

| 风险 | 现状 | 处置 |
|---|---|---|
| 同 realm 残留（localStorage / 全局篡改） | 已知 | V2 iframe/独立 WebContents；V1 明说不构成安全边界 |
| dev HMR 二次 claim | 页面 reload 会重跑 preload；模块热替换极端场景可能拿不到 | dev 用注入 fake；生产以门禁为准 |
| 插件工具误入项目会话 | 设计上不存在（D10） | 装配点只对 `runtimePlugin` 注册；门禁断言项目会话看不到 `plugin__*` |
| 旧插件使用已移除的只读命令 | 会 `not_declared` / 安装拒绝 | P1 更新模板与 SKILL；安装回执给出明确 reason |
| `surface_list` 是否放行 | 禁（D11） | 不开口子；需要清单走宿主 UI |
| 工具 denylist 随版本漂移 | 集中 denylist + `SetToolFilter`（装配后动态注册）+ `HideTool`（模型不可见、调用即拒）双防线 | MCP/插件工具名是动态的，纯 allowlist 会误伤；P2 若改 allowlist，先解决 `mcp__*` 前缀白名单 |
| 空插件会话重启丢失 | **已修（P1e-A）** | 建完即 `Persist`；真 Electron 435B jsonl；重启恢复测试；失败路径空 record 子树的磁盘清理仍未做 |
| 安装期旧命令未拒绝 | **已修（P1e-B）** | `validateSurfaceSource` 声明即 rejected（不写 index/不拷目录）；老已装插件仍走运行期 deny |
| 真 Electron 首启竞态 | 已加固（测试侧） | 预建 `userData` 防 DevToolsActivePort ENOENT；假上游不放明文 key 防 safeStorage 阻塞 |
| 真 Provider 未跑 | 门控 Go smoke 已有，Electron UI 未跑 | `HAI_E2E_SURFACE_REAL=1` 时走真实鉴权/HTTP/SSE；默认无凭据 skip，真实 Electron + Provider 仍待联调 |
| 工具参数 schema 只判形状 | 已知（V1） | 安装期只保证 provider 不 400；required/type/enum 由插件自检并回 `invalid_input`，P3 再评估完整校验 |
| `requires_panel` 粒度为 kind | 已知（多入口） | 当前 scope 有任一该 kind 标签即算开；需要按 entry 判定时再加 `requires_entry`，V1 不扩 |
| 落盘失败空壳 | **已修（hardening）** | jsonl/`<sid>/agents`/空 record 目录一并清；sidecar 索引随 record 删除 |
| 审批卡插件工具名 | **已修（hardening）** | `plugin__*` 显示「插件名 · 工具名」；协议字段不变 |

---

## 13. 评审 blocker 处置记录（2026-11-14）

1. **命令拿不到虚拟路径** → §4.5 `sessionOwner[sid]` + `send()` 优先取。
2. **runtime 分支不全** → §4.3 七点分流表。
3. **缺负向守卫** → §4.4 四道硬拒绝；虚拟路径永不进 `workspaces.json` 为后端不变式。
4. **D3 语义** → 与普通会话一致（切模型更新全局 active）。
5. **工具越权** → §7.2 denylist；Hub/mesh/surface（含 surface_list）/cron/git_worktree/四条旧只读命令全部剔除，`memory_dream` 保留；插件工具只在本插件会话（D10）。
6. **事件/身份** → §6.1–6.3 独立通道、token 派生、payload 不带 plugin_id。
7. **API 收敛** → 只有 `command` + `onEvent`（D5）；具名 API 全部取消。
