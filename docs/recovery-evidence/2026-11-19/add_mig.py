# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
anchor = '''每个工具改一行：`Call` 的返回从 `fmt.Sprintf(...)` 换成 `return t.emit(ctx, result, "")`。'''
addition = '''**§9 各工具要跟着改的段落**（不改这些，base 就是死代码）：

| 工具 | 删掉自己写的那段 | 换成 |
|---|---|---|
| `artifact_publish` | entry 存在性/版本解析 | `base.version`（校验）+ `emit` |
| `artifact_promote` | 目标 id 解析、源路径校验 | `base.version` + `base.lock(id)` |
| `artifact_edit` | 读 manifest → 逐文件 `os.ReadFile` | `base.readVersionFile`；写前 `base.lock(id)` |
| `artifact_export` | `svc.Version(...)` + 参数校验 | `base.version` |
| `source_get` / `source_find` | 自己 resolve + `../` 校验 | `base.version` + `base.readVersionFile` |
| `preview_inspect` | 自己读版本文件 | `base.readVersionFile` |
| `designsystem_get` | 自己遍历 css/tailwind 文件 | 工作区文件（不是版本内），显式用 `artifactSafeJoin(ws, rel)` —— 它读的是**当前**设计系统，不是某个版本 |

> `designsystem_get` 是唯一**不该**走 `readVersionFile` 的：设计系统是工作区级事实，
> 跨 artifact 共享；从某个版本快照读会拿到旧 tokens。

每个工具改一行：`Call` 的返回从 `fmt.Sprintf(...)` 换成 `return t.emit(ctx, result, "")`。'''
assert anchor in s
s=s.replace(anchor,addition,1)
open(p,'w').write(s)
print('ok')
