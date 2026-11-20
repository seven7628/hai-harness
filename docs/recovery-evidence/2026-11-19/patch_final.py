# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

# 1) selectionRefStore.Drop
rep('''// Sweep 周期清理（挂在 revisit 清扫器上，与 artifact_sweep 同一个 tick）。''','''// Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。
func (s *selectionRefStore) Drop(ws, sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.refs {
		if r.WS == ws && r.SID == sid {
			delete(s.refs, id)
		}
	}
}

// Sweep 周期清理（挂在 revisit 清扫器上，与 artifact_sweep 同一个 tick）。''')

# 2) §21.1 rows
rep("| N14b | `desktop/bridge/artifact_commands.go` |",
    "| N14c | `desktop/bridge/artifact_toolbase.go` | `artifactToolBase` / `version` / `readVersionFile` / `emit` / `lock` | ~110 行 | §22.3 |\n| N14d | `desktop/bridge/selection_refs.go` | `selectionRefStore` / `Put` / `Get` / `Drop` / `Sweep` | ~120 行 | §23.2 |\n| N14b | `desktop/bridge/artifact_commands.go` |")

# 3) manager field
rep('''	// artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：
	// 内容寻址去重后版本不变时不重复 reload（避免每次都闪一下）。
	artifactNotified sync.Map // map[string]string''','''	// artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：
	// 内容寻址去重后版本不变时不重复 reload（避免每次都闪一下）。
	artifactNotified sync.Map // map[string]string
	// selectionRefs 选区引用表（页面选中 → Composer chip；§23.2）。
	selectionRefs *selectionRefStore''')

# 4) serve init
rep('''	m.artifactPreview = newArtifactPreviewHost(m.artifacts, m.onArtifactPageMessage)''',
'''	m.artifactPreview = newArtifactPreviewHost(m.artifacts, m.onArtifactPageMessage)
	m.selectionRefs = newSelectionRefStore()''')

# 5) count 10 -> 11
rep("### 21.10 `desktop/bridge/main.go`（10 处）+ `command_registry.go`（1 处）",
    "### 21.10 `desktop/bridge/main.go`（11 处）+ `command_registry.go`（1 处）")
rep("| `desktop/bridge/main.go` | 10 | A3/A4/B | §21.10 |",
    "| `desktop/bridge/main.go` | 11 | A3/A4/B | §21.10 |")

# 6) add E-main.go-11 before §21.11
anchor="### 21.11 前端 store（`checkpoint.ts` 2 处、`useAppStore.ts` 6 处）"
addition = r'''#### E-main.go-11：会话删除时清两张表

**定位**：`desktop/bridge/main.go:4773-4785`（`case CmdDeleteSession:` 调 `m.deleteSession` 处）
或 `deleteSession` 函数体末尾（二选一，**不要两处都加**）。

```go
		m.deleteSession(ws, sid)
+		// 两张 per-session 表必须跟着删：长跑 bridge 会按会话无限增长。
+		//   - artifactRuns：产物声明表（key = ws + "\x00" + sid）
+		//   - selectionRefs：选区引用（30 分钟 TTL 之外还有会话级上限）
+		m.artifactRuns.Delete(ws + "\x00" + sid)
+		if m.selectionRefs != nil {
+			m.selectionRefs.Drop(ws, sid)
+		}
```

> 为什么不在 `bridgeSession` 上挂 finalizer/GC：Go 的 sync.Map 没有弱引用语义，
> 「会话对象没人引用」不会自动清 map 条目；显式删是唯一确定的做法。

'''
rep(anchor, addition+anchor)

# 7) compile gate S12
rep('''| S11 | N19 + E-session guard（已在 S4）+ E-main.go-7 的 selection 分支 + N16 `source.*` | `go test ./desktop/bridge -run TestSelectionGuard` | 过 |''',
'''| S11 | N19 + E-session guard（已在 S4）+ E-main.go-7 的 selection 分支 + N16 `source_*` | `go test ./desktop/bridge -run TestSelectionGuard` | 过 |
| S12 | N14c/N14d（toolbase + selection refs）+ §22 的 Exposure/Annotations/Structured | `go test ./desktop/bridge -run 'TestArtifactTool|TestSelectionRef'` | 过 |
| S13 | §23.4 Composer chip + §23.3 `selection_refs` 注入 | `cd desktop/app && npx tsc --noEmit`；e2e `verify_selection_ref.mjs` | 过 |''')

open(p,'w').write(s)
print('ok')
