# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)
rep("**定位**：`desktop/bridge/main.go:764-800` 的 `switch ev := e.(type)` 里 `case *events.AgentEnd:`\n分支（`bs.resetLive()` 之后）。",
    "**定位**：`desktop/bridge/main.go:954` 的 `case *events.AgentEnd:` 分支（在 `switch ev := e.(type)`\n里，`bs.resetLive()` 之后）。")
rep("**定位**：`desktop/bridge/main.go:3600` 一带（`case CmdAsk:` 与 `case CmdAskBatch:` 两个分支）。",
    "**定位**：`desktop/bridge/main.go:3250`（`case CmdAsk:`）与 `3273`（`case CmdAskBatch:`）。")
open(p,'w').write(s)
print('ok')
