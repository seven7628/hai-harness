# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
old = """	// 产物归档必须在 reducer.Apply 之前：AgentEnd 进 checkpoint 时 bundle 必须已带
	// Version（否则刷新后卡片有入口却打不开）。失败不阻断：Version 留空。
	// 只对根 run 归档：子 agent 的包归它的卡片，父 run 的 checkpoint 不该替它存版本。
	if ae, ok := e.(*events.AgentEnd); ok && len(ae.ArtifactBundles) > 0 &&
		ae.ParentRunId == "" && bs.artifactPublish != nil {
		bs.artifactPublish(ae)
	}"""
new = """	// 产物归档必须在 reducer.Apply 之前：AgentEnd 进 checkpoint 时 bundle 必须已带
	// Version（否则刷新后卡片有入口却打不开）。失败不阻断：Version 留空。
	// **根 run 与子 run 都归档**：子 agent 的包投影在它自己的卡片上（§21.9-5），
	// 没有 Version 那张卡就打不开；多个子 run 写同一个入口时由版本库的内容寻址去重。
	if ae, ok := e.(*events.AgentEnd); ok && len(ae.ArtifactBundles) > 0 && bs.artifactPublish != nil {
		bs.artifactPublish(ae)
	}"""
assert old in s
s=s.replace(old,new,1)
open(p,'w').write(s)
print('ok')
