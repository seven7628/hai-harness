# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)

rep("""### 21.14 `go.mod` / 依赖

**不改。** 逐项核对：

| 需要的能力 | 本仓现状 | 结论 |
|---|---|---|
| HTML/CSS/JS 引用解析 | 自写正则（§6.4，~250 行） | 不引入 `golang.org/x/net/html`：只要 src/href/import，DOM 解析器是杀鸡用牛刀，且要多一个依赖 |""",
"""### 21.14 `go.mod` / 依赖

**只加一个依赖**：`golang.org/x/net`（HTML tokenizer，§6.8 已给 diff）。逐项核对：

| 需要的能力 | 方案 | 结论 |
|---|---|---|
| HTML/CSS/JS 引用解析 | `golang.org/x/net/html` tokenizer（§6.4） | **唯一新依赖**。为什么不用正则：属性可以无引号/跨行、`<style>` 里的 `url()` 与 `@import` 形态多，正则漏掉时的表现是「页面看起来正常、依赖没进包」——最难发现的一类 bug。x/net 是 Go 官方模块，无额外传递依赖树 |""")

rep("""| `go.mod` | 0（无新依赖） | — | §21.14 |""",
    """| `go.mod` / `go.sum` | +1 模块 | A1 | §21.14 |""")

rep("| `x/net` 依赖引入 | 低 | 构建变化 | 只新增一个官方模块；`go mod tidy` 审查 |",
    "| `x/net` 依赖引入 | 低 | 构建变化 | 只新增一个 Go 官方模块（无传递依赖树）；`go mod tidy` 后核对 `go.sum` 只多两行 |")

rep("go.mod / go.sum              + golang.org/x/net",
    "go.mod / go.sum              + golang.org/x/net（唯一新依赖，§6.8）")

open(p,'w').write(s)
print('ok')
