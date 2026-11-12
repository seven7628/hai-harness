# 真实 Provider Hub 端到端评测（P5-10）

`desktop/bridge/hub_eval_real_test.go` 里的 `TestHubRealE2E` 用**真实 provider/model**
把 Agent Hub 的 7 条路径各跑一遍。它默认 `t.Skip`，只有显式开门控才发真实（付费）请求。

## 运行

```bash
# 1) 未开门控：必须 skip
go test ./desktop/bridge -run TestHubRealE2E -count=1

# 2) 真实运行（key 由测试自己从真实 ~/.go-code/settings.json 读取，不要打印/落盘）
HAI_EVAL_HUB_REAL=1 \
HAI_EVAL_HUB_PROVIDER=opencode \
HAI_EVAL_HUB_MODEL=deepseek-v4.1-flash \
HAI_EVAL_HUB_REPORT=/tmp/hub-real-e2e/report.json \
  go test ./desktop/bridge -run TestHubRealE2E -count=1 -v -timeout 20m
```

单次等待上限 `hubRealTimeout=120s`（7×120s=840s < `-timeout 20m`）；真实全量通常
4~8 分钟。报告路径必须是**绝对路径**，相对路径会在创建 manager/发请求之前 `t.Fatalf`
（避免 `go test` 的 cwd 把报告写进仓库）。默认路径带 PID + 时间戳：
`/tmp/hub-real-e2e/report-<pid>-<YYYYmmdd-HHMMSS>.json`（绝对路径）。

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `HAI_EVAL_HUB_REAL` | 空 | 必须为 `1`，否则 skip |
| `HAI_EVAL_HUB_PROVIDER` | `opencode` | 真实 provider 名（settings.json 的 provider 键） |
| `HAI_EVAL_HUB_MODEL` | `deepseek-v4.1-flash` | 真实 model |
| `HAI_EVAL_HUB_API_KEY` | 空 | 可选覆盖：key 已迁到 keys.json/safeStorage 时用环境注入 |
| `HAI_EVAL_HUB_REPORT` | `/tmp/hub-real-e2e/report-<pid>-<时间戳>.json` | 报告路径，**必须绝对**；`events.jsonl` 写同目录 |
| `HAI_EVAL_HUB_ONLY` | 空 | 逗号分隔场景名/别名，只跑指定场景；未知名直接 fail；5/6 必须显式同时选 4 |

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
  errors[], notes[], diagnostics[]
}
totals: { scenarios, passed, failed, skipped, llm_calls, input_tokens, output_tokens, cost_usd }
```

`llm` 从 `llm_end.usage` 聚合；`cost_usd = Σ Cost.Total / 1e6`。若上游
`Cost.Total=0`，报告里的 0 **不代表免费**（网关未上报成本），场景 notes 会标注。

失败时也会写报告（defer），并输出诊断：最近 5 条消息、最近 tool/approval/LLM 事件摘要
（不含 key）。真实模型没按要求调工具时立即失败（`agent_end` 已到但目标事件缺失），
不会静默等到超时。

`HAI_EVAL_HUB_ONLY` 的合法名是 `1_dm … 7_project_session`，也接受序号 `1…7` 与短名
（`dm/tool/approval/all/one/handoff/project`）。空项忽略；未知 token、只选 5/6 而没选
`4_group_mention_all`、或最终 `len(scenarios)==0` 都会 `t.Fatalf`（报告 `status=fail`），
不会 `PASS` + 0 场景。无网络自检：`TestHubRealOnlySelection` 与
`TestHubRealReportPathValidation`。

每个场景的 `llm`/`tool`/`approval` 统计按 `run_id` 过滤掉上一场景的迟到事件；场景 6
结束时还会等 B 的 `agent_end` 与 `hubGroupBusy=false`，场景 7 不会串入场景 6 的事件。
场景 6 的 `prompt` 字段记录实际发送的 promptA + promptB（含两段 B prompt）。
