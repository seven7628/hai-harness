# 09 · 插件 ↔ Agent 双向互通（方案与协议）

> 状态：设计定稿待开工（2026-11-14）
> 决策人：用户（三条已拍板，见 §0）
> 前置：R17a/R17b 已合并；`window.desktop` 收窄（L0）是 P1 的硬前置

## 0. 已拍板的决定（2026-11-14）

1. **插件任务会话必须进左栏会话树**：以**虚拟 Project** 的形态出现（每插件一个）——不绑真实项目、不是智能体/群组，Session 仍是普通 Session。
2. **引用到会话只插草稿**，发送永远由用户按；「自动发送」留到以后，V1 不做。
3. **`agent.run` 加载全部系统内置工具 + 全局 MCP + 全局 subAgents**；插件**不能修改**这三套集合；但**允许插件注册自定义工具**进来（只增不改）。
4. **卸载保留插件会话**（D1）：左栏归入「已卸载插件」，只读、可手动删。
5. **模型按会话独立、用户自由切换**（D3）：插件建会话时继承全局 active provider/model；用户在会话窗口用现有 `switch_model` 选择器切换，**只影响这一条会话**；插件不能指定 provider/model/密钥。
6. **会话 ID 随机、插件可新建**（D4）：不引入 `session_key` 映射；`agent.run` 不传 `session_id` = 新建随机会话，传了就复用；插件自己把 id 存在自己的 localStorage 里。

## 1. 现状与约束（设计必须落在这上面）

### 1.1 两种已有 Session 形态

| 形态 | 运行时 | 存储 | 工具面 |
|---|---|---|---|
| Project + Session | `runtimeProject`（`m.workspaces[path]`） | `~/.go-code/sessions/<workspaceKey>/` | 内置 + 项目/用户 MCP + 项目/全局技能与子代理 |
| 智能体 / 群组 Session（Hub） | `runtimeHubAgent`（`m.hubRuntimes[agentID]`，**刻意不进 workspaces**） | `~/.go-code/hub/homes/<agentID>/{sessions,events}` | 全局层：内置 + 用户级 MCP + 全局技能/子代理；无项目上下文 |

关键代码：`desktop/bridge/main.go:1659`（`runtimeKind`）、`hub_session.go:62`（`runtimeHubAgent` 装配模板）、`sessionRecordKey()`（存储维度键）。

### 1.2 工具装配点

`newSessionEngine()`（`desktop/bridge/main.go:5803`）在会话创建时把工具注册进 `tools.Engine`：

- 内置：`builtin.NewFileTools` + todo + run_python + subagent 工具；
- MCP：`mcpm.Tools(ctx)`（`main.go:5926`）；
- 插件（Browser Use）：`plugin.Registry`，与本题无关。

`Engine.RegisterTool / HideTool / ShowTool` 支持热插拔（`tools/engine.go:192`），`HideTool` 是粘性的（同名重注册不会自动复活）。**代理工具**（Go 侧工具、实际在渲染进程插件里执行）因此可以后注册、卸载时 Hide。

### 1.3 现有会话命令的约束

`new_session` 要求 `workspace` 非空（`main.go:2843`）；`ask` / `interrupt` / `approve` / `switch_model` 等会话命令都按 workspace 解析 runtime。

**关键事实（评审实测，推翻了"命令零改造"的初稿）**：renderer 只有一个全局 `state.workspace`，`send()` 会把它注入**每一条**命令（`useAppStore.ts:3974`、`4024`）。插件会话的归属路径与当前 `state.workspace` 不同，若不补「会话 → 归属路径」这一层，`switch_model` / `ask` / `approve` 在插件会话上会直接 `session_not_found`（`main.go:3389`）。因此 §2.4 引入 `sessionOwner[sid]`，`send()` 优先取它 —— **命令签名不变，但前端要加一个状态**。
## 2. 虚拟 Project：每个插件一个

### 2.1 定义

**插件 = 一个虚拟 Project**（不是第三种 Session，也不是真实工作区目录）：

```
{ virtual: true, plugin_id: "course-mvp", name: "儿童英语课程",
  path: "~/.go-code/plugin-sessions/<id>/home",  // 虚拟根，真实存在
  sessions: [ ... ] }                            // 普通 Session
```

- 它下面的 Session 就是**普通 Session**：`new_session` / `list` / `ask` / `interrupt` / `approve` / `switch_model` 签名全部沿用，`workspace` 传虚拟路径 —— **不加 `plugin_id` 二选一**；但前端要通过 `sessionOwner` 把这个路径注入命令（见 §2.4，评审指出的 blocker）。
- 运行时：新增 `runtimePlugin`（`runtimeKind` 第三值），装配在 `m.pluginRuntimes[path]`，**不进 `m.workspaces`**；`m.runtime(path)` 先查插件表再走项目表。这样 `workspace_list` / reload / MCP 重放 / cron / git 检测这些遍历 `m.workspaces` 的路径天然看不到它。
- 存储：`~/.go-code/plugin-sessions/<id>/{home,sessions,events}`（0700）。`home` 是 bash/file 工具的根，卸载不删（D1）。
- 工具面：照 `runtimeHubAgent` 装配**全局层**（内置 + 用户级 MCP + 全局技能/子代理），**不叠项目层**（虚拟路径没有 `.mcp.json`、没有项目技能）；再叠加该插件自己的工具。
- 模型：Session 自带既有 `switch_model`；用户在会话窗口切换 Provider/Model，**行为与普通项目会话完全一致**（D3；现有实现会更新全局 active，见 §9）。插件不能指定。

### 2.2 与真实 Project / 第三种 Session 的区别

| | 真实 Project | 第三种 Session（旧方案） | **虚拟 Project（选定）** |
|---|---|---|---|
| 用户是否要创建 | 要 | 不要 | **不要**（装插件即出现） |
| Session 命令 | 原生 | 需加 `plugin_id` 解析 | **签名原生**（`workspace`=虚拟路径，靠 owner 注入） |
| 左栏 | 项目分组 | 新分组类型 | **项目分组的变体**（插件徽标） |
| 文件工具根 | 用户项目 | 插件 home | **插件 home** |
| 工具面 | 内置+项目层 | 内置+全局层 | **内置+全局层** |
| 进 workspace_list / 选择器 | 是 | 否 | **否**（`virtual` 过滤） |
| 卸载后 | 目录还在 | 另建 orphan 分组 | **orphan 插件分组**（虚拟 Project 还在，只读） |

### 2.3 生命周期与 runtime 分支

- 首次出现：插件安装并启用后，`plugin_projects_list` 返回一个虚拟 Project（无需用户操作）。
- 会话创建：`agent.run` 不带 `session_id` → 新建一条**随机 id** 的普通会话（D4）；带 → 复用。插件想复用就把 id 存自己的 localStorage。
- 插件停用（enabled=false）：虚拟 Project 仍显示、会话可读；`ask` 在 **dispatch 前置**拒绝 `denied`（此时 runtime 未装配）。
- 插件卸载：停止 runtime、取消运行中的 run；虚拟 Project 保留为「已卸载插件」分组，会话只读、可手动删（`plugin_session_delete`）。
- 插件升级：runtime 重启；虚拟路径与会话不变。

**`runtimePlugin` 必须逐点分流（评审指出 hub 的安全性分散在 6 个 `kind ==` 判定里，只写"照 hub 装配"不够）**：

| 位置 | runtimePlugin 的行为 | 漏掉的后果 |
|---|---|---|
| `createIn` hooks（`main.go:2314`） | 走 `hooksOptionUser`（与 hub 同规） | 读插件 home 的 `.go-code/hooks.json` → 插件写 hooks、宿主执行命令 |
| `sessionRecordKey()`（`hub_session.go:38`） | `plugin-<id>`（不跟路径） | 目录迁移/改 id 后历史会话"丢失" |
| `newSessionEngine` 的 sessions root（`main.go:5877`、`4708`、`codemode.go:232`） | 由 runtime 提供 `sessionsRoot`，不再 `wsSessionsDir(workspace)` | 子 agent journal / codemode store 落到错误目录，`delete_session` 删一半 |
| `deleteSession`（`main.go:4634`） | 用 `resolveRuntime` + `ws.storeDir/eventsDir` | 停不掉运行中的 run、删错目录、会话"删不掉" |
| `shutdownWorkspace/All`、`deleteWorkspace` | 遍历含插件 runtime | 插件 runtime 不优雅关闭 |
| mesh 注册（`main.go:2452`）、hub 队列泵（`1009`）、`autoEnablePlugins`（`2107`） | **跳过** | 插件会话进 mesh、被当项目泵队列 |
| `m.runtime(path)` 未命中插件表 | 若 `isPluginHomePath(path)` → 返回 nil，**绝不**走项目装配 | 造出项目 runtime、把虚拟路径写进 `m.workspaces`、`m.get` nil 解引用 |

### 2.4 会话归属路径（owner path）与前端过滤

**owner path 是 D2 成立的前提**：

- store 增加 `sessionOwner: Record<sid, ownerPath>`；来源：`plugin_projects_list` 水合、`new_session` 回执里新增的 `workspace`（虚拟路径）、`list` 响应按 project 回填。
- `send()` 的 workspace 取值改为：`payload.workspace || sessionOwner[sid] || state.workspace`（`useAppStore.ts:3974`）；`pending`/prefs/响应校验同样用 owner。
- `setActiveWorkspace` **不用于**虚拟 Project；打开插件会话走 `openSession(sid, {ownerPath})`，只切 `activeSessionId` 与 owner，不触发 git/skills/browser/resetView 副作用。
- 新建插件会话：左栏插件分组「新建会话」→ `new_session { workspace: 虚拟路径 }`。

**过滤清单（评审实测 11+ 处；后端不变式见 §2.3，前端只做展示过滤）**：

1. `workspaces.json` 持久化（`syncWorkspacesToBridge` / `persistProjects` 的 `LS_PROJECTS.paths`）；
2. 启动水合 `for (p of projects) send({type:'list'})` —— 虚拟 Project 走 `plugin_projects_list`；
3. workspace 选择器 / 最近项目 / 置顶；
4. `setActiveWorkspace` 的 `workspace_add`、skills、git、browser、`resetView`；
5. `loadWsGit` / git 历史 / 文件树刷新；
6. 左栏 per-project 菜单：pin、files、新会话、删除工作区；
7. `resolveActiveOwner(sid, ws)` → 虚拟路径的 owner 注入（hooks 信任 / IM 路由 / 插件启停）；
8. 项目层 MCP/技能状态展示；
9. 手机端：`mobileAllowedCommands` 加 `plugin_projects_list`，并明确 `switch_model` 在手机上原本就不支持（`MOBILE_UNSUPPORTED_COMMANDS`）；
10. 后端 hub：`ListWorkspaces` / `knownWorkspace` 拒绝虚拟路径；
11. `workspaceSync` / `workspaceAdd` 入口拒绝（`workspace_registry.sync` 不校验目录，必须显式拦）。

### 2.5 插件侧看到的身份

插件只知道自己收到的 `session_id` / `run_id`，不知道虚拟路径，也不能选择真实项目；虚拟路径由 main 从 **token 派生**的 `plugin_id` 注入（payload 里**没有** `plugin_id`，见 §4.2），bridge 只信任 main 注入的值。
## 3. 能力面（插件看到的）

### 3.1 HostApi 新增

```ts
host.agent = {
  session(opts?: { title?: string }): Promise<{ sessionId: string }>
  run(req: AgentRunRequest): AgentRunHandle   // 不传 session_id = 新建随机会话
  cancel(runId: string): void
}

host.composer = {
  quote(input: { text: string; source?: QuoteSource }): { ok: boolean; reason?: string }
}

host.tools = {
  register(name: string, handler: (args: any, ctx: ToolCtx) => Promise<ToolResult>): () => void
}
```

- `AgentRunHandle`：`{ runId, sessionId, on(type, fn), cancel() }`；事件 `delta | tool | usage | done | error`。`sessionId` 在 `run()` 返回时即已确定（新建或复用），插件据此决定要不要持久化。
- `host.tools.register` 必须在 manifest `tools[]` 里声明过；未声明直接抛（与 `events.on` 同口径）。
- 所有返回值恒为对象，不抛传输错误；`reason` 见 §6 错误码。

### 3.2 manifest 新增

```json
{
  "agent": {
    "sessions": true,          // 允许创建/使用自己的 plugin session（默认 false）
    "run": true,               // 允许发起 run（默认 false）
    "quote": true              // 允许插草稿（默认 false）
  },
  "tools": [
    {
      "name": "course_week",
      "description": "查询某周的课程安排",
      "input": { "type": "object", "properties": { "week": {"type":"integer"} }, "required": ["week"] },
      "read_only": true,
      "requires_panel": false
    }
  ]
}
```

- 安装页用自然语言展示能力：**「可发起 AI 请求（消耗你的模型额度）」「可把内容放进输入框草稿」「可向 Agent 提供 3 个工具」**。
- 设置页每项可撤销；撤销后：`agent.run/quote` 立即拒绝，工具从引擎 Hide。
- 工具名：模型侧 `plugin__<id>__<name>`（id 的 `-`→`_`）；**不得与内置/MCP/subagent 工具重名**，冲突整条 manifest 拒绝。

## 4. 协议

### 4.1 通道

```
插件(renderer) --host.agent.run--> loader(闭包持 token) --IPC surface:agent--> main
   --stdio command surface_agent_run--> bridge
bridge --event surface_agent_event--> main --独立 IPC surface:agent-event--> loader --路由 runId--> 插件
```

- **身份只来自 token**：main 在插件加载前签发 per-plugin token（`surface:tokens`），loader 闭包持有；`surface_agent_*` 的 payload **不带 `plugin_id`**，main 校验 token 后注入 `plugin_id`。防跨插件冒充（评审 #8）。
- **独立通道**：agent 事件与工具调用走 `webContents.send('surface:agent-event' | 'surface:tool-call')` → preload 暴露 `onSurfaceAgentEvent` / `onSurfaceToolCall`。**不复用** `onEvent`（它广播全部 bridge 事件，任何持有者能看到所有项目/Hub/IM 消息）。`plugin_projects_changed` 走既有 `ui` op。
- 所有 bridge→插件的 agent 事件只带 `{run_id, session_id, seq, type, data}`；loader 按 run 归属路由。
- **与 transcript 的关系**：同一 run 的文本既会产生会话视图的 `content_chunk`（权威，进 record/checkpoint），也会产生给插件的 `delta`（派生，不落盘）。`delta` 只是同一流的副本，run 结束以会话 transcript 为准；取消时两边一起停。

### 4.2 命令（renderer → bridge）

| 命令 | 载荷 | 回执 |
|---|---|---|
| `surface_agent_session` | `{title?}`（plugin_id 由 main 注入） | `{ok, session_id}`（随机 id） |
| `surface_agent_run` | `{run_id, session_id?, input, instructions?, schema?, tools:"global"\|"none"}` | `{ok, run_id, session_id}`（`session_id` 缺省 = 新建；立即回，流式走事件） |
| `surface_agent_cancel` | `{run_id}` | `{ok}` |
| `surface_tool_result` | `{call_id, ok, result?, error?}` | `{ok}` |
| `plugin_projects_list` | `{}` | `{projects:[{plugin_id,name,enabled,virtual:true,sessions:[...]}]}` |
| `plugin_session_open` | `{plugin_id, id?, create?}` | 与 `new_session` 同形（内部用虚拟路径） |
| `plugin_session_delete` | `{plugin_id, id}` | `{ok}` |

`surface_agent_run` 的 prompt 组装（bridge 侧，插件不可绕过）：

```
user message:
  [来自插件 <name> 的请求 · 不可信输入]
  <instructions>
  <input 作为 fenced json>
```

插件提供的文本永远以**用户消息**进入，不拼 system prompt；`schema` 有值时最终文本必须是合法 JSON 并通过校验。

### 4.3 事件（bridge → renderer）

```json
{ "type":"surface_agent_event",
  "payload": { "plugin_id":"course-mvp", "session_id":"s_...", "run_id":"r_...",
               "seq": 7, "event":"delta", "data": {"text":"..."} } }
```

事件类型：

| event | data | 说明 |
|---|---|---|
| `delta` | `{text}` | 文本增量（50ms 合并） |
| `tool` | `{name, status:'start'\|'end', ok?}` | 只给工具名与状态，不给参数/结果 |
| `usage` | `{input_tokens, output_tokens}` | 结束时一次 |
| `done` | `{text, json?}` | 终态；`json` 仅当 schema 通过 |
| `error` | `{reason, message?}` | 终态 |

`seq` 单调递增，终态之后不再有事件；插件取消/卸载后迟到事件由 loader 丢弃。

### 4.4 工具调用（bridge → renderer → bridge）

```json
// bridge → renderer
{ "type":"surface_tool_call",
  "payload": { "call_id":"tc_...", "plugin_id":"course-mvp",
               "tool":"course_week", "args":{...}, "deadline_ms":10000 } }

// renderer → bridge
{ "type":"surface_tool_result",
  "payload": { "call_id":"tc_...", "ok":true, "result":{...} } }
```

- 超时：默认 10s（manifest 可声明 ≤60s）；超时 → 工具返回 `{ok:false, reason:"timeout"}`，不中断 run。
- 插件卸载/崩溃：等待中的 call 立即失败 `plugin_unloaded`。
- 结果上限 64 KiB（JSON），超出截断并标记 `truncated:true`。
- `requires_panel: true` 的工具在面板未打开时返回 `panel_not_open`（宿主不偷偷挂隐藏实例）。
- 审批：`read_only:false` 的工具走现有工具审批；审批卡显示「<插件名> · <工具名>」，插件不能代点。

### 4.5 引用（renderer 内，不过桥）

```js
host.composer.quote({ text, source: { kind:'pdf', file:'unit-3.pdf', page:3 } })
```

- loader 通过 `uiBus.emit('composer_quote', payload)`；`Composer.tsx` 订阅并把内容插入当前草稿（带来源 chip）。
- 上限 8 KiB；去掉控制字符；**绝不自动发送**；无活动会话 → `{ok:false, reason:'no_session'}`。
- V1 不落审计（纯本地 UI 动作）；P2 若加自动发送再进审计。

## 5. 工具集装配（不可变 + 只增）

| 来源 | 插件会话 | 项目会话 | 插件可否修改 |
|---|---|---|---|
| 内置（file/bash/todo/run_python/ask_user…） | ✅（见下方 denylist） | ✅ | ❌ |
| 全局 MCP | ✅ | ✅（+项目层） | ❌ |
| 全局 subAgents | ✅ | ✅（+项目层） | ❌ |
| 插件自定义工具 | ✅（自己的） | 需用户授权（默认关） | 只能注册自己的，不能覆盖/隐藏别的 |

**denylist（与"加载所有内置工具"的唯一偏差，理由是权限升级）**：`surface_install` / `surface_remove` / `surface_set_enabled`（插件管理权 = 装卸面板）、`session_send` 与 mesh 类（跨会话/跨节点发消息）、`promote_task` / cron 类（后台持久任务）、`git_worktree`（真实仓库操作）、`memory_dream`（写长期记忆）。装配完成后 `buildLoop` 追加的 office/UI/surface 三组注册也要按 `runtimeKind` 过滤（`main.go:6037`）。

- 注册点：`newSessionEngine` 装配完成后，按 `pluginToolsFor(runtime)` 注册；项目会话在授权变化时热注册/Hide。
- 命名：`plugin__<id>__<name>`；冲突（与任何已有工具同名）→ manifest 安装即拒绝。
- 子 agent 继承主引擎 → 插件工具对子 agent 也可用（与现有"继承全部工具"一致）。
- 插件工具**不能**：修改工具集、注册 MCP server、定义 subagent、读取其它工具的 schema/结果。

## 6. 错误码

| reason | 触发 | 插件应做什么 |
|---|---|---|
| `not_declared` | manifest 未声明该能力/工具 | 提示作者改 manifest |
| `denied` | 用户在设置页撤销授权 | 提示用户去设置启用 |
| `quota` | 超出频率/并发/大小 | 退避或提示 |
| `no_session` | quote 时没有活动会话 | 提示先开会话 |
| `panel_not_open` | requires_panel 工具被调但面板没开 | 提示用户打开面板 |
| `plugin_unloaded` | 工具执行中插件被卸载/崩溃 | 可重试一次 |
| `timeout` | 工具超时 | 缩小任务 |
| `invalid_schema` | 模型输出未通过 schema | 展示原文或重试 |
| `cancelled` | 用户/插件取消 | 静默 |
| `bridge_error` | 桥内部错误 | 提示重试 |

## 7. 安全与配额

- **L0 前置，但先做 spike**：`contextBridge.exposeInMainWorld` 注入的 `desktop` 是否可被覆写/删除，尚未验证；先跑 10 行探针看属性描述符。
  - 可覆写 → loader 捕获真对象到模块作用域，再把 `window.desktop` 换成收窄视图；
  - 不可覆写 → 改为 preload 只暴露收窄对象（宿主代码另走一条内部 IPC），或把插件放进 sandboxed iframe（真正的隔离，成本另算）。
- **诚实边界**：插件是 `import()` 进宿主**同一 realm** 的，能读同源 `localStorage`（项目路径、会话标题、prefs）、能改宿主全局。L0 是**降低误用**，不是安全边界；真正的边界只有隔离（iframe / 独立 world / 独立 WebContents）。
- **L0 必删清单**：`onEvent`（广播所有 bridge 事件）、`command`（任意桥命令）、`providerKey` / `providerCopyKey`、全部 `fs*`、`settingsGet/Set`、`oauth*`、`startBridge/stopBridge`、`pick*`、`openPath` / `showItemInFolder`。插件只留 host API。
- **token**：per-plugin capability token，loader 闭包持有；main 校验 token ↔ plugin_id；payload 不带 plugin_id（§4.1）。
- **不可信输入**：插件文本以用户消息 + 来源标注进入；不拼 system prompt；schema 校验后再交给插件。
- **额度**（V1 默认，可在设置页调）：并发 run 1/插件；20 run/小时；input ≤32 KiB；instructions ≤8 KiB；schema ≤16 KiB；单 run ≤10 min；工具结果 ≤64 KiB；取消即停。
- **可见性**：每次 run 在左栏产生/复用一条插件会话，用户能看到完整 prompt、工具调用、用量，随时停止。
- **审计**：`surfaces/audit.jsonl` 追加 `{id, action:"agent_run"|"tool_call"|"quote", ts, ok, session?, run?, tool?, bytes?, reason?}`；写失败不影响主流程。
- **不做**：插件读用户其它会话、插件代点审批、插件指定 provider/模型/密钥、插件自动发送引用、插件修改全局工具集、**插件装卸插件**。

## 8. UI 变更清单

1. 左栏：插件会话分组（§2.4）。
2. 会话视图：插件徽标；顶部 Provider/Model 选择器与普通会话完全一致（现有 `switch_model` 会更新全局 active，不承诺按会话隔离）；空态说明「该会话由插件创建，使用全局工具集与当前模型」。
3. 设置页插件行：能力开关（agent.run / quote / tools）、当前会话数、审计入口。
4. 安装页/回执：能力摘要（自然语言），安装前可见。
5. 插件面板：`host.agent` / `host.composer` 的示例（SKILL 提供）。
6. Composer：来源 chip + 删除；不自动发送。

## 9. 决策记录

- **D1（已定）卸载后的插件会话**：**保留**，左栏「已卸载插件」分组，只读 + 手动删除（`plugin_session_delete`）。
- **D2（已定）虚拟 Project**：每插件一个虚拟 Project，Session 签名不变、左栏复用项目行；但不绑真实项目、不进 workspace_list、文件工具根在插件 home。**代价不是零**：前端要加 owner path（否则 `switch_model/ask` 全 `session_not_found`）、后端要补 6 处 runtime 分支与 4 道负向守卫、前端 11 处过滤。
- **D3（已定）模型**：建会话时继承全局 active；用户在会话窗口自由切换 Provider/Model。**与普通项目会话完全一致：现有 `switch_model` 会更新全局 active**（`main.go:3426-3444`）；若要像 Hub 那样"只影响本条会话"，需另开一轮加 `isHub` 同款豁免。插件不能指定。
- **D4（已定）随机会话 ID**：不做 `session_key` 映射；插件可新建，也可复用自己记住的 id。

### D2 备选方案的成本对比（存档）

| | 虚拟 Project（选定） | 真实 Project | 第三种 Session |
|---|---|---|---|
| Session 命令 | 签名不变，但前端要加 `sessionOwner[sid]`（N 处过滤） | 原生 | 需加 `plugin_id` 二选一 |
| 左栏 | 项目分组变体 | 项目分组 | 新分组类型 |
| 文件工具根 | 插件 home（沙盒） | **用户项目目录** | 插件 home |
| 工具面 | 内置+全局层 | 内置+项目层（随项目漂移） | 内置+全局层 |
| 使用门槛 | 装完即用 | 必须先建 Project | 装完即用 |
| 卸载/清理 | orphan 分组 | 混在项目里 | orphan 分组 |
| 主要成本 | runtime 6 处分支 + owner path + 11 处过滤 | 零改造 | 命令解析 + 新 runtime |

**折中（V2，不阻塞）**：虚拟 Project 默认无真实项目；`agent.run({ workspace })` 显式申请一次项目上下文，宿主弹确认「插件 X 想读取项目 Y」，同意后才把文件工具根切过去。

## 10. 分阶段与工作量

| 阶段 | 内容 | 估 |
|---|---|---|
| **L0** | 先 spike（属性可覆写？）+ `window.desktop` 收窄/token/独立 IPC + 探针；不可覆写则改 preload 收窄或 iframe | 1–2 轮（spike 决定上界） |
| **P1** | `runtimePlugin` + 6 处分支/4 道守卫 + `sessionOwner` + 11 处过滤 + `agent.run` + `composer.quote` | **5–6 轮** |
| **P2** | manifest `tools[]` + 代理工具 + 工具调用协议 + 审批 + 项目会话授权 | 2–3 轮 |
| **P3** | 当前会话只读订阅 / 通用 channel（可选） | 2 轮 |

P1 完成即可跑通 PDF 阅读器：选中 → 引用草稿；选中 → 翻译（run → 流式渲染回面板）。
P2 完成才有「用户在项目会话里说一句，PDF 面板自己更新」。

## 12. 评审吸收（2026-11-14，独立子智能体）

评审结论 **revise**，三条 blocker 已并入本版：

1. **命令拿不到虚拟路径**（renderer 只有一个 `state.workspace`）→ 新增 §2.4 owner path（`sessionOwner[sid]` + `send()` 优先取 + prefs/响应/owner 注入同源）。
2. **runtime 分支不全**（hooks 走项目分支会读插件 home 的 hooks.json；delete/shutdown/存储根错）→ 新增 §2.3 分流表（7 个点）。
3. **缺负向守卫**（停用/卸载后 `m.runtime` 会兜底造项目 runtime，把虚拟路径写进 workspaces.json）→ `isPluginHomePath` 在 `m.runtime` / `createOpts` / `workspaceSync|Add` / `knownWorkspace` 四道硬拒绝；"虚拟路径永不进 workspaces.json"是**后端不变式**，不是前端过滤。

其余已吸收：D3 改为"与普通会话一致，切模型会更新全局 active"；工具集加 denylist（surface_*/mesh/cron/promote_task/git_worktree/memory_dream）；agent 事件走独立 IPC、不复用 `onEvent`；plugin_id 由 token 派生；L0 先 spike，且明说"同 realm 下 L0 不是安全边界"；成本 P1 从 3 轮上调到 **5–6 轮**。

## 11. Skill 同步大纲（`skills/hai-surface-dev/SKILL.md` 新增一节）

1. 何时需要 Agent：引用 vs 跑任务 vs 供工具；一张决策表。
2. manifest 声明示例 + 安装页会展示什么。
3. `host.composer.quote` recipe（含来源字段、上限、不自动发送）。
4. `host.agent.run` recipe（新建/复用 session_id、schema、delta/done/error、取消；id 存插件自己的 localStorage）。
5. `host.tools.register` recipe（manifest 对齐、read_only、requires_panel、超时、结果上限）。
6. 错误码表 + 额度表。
7. 反例：不要伪造数据、不要假装宿主会持久化、不要试图改全局工具集。
