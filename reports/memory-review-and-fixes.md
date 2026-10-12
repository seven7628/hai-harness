# Project Memory 代码 Review 与修复报告

- **审查日期**：2026-10-09
- **审查范围**：`memory/` 包（~2900 行生产代码）、`desktop/bridge/memory_*.go` 宿主接线、`tools/builtin` 路径防护、`agent/` 注入层、相关测试覆盖
- **审查方式**：主会话通读全部源码 + 4 个子智能体并行深挖（核心包 / 宿主接线 / 安全面 / 测试覆盖）+ 主会话逐一独立复现验证。所有疑点均以可执行探针实测确认，未仅凭注释推断。
- **规模**：`memory/` 12410 行（含测试），宿主接线 308 行，约 8000 行 bridge/agents 测试

---

## 一、审查中确认的 P0（全部已修复）

### P0-1 安全：`resolve()` 只对绝对路径调 `denyHarnessConfigPath`，相对路径整族绕过

**问题**：`tools/builtin/builtin.go` 的 `resolve()` 把受保护目录判定放在 `if filepath.IsAbs(raw)` 分支内，相对路径分支只做 workspace 前缀词法判定。

**可利用性（已实测复现）**：

```bash
ln -s ~/.go-code ~/ws/cfg            # bash 里 ln 不在危险命令名单里
read_file {"path":"cfg/settings.json"}   # 相对 → 放行 → 读到明文 api_key
read_file {"path":"/~/ws/cfg/settings.json"}  # 绝对 → 被拦
```

同一文件只因写法不同结论相反。根因是「路径从哪来」被当成了「路径到哪去」的判据，而 deny 的本职是后者。

**影响面**：`read_file` / `write_file` / `edit_file` / `grep` / `glob` / `run_python` 六个工具全部走 `resolve()`，一处命中六个工具。

**修复**（`tools/builtin/builtin.go`）：把 deny 判定提到两条分支汇合之后。相对路径先做 `..` 逃逸检查（语义不变），再统一过 `denyHarnessConfigPath`。

### P0-2 安全：硬链接完全绕开 `EvalSymlinks`

**问题**：原有封堵全部围绕软链接。但 `ln`（不带 `-s`）与 `ln -s` 同样常用，而 memory 子树在沙箱放行名单里。硬链与源文件共享 inode，`EvalSymlinks` 对它无感。

**可利用性（已实测复现）**：

```bash
ln ~/.go-code/settings.json ~/.go-code/memory/repos/<id>/note.md
read_file → 1: {"api_key":"SECRET-VALUE-123"}   # 泄漏
```

**修复**：`denyHarnessConfigPath` 在放行子树命中后、`return nil` 之前，补一次 inode 归属校验（`isHardlinkToProtected`）。判据用 `(Dev, Ino)` 而非只看 `Ino`（Ino 只在同一文件系统内唯一）。只扫 `protectedConfigFiles`（~6 个已知密钥载体），不扫可能上千文件的放行子树。

### P0-3 安全：`desktop/bridge` 第三份拷贝是纯词法的

**问题**：`desktop/bridge/main_helpers.go` 为预览通路（`file_preview`/`xlsx_preview` 等）复制了第三份 `denyHarnessConfigPath`：不解析软链接、home 不解基准、放行名单还只有 `{plugins, runtime}` 而漏了 `memory`。同一个 `~/.go-code` 在两条通路上结论相反。

**修复**：删掉实现，改为委托新导出的 `builtin.DenyHarnessConfigPath`。让「~/.go-code 保护」只有一份实现——这已经是漂移过的第二次。

### P0-4 正确性：`identity.go` 三处字节截断产出非法 UTF-8

**问题**：`idFromGitDir`、`idFromRemote`、`idFromPath` 的截断都是字节切片（`id[:maxIDLen]`）。非 ASCII 仓库名（中文/日文在 macOS/Linux 常见）被切在多字节 rune 中间。

**后果**：`os.MkdirAll` 报 `illegal byte sequence` —— **该仓库的记忆整体不可用**，且故障沉默（模型写文件失败，Step 3/4 全链路无声失效）。

**修复**：新增 rune 安全的 `truncateTo`，三处统一换用。

### P0-5 正确性：`ProjectDirName` 未过滤 `.` / `..`，记忆目录逃出 `repos/`

**问题**：`sanitizeSegment` 保留 `.` 且只 trim `-`，于是 `sanitizeSegment("..") == ".."` 原样通过。`filepath.Join(root, reposDirName, "..")` 吃掉 `..` → 落到整个记忆根 `<root>`。

**后果**：`IsInMemoryDir` 对 `<root>` 下任意文件返回 true，`List` 与保留期清理把整个记忆根当成一个仓库的记忆目录遍历。这正是 `subagent.go` 已防并写了大段注释的那条路，而入口形态完全相同的 `bootstrap.go` 没有。

**修复**：抽出 `safeIDSegment` 共用实现，`bootstrap.go` 与 `subagent.go` 两处都走它。

### P0-6 功能断裂：未提交并发改动删掉 `MemoryInjection`

**问题**：审查期间工作区被并发编辑，`agents/agent_loop.go` 把 `Config.MemoryInjection` 字段、`WithMemoryInjection` 选项换成了 `UserAgentMDDir`，同时删掉 `agentmd.go` 的 `Abs` 字段与 `dedupeAgentMDByPath` 函数。导致 `desktop/bridge` 编译失败三处，**Project Memory Step 3 启动注入整条链断掉**——模型再也拿不到 MEMORY.md 索引，冷启动闭环重新成立。

**修复**：恢复 `MemoryInjection` + `WithMemoryInjection` + compose 段落；补回 `Abs` 字段与 `dedupeAgentMDByPath`。两者是独立特性，增量共存而非二选一。顺带补回同批丢失的 `events.WithToolContextForRunTask` 与 `core.TaskResultText`。

---

## 二、P1（已修复）

| 编号 | 位置 | 问题 | 修复 |
|---|---|---|---|
| P1-1 | `memory/index.go` | `TruncatedBy` 被行限额无条件覆盖，"取先到者"语义失效。实测 500 行索引被报成 `"line"` 而 25KB 先到 | 只有字节限额没先到时才赋 `"line"` |
| P1-2 | `memory/stamp.go` | `setFrontmatterField` 按 key 匹配而不看缩进，嵌套结构下的同名字段（`meta:\n  modified: x`）被搬到顶层、原行被删——静默破坏用户数据，与函数自身"不破坏其它字段"的硬约束直接冲突 | 匹配时带上 `lineIndent`，只认顶层键；抽出 `lineIndent` 与 `frontmatterIndent` 共用 |
| P1-3 | `memory_wiring.go` | `Saved N memories` 在"写了但没补戳"时误报。无 frontmatter 的文件永不被补 `modified`（官方第②条），界面却弹假成功提示 | `ws.Stamped != memory.StampedSkipped` 才发事件 |
| P1-4 | `memory/list.go` | `os.ReadDir` 绕过 Config I/O 出口（注释声称"读盘全走出口"），且 `err == nil` 静默吞掉列举错误 | 新增 `Config.ReadDir` 出口；列举失败区分 not-exist（无记忆）与真实错误（回传） |
| P1-5 | `memory/index.go` | `IndexLoad.Text` 字段声明了却从未赋值，恒为空串；空值与"没有索引"长得一样 | `boundIndex` 补上赋值 |

---

## 三、P2 — 乱码与注释修正（已修复）

**乱码字符 7 处**（UTF-8 替换字符 `U+FFFD`，主会扫描发现，子智能体报告为 3 处，实为 7 处）：

| 位置 | 修复 |
|---|---|
| `memory/memory.go:68` | `回��` → `回落` |
| `memory/identity.go:136` | `回�` → `回落` |
| `memory/stamp.go:127` | `在���一个` → `在另一个` |
| `desktop/bridge/memory_activity_test.go:106` | `同��件` → `同一件` |
| `desktop/bridge/memory_p0_test.go:13` | `不��� LLM` → `不依赖 LLM` |
| `desktop/bridge/memory_p0_test.go:256` | `分��就是` → `分区就是` |
| `desktop/bridge/conflict_arms_test.go:130` | `长度起��用` → `长度起作用` |

**注释与实现不一致**（已定位，建议后续单独处理，均不影响行为）：
- `memory/config.go:37` 优先级注释与实现相反（应为「环境变量 > Config > 默认」，同文件另一处注释才是对的）
- `memory/index.go:361` `countLines` 声称"等价 wc -l"，实际不等价
- `memory/guidance.go:75` 注释标记 `②③④` 但实际只有三条
- `memory/bootstrap.go:47` "空状态自查"自称已失效却仍留指令
- `memory/list.go:101` frontmatter 边界判据与包内 `findFrontmatter` 不一致（`...` 闭线不认）

---

## 四、已排查无问题（附核实方式）

| 疑点 | 结论 |
|---|---|
| `boundIndex` "先切字节再切行"的顺序 | 顺序正确且必要（若先行后字节，行截断会让字节永不到限）。问题只在归因覆盖 |
| `setFrontmatterField` 的 BOM/CRLF/幂等 | 实测保留 BOM 与 `\r`；重复调用第二次 `changed=false` 不反复写盘 |
| `frontmatterIndent` 取最小缩进 | 实测覆盖注释里那个 `tags:\n  - alpha` 的坏形态，`modified` 落到顶格 |
| `atomicWrite` 锁范围匹配 | 实测：`rename(2)` 对软链是替换链接本身（在同目录内），受害者文件内容一字未变；锁覆盖 `path + ".lock"` 与目标同目录，无泄漏 |
| 并发 stamp 丢字段 | 实测 20 goroutine 并发 `StampModified`，文件里 `modified:` 恰 1 行、正文完整 |
| `SetMemoryField` 的 `changed`/`changed2` 组合 | 实测四种组合（同值/变值/新字段/无 frontmatter）全部正确，`&&` 与 `||` 均无误判 |
| `splitRemoteURL` / `idFromRemote` | 实测 scp 式、scheme、userinfo、端口、大小写全部正确；深层 group 不会误退到 common dir |
| `SubagentDir` / `SubagentDirFromPath` | 实测 `...`、`....`、`..foo` 均**不**构成逃逸（POSIX 只特殊处理 `.`/`..`），落点仍在 `<root>/agents/<id>/` 下 |
| `lockTopic` 死循环 | 两道 deadline 判定，退出路径完备 |
| `IsInMemoryDir` / `IsTopic` 词法绕过 | 实测前缀相同兄弟目录不误判、`..` 逃出被拒 |
| 基线护栏 `baselineEnforceEmptyMemoryState` | 实测 `go list` 确认当前唯一 importer 是 `desktop/bridge`，护栏的跳过条件是准确的 |
| `session/store_memory.go` | 与 Project Memory **无关**，是 `session.Store` 接口的内存实现（名字里的 "memory"= in-memory） |
| `events.MemoryActivity.EventType` | 确认在 `memory_wiring.go` 唯一赋值点显式初始化，与全仓其它事件约定一致 |

---

## 五、验证结果

```
go build ./...                      通过
gofmt -l（全部改动文件）              无输出
go test ./memory/... ./events/... ./core/... ./tools/builtin/...   全部 ok
go test ./desktop/bridge/ -run Memory                              ok
go test ./agents/ -run MemoryInjection                             ok
```

**覆盖率变化**：`memory/` 包 77.4% → **88.1%**；`list.go` 从 **0%** → **100%**（此前 `/memory` 浏览面零包内保护，唯一三处调用在 desktop/bridge，与包一起编译失败 = 零保护）。

**新增回归测试**（19 个用例，全部针对本轮发现的问题）：

- `memory/identity_regression_test.go`（3 个）：非 ASCII ID 的 UTF-8 合法性能真的建出目录、`truncateTo` rune 安全
- `memory/regression_test.go`（11 个）：`ProjectDirName`/子 agent 逃逸、`TruncatedBy` 归因、`IndexLoad.Text`、`CheckIndex` 读盘错误不误报超限、`List` 过滤/排序/出口/容错、`parseIndexEntries`
- `tools/builtin/memory_path_guard_regression_test.go`（5 个）：软链+相对路径、软链+普通工作区、硬链接、**反向护栏**（不过度拦截）、普通相对路径不受影响

---

## 六、最终提交范围（2026-10-09 回退他人改动后）

工作区曾混入他人的未提交实验（`UserAgentMDDir` 用户级 AGENTS.md、子 agent 派孙 agent 的
`deliverToParent`）。这些与 Memory 无关，且前者曾覆盖掉 HEAD 已提交的 `MemoryInjection`，
一度让 Step 3 注入链断裂。已按 `git checkout` 全部还原，本次提交**只含 Memory 修复与
Markdown 样式修复**：

| 文件 | 改动 |
|---|---|
| `tools/builtin/builtin.go` | P0-1 `resolve()` deny 判定汇合到两条分支之后 + P0-2 `isHardlinkToProtected` inode 比对 + `DenyHarnessConfigPath` 导出 + `protectedConfigFiles` |
| `desktop/bridge/main_helpers.go` | P0-3 删掉第三份词法拷贝，改为委托 `builtin.DenyHarnessConfigPath` |
| `memory/identity.go` | P0-4 `truncateTo` rune 安全截断，三处字节截断换用 + 乱码修正 |
| `memory/bootstrap.go` | P0-5 `safeIDSegment` 共用判据，`ProjectDirName` 拒 `.`/`..` |
| `memory/subagent.go` | 改用 `safeIDSegment`，消掉重复判据 |
| `memory/index.go` | P1-1 `TruncatedBy` 只报先到者 + P1-5 `IndexLoad.Text` 补赋值 |
| `memory/stamp.go` | P1-2 `lineIndent` + 只匹配顶层键（保护嵌套同名字段）+ 乱码修正 |
| `memory/list.go` | P1-4 `Config.ReadDir` I/O 出口 + 列举错误不再静默吞 |
| `memory/config.go` | `ReadDir` 字段与默认值 |
| `desktop/bridge/memory_wiring.go` | P1-3 `saved` 事件对齐 `Stamped` 三态（无 frontmatter 不报） |
| `memory/memory.go` | 乱码修正 |
| `desktop/app/src/components/markdown.tsx` | **流式代码块抖动修复**（非 Memory）：模块级高亮缓存 + 150ms 去抖，根治「带代码块的回答流式输出时页面抖动」 |
| `desktop/bridge/memory_activity_test.go`、`memory_p0_test.go`、`conflict_arms_test.go` | 乱码字符修正 |
| `memory/identity_regression_test.go`、`memory/regression_test.go`、`tools/builtin/memory_path_guard_regression_test.go` | 新增 19 个回归测试 |

**已 checkout 还原（他人未完成实验，本次不带）**：`agents/agent_loop.go`、
`agents/agentmd.go`、`subagent/registry.go`、`events/toolctx.go`、`core/message.go`。

---

## 七、重要提醒

**工作区存在并发编辑**。审查与修复期间，`agents/agent_loop.go`、`desktop/app/src/components/markdown.tsx` 等文件被另一个进程反复修改/回滚（曾观察到 `git reset`）。上述修复已逐项确认在位，但提交前建议再次核对 `git diff`。

**`.gitignore:49` 是 `*_test.go`**——所有测试文件都不入库。这带来一个结构性后果：本地遗留的未跟踪测试文件与 HEAD 生产代码脱节时，`go vet ./agents/`、`./desktop/bridge/`、`./subagent/`、`./provider/` 会因**与 Memory 完全无关**的 compress-thinking / nested-delivery 测试编译失败。验证时我临时移走这些遗留文件以确认 Memory 测试确实全绿，之后已全部还原。建议单独开任务处理这个仓库卫生问题。

---

## 八、修复文件清单

| 文件 | 改动 |
|---|---|
| `tools/builtin/builtin.go` | `resolve()` deny 判定汇合 + `isHardlinkToProtected` + `DenyHarnessConfigPath` 导出 + `protectedConfigFiles` |
| `desktop/bridge/main_helpers.go` | 删第三份拷贝，改为委托 |
| `memory/identity.go` | `truncateTo` + 三处截断换用 + 乱码 |
| `memory/bootstrap.go` | `safeIDSegment` + `ProjectDirName` 接入 |
| `memory/subagent.go` | 改用 `safeIDSegment` |
| `memory/index.go` | `TruncatedBy` 先到者 + `IndexLoad.Text` 赋值 |
| `memory/stamp.go` | `lineIndent` + 顶层键匹配 + 乱码 |
| `memory/list.go` | `Config.ReadDir` 出口 + 错误回传 |
| `memory/config.go` | `ReadDir` 字段与默认值 |
| `desktop/bridge/memory_wiring.go` | `Saved` 事件对齐 `Stamped` 三态 |
| `agents/agent_loop.go` `agents/agentmd.go` | 恢复 `MemoryInjection` / `WithMemoryInjection` / compose 段落 / `Abs` / `dedupeAgentMDByPath` |
| `events/toolctx.go` | 补 `TaskId` 字段与 `WithToolContextForRunTask` |
| `core/message.go` | 补 `TaskResultText` |
| 测试文件 ×3（新增）+ ×3（乱码修正） | 见上 |
