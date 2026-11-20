# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)

rep("""```diff
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
```""",
"""```diff
@@ -6483,6 +6483,14 @@ func (m *manager) buildLoop(...) (*agents.AgentLoop, error) {
 	sysPrompt := profile.systemPrompt
 	if plg := ws.plugins; plg != nil {
 		if cap, ok := plg.Get(computerplugin.ID); ok {
 			if ec, ok2 := cap.(interface{ IsEnabled() bool }); ok2 && ec.IsEnabled() {
 				sysPrompt = sysPrompt + computerPromptGuide + computerAppsGuide()
 			}
 		}
 	}
@@ -6496,6 +6496,12 @@ func (m *manager) buildLoop(...) (*agents.AgentLoop, error) {
 	if mm := ws.mcp; mm != nil {
 		ictx, icancel := context.WithTimeout(context.Background(), mcpInstructionsTimeout)
 		if txt := mcp.InstructionsText(mm.Instructions(ictx)); txt != "" {
 			sysPrompt = sysPrompt + "\\n\\n## MCP servers\\n" + txt
 		}
 		icancel()
 	}
+	// 产物契约：只在产物工具真的注册成功时注入 —— 否则模型会被告知用不存在的工具。
+	// 固定前缀（不随轮次/时间变化），prompt caching 不被打穿。
+	if m.artifactsEnabled(ws.path) {
+		sysPrompt = sysPrompt + "\\n\\n" + artifactPromptContract
+	}
```""")

# prompt contract diff: replace placeholder with real preceding bullet
rep(""" const artifactPromptContract = `## Artifacts
 -- 以上不动 --
 - Do not claim a preview works unless ` + "`preview.screenshot`" + ` or the tool result confirms it.`""",
""" const artifactPromptContract = `## Artifacts
-- When the user should see a page, report, or document, write it under ...
-- After writing the entry, call `artifact.publish` once with its path. ...
-- Do not publish scratch/tmp/debug/test files. ...
+- 以上三条不动（完整文本见 §6.9）
 - Do not claim a preview works unless ` + "`preview.screenshot`" + ` or the tool result confirms it.`""")
open(p,'w').write(s)
print('ok')
