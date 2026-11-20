# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)

rep("desktop/bridge/artifact_embed.go\ndesktop/bridge/artifact_commands.go",
    "desktop/bridge/artifact_embed.go\ndesktop/bridge/artifact_prompt.go\ndesktop/bridge/artifact_commands.go")

# §21.1: add electron verify file + bridge.ts
rep("| N23 | `scripts/artifact_eval.py` | 指标评估 | ~40 行 | §18.3 |",
"""| N23 | `scripts/artifact_eval.py` | 指标评估 | ~40 行 | §18.3 |
| N25 | `desktop/app/electron/artifact-verify.ts` | `verifyArtifact` / `screenshotArtifact` / `inspectArtifact` | ~120 行 | §11.7.1 |
| N26 | `desktop/app/src/lib/bridge.ts`（**修改**） | `artifactVerify` / `artifactScreenshot` / `artifactInspect` | +12 行 | §11.7 |""")

# §21.2 add bridge.ts row
rep("| `desktop/app/src/components/Narrative.tsx` | 1 | A3 | §21.12 |",
"""| `desktop/app/src/lib/bridge.ts` | 1（3 个函数） | A4 | §21.13 |
| `desktop/app/src/components/Narrative.tsx` | 1 | A3 | §21.12 |""")

# §21.13 add bridge.ts section before "> `embed.ts` **不改**"
rep("""> `embed.ts` **不改**：`createEntry` / `openEmbedTarget` 已有 `srcPath` 去重语义""",
"""#### E-bridge.ts-1：渲染进程的 IPC 封装

**定位**：`desktop/app/src/lib/bridge.ts` 末尾（与 `embedOpenTarget` 同族）。

```ts
// 产物预览/自检：渲染进程只拿 URL 与结果，不碰 Chromium。
export const artifactVerify = (url: string) =>
  window.hai.artifact.verify({ url, sid: useAppStore.getState().activeSessionId ?? '' })
export const artifactScreenshot = (url: string, opts?: { fullPage?: boolean }) =>
  window.hai.artifact.screenshot({ url, opts })
export const artifactInspect = (url: string, selector: string) =>
  window.hai.artifact.inspect({ url, selector })
```

> `embed.ts` **不改**：`createEntry` / `openEmbedTarget` 已有 `srcPath` 去重语义""")

open(p,'w').write(s)
print('ok')
