# 群聊硬化 Follow-up（P7）

> 来源：P6 最终独立 Review 的残余项（I3/N7/压测/I1 收尾）。P6 已收口；本文件是 P7 的唯一执行契约。
> 进度只认 [`PROGRESS.html`](./PROGRESS.html)；本文档由主 Session 维护，子智能体不得修改。

## 0. 范围与顺序

| ID | 任务 | 依赖 | 交付 |
|---|---|---|---|
| P7-01 | I3：群组编辑原子化（`hub_group_edit`） | — | 单次校验 + 单次 `SaveGroup` + 一次 roster 重建；前端/ mock / 浏览器回归 |
| P7-02 | N7：同内容指纹改为 per-target 跳过 | — | 只跳过命中的 target；其他 target 继续；全部跳过才 `waiting_human` |
| P7-03 | 12 人群 @all 并发/429 压力回归 | — | 确定性 fake provider：人类 @all=12 并发 / owner @all=11 并发、3 条 429 退避重投、不丢不重复 |
| P7-04 | I1 收尾 + 小 nits | P7-01/02 | 设置保存后立即刷新状态条；已挂 run 超时热更新；429 清 timer；文案/表驱动/stat 移出锁 |

执行顺序：P7-01 / P7-02 / P7-03 可并行；P7-04 在 P7-01、P7-02 合并后做，避免同文件冲突。

## 1. P7-01 I3：群组编辑原子化

现状：`saveGroupEdit` 依次发 `hub_group_update`（转让）→ `hub_group_member_remove`×N → `hub_group_member_add`×N → `hub_group_update`；每条命令各自 `LoadGroup`/`SaveGroup`。任一步失败会留下半改状态（owner 已换、成员删了一半等）。

要求：

- 新增 bridge 命令 `hub_group_edit`，payload：
  - `group_id`（必填）
  - `name?`、`mode?`、`owner?`（可选字段级 patch）
  - `add_members?: string[]`、`remove_members?: string[]`
- **先全量校验再落盘**：群存在、成员 profile 存在、去重、12 人上限、owner 不可被移除、owner 必须是成员、不能把自己加两次；任一失败返回明确错误且**群完全不变**。
- 校验通过后只做一次 `SaveGroup`，然后一次 `refreshHubGroupRoster`；不要逐成员循环保存。
- 保留现有单命令（`hub_group_update`/`member_add`/`member_remove`）供其他路径使用；前端 `saveGroupEdit` 改调 `hub_group_edit`。
- mock 同步实现 `hub_group_edit`；浏览器群组编辑回归走同一条路径。
- 测试：
  - 成功：一次编辑同时改 name/mode/owner + 增删成员，最终 group 正确、roster 一次重建。
  - 原子性反证：add 一个不存在的 agent / 移除 owner / 超过 12 人 / 重名，命令失败后 group 的 name/mode/owner/members 与调用前逐字段相等。
  - 前端 `test-hub-group-edit` 断言只发一条命令；浏览器 `check-hub-group-edit` 全绿。
- 迁移/兼容：旧客户端仍可发旧命令；新命令不改变 `hub_group_update` 语义。

## 2. P7-02 N7：指纹 per-target

现状：`Offer` 的 `conv.prints[p.pair].has(fp)` 命中时直接 `exhaustLocked`，整批（含其他 target）熔断。

要求：

- 命中指纹的 target 只记 `skipped`，原因码新增 `fingerprint_loop`（文案：「同一对重复内容，已跳过」），**不**整批 `exhausted`。
- 其他未命中的 target 照常入队；只有全部 target 都被跳过时才 `waiting_human`（与 pair/depth 同口径）。
- 指纹只在**真正入队**的 target 上记录；被跳过的 target 不写指纹，避免影响后续。
- 前端 `deliveryReasonText` 补原因码；`ConversationBar` 能看到「跳过 1」。
- 测试：
  - A→[B,C] 且 A↔B 已重复同一句：B skipped(fingerprint_loop)，C 仍入队并投递。
  - A→[B] 重复同一句：只 skipped，状态 `waiting_human` 而不是 `exhausted`。
  - 反证：旧实现会返回 `ErrConversationExhausted` 且 C 不入队。
- 文档：`GROUP-CHAT-HARDENING.md` §7 的 N7 follow-up 标记完成；P6 契约 §1.1 的「同内容指纹」表述同步为 per-target。

## 3. P7-03 12 人群 @all 并发/429 压力回归

目标：把「并发=本轮目标数」和「429 退避重投」用确定性测试钉住，不依赖真实上游/网络。

要求：

- 12 人群（1 owner + 11 成员），覆盖两条 @all 口径：
  - **人类 @all**：sender 是人类、不在成员表 → targets = 12（owner + 11 成员），峰值 = 12；
  - **owner 智能体 @all**：排除自己 → targets = 11，峰值 = 11。
- fake provider 记录并发峰值、每个 target 的调用次数。
- 断言：
  - 人类 @all 峰值 = 12、owner @all 峰值 = 11；所有目标都真的启动；
  - 人类 @all 的 12 条里选 3 条 provider 返回 429（run 中途，非投递窗口），退避后重投成功；其余 9 条不受影响；
  - 账本：3 条 attempts≥2 且最终 done/running，无 failed；无重复 target、无丢 wake；
  - 群状态条最终队列 0（本仓库没有 `done` 这个 ConversationStatus）；无 goroutine 泄漏（可用现有 testManager 清理路径）。
- 建议文件：`desktop/bridge/hub_concurrency_stress_test.go`；测试名 `TestHubAllMembersConcurrencyAnd429`。
- 真实 provider 压测（opencode/deepseek 等）需要凭据与费用，不在本任务自动化；在文档里保留「真实 11 并发」待人工执行。

## 4. P7-04 I1 收尾 + 小 nits

- 设置保存后立即刷新状态条：`reload_settings` 触发对话限额重载后，对所有已有群 emit 一次 `hub_conversation`（新增 controller `Groups()` 或 bridge 侧 group 列表），不能等下一次群事件。
- 已挂 run 超时热更新：`run_timeout_ms` 变化后，把当前 `hubRunTimers` 按新值重挂（从现在起算），并保证旧定时器 Stop；或明确保留旧值并在 UI 标注。二选一，测试证明。
- 429 分支显式 `clearHubRunTimeout(gid, aid)`（review nit #6）。
- `waiting_human + queue>0` 文案不能再说「队列已排空」；状态条应显示队列继续/等待投递。
- `ConversationView.Schedulable()` 补 5 状态 × 队列 0/1 表驱动测试。
- `loadHubConversationLimitsCached()` 的 `os.Stat` 移出 `m.mu`（先取限额，再进锁）。
- 清理 `Offer` stale 分支的 `skipped++` 死代码（若确认无用途）。
- 测试：bridge 热更新事件/定时器、前端状态条文案、`go test ./agenthub ./desktop/bridge`、tsc/test:hub-conv。

## 5. 子智能体交付契约

1. 只在自己的 worktree 改；不碰主工作树、不 commit/push 到远端。
2. 每个任务独立 commit（message 带 `P7-xx`）；不 amend 已 Review 的 commit。
3. 报告：改动文件/行为、测试命令 + exit code、反证（修复前红/后绿）、`git status --short`、残余风险。
4. 不修改 `docs/agent-hub/GROUP-CHAT-FOLLOWUP.md`、`GROUP-CHAT-HARDENING.md`、`tasks.json`、`PROGRESS.html`。
5. 测试端口不得用 5173/5180；用 52xx，跑完 kill。
