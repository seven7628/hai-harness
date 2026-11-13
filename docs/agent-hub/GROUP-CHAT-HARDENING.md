# 群聊硬化（P6）· 统一进度与执行契约

> 状态：进行中（Wave 1）
> 更新：2026-11-19
> 范围：P6-01 … P6-15（P0-1 … P2-3 的全部群聊隐患）
> 原则：子智能体实现；主 Session 只做验证、更新本文档、再分配、合并与删除 worktree。

## 0. 已锁定的产品决策

| # | 决策 | 落地口径 |
|---|---|---|
| D1 | 删除 `MaxParallelRuns` | `GroupPolicy` 不再有该字段；并发 = 本轮目标数（上限 = 活跃群成员数）。12 人群 @all = 11 并发。 |
| D2 | PairLimit 保留，防两两无限循环 | **无序 pair**：`A→B == B→A`；全局 Settings，默认 **10**；按 pair 独立计数；投递成功才 +1；到限只跳过该目标，不整批熔断。 |
| D3 | Depth 放到全局 Settings | 默认 **1000**；按**每条链**计深度，不是群级全局值；人类新输入开新链并归零。 |
| D4 | Mode 真接线 | `auto / ask / readonly` 必须真正影响工具执行；不能再是展示字段。 |
| D5 | 人类插话不停止在飞 run | 在飞 run 继续；人类消息排到对应 agent 的“下一个输入”；旧链未启动的 wake 丢弃；旧 run 的 @ 不得进入新链。 |
| D6 | 对话状态必须持久化 | 队列/状态/深度/pair 计数落盘；重启可恢复；已投递未完成的 run 标 interrupted，不自动重放副作用。 |
| D7 | observer 删除 | 身份只剩：人 / owner 智能体 / member 智能体；旧数据 `observer → member`。 |
| D8 | 全局 Settings 位置 | `settings.json` 顶层 `hub.conversation`：`max_depth`、`pair_limit`、`pair_window_ms`（默认 10 分钟）、`idle_reset_ms`（默认 30 分钟）、`cooldown_ms`（默认 0）、`run_timeout_ms`、`max_pending_wakes`。 |

## 1. 设计契约

### 1.1 PairLimit（无序、成功投递才计数）

- 键：`sorted([sender, target])`，例如 `A↔B`；`A→B` 与 `B→A` 同一个键。
- 作用域：一个 **conversation generation**（人类一次输入开启的新一代）。
- 计数时机：`wakeGroupAgent` 成功把输入交给目标会话之后；忙/失败/停止不计。
- 到限行为：
  - 只跳过该 target，记录 `skipped: pair_limit`；
  - 其他 target 继续投递；
  - 全部 target 都 skipped → `waiting_human`，写一条系统说明，不设 `exhausted`。
- 计数是**滚动窗口**：只统计最近 `pair_window_ms`（默认 10 分钟）内的成功投递；更早的计数惰性过期。
- 重置/清零时机（必须明确）：
  1. **人类新输入**：消息被接受时立即开新 generation，清零所有 pair 计数、depth、指纹；这是主重置。
  2. **pair 窗口自动过期**：A↔B 的 10 次只在最近 10 分钟内有效；窗口滑过后该 pair 自动恢复，不需要人类操作，避免永久卡死。惰性判断，不靠定时器。
  3. **空闲重置**：群连续 `idle_reset_ms`（默认 30 分钟）没有 run/wake，状态回 `idle`，计数清零。
  4. **手动重置**：状态条提供“重置本群限制”按钮（`hub_conversation_reset`），立即清零计数并解除 waiting_human，不自动唤醒任何人；用户下一次 @ 立即生效。
  5. **重启加载**：持久化的 pair 计数带时间戳；加载时先按窗口过期，再恢复。
- `A→B, B→A, A→C` 中 `A↔C` 是新键，从 0 开始；`A↔B` **不因 A 去跟 C 说话而清零**（否则换目标就能洗白循环）。
- 旧 generation 的在飞 run 保留旧计数，但不能向新 generation 路由。
- UI：显示 `A ↔ B 10/10`，并显示“约 N 分钟后自动恢复”；不是 `A → B`。
- handoff 的 10 分钟/2 次是另一套，保留；文档/UI 要区分“对话 pair”和“交接 pair”。

### 1.2 Depth（每条链独立）

- `wake.depth = parent.depth + 1`；人类输入首跳 = 1。
- 同一 generation 内，并行分支互不影响；浅分支跑完不会把深分支的 depth 重置。
- 到 `max_depth`：只拒绝该链继续 @，不把整个群判死；写系统说明。
- 重置/清零时机：人类新输入开新 generation；群空闲 `idle_reset_ms` 后回 idle 并清零；手动“重置本群限制”立即清零。旧 run 保留旧 depth，且不能路由进新 generation。
- 兜底（不是替代）：
  - PairLimit 10；
  - 同内容指纹；
  - `max_pending_wakes`（默认 200）；
  - 全局/群/agent 日预算；
  - `run_timeout_ms`（默认 10 分钟）。

### 1.3 并发与调度

- 一个群一个调度器；人类与 agent 的 wake 都走它。
- 并发上限 = 本轮 target 数（12 人群 @all = 11；显式 @3 = 3）。
- 公平性：人类目标优先；auto wake 按入队顺序；同一 agent 不并发跑两个群 run。
- 目标忙：进入该 agent 的输入队列，不丢、不报 failed。
- provider 429：按 provider 信号量排队 + 指数退避；不把目标标 failed。

### 1.4 Mode

- 复用工具元数据 `ReadOnly_` 分类；`group_say`/`group_members`/`group_history`/`agent_status` 视为通信/只读，始终允许。
- `readonly`：拒绝所有非只读工具（bash/write/run_python/session_send/assign_task/handoff 等），返回明确错误。
- `ask`：非只读工具创建审批 Need，等待人类；超时/拒绝 → 工具失败；审批卡出现在群 Needs。
- `auto`：不加群级审批；仍受 agent profile、sandbox、全局权限约束。
- 人类 UI 的群模式选择必须与实际行为一致；不再保留“假开关”。

### 1.5 人类插话 / 代际

- 人类消息落群日志；开启新 generation。
- 在飞 run 不中断；其 final 可以落账，标记为旧 generation。
- 旧 generation 尚未投递的 auto wake 丢弃；不进入新 generation。
- 人类消息按 target 排入“下一个输入”：
  - 目标空闲 → 立即投递；
  - 目标在跑 → 当前 run 结束后投递；
  - 多个 target → 各自队列，不互相阻塞。
- 旧 run 的 `group_say`/final @ 不得向新 generation 路由（校验 generation id）。
- 新 generation 的 depth/pair 从零开始；旧 run 不能消耗新额度。

### 1.6 持久化与恢复

- 每群 `conversations/<group>.json`（FileStore 原子写）：
  - generation id / human seq / status / stop reason / turns；
  - queue（target、trigger_seq、round_id、depth、enqueued_at、delivery status）；
  - 无序 pair counts（带最近一次成功投递时间戳，供滚动窗口过期）；
  - 指纹集合（每 pair 有上限，避免无限增长）。
- 写时机：enqueue/dequeue/deliver/complete/stop/pair 变更；不按 token 写。
- 重启：
  - 未投递 wake → 恢复排队；
  - 已投递未完成 → `interrupted`，写系统消息，不自动重试副作用；
  - 已完成 → 不重放；
  - 群已删除 → 清理。
- 兼容旧数据：没有 conversation 文件 = 空；坏文件不覆盖，告警。

### 1.7 身份与 observer

- `MemberRole` 只剩 `owner` / `member`。
- `owner` 是智能体，不是人；人类不在 `Members`。
- `@all` = 除发送者外全部活跃成员（observer 删除后无例外）。
- 旧 JSON `observer` 在 `ApplyDefaults`/加载时转 `member`；新写入拒绝 observer。
- 删除 `group_say`/`handoff`/assign 的 observer 特判、UI 标签、mention picker 过滤、mock 错误。

## 2. 任务表

| ID | 任务 | 依赖 | 主要文件 | 验收 |
|---|---|---|---|---|
| P6-01 | 投递账本：每 target `queued/running/done/skipped/failed` | — | `agenthub/conversation.go`、`desktop/bridge/hub_conversation.go`、`hub_group_session.go`、`hub_group_commands.go` | 忙目标排队重试；不丢；命令返回 deliveries；UI 类型 |
| P6-02 | 统一调度器：并发=目标数，删除 MaxParallelRuns | P6-01 | 调度器/pump、`agenthub/group.go`、UI/mock | 12 人群 @all 11 并发；agent @all 也并行；字段删除 |
| P6-03 | 预跑预算 + provider 信号量 + 429 退避 | P6-02 | `hub_conversation.go`、预算/guard、provider 调用层 | 预算不足不启动；429 排队；不标 failed |
| P6-04 | 全局对话设置 + PairLimit 算法 | P6-01 | `settings`、`agenthub/conversation.go`、bridge loader、UI | 无序/10/成功投递计数/逐目标 skip/不整批熔断；10 分钟滚动窗口；人类/空闲/手动/重启四类清零时机 |
| P6-05 | 每链 Depth + 全局 1000 + 重置 | P6-04 | `agenthub/conversation.go`、wake header、UI | 并行分支互不影响；深/浅不串；人类/空闲/手动清零 |
| P6-06 | Mode 真接线（auto/ask/readonly） | — | 工具包装、群策略、Needs/审批、UI | readonly 拒绝副作用；ask 弹审批；auto 直跑 |
| P6-07 | 人类插话下一输入 + generation 隔离 | P6-01/02/04/05 | `hub_group_commands.go`、`hub_conversation.go`、`hub_group_session.go` | 旧 run 继续；旧 wake 丢弃；旧 @ 不路由；目标排队 |
| P6-08 | 按 agent 停止 + run 超时 | P6-01/02 | stop 命令、scheduler | 只停一个 agent；超时中断并继续队列 |
| P6-09 | 对话状态持久化 + 重启恢复 | P6-01/04/05/07 | `agenthub/store*.go`、conversation、bridge startup | 队列/状态/pair 恢复；interrupted 不重放 |
| P6-10 | @all 状态/沉默语义 + UI | P6-01/02 | bridge 事件、`useHubStore`、`ConversationBar` | 已入队/已回复/未回复/失败可见；不再只显示最热 pair；pair 恢复倒计时 + 手动重置按钮 |
| P6-11 | agent 来源标记 + 工具策略审计 | P6-06 | wake header、prompt、审计/测试 | 来源可追溯；ask/readonly 对 agent-origin 同样生效 |
| P6-12 | observer 删除与迁移 | — | `agenthub/group.go`、bridge tools、前端 mentions/UI/mock | 旧 observer→member；角色只剩两种；无 observer 特判 |
| P6-13 | 死旋钮清理 | P6-02/06 | `agenthub/group.go`、settings、UI/docs | 删除 MaxParallelRuns/MaxTurnsPerHuman；Mode 已生效；每个旋钮有效果测试 |
| P6-14 | 会话区/右栏色板统一 + ConversationBar | P6-05/10 | `hub.css`、`demo.css`、`ConversationBar.tsx`、`conversation.ts` | 三主题中心/右栏一致；状态条显示本轮/本链/队列/pair/失败 |
| P6-15 | 最终回归 + 独立 Review | 全部 | 全仓 | Go/TS/browser/真实 E2E；文档/看板更新 |
| P6-16 | 头部「更多」菜单层级/样式修复 | — | `hub.css`、`GroupView.tsx`、`HubSurface.tsx` | 菜单永远压在消息/表格/右栏之上；无内容透出；三主题/1280/640；外点/Esc 关闭 |
| P6-17 | 群组编辑 + 成员增删/群主转让 | — | `GroupEditor.tsx`、`HubInspector.tsx`、`HubSidebar.tsx`、`useHubStore.ts`、`transport/mock.ts`、`i18n.ts` | 编辑群名/模式/群主；添加/移除成员；owner 不可移除；12 人上限；mock + 浏览器验收 |

## 3. 执行波次与 worktree

### Wave 1（并行）

| worktree | 子智能体任务 | 说明 |
|---|---|---|
| `group-core` | P6-01 → P6-02 → P6-03 → P6-04 → P6-05 | 全部 conversation/scheduler/domain，串行做，避免 `conversation.go` 冲突 |
| `group-surface` | P6-06 → P6-11 → P6-12 → P6-16 → P6-17 | Mode/来源/observer/头部菜单层级/群组编辑；避免碰 conversation 核心 |

### Wave 2（Wave 1 合并后）

| worktree | 子智能体任务 | 说明 |
|---|---|---|
| `group-lifecycle` | P6-07 → P6-08 → P6-09 → P6-10 → P6-13 → P6-14 | 依赖 Wave 1 的 ledger/scheduler/generation；串行做 |

### Wave 3

- P6-15：主 Session 起独立 Review + 全量回归；不新开功能。

## 4. 子智能体交付契约

每个子智能体必须：

1. 只在自己的 worktree 改；不碰主工作树、不 commit/push。
2. 每个任务独立 commit（message 带 `P6-xx`）。
3. 交付报告包含：
   - 改动文件与行为；
   - 测试命令 + exit code；
   - 反证（改动前红/改动后绿）；
   - `git status --short`（只列自己改的）；
   - 未解决风险/需要主 Session 决策的点。
4. 不修改 `docs/agent-hub/GROUP-CHAT-HARDENING.md`（主 Session 维护进度）。
5. 测试端口不得用 5173/5180；用 52xx 随机端口，跑完 kill。

## 5. 主 Session 验证 / 合并协议

1. 收到子智能体回推 → 在对应 worktree 跑：
   - 任务指定的 Go/Node/browser 测试；
   - `npx tsc --noEmit`、`npm run test:css`（前端改动）；
   - 必要的反证（回退关键改动应变红）。
2. 验证通过 → 更新本文档任务状态（`todo/in_progress/review/done` + commit + 验证摘要）。
3. 合并顺序：`group-core` → `group-surface` → `group-lifecycle`。
4. 合并后跑 `go test ./...`（或受影响包）+ `npm run test:p4` + 浏览器关键脚本；失败则回到子智能体修复，不 amend 已 Review 的 commit。
5. 合并完成 → `git_worktree remove`；主工作树保持 clean。
6. 任何 Review change_requested：只修 blocking，修完定向复审。

## 6. 进度

| ID | 状态 | worktree | commit | 验证 |
|---|---|---|---|---|
| P6-01 | in_progress | group-core | — | — |
| P6-02 | in_progress | group-core | — | — |
| P6-03 | in_progress | group-core | — | — |
| P6-04 | in_progress | group-core | — | — |
| P6-05 | in_progress | group-core | — | — |
| P6-06 | in_progress | group-surface | — | — |
| P6-07 | todo | group-lifecycle | — | — |
| P6-08 | todo | group-lifecycle | — | — |
| P6-09 | todo | group-lifecycle | — | — |
| P6-10 | todo | group-lifecycle | — | — |
| P6-11 | in_progress | group-surface | — | — |
| P6-12 | in_progress | group-surface | — | — |
| P6-13 | todo | group-lifecycle | — | — |
| P6-14 | todo | group-lifecycle | — | — |
| P6-15 | todo | main | — | — |
| P6-16 | todo | group-surface | — | — |
| P6-17 | todo | group-surface | — | — |

## 7. 残余风险 / 待观察

- `ask` 审批的阻塞时长与超时策略需要真实 provider E2E 验证。
- 11 并发对本地 vLLM/远端 provider 的 429 行为需要压测。
- 持久化文件写入频率与群日志一致性（消息已落、conversation 未落）需要崩溃注入测试。
- 旧 observer 数据迁移只做加载时转换；是否需要一次性回写由 P6-12 决定并记录。
