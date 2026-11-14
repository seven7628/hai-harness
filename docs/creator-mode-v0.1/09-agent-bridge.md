# 09 · 插件 ↔ Agent 双向互通（方案与协议）

> 状态：设计定稿待开工（2026-11-14）
> 决策人：用户（三条已拍板，见 §0）
> 前置：R17a/R17b 已合并；`window.desktop` 收窄（L0）是 P1 的硬前置

## 0. 已拍板的三条

1. **插件任务会话必须进左栏会话树**，且是**第三种简单形态**：不绑 Project、不是智能体/群组会话，只知道「自己这个 Session」。
2. **引用到会话只插草稿**，发送永远由用户按；「自动发送」留到以后，V1 不做。
3. **`agent.run` 加载全部系统内置工具 + 全局 MCP + 全局 subAgents**；插件**不能修改**这三套集合；但**允许插件注册自定义工具**进来（只增不改）。
4. **卸载保留插件会话**（D1）：左栏归入「已卸载插件」，只读、可手动删。
5. **模型跟随全局、可自由切换**（D3）：插件不能指定 provider/model/密钥；用户在会话里切换与普通会话同一条路。
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

## 2. 第三种 Session：Plugin Session

### 2.1 定义

```
kind: plugin
owner: plugin:<id>
fields: { id, title, plugin_id, created_at, updated_at, running, pinned? }
没有: project / workspace 绑定、agent profile、group 成员、项目技能层、项目 MCP 层
```

- **存储**：`~/.go-code/plugin-sessions/<id>/{home,sessions,events}`（0700）。
  - `home/`：该插件的虚拟工作目录，bash/file 工具的根（等价 `hubAgentHome`，与代码目录物理分开）。
  - 不设 `index.json` / `session_key` 映射（D4）：会话 id 由 bridge 随机生成，列表直接扫 `sessions/*.jsonl`；「复用哪条会话」由插件自己记住 id（存它自己的 localStorage）。
- **运行时**：`runtimePlugin` 加入 `runtimeKind`（`pluginID` 字段），登记在 `m.pluginRuntimes[pluginID]`，**不进 `m.workspaces`**（否则 `workspace_list`/reload/MCP 重放会把插件当项目遍历）。
- **工具面**：与 `runtimeHubAgent` 同一套全局层（内置 + 用户级 MCP + 全局技能/子代理），**外加该插件自己注册的工具**。
- **模型**：跟随全局 active provider/model，用户可在会话里自由切换（D3）；插件**不能**指定 provider、模型或密钥。

### 2.2 生命周期

- 懒创建：第一次 `agent.run` 或用户从插件面板点「打开会话」时创建 runtime。
- 会话创建：`agent.run` 不带 `session_id` → 新建一条随机 id 的会话；带 `session_id` → 复用。插件想「一个功能一条会话」，就在第一次拿到的 id 存进自己的 localStorage，后续带上；想每次新开就都不带。宿主不替它记映射。
- 插件卸载：**停止 runtime、取消所有在跑的 run**；会话记录**保留**（D1），左栏归入「已卸载插件」分组，只读可删。
- 插件停用（enabled=false）：runtime 不装配；已有会话可读，不能继续跑。
- 插件升级（hash 变）：runtime 重启；会话保留。

### 2.3 命令解析：一个 sessionRef

所有会话命令（`new_session`/`ask`/`interrupt`/`approve`/`answer_question`/`compact`/`session_send`…）统一接受二选一：

```json
{ "workspace": "/abs/project" }        // 现有
{ "plugin_id": "course-mvp" }          // 新：解析到 m.pluginRuntimes[id]
```

两个都给 → `bad_request`；都不给 → 现有报错口径。`bridgeSession` 上记录 `origin: "project" | "plugin"` 与 `plugin_id`，供事件与左栏用。

### 2.4 左栏（第三种形态的 UI）

- 项目区之上/之下新增一个**「插件会话」分组**：按插件分组，插件名 + 图标 + 会话列表；空则整组隐藏。
- 每条会话显示：标题、运行中 spinner、停止按钮、插件来源标记；未读/最近排序与现有会话一致。
- 点开即普通会话视图（消息、工具卡、审批、TODO 全部复用）；顶部显示「插件会话 · <插件名>」徽标。
- 不显示「项目路径」；文件工具显示的是插件 home，不是用户项目。
- 事件：`plugin_sessions_changed { plugin_id }` → 前端刷新该分组。

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
| `plugin_session_list` | `{}` | `{plugins:[{id,name,enabled,sessions:[...]}]}` |
| `plugin_session_open` | `{plugin_id, id?, key?, create?}` | 与 `new_session` 同形 |
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
2. 会话视图：插件徽标；空态说明「该会话由插件创建，使用全局工具与模型」。
3. 设置页插件行：能力开关（agent.run / quote / tools）、当前会话数、审计入口。
4. 安装页/回执：能力摘要（自然语言），安装前可见。
5. 插件面板：`host.agent` / `host.composer` 的示例（SKILL 提供）。
6. Composer：来源 chip + 删除；不自动发送。

## 9. 决策记录

- **D1（已定）卸载后的插件会话**：**保留**，左栏「已卸载插件」分组，只读 + 手动删除（`plugin_session_delete`）。
- **D2（已定）不绑定 Project，保留第三种 Session**。理由见下；绑项目的成本对比仅省 1.5–2 轮，但代价是插件拿到项目级 bash/file 工具（爆炸半径）、工具面随项目漂移、空项目用户无法使用。
- **D3（已定）模型**：跟随全局 active provider/model，用户可自由切换；插件不能指定。
- **D4（已定）随机会话 ID**：不做 `session_key` 映射；插件可新建，也可复用自己记住的 id。

### D2 备选方案的成本对比（存档）

| | 第三种 Plugin Session（选定） | 绑定 Project |
|---|---|---|
| 实现成本 | 多 1.5–2 轮（runtime kind + 命令解析 + 左栏分组 + 空态） | 复用现有 workspace runtime |
| 工具根 | 插件自己的 home（沙盒） | **用户项目目录**（bash/read/write 全开） |
| 工具面 | 内置 + 全局 MCP + 全局 subAgents（确定） | 额外叠项目技能/MCP/hooks（随项目变） |
| 使用门槛 | 装完即用 | 必须先有 Project |
| 卸载/清理 | 独立分组，归属清晰 | 混在项目会话里，需额外区分 |

**折中（V2，不阻塞）**：plugin session 默认无项目；`agent.run({ workspace })` 显式申请一次项目上下文，宿主弹确认「插件 X 想读取项目 Y」，用户同意才把文件工具根切过去。

## 10. 分阶段与工作量

| 阶段 | 内容 | 估 |
|---|---|---|
| **L0** | `window.desktop` 收窄 + token + 探针（插件拿不到 providerKey/fs/command） | 1–2 轮 |
| **P1** | `runtimePlugin` + 命令解析 + 左栏第三种会话 + `agent.run`（流式/schema/配额/审计）+ `composer.quote` | 3 轮 |
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
