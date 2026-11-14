# 09 · 插件 ↔ Agent 双向互通（方案与协议）

> 状态：设计定稿待开工（2026-11-14）
> 决策人：用户（三条已拍板，见 §0）
> 前置：R17a/R17b 已合并；`window.desktop` 收窄（L0）是 P1 的硬前置

## 0. 已拍板的三条

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

`new_session` 要求 `workspace` 非空（`main.go:2843`）；`ask` / `interrupt` / `approve` 等会话命令都按 workspace 解析 runtime。第三种形态因此必须在**命令解析层**增加 `plugin_id` 这一路，而不是伪造一个项目路径。

## 2. 虚拟 Project：每个插件一个

### 2.1 定义

**插件 = 一个虚拟 Project**（不是第三种 Session，也不是真实工作区目录）：

```
{ virtual: true, plugin_id: "course-mvp", name: "儿童英语课程",
  path: "~/.go-code/plugin-sessions/<id>/home",  // 虚拟根，真实存在
  sessions: [ ... ] }                            // 普通 Session
```

- 它下面的 Session 就是**普通 Session**：`new_session` / `list` / `ask` / `interrupt` / `approve` / `switch_model` 全部沿用，`workspace` 传虚拟路径即可 —— **不加 `plugin_id` 二选一**。
- 运行时：新增 `runtimePlugin`（`runtimeKind` 第三值），装配在 `m.pluginRuntimes[path]`，**不进 `m.workspaces`**；`m.runtime(path)` 先查插件表再走项目表。这样 `workspace_list` / reload / MCP 重放 / cron / git 检测这些遍历 `m.workspaces` 的路径天然看不到它。
- 存储：`~/.go-code/plugin-sessions/<id>/{home,sessions,events}`（0700）。`home` 是 bash/file 工具的根，卸载不删（D1）。
- 工具面：照 `runtimeHubAgent` 装配**全局层**（内置 + 用户级 MCP + 全局技能/子代理），**不叠项目层**（虚拟路径没有 `.mcp.json`、没有项目技能）；再叠加该插件自己的工具。
- 模型：Session 自带既有 `switch_model`，用户自由切换 Provider/Model（D3），插件不能指定。

### 2.2 与真实 Project / 第三种 Session 的区别

| | 真实 Project | 第三种 Session（旧方案） | **虚拟 Project（选定）** |
|---|---|---|---|
| 用户是否要创建 | 要 | 不要 | **不要**（装插件即出现） |
| Session 命令 | 原生 | 需加 `plugin_id` 解析 | **原生**（`workspace`=虚拟路径） |
| 左栏 | 项目分组 | 新分组类型 | **项目分组的变体**（插件徽标） |
| 文件工具根 | 用户项目 | 插件 home | **插件 home** |
| 工具面 | 内置+项目层 | 内置+全局层 | **内置+全局层** |
| 进 workspace_list / 选择器 | 是 | 否 | **否**（`virtual` 过滤） |
| 卸载后 | 目录还在 | 另建 orphan 分组 | **orphan 插件分组**（虚拟 Project 还在，只读） |

### 2.3 生命周期

- 首次出现：插件安装并启用后，`plugin_projects_list` 返回一个虚拟 Project（无需用户操作）。
- 会话创建：`agent.run` 不带 `session_id` → 新建一条**随机 id** 的普通会话（D4）；带 → 复用。插件想复用就把 id 存自己的 localStorage。
- 插件停用（enabled=false）：虚拟 Project 仍显示、会话可读；`ask` 拒绝 `denied`（runtime 不装配）。
- 插件卸载：停止 runtime、取消运行中的 run；虚拟 Project 保留为「已卸载插件」分组，会话只读、可手动删（`plugin_session_delete`）。
- 插件升级：runtime 重启；虚拟路径与会话不变。

### 2.4 左栏与前端状态

- `projects` 里增加 `virtual?: 'plugin'` + `pluginId` + `pluginName`；插件分组带插件图标与「插件」徽标。
- 必须过滤虚拟 Project 的路径（逐条核过）：
  - `syncWorkspacesToBridge`（**绝不能写进 workspaces.json**）；
  - workspace 选择器 / 最近项目 / 置顶；
  - `loadWsGit` / git 历史 / 文件树刷新（虚拟根不是仓库）；
  - 项目层 MCP/技能的状态展示。
- 启动：`plugin_projects_list` 拉虚拟 Project 与其 sessions；普通 `list` 仍只跑真实工作区。
- 事件：`plugin_projects_changed`（安装/卸载/启用/停用）→ 前端刷新分组；会话增删走既有 `list` 响应。

### 2.5 插件侧看到的身份

插件只知道自己收到的 `session_id` / `run_id`，不知道虚拟路径，也不能选择真实项目；虚拟路径由 bridge 根据 token 里的 `plugin_id` 内部映射。
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
bridge --event surface_agent_event--> main --webContents.send--> loader --路由 runId--> 插件
```

- token：main 在**插件加载前**签发（`surface:tokens`），loader 捕获真实 `window.desktop` 后把全局对象收窄（L0）；插件拿不到 token。
- 所有 bridge→插件的 agent 事件只带 `{run_id, session_id, seq, type, data}`；loader 按 run 归属路由，插件看不到别人的。

### 4.2 命令（renderer → bridge）

| 命令 | 载荷 | 回执 |
|---|---|---|
| `surface_agent_session` | `{plugin_id, title?}` | `{ok, session_id}`（随机 id） |
| `surface_agent_run` | `{plugin_id, run_id, session_id?, input, instructions?, schema?, tools:"global"\|"none"}` | `{ok, run_id, session_id}`（`session_id` 缺省 = 新建；立即回，流式走事件） |
| `surface_agent_cancel` | `{plugin_id, run_id}` | `{ok}` |
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
| 内置（file/bash/todo/subagent/run_python） | ✅ | ✅ | ❌ |
| 全局 MCP | ✅ | ✅（+项目层） | ❌ |
| 全局 subAgents | ✅ | ✅（+项目层） | ❌ |
| 插件自定义工具 | ✅（自己的） | 需用户授权（默认关） | 只能注册自己的，不能覆盖/隐藏别的 |

- 注册点：`newSessionEngine` 装配完成后，按 `pluginToolsFor(runtime)` 注册；项目会话在授权变化时热注册/Hide。
- 命名：`plugin__<id>__<name>`；冲突（与任何已有工具同名）→ manifest 安装即拒绝。
- 子 agent 继承主引擎 → 插件工具对子 agent 也可用（与现有“继承全部工具”一致）。
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

- **L0 前置**：插件加载前收窄 `window.desktop`（当前 91 个方法，含 `providerKey` / `fsTrash` / 通用 `command`）；插件只能拿 host API。未完成 L0 之前不开 `agent` 能力。
- **token**：per-plugin capability token，loader 闭包持有；main 校验 token ↔ plugin_id。
- **不可信输入**：插件文本以用户消息 + 来源标注进入；不拼 system prompt；schema 校验后再交给插件。
- **额度**（V1 默认，可在设置页调）：并发 run 1/插件；20 run/小时；input ≤32 KiB；instructions ≤8 KiB；schema ≤16 KiB；单 run ≤10 min；工具结果 ≤64 KiB；取消即停。
- **可见性**：每次 run 在左栏产生/复用一条插件会话，用户能看到完整 prompt、工具调用、用量，随时停止。
- **审计**：`surfaces/audit.jsonl` 追加 `{id, action:"agent_run"|"tool_call"|"quote", ts, ok, session?, run?, tool?, bytes?, reason?}`；写失败不影响主流程。
- **不做**：插件读用户其它会话、插件代点审批、插件指定 provider/模型/密钥、插件自动发送引用、插件修改全局工具集。

## 8. UI 变更清单

1. 左栏：插件会话分组（§2.4）。
2. 会话视图：插件徽标；顶部 Provider/Model 选择器与普通会话完全一致（按会话独立）；空态说明「该会话由插件创建，使用全局工具集与当前模型」。
3. 设置页插件行：能力开关（agent.run / quote / tools）、当前会话数、审计入口。
4. 安装页/回执：能力摘要（自然语言），安装前可见。
5. 插件面板：`host.agent` / `host.composer` 的示例（SKILL 提供）。
6. Composer：来源 chip + 删除；不自动发送。

## 9. 决策记录

- **D1（已定）卸载后的插件会话**：**保留**，左栏「已卸载插件」分组，只读 + 手动删除（`plugin_session_delete`）。
- **D2（已定）虚拟 Project**：每插件一个虚拟 Project，Session/命令/左栏全部复用，但不绑真实项目、不进 workspace_list、文件工具根在插件 home。比「第三种 Session」省掉命令解析改造，比「真实 Project」少掉爆炸半径与使用门槛。
- **D3（已定）模型**：建会话时继承全局 active；用户在会话窗口自由切换 Provider/Model（既有 `switch_model`，按会话独立，不改全局）；插件不能指定。
- **D4（已定）随机会话 ID**：不做 `session_key` 映射；插件可新建，也可复用自己记住的 id。

### D2 备选方案的成本对比（存档）

| | 虚拟 Project（选定） | 真实 Project | 第三种 Session |
|---|---|---|---|
| Session 命令 | 原生 | 原生 | 需加 `plugin_id` 二选一 |
| 左栏 | 项目分组变体 | 项目分组 | 新分组类型 |
| 文件工具根 | 插件 home（沙盒） | **用户项目目录** | 插件 home |
| 工具面 | 内置+全局层 | 内置+项目层（随项目漂移） | 内置+全局层 |
| 使用门槛 | 装完即用 | 必须先建 Project | 装完即用 |
| 卸载/清理 | orphan 分组 | 混在项目里 | orphan 分组 |
| 主要成本 | 过滤虚拟路径（6 处遍历） | 零改造 | 命令解析 + 新 runtime |

**折中（V2，不阻塞）**：虚拟 Project 默认无真实项目；`agent.run({ workspace })` 显式申请一次项目上下文，宿主弹确认「插件 X 想读取项目 Y」，同意后才把文件工具根切过去。

## 10. 分阶段与工作量

| 阶段 | 内容 | 估 |
|---|---|---|
| **L0** | `window.desktop` 收窄 + token + 探针（插件拿不到 providerKey/fs/command） | 1–2 轮 |
| **P1** | `runtimePlugin` + 虚拟 Project（左栏/列表/过滤 6 处）+ `agent.run`（流式/schema/配额/审计）+ `composer.quote` | 3 轮 |
| **P2** | manifest `tools[]` + 代理工具 + 工具调用协议 + 审批 + 项目会话授权 | 2–3 轮 |
| **P3** | 当前会话只读订阅 / 通用 channel（可选） | 2 轮 |

P1 完成即可跑通 PDF 阅读器：选中 → 引用草稿；选中 → 翻译（run → 流式渲染回面板）。
P2 完成才有「用户在项目会话里说一句，PDF 面板自己更新」。

## 11. Skill 同步大纲（`skills/hai-surface-dev/SKILL.md` 新增一节）

1. 何时需要 Agent：引用 vs 跑任务 vs 供工具；一张决策表。
2. manifest 声明示例 + 安装页会展示什么。
3. `host.composer.quote` recipe（含来源字段、上限、不自动发送）。
4. `host.agent.run` recipe（新建/复用 session_id、schema、delta/done/error、取消；id 存插件自己的 localStorage）。
5. `host.tools.register` recipe（manifest 对齐、read_only、requires_panel、超时、结果上限）。
6. 错误码表 + 额度表。
7. 反例：不要伪造数据、不要假装宿主会持久化、不要试图改全局工具集。
