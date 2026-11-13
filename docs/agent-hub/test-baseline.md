# 测试基线（P2.5-01）

## 现状

- **已入库**：12 个 helper 定义文件（`agents/agent_loop_test.go`、`skills_inject_test.go`、
  `llm_compressor_test.go`、`backoff_test.go`、`skills_integration_test.go`、
  `subagent/subagent_test.go`、`registry_output_test.go`、`nested_loop_e2e_test.go`、
  `background_contract_test.go`、`provider/openai_test.go`、`tools/engine_test.go`、
  `mcp/manager_test.go`）以及 `skills/testdata/`。
- **仍未入库**：开发树 `.git/info/exclude` 里约 265 个 `*_test.go`（33 个目录）。
  其中 `desktop/bridge` 62、`agents` 26、`provider` 20、`tools/builtin` 20、`session` 18、
  `subagent` 12、`sandbox` 10、`plugin/browser` 10、`tools` 9、`provider/anthropic` 8 等。
- **CI**：`.github/workflows/test.yml` 在 push(main)/PR 跑
  `go test ./... -run '^$'`（编译门）+ `go vet ./...`
  + `go test ./agenthub/ ./agents/ ./subagent/ ./provider/ ./tools/ ./mcp/`。
  tag 发布仍跑完整 `go test ./...`（`release.yml`）。

## 判据

只有**干净检出**能证明基线；本机 `go test` 不代表 CI：

```bash
git worktree add --detach /tmp/hub-clean HEAD
cd /tmp/hub-clean
go test ./... -run '^$'          # 编译门
go test ./agenthub/ ./agents/ ./subagent/ ./provider/ ./tools/ ./mcp/
```

## 逐包补全清单（勾选后更新本文件）

| 包 | tracked 测试 | 仍缺失 | 状态 |
|---|---|---|---|
| `agents` | helper 闭包 | 21 | 待补 |
| `provider` | `openai_test.go` | 19 | 待补 |
| `tools` | `engine_test.go` | 8 | 待补 |
| `tools/builtin` | 0 | 20 | 待补 |
| `subagent` | helper 闭包 | 8 | 待补 |
| `mcp` | `manager_test.go` | 5 | 待补 |
| `session` | 0 | 18 | 待补 |
| `desktop/bridge` | 编译通过 | 62 | 待补（含 flaky/隔离问题） |
| `skills` | `skills_integration_test.go` 间接 | 1 | 待补（fixture 已入库） |

## 已知噪声（补全时必须先处理）

- `desktop/bridge/TestReducerLLMEndDedupAndUsage`：全局价表被其他用例污染，整包跑失败、单跑通过。
- `desktop/bridge/TestHubSessionSendQueuesWhileTargetRunning`：TempDir cleanup 偶发
  `directory not empty`（project 会话的插件/事件收尾竞态）。
- `subagent` 的 `-race` 在干净检出即报 `store_test.go` 的 DATA RACE。
- 真实 provider 探活测试（`TestP0*`）在网关不可用时故意响亮失败；
  `HAI_EVAL_ALLOW_PING_SKIP=1` 可显式降级。
