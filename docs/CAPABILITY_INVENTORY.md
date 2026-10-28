# HAI Harness 内部能力全清单（原 README，不对外）

> **这是内部文档，不在开源范围**（见 `oss/scope.txt`）。
> 对外门面是仓库根的 `README.md`（英文）与 `README.zh-CN.md`（中文）。
> 本文保留原因：它按「Agent Harness 层 / DESKTOP 应用层」逐项记录了产品能力与实现状态，
> 以及指向 `docs/` 内部设计文档的导航 —— 这些链接在公开树里不存在，故不适合放在 README。
>
> 记录时间：2026-09-23（迁移自当时的 README.md，内容未改）。

---

# go-code

> **事件驱动、长时运行的 Agent Harness SDK** —— 把「模型调用 + 工具执行 + 上下文管理 + 持久化恢复」封装成一层可编程运行时；上层产品（CLI / Electron 桌面端）只负责**驱动 `Session`、消费事件流**。

- **内核语言**：Go 1.25.5（module `go-code`）；桌面端 TypeScript / React
- **内核依赖**：`go-openai`（OpenAI 兼容 / Responses 协议）、`anthropic-sdk-go`（Anthropic Messages 协议）、`mcp-go`（MCP 客户端）、`yaml.v3`（SKILL.md frontmatter）
- **设计哲学（归位原则）**：SDK 只收「需要动内核才能获得」的机制；纯接口之上能组合出来的归产品层，SDK 最多给参考实现与文档示例 —— 详见 [docs/ROADMAP.md](docs/ROADMAP.md) §0。

## 文档导航

| 文档 | 内容 |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 5 分钟心智模型（新读者第一站） |
| [docs/ROADMAP.md](docs/ROADMAP.md) | 能力差距分析（vs Claude Code / Codex / Hermes / Pi）+ 归位判定 |
| [docs/DESIGN.md](docs/DESIGN.md) | 落地后的设计决策记录 |
| [docs/EXECUTION.md](docs/EXECUTION.md) | 执行流程细节 |
| [docs/DESKTOP.md](docs/DESKTOP.md) | 桌面端设计 |
| [docs/PROVIDER_CHAIN_REVIEW.md](docs/PROVIDER_CHAIN_REVIEW.md) | Provider 全链路审查 + 修复记录 |
| [docs/CODEBASE_REVIEW_V2.md](docs/CODEBASE_REVIEW_V2.md) | 第二轮细粒度审查（附修复状态附录） |
| [docs/office/](docs/office/README.md) | 办公能力（Cowork 式）：技术方案 + 缺口盘点 + 开源调研 |

---

## 当前支持的完整能力

> 能力分两层：**Agent Harness 层**（SDK：`session`/`agents`/`events`/`tools`/`provider`/`subagent` 等，可嵌入任意产品）与 **DESKTOP 应用层**（Electron 产品，把 Harness 组装成可用桌面应用）。前者是引擎，后者是产品。

### 一、Agent Harness 层（SDK 内核）

#### 1.1 运行引擎
- **事件驱动核心**：LLM 流式 + 限流退避（429 指数退避 + 流中段限流识别）→ **双层事件模型**（LLM 原始事件 → `AgentLoop.translate` → Agent 事件），事件自包含 RunId / RequestId / Timestamp / 块序号，可独立审计、可 SSE 推送、可重放。
- **多轮 ReAct 循环**：压缩检查 → LLM 调用 → 工具执行 → 回写，直到 stop / abort / error；轮边界插话检查点 + MaxIterations 护栏 + abort 级联（含后台任务）。
- **上下文压缩**：LLM 结构化摘要（SummaryPromptV3 滚动合并、真实 usage 自适应触发、任务锚点逐字保留、滚动近窗原文保留、失败降级 + 退避、压缩计入成本）；完整历史与有效上下文分离，压缩只动「发给模型的」。
- **崩溃恢复**：每轮落盘检查点（恢复粒度 = 一轮）+ **checkpoint-first 恢复**（`SessionRecord` = SDKState + ViewCheckpoint 同 revision 原子提交）+ legacy 迁移 + 会话锁（flock）。
- **HITL 审批 + 模式**：危险工具逐个发 `ToolApprovalRequested`，`Session.Approve` 注入；模式 = 工具可见性 × 审批策略 × 行为提示（`plan` / `normal` / `all-pass` / `full-access`），Run 级快照切换。
- **命令框架（B）**：`Session.RegisterCommand / Command` —— 一条与 LLM 工具调用**不同源、LLM 不可见**的执行通道；内置 `compact` / `skills` / `clear`。命令产出的上下文内容（`skills load` 的技能指令）**原位进对话历史**（`core.NewSkillMessage`，user + `skill` 块，与 `load_skill` 工具结果同构），**不改系统提示词** —— system 是 provider 缓存前缀首段，会话中途加层会让整段上下文按全价重写缓存。
- **Hooks 拦截（E）**：`PreToolUse`（改写/阻断）/ `PostToolBatch` / `PreCompact`（注入摘要）；子 agent 可继承或禁用。
- **运行时提醒**：目标一致性提醒（GoalAlignmentReminder，按轮次阈值注入）、工具轮后空回复续跑提醒（PostToolNudge）。~~后台任务在途提醒（PendingTasksNudge）、收尾前完成度检查（TodoGate）~~ **已于 2026-10 删除**（判据错 + 冗余，实测见 `PROMPT_CATALOG_CN.md` §10.8）。

#### 1.2 LLM 接入（provider 层）
- **三协议**：Chat Completions（OpenAI 兼容：OpenAI/DeepSeek/Kimi/Zhipu/opencode/自定义网关，含 Minimal 兼容模式）、OpenAI Responses、Anthropic Messages；
- **20+ 厂商预设**：deepseek / openai / opencode / kimi / zhipu / anthropic / groq / xai / mistral / minimax / qwen / xiaomi / together / fireworks / cerebras / baseten / nvidia / huggingface / openrouter / openai-codex / github-copilot / kimi-coding；
- **模型注册表**：真实元数据（窗口/输出上限/价表/推理/思考档位/多模态），统一思考档位 clamp、图片降级（非视觉模型占位）、推理内容回传策略（按模型/网关自动判断）；
- **OAuth 订阅登录**：Anthropic / xAI / OpenAI Codex / GitHub Copilot / Kimi Coding / OpenRouter（PKCE + device flow，token 注入 transport，401 自动刷新重试，未登录不静默回退）；
- **错误分类**：429 限流 / 4xx 永久 / 5xx 瞬时 / 上下文超限（`ClassifyError` 语义标签全链路传导：rate_limit/permanent/canceled/generic/context_exceeded）。

#### 1.3 工具与执行
- **工具引擎**：注册 → 稳定序 schema（缓存友好）→ 分桶（seq/par/blocked）→ 模式过滤 → 审批 → 并行/顺序执行（并发上限可配）→ 超时/截断 → panic 兜底 → 运行时 promote 后台（`promote_task`）；采集用量与 diff。
- **内置工作区工具**：`read_file` / `write_file` / `edit_file`（原子写 + 锁 + diff）/ `grep`（context/include）/ `glob` / `bash` / `http_get`（SSRF 防护 + 20KB 截断）/ `ask_user`（**批量提问 questions[1-8]**）/ `plan_submit` / `git_worktree`；
- **会话状态工具**：`todo_add/update/list`（待办随 State 持久化，压缩/恢复幸存）；
- **bash 安全**：进程组隔离（Setpgid）+ 最小环境白名单 + 超时杀整组 + **高危命令静态拦截**（rm 递归强删/磁盘操作/sudo/fork 炸弹/curl|sh 等，给可操作修正提示）；执行后端可注入 `sandbox.Sandbox` —— **macOS Seatbelt**（敏感路径 deny、工作区外写入拦截、受管插件子树放行、拦截放行确认）已落地，Linux bwrap 同接口扩展。
- **工具结果瘦身（C）**：只读结果去重替换 + 超龄占位替换 + spill 溢出写文件留预览（`WithToolResultSlim`，默认关）。

#### 1.4 多 agent 与异步任务
- **同步子代理**：`subagent_explore` / `subagent_research`（父子事件树 ParentRunId/Depth、审批/模式/Hooks 继承、深度限制、成本经 ToolResult.Usage 传导）；
- **异步后台任务**：`agent_spawn` / `agent_send` / `agent_wait` / `agent_interrupt` / `agent_list` / `TaskList` / `TaskOutput`（Registry 并发槽 + 跨 Run 存活、interrupt = abort 非失败、任务输出 journal 落盘、`agent_explore` 只读调研）；
- **工具运行时 promote**：长工具可摘离为后台任务（`promote_task` + 占位结果 + 完成自动回传主会话 + 精确中断 `interrupt_task`）。

#### 1.5 上下文与记忆
- **工作记忆**：Agent.md（AGENTS.md/CLAUDE.md 递归发现）+ **SystemPrompt 四层分层**（base 行为契约 → 产品层 → 工作记忆 → 技能清单），恢复时按内容比较替换（配置变更自动生效、字节稳定保缓存命中）；
- **Skills（Agent Skills 开放标准）**：SKILL.md + **渐进披露**（元数据进系统提示、正文按需加载）+ `load_skill` + 分层注册表（全局 + 工作区，同名工作区优先，启用真过滤）；
- **远程技能**：`skill_install` / `skill_update` / `skill_uninstall` / `skill_browse` / 技能市场（`market_add/list/remove`，按市场批量更新）；
- **缓存优化**：分层前缀稳定字节 + ToolParams 字典序 + 只读桶在前稳定序（真实 API 验证高缓存命中率）。

#### 1.6 会话生命周期
- **输入队列**：`Ask` 入队即返回（缓冲 64）+ 批量消费 + 队列满背压（不丢消息）；
- **成本预算**：`WithMaxCost` 超限拒绝 + `SessionBudgetExceeded` 事件；
- **优雅退出**：`Session.Shutdown`（拒新输入 + abort 当前轮 + 等收尾落盘）+ `ShutdownOnSignals` 一行接入；
- **审批 FIFO**：多会话/子代理审批统一队列（`approval.go`）。

**可扩展挂点**：`provider.Provider` · `tools.Tool` / `BaseTool` · `agents.Compressor` · `session.Store` / `Locker` · `sandbox.Sandbox` · `events.Approver` · `agents.Backoff`。

### 二、DESKTOP 应用层（`desktop/`，产品）

#### 2.1 架构
- **Electron + React/TS**（Vite + Zustand + Tailwind v4 + shiki）↔ **Go stdio-NDJSON bridge**（单进程、多工作区宿主：组装 Session+AgentLoop+引擎、路由命令、扁平化事件到 stdout、微批量 flush）。
- **双通道布局**：叙述通道 + 活动通道（react-resizable-panels 可拖拽）、Agent 转录、审批中心（可编辑参数）、变更 diff、Todo 面板。

#### 2.2 会话与对话
- **Persona 双档**（code / work）+ 模型切换 + 思考强度切换 + **Provider 热切换**（`set_provider` 重建 loop 不重启，三协议分发）+ 模型窗口/输出上限前后端联动（1M 上下文）；
- **待发送队列**：运行中连发消息全部即时推送 + `user_inputs_consumed` 事件精确感知消费；无会话时自动建会话（发送时创建）；
- **会话管理**：多工作区 / 会话列表 / 置顶（session_pin）/ 删除 / 标题覆盖 / 最近会话（LRU 10）/ 跨 Provider 模型归属不漂移；
- **左栏双视图**（2026-09-20）：任务列表 ↔ **工作区文件浏览**——入口在**每个工作区行**（悬停浮出的文件夹按钮，与该行 pin/新建/删除同级；**面板 <240px 时行内操作收成一个「⋯」溢出菜单**，≥240px 铺开 4 个按钮）：点它即以该工作区为根进入文件视角（非活跃先切活跃）；视角内「返回任务」+ 搜索 + 目录懒展开 + 顶部工作区切换下拉（列出全部工作区/添加工作区）；点文件经 `openFile` 在右侧文件栏展示（目录树走 Electron `fsList` IPC，内容走 bridge `file_preview`）；工作区行/「选择文件夹」统一用文件夹图标；行内操作按钮垂直居中（`top:50%+translateY`）；分隔条显式 `width:4px`（库的 inline `flex:0 0 auto` 会把它压成 0 宽 → 面板此前实际拖不动）；
- **插话**：运行中用户可连发消息，下轮 LLM 自动并入上下文；`interrupt` 硬停（abort 当前轮）+ `interrupt_task` 精确中断后台任务。

#### 2.3 Provider 设置
- **内置预设**（20+ 厂商下拉添加）+ 连接测试（`test_connection`）+ 拉取模型列表（含能力标注：窗口/上限/多模态/推理）+ 自定义 OpenAI 兼容端点（minimal 模式）；
- **OAuth 登录/登出/状态**（登录事件推送 + 浏览器唤起 + manual 授权码回填 + 精确路由）；
- **密钥管理**：API key 明文持久化 settings.json（用户决策 2026-08-16，0600）+ safeStorage 兜底。

#### 2.4 后台任务与多 agent（产品侧）
- **任务卡片**：异步任务（agent_spawn / promoted 工具）在右栏任务 Tab 实时展示（运行/中断/完成/失败状态 + 思考/文本/工具转录 + 成本）；
- **任务控制**：`promote_task` 摘离为后台 + `interrupt_task` 精确停止 + 主停止可配置是否级联停后台（runtime 设置）；
- **任务结果**：完成自动回传主会话（折叠 Markdown 块）+ 轻量通知。

#### 2.5 技能与市场（产品侧）
- **技能管理页**：启用/查看/更新/卸载已安装技能 + 斜杠命令面板（`/技能名`、`/summary`、`/clear`、`/reload_skills`）；
- **技能市场**：市场 URL 管理（`market_add/list/remove`）+ 浏览/安装/更新 + 按市场批量更新已装技能（manifest 记 commit 判更新）；
- **外部技能导入**：检测并导入 `~/.codex` / `~/.claude` 外部技能（`external_skills_status/import`）。

#### 2.6 Git 集成
- **只读**：快照（仓库状态/分支/变更文件）、文件 diff、历史、提交 diff；
- **写操作**：暂存 / 取消暂存 / 提交 / 分支切换 / init（统一 busy 防并发 + 双层重拉 + 路径限制工作区 + 破坏性操作前端二次确认）。

#### 2.7 定时任务（cron）
- **宿主级任务**：`cron_list/create/update/delete/clean` + 到点触发 + 运行账本（`cron_runs` / `cron_runs_detail`）+ 派生会话独立历史目录 + 自动清理（auto_clean）；
- **工具注入**：会话内 `cron_*` 工具族（CronCreate/CronList/CronUpdate/CronDelete）+ 设置开关。

#### 2.8 浏览器集成
- **Browser Use 插件**：内嵌视图（WebContentsView + CDP 驱动）+ 多页面池（开/切/关真实 target）+ 导航状态/AI 动作订阅 + 调试代理 + Chrome 登录态一键导入（复制文件零解密）；
- **浏览器命令**：`browser_status/pages/screenshot/close` + 配置（`browser_config_get/set`）。

#### 2.9 MCP 管理
- **配置分层**：用户级 `~/.go-code/settings.json` < 项目级 `{ws}/.mcp.json`（Claude Code 项目级约定，跨工具可复用）< 项目级 `{ws}/.go-code/settings.json`（go-code 原生，最高优先）——同名服务器后层覆盖前层，项目层可用 `"enabled": false` 关闭用户层某台服务器；`mcpServers` 段（stdio/http/streamable_http/sse + 远端 `headers` 请求头）+ 校验统一（未知 type 跳过；`streamable_http`/`streamable-http` 为 `http` 别名，归一为 `http`）+ 每工作区 Manager（`mcp/`：`Layer`/`LoadLayers`/`Merge`/`Loader`）；
- **动态监听（fsnotify）**：三层配置文件**自动热应用**——新建 / 内容改写 / 编辑器原子替换（tmp+rename）都感知，250ms 静默期合并成一次重载；目录后建（`{ws}/.go-code` 首次出现）自愈重挂；内容未变则零副作用（不碰连接、不重建 loop）；`delete_workspace`/退出时停止监听；
- **损坏恢复（last-known-good）**：坏 JSON / 半写 / 空文件 / 读失败 → **沿用上次有效内容**而不是清空该层（否则一次半写会让全部服务器消失），恢复后自动切回；文件删除视为有意为之（清空该层）；单条目非法只跳过该条并在 `Skipped` 里列出；层状态（`stale`/`error`/`skipped`/`updated_at`）经 `mcp_list` 的 `layers` 字段暴露；
- **客户端（2026-09-21 改造：全局 × 项目两级）**：MCP 设置页一级是来源分组卡片——「全局（跨工作区）」= 用户层，「项目」= 每个已绑定且仍存在的项目一张卡（**已删除的项目不展示**）；点卡片进二级详情：用户层可增删改 + 启停 + 刷新连接，项目层的 `{ws}/.go-code/settings.json` 可增删改 + 启停（`mcp_project_set` 写盘 + 热应用），`{ws}/.mcp.json` 只读展示（随仓库分发），层文件的存在性/有效性/台数/`stale` 损坏提示都在项目详情里可见（多项目汇总 `mcp_projects` 不创建运行态）；
- **连接**：`mcp_list`（状态 + `source` 标注来自哪一层 + `layers` 层状态）/ `mcp_set`（用户层全量热应用 + 叠加各工作区项目层 + rebuildAll）/ `mcp_refresh`（强制重连）/ `mcp_reload`（手动重读分层，监听失败时兜底）/ `mcp_projects`（多项目项目层汇总，只读文件不建运行态）/ `mcp_project_set`（项目层全量保存 → 写 `{ws}/.go-code/settings.json` + 热应用）；工具注入 `mcp__<server>__<tool>`，连接失败不阻塞；
- **远端鉴权与启动（2026-10）**：远端传输（http/streamable_http/sse）支持每台服务器自定义**请求头** `headers`（如 `Authorization: Bearer …`；明文存 settings.json，仅远端生效）——需鉴权的托管端点没有它必然连不上；stdio 的启动方式面板预设 `npx/uvx/python/python3`，命令不可用时（GUI 启动 PATH 缺解释器 / 路径写错）连接状态给出**可操作报错**而不是 exec 原文（`mcp/server.go` 的 `commandProblem`/`startError`）；
- **写盘**：Electron 侧 `settings.json` 改为原子写（临时文件 + rename，0600）——原 `writeFileSync` 先截断再写，崩溃或并发读会留下半截文件（provider key 与 MCP 一起丢）；
- **安全语义**：项目层配置随仓库分发且无 project trust 门禁——其中的 stdio `command` 在连接时直接执行并继承本进程环境变量（打开不可信仓库前请先看其 `.mcp.json`）。

#### 2.9.1 钩子（hooks）管理
- **能力**：外部命令 hook（7 个生命周期事件：SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / PostToolBatch / PreCompact / Stop）——stdin/stdout JSON 协议、超时/异步/失败模式、上下文注入与阻断，异常永不阻断会话（`hooks/` + `desktop/bridge/hooks_wire.go`，规范见 `docs/graft-integration/05-hooks-能力设计规范.md`）；
- **配置分层**：用户级 `~/.go-code/settings.json` 的 `hooks` 段 → 项目级 `{ws}/.go-code/hooks.json`（默认需 `allowProjectHooks`）→ 只读兼容 `~/.claude/settings.json` 与项目 `.claude/settings*.json`（Claude Code / Codex 形态，两种配置写法都认）；
- **信任**：项目层随仓库分发，按**命令内容哈希**存在 `~/.go-code/hooks-state.json`，未信任/已改动的命令一律不执行（`trusted|managed` 放行）；
- **管理面板（2026-09-21 新增）**：设置 → 钩子——一级分组卡片「来自配置」（用户配置 / Claude 兼容层只读）+「来自项目配置文件」（每个仍存在的项目一张卡，已删除项目不展示）；二级详情按事件分组列出钩子（展开看 处理程序/命令/超时/matcher/异步/失败模式/信任），支持单条启停 + 增删改 + 批量信任项目钩子；用户配置页另有全局开关（停用全部 / 允许项目钩子 / 读取 Claude 兼容层 / 全部都需信任）。「是否真的会执行」在每条钩子上显式标注（未启用/总闸关/未信任/项目层未启用），不静默隐藏；
- **命令与生效**：`hooks_list` / `hooks_set`（用户级写 settings.json 的 `hooks` 段、项目级写 `{ws}/.go-code/hooks.json`，严格校验拒绝非法事件/类型/超时）/ `hooks_trust`；保存后经 `Session.SetHooks` 热替换，**下一次 Run 生效**（运行中的 turn 用旧快照收尾），信任变更立即生效；

#### 2.10 插件系统
- `plugin_list/install/uninstall/enable/disable` + 自动恢复上次启用状态 + Browser Use 等插件（子进程 + MCP 桥接）。

#### 2.11 设置与使用统计
- **设置系统**：分组导航（基础 / 模型 / Agent 能力 / 数据与统计）—— 通用 / 快捷键 / Provider / MCP（全局 × 项目）/ 钩子（全局 × 项目）/ 插件 / IM 集成 / 会话通信 / 使用统计 + 沙箱模式（seatbelt/none）+ 权限白名单（open 命令 per-app）+ 提醒轮次 + 语言/主题；
- **使用统计（设置 → 数据与统计）**：跨会话用量看板 —— 指标卡（累计 Token / 峰值 Token / 最长聊天时长 / 当前·最长连续天数 / 累计成本，全历史口径）+ Token 活动热力图（每日/每周/累计，近 53 周，**hover 出该日用量与截至该日累计**）+ 每日 Token 趋势图（近 7 日 / 近 30 日，按 `provider/model` 分线，>5 模型合并「其他」）；数据 = bridge `metrics` 命令聚合 `~/.go-code/metrics.jsonl`（`MetricsStore` 抽象 + File JSONL v1，成本按价表现算）；空会话中部复用同一份报告的近 90 天热力图（5 行 × 18 天横向扁条、整块居中；hover 内联当日/累计）；会话级实时指标（成本 / 每轮 / 缓存）在状态栏与链路 tab；
- **文件引用**：`file_preview`（1MB 截断）+ `fs_list` / `fs_read_ref`（@ 引用展开，工作区 containment + 敏感目录 deny）。

#### 2.12 持久化与恢复（产品侧）
- **checkpoint-first**：SessionRecord + ViewCheckpoint 同 revision 原子提交（bridge 协调器，800ms debounce + 语义边界提交 + 失败可观察 `session_persist_error`）；
- **项目列表 / 标题覆盖 / 最近会话**：localStorage + 会话级偏好固化（Provider/model/mode/effort 按会话独立，切回恢复）。

### 三、能力现状一句话

常规 `go test ./...` **25 包全绿**（macOS 上含 seatbelt 沙箱集成测试）；`go vet` / `gofmt` / `go mod tidy` 干净；`go test -race` 关键包通过；前端 `tsc` 0 错误 + `eslint` 0 错误 + 4 大测试套件全绿 + `npm run build` 通过（含 Go bridge 编译）。`examples/flappy` 用真实 DeepSeek API 端到端验证过（多轮工具调用 → 产出 HTML 游戏 + 事件级 trace，缓存命中率/成本聚合实证）。

---

## 架构：三个核心对象

```
Session（会话容器，长时存在）
  └─ AgentLoop（运行引擎，每批输入跑一次 Run；不可变配置、可并发）
       └─ AgentContext（一次 Run 的运行状态，跑完即弃）
```

- **Session**：输入队列（`Ask` 入队即返回）、完整历史、有效上下文、成本预算、待办、持久化与恢复、人工审批 —— **一切跨轮状态都在 Session 上**。
- **AgentLoop**：多轮循环（LLM 调用 → 工具执行 → 回写 → 再调）直到 stop/abort/error。
- **AgentContext**：单次 Run 的临时状态，用完即弃。

一次输入的生命周期：`Ask` 入队落盘 → `Session.Run`（取一批 + 会话锁）→ `AgentLoop.RunStream`（压缩检查 → LLM 流式重试 → 工具轮 → 检查点）→ 吸收结果落盘 → 队列清空返回。

---

## 目录结构（每个目录负责什么）

| 目录 | 负责的能力 | 关键入口 |
|---|---|---|
| `core/` | **零依赖共享协议层**：消息/工具调用/用量/成本/diff 线类型与不变量 | `Message`, `ToolCall`, `Usage`, `Cost`, `Reparentable` |
| `events/` | **对外唯一交互标准**：事件流契约 + HITL 原语 + 工具上下文 + Hook 类型（只有类型，无循环依赖） | `Event`, `Approver`, `QuestionWaiter`, `ToolHooks` |
| `provider/` | **LLM 接入层**：三协议（chat-completions/responses/anthropic）+ 模型注册表（20+ 厂商）+ 错误分类 + OAuth 订阅登录 | `Provider`, `Registry`, `ParseProtocol`, `ClassifyError` |
| `provider/auth/` | OAuth 凭证存储 / PKCE / device flow / token 注入 transport | `Store`, `Flow`, `Transport` |
| `tools/` | 工具协议 + 执行引擎 + 内置工作区工具集（含 bash 安全、原子写、diff 采集、SSRF 防护） | `Tool`, `Engine`, `FileTools`, `NewBashTool`, `NewHTTPTool` |
| `agents/` | **运行引擎**：多轮循环 / 压缩 / 重试退避 / 系统提示分层 / 模式 / 命令 / Hooks / 结果瘦身 | `NewAgentLoop`, `AgentContext`, `LLMCompressor`, `Backoff` |
| `session/` | **长时会话容器**：输入队列 / 历史 / 成本预算 / 审批 / 命令 / 后台任务结果 / checkpoint-first 持久化恢复 / 优雅退出 | `NewSession`, `FileStore`, `SessionRecord`, `ShutdownOnSignals` |
| `subagent/` | 子 agent 工具（同步父子继承）+ 异步后台任务注册表 + agent_* 工具族 | `NewTool`, `NewBackgroundTool`, `Registry` |
| `todo/` | 会话级待办（随 State 持久化，压缩/恢复幸存），`todo_add/update/list` 工具 | `Store`, `Tools()` |
| `skills/` | Agent Skills：SKILL.md 解析 + 分层注册表 + `load_skill` + `remote/` 远程下载/更新/市场浏览 | `Registry`, `NewLayeredRegistry`, `remote.Browse/Install` |
| `mcp/` | MCP 客户端参考实现：mcpServers 分层配置（用户 + 项目）→ stdio/http/streamable_http/sse 连接（远端可带 `headers` 请求头）→ 适配成 `tools.Tool` | `Manager`, `LoadLayers`, `Merge`, `NewAdapter` |
| `sandbox/` | bash 执行后端抽象：`NoSandbox` 为默认；**Seatbelt**（macOS `sandbox-exec`，策略动态生成 + 受管插件放行 + `WithRelease` 放行门）已实现 | `Sandbox`, `Seatbelt`, `WithRelease`, `ExecSpec` |
| `cron/` | 定时任务：定义存储 + 运行账本 + 派生会话清理 | `Store`, `Ledger`, `NewTools` |
| `plugin/` | 插件系统：Browser Use 等（子进程 + MCP 桥接） | `Plugin`, `browser.Plugin` |
| `internal/` | 集成测试链：工具 → 子 agent → 压缩 → 检查点/崩溃恢复（可编程 mock provider） | `internal/integration` |
| `examples/` | SDK 使用示例 + 真实 API 验证（flappy：DeepSeek 端到端 + 事件级 trace） | `examples/flappy` |
| `desktop/` | **产品层**：`bridge/` Go stdio 宿主（多工作区、命令路由、事件扁平化、checkpoint 协调）+ `app/` Electron/React 渲染 | `desktop/bridge/main.go`, `desktop/app/src` |
| `brand/` | 品牌 logo 资产（HAI 深/亮双版本） | `brand/hai-logo-light.png` |

**依赖关系速记**：`core`（无依赖）← `events` / `provider` ← `tools`（含 `sandbox` 后端）← `agents` ← `session` / `subagent` / `todo` / `skills` / `mcp` / `cron` ← `desktop`（产品把整条链组装成可运行应用）。

---

## 项目优点（设计取舍）

1. **归位原则**：SDK 只收需要动内核的机制，其余归产品层 —— 内核保持小而稳定，产品层自由组合。
2. **事件自包含**：所有事件带 RunId/RequestId/Timestamp/Index，可独立审计、可重放、可 SSE 推送，无隐式全局状态。
3. **状态分层清晰**：Session（跨轮）→ AgentContext（单轮）→ checkpoint（崩溃恢复），职责不重叠。
4. **三协议统一抽象**：一个 `Provider.Stream` 接口承载 chat-completions/responses/anthropic，消息/工具/思考翻译在各子包收敛，错误分类三协议共用。
5. **模型元数据单一事实源**：注册表（窗口/上限/价表/推理/模态）+ 用户覆盖，思考档位 clamp 防 400。
6. **兼容性防御**：空消息过滤、tool 空结果占位、推理回传策略、Minimal 网关模式、图片降级 —— 每个兼容坑都有实证来源注释。
7. **checkpoint-first 恢复**：SDKState + ViewCheckpoint 同 revision 原子提交，崩溃恢复粒度 = 一轮，UI 与模型上下文同语义点。
8. **安全纵深**：bash 静态高危拦截 + Seatbelt 沙箱 + SSRF 防护 + 路径 containment + 敏感目录 deny + 文件 0600/0700。
9. **可测试性**：Provider 依赖注入（mock 全链路）、事件流可编程脚本、集成测试覆盖压缩/崩溃恢复/子 agent。

---

## 构建与运行

```bash
# SDK 测试（全部）
go test ./...          # 常规测试全绿；macOS seatbelt 集成测试需本机沙箱环境
go test -race ./...    # race 检测（macOS 本机验证；OAuth/并发路径已修复）
go vet ./...           # 静态检查
bash scripts/check.sh  # 本地门禁（gofmt/tidy/vet/build/test/race/前端）

# 真实场景示例（需要 DeepSeek API Key）
cd examples/flappy && go run .     # 生成 output/flappy.html + trace.jsonl

# 桌面端
cd desktop/app
npm install
npm run dev            # Vite + Electron（浏览器直开 localhost:5173 走 mock 事件流，Electron 走真实 bridge）
npm run build          # vite build + electron tsc + Go bridge 编译
npm run lint           # eslint（flat config，0 errors）
npm run check          # typecheck + lint + 全部前端测试
```

> 桌面端 bridge 是独立 Go 程序（`desktop/bridge`），`npm run build` 已自动编译（跟随当前平台；`npm run build:bridge:mac` 交叉编译 darwin/arm64）。

---

## 迭代记录（近期）

> 详细设计决策见 [docs/DESIGN.md](docs/DESIGN.md) 与 [docs/PROVIDER_CHAIN_REVIEW.md](docs/PROVIDER_CHAIN_REVIEW.md) §8 修复记录。

| 时间 | 里程碑 |
|---|---|
| 2026-08-30 | **Provider 全链路审查 + 41 项修复落地**：三协议流中段限流分类、OAuth 401 友好提示、ParseProtocol 严格化、注册表单一事实源、checkpoint SDK/UI 快照同语义、Ask/Shutdown 线性化、ToolEngine join 硬上限、SSRF 防护、路径 containment、文件权限收紧、错误码体系、命令类型化（`Command`）、前端 lint 从无到有 + 10 个 TS 错误清零 |
| 2026-08-30 | 压缩预估改用「真实 usage 触发」（替代字符/4 自适应），SummaryPromptV3 决策层强化 |
| 2026-08-29 | Anthropic Messages / OpenAI Responses 协议落地（T5/T6），模型注册表 20+ 厂商 |
| 2026-08-28 | checkpoint-first 恢复（SessionRecord + ViewCheckpoint）、Seatbelt 插件放行修复 |
| 2026-08-26 | Session 恢复链路重做（崩溃恢复粒度 = 轮） |
| 2026-08-24 | 压缩近窗保留（KeepRecentMessages）、自动压缩退避 |
| 2026-08-22 | 会话配置固化（跨 Provider 模型归属不漂移）、缓存 LRU 10 项 |
| 2026-08-19 | 后台任务异步化（agent_spawn 工具族）、Skills 渐进披露 |
| 2026-08-17 | 中断链路修复（硬停语义、路由兜底） |
| 2026-08-16 | 桌面端 Provider 面板 + API key 明文持久化决策 + 1M 上下文联动 |

---

## 演进方向

已全部落地的 SDK 机制：命令框架 B、Hooks 拦截 E、工具结果瘦身 C、异步多 agent A、门面包示例 F（详见 [docs/PLAN.md](docs/PLAN.md) 全景）。候选按产品需求触发：Go↔TS 协议 codegen（单一类型源）、巨型 dispatch switch 拆分（待 git 基线）、40 个 lint warnings 收紧、`http_get` 连接期二次 SSRF 校验、多模态 vision 桥、记忆写回。
