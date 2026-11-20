# 事故恢复状态总览 —— 2026-11-19 大规模误删

> 更新时间：2026-11-19 23:40
> 完整事件记录见 `docs/RECOVERY_2026-11-19.md`
> 防护修复方案见 `docs/fix_rm_guard_proposal.md`

## 恢复总表

| # | 项目 | 状态 | 说明 |
|---|---|---|---|
| 1 | `~/workspace/go-code` 仓库本体 | ✅ **已恢复** | 从 `origin/main` @ `98028fb` 克隆回原位，1776 文件，git 干净，`go build ./...` 通过 |
| 2 | `workdir` 包（S7 新建） | ✅ **已恢复** | GoLand LocalHistory 挖出**原版** `workdir.go`（193 行）+ `workdir_test.go`（8 个测试全绿） |
| 3 | `tools/builtin/workdir.go`（工具层接合） | ✅ **已恢复** | LocalHistory 原版，与代理独立重建结果逐字一致 |
| 4 | S7 轮对 `main.go` 等 30+ 文件的改动 | ✅ **已恢复** | 从 `/private/tmp/main.go.bak3` + `patch_s7*.py` 移植，build/test 通过 |
| 5 | Go 工具链 `~/go/go1.25.5` | ✅ **已恢复** | `GOTOOLCHAIN` 自动重装后拷回原路径，`go version` 正常 |
| 6 | `~/.go-code` 目录骨架 | ✅ **已重建** | 10 个子目录 + 8 个配置文件，应用可正常启动 |
| 7 | **agent-server-g 会话**（49952 条） | ✅ **已恢复** | 从 `/private/tmp/hai-lab` 幸存副本还原，302MB JSONL，已放回 `~/.go-code/sessions/` |
| 8 | `~/.go-code` 其余会话（hai-harness 工作区） | ❌ **不可恢复** | 无磁盘副本 |
| 9 | `agent-server-g` 仓库源码 | ⏳ **待拉取** | 内网 GitLab 可达（HTTP 302），**缺凭据** |
| 10 | 其余 4 个 worktree 的未提交改动 | ❌ **不可恢复** | 无副本 |
| 11 | 本地 11 个未推送提交 | ❌ **不可恢复** | 远端仅 6 分支 + 11 PR，无本地提交 |
| 12 | `~/Desktop` `~/Documents` `~/Downloads` `~/Pictures` `~/Music` | ❌ **不可恢复** | 无副本 |
| 13 | `~/.Trash` 内容（含 181MB `agent-server-g.git`） | ❌ **不可恢复** | 无副本 |
| 14 | `~/Library/Application Support/go-code-desktop*`（16 个） | ❌ **不可恢复** | 无副本 |
| 15 | `~/.local/bin/go-cache-trim.sh` + plist | ❌ **不可恢复** | 无副本（**launchd 任务已卸载+禁用**） |
| 16 | `com.user.tmp-home-sweep` plist + 脚本 | ❌ **不可恢复** | 无副本（**launchd 任务已卸载+禁用**） |

## 被删项目完整清单（从恢复的会话记录里反查得出）

`/Users/admin/cybertron-gogroup/` 下的项目：

| 项目 | 类型 | 恢复途径 |
|---|---|---|
| `agent-server-g` | Go（重构后新架构 `internal/core`） | 内网 GitLab（需凭据）；模块缓存只有 **v1.0.23 旧架构**（`internal/service`），非重构版 |
| `cybertron` | Python / Django | 内网 GitLab（需凭据），模块缓存无 |
| `saibotan-html` | 前端 | 内网 GitLab（需凭据），模块缓存无 |
| `api-test-platform` | 未知 | 同上 |
| `agent-task-gateway-g` / `agent-chat-model-g` / `agent-chat-server-g` / `gaia_agent_server` | Go | 模块缓存有相近仓库（`agent-chat-api-g` 等），非本体 |

`agent-server-g` 的 worktree（= 分支，共 30+ 个，仅 `agent-server-g-refactor` 最活跃，16107 次引用）：

```
agent-server-g-refactor   unmeasured-review    fix-specs5      review-snapshot
step3                      fix-tenant-agenttype swa-c           fix-orgclient
r18-paste                  close-findings       merge-rehearsal p3-manualreply
voice-p2                   legacyfix            slots           r49-highlight
batch10-5xx                align-ee             fix-path-select fix-tenant-agent-scope
review-b1                  ee-p2                adr3            align-rulings
curd                       bootgate             avatar-order    fix13
r13-save-pipeline          ent-c78              ...
```

⚠️ 这些 worktree 的**未提交改动**全部丢失（无副本）。worktree 名 = 分支名，
说明该仓库至少有 30 个活跃分支，其中 `agent-server-g-refactor` 是主力。

## 已停用的东西（防复发）

```
launchctl print-disabled gui/501 | grep -iE "sweep|trim"
  "com.user.tmp-home-sweep" => disabled
  "com.user.go-cache-trim"  => disabled
```

## 发现的根因

`tools/builtin/bash.go` 的 `rm -rf` 防护**只拦宽泛目标**（`.` `..` `/` `~` `$HOME` `*` `.*`），
**不拦家目录 / 工作区下的具体子路径**。实测（`checkHighRisk` 返回空串 = 放行）：

```
rm -rf /Users/admin/workspace/go-code                        => 放行
rm -rf ~/workspace/go-code                                   => 放行
rm -rf $HOME/workspace/go-code                               => 放行
rm -rf /Users/admin/cybertron-gogroup                        => 放行
find /Users/admin -maxdepth 2 -name go-code -exec rm -rf {} + => 放行
```

现场删除模式与该放行集合**完全吻合**。修复方案见 `docs/fix_rm_guard_proposal.md`。

## 恢复工具（已入库）

`session/recover_sessions_test.go` —— 从幸存的 `*.record.json` + `*.record.json.blocks`
把会话还原成可读 JSONL：

```bash
RECOVER_ROOT=<sessions根> RECOVER_OUT=<输出目录> \
  go test ./session/ -run TestRecoverAllSessions -v
```

## 待用户提供的凭据（用于拉回 agent-server-g）

内网 GitLab `https://git.100credit.cn/pd_eng/agentgroup/gogroup/agent-server-g.git`
网络可达（ping 12ms，HTTP 302），但需要认证。机器上**没有**任何凭据
（keychain / `.gitconfig` / `.git-credentials` / `~/.go-code` 均无）。

已确认该仓库的历史分支：`fix-import-task-homeland`、`fix-release-unbind-workflow`、
`noise-reduction`、`terminal-panel`、`agent-server-g-refactor`。

## 验证状态

```
go build ./...              ✅ 通过
go test ./workdir/...       ✅ 8/8 通过
go test ./tools/builtin/... ✅ 通过（32.9s + 14.7s）
go test ./...                ✅ 通过（agents 包 TestAbortStopsBufferedStreamEmission 为 flaky，
                              带改动/不带改动均偶发失败，非本次恢复引入的回归）
go vet ./session/           ✅ 通过
```
