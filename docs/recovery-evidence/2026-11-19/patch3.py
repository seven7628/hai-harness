# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
L=open(p).read().split('\n')

def sub_line(needle, newtext, occ=0):
    hits=[i for i,l in enumerate(L) if needle in l]
    assert hits, needle
    L[hits[occ]]=newtext

# §7.5 + §21.10-4 field type (2 occurrences, with their comment lines)
sub_line("+\t// selectionScope 当前选区任务的作用域（nil = 无选区任务；见 selection_guard.go）。",
         "+\t// selectionScope 选区写范围守卫（nil = 未启用）；并发安全由它自己的 mu 负责。")
sub_line("+\tselectionScope atomic.Pointer[selectionScope]",
         "+\tselectionScope *selectionScope")
sub_line("\t// selectionScope 当前选区任务的作用域（nil = 无选区任务）。emit 的 AgentEnd",
         "\t// selectionScope 选区写范围守卫（nil = 未启用）。emit 的 AgentEnd 与 Ask 入口")
sub_line("\tselectionScope atomic.Pointer[selectionScope]",
         "\tselectionScope *selectionScope // 并发安全由 selectionScope.mu 负责")

s='\n'.join(L)
def rep(old,new):
    global s
    assert old in s, old[:70]
    s=s.replace(old,new,1)

rep("| N6 | `desktop/bridge/artifact_prompt.go` | `artifactSystemPrompt` | ~25 行 | §6.9 |",
    "| N6 | `desktop/bridge/artifact_prompt.go` | `artifactPromptContract` | ~25 行 | §6.9 |")

start = s.index("**同一文件、同一函数**，在构造 `RunOptions` 的地方（不是 loop）加系统提示：")
end = s.index("组合函数见 §6.9。", start) + len("组合函数见 §6.9。")
new = """**同一文件、同一函数**，在 `buildLoop` 组装系统提示处（`main.go:6483-6501`，`sysPrompt` 拼完
MCP 段之后）追加：

```diff
 	sysPrompt := profile.systemPrompt
 	if … {
 		sysPrompt = sysPrompt + computerPromptGuide + computerAppsGuide()
 	}
 	if txt := mcpInstructions(); txt != "" {
 		sysPrompt = sysPrompt + "\\n\\n## MCP servers\\n" + txt
 	}
+	// 产物契约：只在产物工具真的注册成功时注入 —— 否则模型会被告知用不存在的工具。
+	// 固定前缀（不随轮次/时间变化），prompt caching 不被打穿。
+	if m.artifactsEnabled(ws.path) {
+		sysPrompt = sysPrompt + "\\n\\n" + artifactPromptContract
+	}
```

> 为什么不用 `RunOptions.SystemExtra`：那个字段已经被 `session` 的 `systemExtraForRun`
> 占用（`agents/agent_loop.go:949`），bridge 再覆盖会丢 SessionStart 注入；而这是
> **loop 级、整会话稳定**的内容，放 `buildLoop` 才对（与 `computerPromptGuide` 同层）。"""
s = s[:start] + new + s[end:]

rep(">           void openArtifactBundle(first.id, first.version!, sid)",
    ">           void getState().openArtifactPreview(first, { activate: false, quiet: true })")
rep("> `openArtifactBundle` 是 `useAppStore.ts:5700` 一带新增的 action（§11.2 已给全文），",
    "> `openArtifactPreview` 是 §11.2 已给全文的 store action（`useAppStore.ts:5826` 一带），")
rep("            onOpenBundle={(id, version) => openArtifactBundle(id, version, sid)}",
    "            onOpenBundle={(b) => void openArtifactPreview(b, { activate: true })}")
rep("  onOpenBundle?: (id: string, version: string) => void",
    "  onOpenBundle?: (b: ArtifactBundleItem) => void")

open(p,'w').write(s)
print('ok')
