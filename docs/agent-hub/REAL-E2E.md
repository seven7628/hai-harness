# 真实 Provider Hub 端到端评测（P5-10 + round2）

`desktop/bridge/hub_eval_real_test.go` 里的 `TestHubRealE2E` 用**真实 provider/model**
把 Agent Hub 的 12 条路径各跑一遍（base 1–7 + round2 8–12，后者在
`desktop/bridge/hub_eval_real_round2_test.go`）。它默认 `t.Skip`，只有显式开门控才发
真实（付费）请求。报告协议是 `hub-real-e2e/v2`（round2 增加 needs/bubbles/isolation/
push/merge 证据块）。

## 运行

```bash
# 1) 未开门控：必须 skip
go test ./desktop/bridge -run TestHubRealE2E -count=1

# 2) 真实运行（key 由测试自己从真实 ~/.go-code/settings.json 读取，不要打印/落盘）
HAI_EVAL_HUB_REAL=1 \
HAI_EVAL_HUB_PROVIDER=opencode2 \
HAI_EVAL_HUB_MODEL=deepseek-v4.1-flash \
HAI_EVAL_HUB_REPORT=/tmp/hub-real-e2e-r2/report.json \
  go test ./desktop/bridge -run TestHubRealE2E -count=1 -v -timeout 45m

# 3) 只跑 round2（按稳定性/成本排序，单场景一条命令）
HAI_EVAL_HUB_REAL=1 HAI_EVAL_HUB_PROVIDER=opencode2 HAI_EVAL_HUB_MODEL=deepseek-v4.1-flash \
HAI_EVAL_HUB_ONLY=10 HAI_EVAL_HUB_REPORT=/tmp/hub-real-e2e-r2/report-10.json \
  go test ./desktop/bridge -run TestHubRealE2E -count=1 -v -timeout 45m
```

单次等待上限 `hubRealTimeout=120s`；12 个场景的等待预算（12×120s=1440s）留在
`-timeout 45m` 里。真实全量（1–12）通常 6~10 分钟；只跑 round2 的 8–12 实测 115s
（22 次 LLM 调用）。报告路径必须是**绝对路径**，相对路径会在创建 manager/发请求之前 `t.Fatalf`
（避免 `go test` 的 cwd 把报告写进仓库）。默认路径带 PID + 时间戳：
`/tmp/hub-real-e2e/report-<pid>-<YYYYmmdd-HHMMSS>.json`（绝对路径）。

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `HAI_EVAL_HUB_REAL` | 空 | 必须为 `1`，否则 skip |
| `HAI_EVAL_HUB_PROVIDER` | `opencode2` | 真实 provider 名（settings.json 的 provider 键；`opencode2` 与 `opencode` 同端点、不同配置块） |
| `HAI_EVAL_HUB_MODEL` | `deepseek-v4.1-flash` | 真实 model |
| `HAI_EVAL_HUB_API_KEY` | 空 | 可选覆盖：key 已迁到 keys.json/safeStorage 时用环境注入 |
| `HAI_EVAL_HUB_REPORT` | `/tmp/hub-real-e2e/report-<pid>-<时间戳>.json` | 报告路径，**必须绝对**；`events.jsonl` 写同目录 |
| `HAI_EVAL_HUB_ONLY` | 空 | 逗号分隔场景名/别名，只跑指定场景；未知名直接 fail；5/6 必须显式同时选 4；8–12 无依赖 |

## 隔离与凭证

- 真实 `~/.go-code/settings.json` **只读**：取选中 provider 的配置块，复制到
  `testManager` 的临时 HOME（`HOME`/`USERPROFILE` 都指向 `t.TempDir()`）。真实 HOME 不写。
- 临时 settings.json **不写 `api_key`**（写后自检文件里没有该字段/没有 key 原文）；
  `loadSettings()` 之后用 `m.provCfg.setKey(provider, key)` 只注入内存。SIGKILL /
  超时 / 崩溃都不会在临时盘留下明文 key。
- 测试**不单独注册**删除 settings 的 cleanup：`testManager` 注册的
  `shutdownHubRuntimes` 先跑，`t.TempDir` 的删除后跑（LIFO），不会出现“先删 home
  再收尾”的竞态。
- 报告与 `events.jsonl` 写入前把 key 字符串替换为 `[REDACTED]`；报告不含 base_url；
  失败诊断/`t.Log` 也走同一脱敏。
- `testManager` 已隔离 appdata（Hub store/会话/事件）与全局价表（`isolateUserPrices`）。
- 场景 7 的项目根建在临时 HOME 下（`hub-eval-project-roots/`），不用真实目录。

## 门控 skip 与报告

门控判断在创建 manager **之前**：`HAI_EVAL_HUB_REAL!=1` 时不建 manager、不发请求，
写 `<report>.skip.json` 后 `t.Skip`。skip 报告**不会覆盖** `<report>` 本身，上一次的
真实 pass/fail 报告与 `events.jsonl` 原样保留（没有事件流时不写 events 文件）。
provider 配置缺失同样写 skip 报告并 skip。

## 场景（每项都有结构化断言 + 报告证据）

1. **1_dm**：真实 agent，DM「只回复 HAI_DM_OK」；DM 索引恰好 human+agent 两条。
2. **2_tool_auto**：bash `printf HAI_TOOL_OK`；断言 `tool_run_end` 参数/结果/非错误与最终回复。
3. **3_approval_hitl**：会话切 `hitl`；等 `tool_approval_requested`（bash），
   `Approve(id,true)` 后断言工具成功执行、最终回复含 token。
4. **4_group_mention_all**：2 成员群，人 `@all`；每个成员在触发 seq 之后都有含 token 的
   agent 回复；只应有 1 条 `hub_round`（回复里出现 @ 会触发自动接龙 → 响亮失败）。
5. **5_group_mention_one**：人只 `@B`；round targets 只有 B，B 唤醒发号 +1、有回复；
   A 无新 agent 消息、唤醒发号不变、游标不动。
6. **6_handoff**（四阶段）：`@A` 调 `handoff`（to=B，instruction=只回复 HANDOFF_OK）→
   断言 `hub_handoff` 事件 + 落盘 `pending_ack` + from/to；`@B` 第一阶段必须**实际调用**
   `handoff_ack`（accept=true）→ 断言 accepted 卡片 `message_seq`；`handoff_ack` 的
   工具结果与 `LoadHandoff.Instruction` 必须含 instruction/token。第二阶段在 ack 之后
   发新 prompt（**不含 token**，只给 handoff_id，要求按工具返回的 instruction 回复）→
   B 的最终回复 `seq > accepted 消息 seq`，且来自 ack 之后新起的 `run_id`（不是 A 的
   handoff run、也不是上一场景的旧 run）。
7. **7_project_session**：人 `@ProjectRunner`，要求它自主开项目并派任务：
   - 真实调用 `workspace_create` → `workspace_list` → `session_create` → `session_list`
     → `session_send`（断言每个工具的参数与结果，不是只看模型文本）；
   - 目录真的存在且在 `policy.workspace_roots` 内；session 在 `m.get(wsPath,sid)` 与
     `listSessions` 里可查；
   - `session_send` 返回 `accepted=true` + `task_id`；
   - **项目会话真的跑**：按 `session_id` 过滤到 `llm_end`/`agent_end` 且正文含
     `HAI_PROJECT_SESSION_OK`，会话回到 `running=false`；
   - 账本任务结算为 `completed/delivered`，`Result.Summary` 含 token；
   - 报告记录 workspace 路径、session_id、task_id、项目会话 LLM usage 与最终正文。

8. **8_dm_group_isolation**：同一智能体在 DM 与群里各记一个哨兵（`DM_ONLY_SENT` /
   `GRP_ONLY_SENT`），然后分别在两个作用域里复述。断言三条：①落盘（DM 索引 vs 群日志）
   各自只含本方哨兵；②**真实模型请求**（`recordingProvider` 记录 `StreamRequest` 后
   透传，走生产 `hubLoopOptions` 装配）各自只含本方哨兵；③`hub_agent_prompt_preview`
   的作用域分叉（不带 group_id = `dm`，无群工具/名册；带 group_id = `group`，有
   `## Who is here` 名册层）。模型不肯只回 token 时做 **bounded 1 次**明确重发（记 note）。
9. **9_inline_needs_todos**：harness 先建 hitl 项目会话，群内智能体用一次
   `session_send` 把「bash（需审批）→ todo_add → ask_user → 回复 SC9-DONE」派过去。
   断言：审批 need（`hub_need_resolve approve`）与提问 need（按 need body 里的单题 id
   注入回答）都 `resolved`；`tool_run_end.todos` 含待办哨兵；`ask_user` 的工具结果回显
   回答哨兵；need 形状（origin 名字/target workspace+session/task_id/anchor）经
   `hub_need_list` 复核；resolve 之后 Run 继续跑到 `SC9-DONE`；任何**智能体署名的**
   群/DM 消息都不含 Project 侧哨兵（人类 prompt 里的任务正文不算泄漏）。
10. **10_multi_bubble**：DM 提示词强制「第一轮正文 + bash 工具 → 第二轮正文」。断言
    气泡数 == 非空 `llm_end` 数（**硬断言**，bounded 1 次纠正之后仍不成立就 FAIL）、
    `message_id` 互异、每段 `final` 晚于 `delta`、段内 delta index 从 0 连续。
11. **11_task_result_push**：群内智能体 `session_send` 派 Project 任务。断言账本
    `delivered` 且 summary 含项目哨兵；群里**恰好一张** `task_result` 卡（署名发起者）；
    发起者在**派发轮收尾之后**新起一轮 run 且只落一条总结气泡（基线见 `PushEndBase`，
    不被派发轮自己的 agent_end 平凡满足）；发起者请求里真的有结果正文；旁观者 0 次
    LLM、0 张卡、0 条 @ 路由；重放同一结算 + 再排一次通知仍只有 1 张卡 / 1 次
    `task_result_delivered`。
12. **12_session_send_merge**：一次 `session_send` 同时带 `command=compact` + text +
    files + images。断言参数四要素（成功调用**恰好一次**；失败重试记 note）、回执
    `compact/delivered`、账本两个附件（归一化路径/名字/字节数，无 data URL）、项目侧
    事件序恰好 `agent_start→compress_start→compress_end→llm_end→agent_end`、只有 1 个
    `agent_start`、`compress_end.reason=manual` / `error` 为空 / `before>0`、正文那轮
    真的回答了 text 里的 token。预热历史由 harness 用 fake loop 预置 3 轮后还原生产
    loop；PNG 由 harness 写（不让模型生成）。

## 报告 schema（`report.json`）

```
protocol, started_at, finished_at, duration_ms, provider, model, gate,
status(pass|fail|skipped), skip_reason, report_path, events_path,
scenarios[]: {
  name, status, duration_ms, prompt, final_text, agent_replies[],
  messages[], tool_calls[], approvals[], handoffs[], llm{}, llm_errors[],
  project? { workspace_path, session_id, task_id, task_status, accepted,
             workspace_create/workspace_list/session_create/session_list/session_send
             （参数+结果+is_error）, session_llm{}, session_final_text,
             task_result_status, task_result_finish_reason, task_result_summary },
  isolation? { dm{session_id,messages,has_dm_sentinel,has_group_sentinel,reply},
               group{...}, dm_request{calls,has_dm_sentinel,has_group_sentinel,prompt_len},
               group_request{...}, preview_dm_scope, preview_group_scope,
               preview_dm_layers[], preview_group_layers[], attempts, pass },
  bubbles?  { session_id, agent_id, attempts, total_llm_ends, non_empty_llm_ends,
              bubbles[]{seq,message_id,text,delta_events,delta_indexes,final_seen,
                        delta_before_final}, distinct_ids, pass },
  needs?    { project_session_id, project_workspace, task_id,
              approval{...}/question{...}（id/kind/status/anchor/session/workspace/
              task_id/agent_id/group_id/origin_*/target_*/run_id/depth/title/body）,
              approval_status, question_status, ask_question_id,
              todo_run_end{}, todo_has_sentinel, ask_run_end{}, ask_answer_echoed,
              bash_run_end{}, project_agent_end, project_done,
              group_agent_leak[], dm_agent_leak[], pass },
  push?     { task_id, group_id, status, delivered, summary, cards, card_seq,
              card_sender, card_text, origin_new_run, origin_agent_ends,
              origin_bubbles, origin_bubble_text, origin_request_has_result,
              bystander_llm_ends, bystander_task_result, routing_rounds,
              delivered_events, replay_cards, replay_delivered_events, pass },
  merge?    { group_session_id, project_session_id, project_workspace, task_id,
              session_send_calls, args_command, args_text_has_token, args_files[],
              args_images[], accepted, queued, receipt_name, receipt_delivered,
              ledger_command, ledger_attachments[], event_order[], agent_starts,
              compress_reason, compress_error, compress_before, compress_after,
              compress_summary_len, llm_end_text, answer_echoes_token, warmup_runs,
              pass },
  errors[], notes[], diagnostics[]
}
totals: { scenarios, passed, failed, skipped, llm_calls, input_tokens, output_tokens, cost_usd }
```

`llm` 从 `llm_end.usage` 聚合；`cost_usd = Σ Cost.Total / 1e6`。若上游
`Cost.Total=0`，报告里的 0 **不代表免费**（网关未上报成本），场景 notes 会标注。

失败时也会写报告（defer），并输出诊断：最近 5 条消息、最近 tool/approval/LLM 事件摘要
（不含 key）。真实模型没按要求调工具时立即失败（`agent_end` 已到但目标事件缺失），
不会静默等到超时。

`HAI_EVAL_HUB_ONLY` 的合法名是 `1_dm … 12_session_send_merge`，也接受序号 `1…12`
与短名（`dm/tool/approval/all/one/handoff/project` +
`isolation/needs/bubble/push/merge`）。空项忽略；未知 token、只选 5/6 而没选
`4_group_mention_all`、或最终 `len(scenarios)==0` 都会 `t.Fatalf`（报告 `status=fail`），
不会 `PASS` + 0 场景。无网络自检：`TestHubRealOnlySelection` 与
`TestHubRealReportPathValidation`。

round2 的 8–12 各自建智能体/群/工作区，没有跨场景依赖；`isolation`/`bubbles`/`needs`/
`push`/`merge` 五块证据都由 `sync()` 从事件流、消息、账本与 recorder 快照重建（失败报告
同样可读）。每个场景的 `llm`/`tool`/`approval` 统计按 `run_id` 过滤掉上一场景的迟到事件；场景 6
结束时还会等 B 的 `agent_end` 与 `hubGroupBusy=false`，场景 7 不会串入场景 6 的事件。
场景 6 的 `prompt` 字段记录实际发送的 promptA + promptB（含两段 B prompt）。
