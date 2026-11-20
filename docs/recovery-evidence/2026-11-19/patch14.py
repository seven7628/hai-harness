# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)

rep("### 21.10 `desktop/bridge/main.go`（8 处）+ `command_registry.go`（1 处）",
    "### 21.10 `desktop/bridge/main.go`（10 处）+ `command_registry.go`（1 处）")

rep("| `desktop/bridge/main.go` | 8 | A3/A4/B | §21.10 |",
    "| `desktop/bridge/main.go` | 10 | A3/A4/B | §21.10 |")

rep("| 文件 | 处数 | PR | 本章 |\n|---|---|---|---|\n| `core/tool.go` | 2 | A1 | §21.3 |",
    "| 文件 | 处数 | PR | 本章 |\n|---|---|---|---|\n| `core/tool.go` | 2 | A1 | §21.3 |")

# append E-main.go-9 / -10 after E-main.go-8 block (before §21.11)
anchor = "### 21.11 前端 store（`checkpoint.ts` 2 处、`useAppStore.ts` 6 处）"
addition = r'''#### E-main.go-9：`emit` 清选区作用域（AgentEnd）

**定位**：`desktop/bridge/main.go:764-800` 的 `switch ev := e.(type)` 里 `case *events.AgentEnd:`
分支（`bs.resetLive()` 之后）。

```go
	case *events.AgentEnd:
		bs.resetLive()
+		// 选区任务结束：恢复常规写面。放在 emit（而不是 publishArtifactBundles）里，
+		// 因为它必须对所有结束路径生效：正常完成 / 中断 / 失败 / 子运行收尾。
+		if bs.selectionScope != nil {
+			bs.selectionScope.clear()
+		}
```

#### E-main.go-10：`CmdAsk` / `CmdAskBatch` 清选区作用域

**定位**：`desktop/bridge/main.go:3600` 一带（`case CmdAsk:` 与 `case CmdAskBatch:` 两个分支）。

```go
	case CmdAsk:
		bs := m.get(ws, sid)
		if bs == nil {
			m.resp(c, false, "session not found", nil)
			return
		}
+		// 用户手动发消息 = 离开选区任务：恢复常规写面（否则 bash 会被一直拦着）。
+		// 放在 m.ask 之前：Ask 入队后 run 可能立刻开始，这里晚一步就有一个窗口。
+		if bs.selectionScope != nil {
+			bs.selectionScope.clear()
+		}
		m.ask(bs, core.Message{Role: "user", Content: text}, ...)
```

> 两个分支都要加（`CmdAskBatch` 同形）。这就是「选区守卫激活晚于 `Ask`」那条修法的落点：
> 激活必须**先于** `Ask`（在 `onArtifactPageMessage` 的 commit 分支里 `set` →
> `buildSelectionPrompt` → `Ask`），清除必须**先于**任何新的 `Ask`。

'''
rep(anchor, addition+anchor)

# counts in §0.3 (A modified 16 -> 17)
rep("| A 基座（检测/判定/版本/预览/产出卡） | 16 | 16 | ~4,200 行 | 是（4 个 PR） |",
    "| A 基座（检测/判定/版本/预览/产出卡） | 17 | 17 | ~4,400 行 | 是（4 个 PR） |")

# §21.2 header count
rep("### 21.2 修改文件总表（10 个 Go + 6 个前端/Electron）",
    "### 21.2 修改文件总表（10 个 Go + 7 个前端/Electron）")

open(p,'w').write(s)
print('ok')
