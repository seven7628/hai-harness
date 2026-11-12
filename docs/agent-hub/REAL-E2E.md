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

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `HAI_EVAL_HUB_REAL` | 空 | 必须为 `1`，否则 skip |
| `HAI_EVAL_HUB_PROVIDER` | `opencode` | 真实 provider 名（settings.json 的 provider 键） |
| `HAI_EVAL_HUB_MODEL` | `deepseek-v4.1-flash` | 真实 model |
| `HAI_EVAL_HUB_API_KEY` | 空 | 可选覆盖：key 已迁到 keys.json/safeStorage 时用环境注入 |
| `HAI_EVAL_HUB_REPORT` | `/tmp/hub-real-e2e/report.json` | 报告路径；`events.jsonl` 写同目录 |
| `HAI_EVAL_HUB_ONLY` | 空 | 逗号分隔场景名，只跑指定场景（调试/省钱；5/6 依赖 4 建的群） |

## 隔离与凭证

- 真实 `~/.go-code/settings.json` **只读**：取选中 provider 的配置块，复制到
  `testManager` 的临时 HOME（`HOME`/`USERPROFILE` 都指向 `t.TempDir()`），随后
  `m.provCfg.loadSettings()`。真实 HOME 不写。
- key 只存在于临时 settings.json（0600）与进程内存；测试 cleanup 删除该文件。
- 报告与 `events.jsonl` 写入前把 key 字符串替换为 `[REDACTED]`；报告不含 base_url。
- `testManager` 已隔离 appdata（Hub store/会话/事件）与全局价表（`isolateUserPrices`）。
- 场景 7 的项目根建在临时 HOME 下（`hub-eval-project-roots/`），不用真实目录。

## 场景（每项都有结构化断言 + 报告证据）

1. **1_dm**：真实 agent，DM「只回复 HAI_DM_OK」；DM 索引恰好 human+agent 两条。
2. **2_tool_auto**：bash `printf HAI_TOOL_OK`；断言 `tool_run_end` 参数/结果/非错误与最终回复。
3. **3_approval_hitl**：会话切 `hitl`；等 `tool_approval_requested`（bash），
   `Approve(id,true)` 后断言工具成功执行、最终回复含 token。
4. **4_group_mention_all**：2 成员群，人 `@all`；每个成员在触发 seq 之后都有含 token 的
   agent 回复；只应有 1 条 `hub_round`（回复里出现 @ 会触发自动接龙 → 响亮失败）。
5. **5_group_mention_one**：人只 `@B`；round targets 只有 B，B 唤醒发号 +1、有回复；
   A 无新 agent 消息、唤醒发号不变、游标不动。
6. **6_handoff**：`@A` 调 `handoff`（to=B, instruction=只回复 HANDOFF_OK）→ 断言
   `hub_handoff` 事件 + 落盘 `pending_ack` + from/to；`@B` 必须**实际调用** `handoff_ack`
   （accept=true）→ 断言 accepted + B 最终回复含 HANDOFF_OK。
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
