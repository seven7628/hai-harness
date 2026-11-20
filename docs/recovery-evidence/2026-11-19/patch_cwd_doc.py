import io, sys

p = '/Users/admin/workspace/go-code/docs/AGENT_CWD_DESIGN_2026-11-19.md'
lines = open(p, encoding='utf-8').read().split('\n')

# lines list is 0-indexed; file line N == lines[N-1]
start = 722  # "## 8. 开放问题（待调研/讨论回填）"
end = 755    # last line of V7 table row
assert lines[start-1].startswith('## 8.'), lines[start-1]
assert lines[end-1].startswith('| V7'), lines[end-1]

new = '''## 8. 开放问题（调研已回填；以下为**待拍板项**）

### 8.1 调研已答（不再开放）

- ✅ Claude Code 的 Bash **不是持久 shell**，但 cwd 跨调用保持（session 级状态；越界重置+提示；子 agent 不继承）—— §4.1
- ✅ Claude Code 的环境信息**在对话里、不在 system prompt**，每请求重算**只发差异**，载体是 `isMeta` user 消息 —— §4.4
- ✅ Claude Code 的 EnterWorktree 改的是 **session cwd + 写权限锚点 + 配置发现根**，不是进程 chdir —— §4.2
- ✅ hai 的系统提示词是**每 Run 重建一次**（`agent_loop.go:1044`），Run 内不变 —— §5.8
- ✅ hai 的动态注入通道全清单（A/B/C/D/E 五类）—— §5.9
- ✅ hai 的工具结果**直接进 `ac.Messages`**，不经 events→前端 —— §5.10
- ✅ hai 全仓**没有**任何"当前 cwd"的环境层 —— §5.12

### 8.2 待拍板（方案里给出推荐，实施前需确认）

| # | 问题 | 选项 | **推荐** |
|---|---|---|---|
| **Q1** | 文件工具的相对路径基准 | ① 当前 cwd ② workspace root（现状） | **①**（对齐 Claude Code；需同步改文案与契约测试） |
| **Q2** | cd **越界**（出 workspace root）怎么办 | ① 重置回 root + 工具结果提示（Claude Code）② 报错拒绝 ③ 允许但文件工具仍按 root | **①**（既对齐又保安全） |
| **Q3** | cwd 状态是否**进 checkpoint/replay** | ① 不进（内存态，恢复后重发完整 env 块）② 进（持久化 cwd） | **①**（零 checkpoint 风险；恢复后重发全量块即可） |
| **Q4** | rebuildLoop（切模型/切人格）后 cwd | ① 保留 ② 重置 | **①**（cwd 是会话状态，不该被切模型清掉） |
| **Q5** | 子 agent 的 cwd | ① 独立分区（现状 TaskId 分区，起点=root）② 继承父 cwd | **①**（与 Claude Code「子 agent 不继承」一致） |
| **Q6** | worktree 进入方式 | ① `create` 自动切 ② 新增 `enter`/`exit` 操作 ③ 不切，靠模型自己 cd | **②**（对齐 EnterWorktree 的 Scope 规则：只允许进本会话建的） |
| **Q7** | env 块是否**持久化**到 history | ① ephemeral（只进本次请求）② 进 history | **①**（对齐 Claude Code 的 isMeta 语义；不动 checkpoint） |
| **Q8** | 是否提供**跨会话**附加目录（`/add-dir` 等价物） | ① 本期不做 ② 做 | **①**（超出当前诉求，另开） |

---

## 9. 验证计划（总纲；详细测试代码见 §12）

必须能**证伪**的几条：

| # | 验证项 | 方法 | 对应测试 |
|---|---|---|---|
| V1 | cd 后**文件工具**跟着走 | cd 到子目录 → write_file 相对路径 → 断言落在子目录 | §12.3 `TestFileToolsFollowCwd` |
| V2 | 模型**知道** cwd 变了 | 断言下一轮 LLM 请求的 messages 里出现 `# Environment update` | §12.4 `TestEnvUpdateInjected` |
| V3 | 主/子 agent cwd 隔离不回归 | 现有 `bash_cwd_test.go` 保持绿 + 文件工具维度新增隔离测试 | §12.3 `TestCwdIsolatedPerCaller` |
| V4 | 切档位后 cwd 语义 | rebuildLoop 前后断言（按 Q4） | §12.5 |
| V5 | **checkpoint / replay 不受影响** | 沿用我们 events↔checkpoint 的逐字节等价性做法 | §12.6 |
| V6 | 越界不放大 | cd 出 root ⇒ 重置 + 提示；写文件被拒 | §12.3 `TestCwdResetOutsideRoot` |
| V7 | worktree 端到端 | 创建 → enter → 在 worktree 里改文件 → 断言落在 worktree 内 | §12.7 |
| V8 | env 块**不重复发** | 同一 cwd 连续 3 次请求 ⇒ 只发 1 次 | §12.4 `TestEnvNotResentWhenUnchanged` |
| V9 | **不破前缀缓存** | env 块不在 system 里（断言 system 消息字节不变） | §12.4 `TestEnvNotInSystemPrompt` |
'''

lines[start-1:end] = new.split('\n')
open(p, 'w', encoding='utf-8').write('\n'.join(lines))
print('ok, new line count =', len(lines))
