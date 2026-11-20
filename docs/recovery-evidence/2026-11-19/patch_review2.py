# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

# 1) §24 new findings
rep("""| R16 | **P2** | §22 | 工具名/结果形状没有 CodeMode 契约 | 新增 §22 整章 |""",
"""| R16 | **P2** | §22 | 工具名/结果形状没有 CodeMode 契约 | 新增 §22 整章 |
| R17 | **P1** | §9 `artifact_edit` | 先写工作区再 commit：commit 失败会留下「工作区已改、版本没变」的脏状态 | commit 失败时用 base 原子回写（best-effort），并在结果里说明 |
| R18 | **P1** | §10.4 激活时机 | 用户在**普通 run 还在跑**时点「直接改」→ scope 立刻生效，会把**当前这条 run** 的工具一起拦掉 | 激活前检查 `bs.running != nil` → 返回 `session_busy`（前端提示「等这轮结束」）；引用路径不受影响 |
| R19 | **P1** | `subagent/subagent.go:243` | 子 agent 的 `RunOptions` 只继承 Hooks，不继承 `ArtifactRun` → 子 agent 里 `artifact_publish` 拿到 nil，声明丢失（子卡片永远没有包） | 子 RunOptions 加 `ArtifactRun: tc.ArtifactRun`（§21.16） |""")

# 2) §10.4 busy check snippet before "AgentEnd 清 scope"
rep("""AgentEnd 清 scope（`bs.emit` diff）：""",
"""激活前必须确认会话**空闲**（否则会把正在跑的那条 run 一起拦掉）：

```go
// onArtifactPageMessage 的 selection.commit(mode=edit) 分支
if bs.running != nil { // 有 run 在跑：不要改 scope，否则当前 run 的工具立刻被拦
	m.resp(c, false, "session is busy; wait for the current run to finish",
		map[string]any{"code": "session_busy"}, nil)
	return
}
bs.selectionScope.set(ref.ArtifactID, v.Manifest.Entry, files) // 先激活
text := buildSelectionMessage(instruction, ...)
if err := bs.s.Ask(ctx, text); err != nil { ... }             // 再 Ask
```

> `bs.running` 是 Session 的当前运行句柄（`session.Session.running`，有锁）。用「忙就拒绝」
> 而不是排队：排队意味着用户点完没反应，10 分钟 TTL 可能先到期。

AgentEnd 清 scope（`bs.emit` diff）：""")

# 3) §21.16 new subsection before §22
anchor = "## 22. CodeMode 支持（脚本内调用这 9 个工具）"
addition = r'''### 21.16 `subagent/subagent.go`（1 处，产物声明表继承）

**定位**：`subagent/subagent.go:243-258`（`opts := &agents.RunOptions{Depth: childDepth, Name: name}` 与 `tc.Hooks` 继承块）。

```go
	opts := &agents.RunOptions{Depth: childDepth, Name: name}
	// …既有字段…
	if tc := events.ToolContextFrom(ctx); tc != nil {
		// 既有：hooks 继承（DisableHookInherit 可关）
		if tc.Hooks != nil && !t.spec.DisableHookInherit {
			opts.PreToolUse = tc.Hooks.PreToolUse
			opts.PostToolBatch = tc.Hooks.PostToolBatch
			opts.PreCompact = tc.Hooks.PreCompact
		}
+		// 产物声明表必须继承：否则子 agent 的 artifact_publish 拿到 nil，
+		// 声明直接丢弃（子卡片永远没有产物包）。与 Hooks 不同，它不受
+		// DisableHookInherit 影响 —— 那是「用户 hooks 不要传染给子 agent」，
+		// 而声明表是**同一次交付**的一部分。
+		opts.ArtifactRun = tc.ArtifactRun
	}
```

> 子 run 的 `AgentEnd` 带着自己的 `RunId`，`Take(subRunId)` 取到的是它自己的声明；
> 父 run 的声明不受影响（表按 RunId 分桶）。

'''
rep(anchor, addition+anchor)

# 4) compile gate S14
rep("""| S13 | §23.4 Composer chip + §23.3 `selection_refs` 注入 | `cd desktop/app && npx tsc --noEmit`；e2e `verify_selection_ref.mjs` | 过 |""",
"""| S13 | §23.4 Composer chip + §23.3 `selection_refs` 注入 | `cd desktop/app && npx tsc --noEmit`；e2e `verify_selection_ref.mjs` | 过 |
| S14 | §21.16 子 agent 继承 `ArtifactRun` | `go test ./subagent -run TestArtifactRunInherited` | 过 |""")

open(p,'w').write(s)
print('ok')
