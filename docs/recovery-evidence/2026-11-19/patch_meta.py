# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

rep("> 状态：**方案定稿，待评审**（2026-11-19；同日 v2 增 §21「代码变更全集」）",
    "> 状态：**方案定稿，待评审**（2026-11-19；v2 增 §21 代码变更全集；v3 增 §22 CodeMode、§23 选区引用、§24 Review 修正）")

rep("""| 协作（分享/评论/共编） | **后置到 Phase D，且先做 handoff 包** | 本仓是本地单机优先；实时共编需要服务端与 Yjs，和现有架构不同量级 |""",
"""| 协作（分享/评论/共编） | **后置到 Phase D，且先做 handoff 包** | 本仓是本地单机优先；实时共编需要服务端与 Yjs，和现有架构不同量级 |
| 工具名 `artifact.publish`（点号） | **wire 名 `artifact_publish`（下划线）** | OpenAI/Anthropic 函数名不允许点号，且 codemode 脚本里 `tools.artifact.publish` 是语法错误（§22.1）；点号只保留为文档别名 |
| 选中即发一条改稿任务 | **默认「引用到 Composer」，另给「直接改」** | 用户多数是「先引用、再自己写一句」；引用不激活写守卫（§23.1） |""")

rep("| A 基座（检测/判定/版本/预览/产出卡） | 17 | 17 | ~4,400 行 | 是（4 个 PR） |",
    "| A 基座（检测/判定/版本/预览/产出卡） | 18 | 17 | ~4,600 行 | 是（4 个 PR） |")
rep("| B 选区改稿 | 5 | 9 | ~1,800 行 | 是（3 个 PR） |",
    "| B 选区改稿 | 6 | 9 | ~2,000 行 | 是（3 个 PR） |")

rep("""> 计数口径：**只算生产文件**（不含 `_test.go` / e2e / 评估脚本，那些在 §17 单列）。""",
"""> 计数口径：**只算生产文件**（不含 `_test.go` / e2e / 评估脚本，那些在 §17 单列）。
> `CodeMode 支持`（§22）不新增独立文件（并入工具实现），但新增 `artifact_toolbase.go`；
> `选区引用`（§23）新增 `selection_refs.go`。""")

# §21.2 note pointing to §22/§23
rep("""| `go.mod` / `go.sum` | +1 模块 | A1 | §21.14 |""",
"""| `go.mod` / `go.sum` | +1 模块 | A1 | §21.14 |

> 本表只列**已有文件的改动**。两章新增的行为契约不含独立文件改动、但必须一起落：
> **§22（CodeMode：命名/结构化/档位/并发）** 与 **§23（选区引用：ref store + Composer chip）**。""")

open(p,'w').write(s)
print('ok')
