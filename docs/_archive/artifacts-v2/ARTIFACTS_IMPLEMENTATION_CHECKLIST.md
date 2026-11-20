# Artifacts 实施清单

> 由 `docs/ARTIFACTS_AGENT_DESIGN.md`（§21–§24 逐字恢复 + 元文档对照）导出，可直接当任务表用。
> 每项都带证据出处；`☐` 留作实施时勾选。

## 0. 前提事实

| 项 | 值 | 依据 |
|---|---|---|
| 新增文件 | 24 项（含测试与脚本） | §21.1 |
| 修改文件 | 10 个 Go + 7 个前端/Electron | §21.2 |
| 唯一新依赖 | `golang.org/x/net/html` tokenizer | §21.14 |
| 线上工具名 | **一律下划线**（`artifact_publish` 等） | §22.1 |

## 1. 改动点（E-*，共 11 项）

| # | 改动点 | 定位（原文行号） | 状态 |
|---|---|---|---|
| 1 | **`E-main.go-12`** `CmdAsk` / `CmdAskBatch` 解析 `selection_refs` | `desktop/bridge/main.go:3250`（`case CmdAsk:` 的 `text := str(c.Payload, "text")` 之后） | ☐ |
| 2 | **`E-main.go-9`** `emit` 清选区作用域（AgentEnd） | `desktop/bridge/main.go:764-800` 的 `switch ev := e.(type)` 里 `case *events.AgentEnd:` | ☐ |
| 3 | **`E-main.go-10`** `CmdAsk` / `CmdAskBatch` 清选区作用域 | `desktop/bridge/main.go:3600` 一带（`case CmdAsk:` 与 `case CmdAskBatch:` 两个分支）。 | ☐ |
| 4 | **`E-bridge.ts-1`** 渲染进程的 IPC 封装 | `desktop/app/src/lib/bridge.ts` 末尾（与 `embedOpenTarget` 同族）。 | ☐ |
| 5 | **`E-main.go-11`** 会话删除时清两张表 | `desktop/bridge/main.go:4773-4785`（`case CmdDeleteSession:` 调 `m.deleteSession` 处） | ☐ |
| 6 | **`E-agents/agent_loop.go-5`** 收尾合并快照 + 声明 + 分类 | — | ☐ |
| 7 | **`E-ArtifactsBlock.tsx-1`** `bundles` prop | — | ☐ |
| 8 | **`E-Narrative.tsx-1`** 把 bundles 传进产出卡 | — | ☐ |
| 9 | **`E-command_registry.go-1`** 命令名常量 | — | ☐ |
| 10 | **`E-ArtifactsBlock.tsx-2`** 渲染位置 | — | ☐ |
| 11 | **`E-main.go-7`** 命令分发 | — | ☐ |

> 每条改动的完整代码见 `ARTIFACT_CODE_FRAGMENTS.md`（177 段）与 `ARTIFACT_SECTIONS_EXTRA.md`。

## 2. 协议实现清单

| # | 项 | 内容 | 依据 | 状态 |
|---|---|---|---|---|
| P1 | 引用类型 8 种 | element / elements / text / comment / region / page / artifact / file，各带 payload 契约 | §23.1b | ☐ |
| P2 | 选区引用表 | `desktop/bridge/selection_refs.go`（新增全文，含结构体与索引） | §23.2 | ☐ |
| P3 | 命令入口 | `handleArtifactSelection` | §23.2b | ☐ |
| P4 | 注入格式 | `<selection>` 是**数据**不是指令；只有 `<user_instruction>` 是指令 | §23.3 | ☐ |
| P5 | 失效与漂移 | version 过期后的重定位与「选区已失效」提示 | §23.5 | ☐ |
| P6 | 写守卫 | 选区任务期间的写范围限制；守卫需**常驻安装**（active=false 零成本） | §23.6 + patch_doc.py 证据 | ☐ |
| P7 | Inspector | 四种取选模式（对 §8.5 的增量） | §23.1c | ☐ |
| P8 | Composer | 复用已有 chip，不新增输入框 | §23.4 | ☐ |

## 3. CodeMode 编排清单（§22）

| # | 项 | 内容 | 状态 |
|---|---|---|---|
| C1 | 命名 | 线上名一律下划线 | ☐ |
| C2 | 暴露档位 | 注解 + 「可编排」广告 | ☐ |
| C3 | 结构化结果 | 脚本拿到对象，不是 JSON 字符串 | ☐ |
| C4 | 路径语义 | 脚本路径 vs 模型路径的隐式差异必须写进设计 | ☐ |
| C5 | 图片降级 | 图片类工具在脚本里降级 | ☐ |
| C6 | 并发 | per-artifact 串行 | ☐ |
| C7 | 目录预算 | 单目录产出量限制 | ☐ |

## 4. 验收清单

| # | 项 | 要求 | 依据 | 状态 |
|---|---|---|---|---|
| T1 | CodeMode 测试 | 见 §22.8 全文 | §22.8 | ☐ |
| T2 | 选区测试 | 见 §23.8 全文 | §23.8 | ☐ |
| T3 | 失败语义 | 所有新工具/新命令都配「失败语义 + 测试用例」 | 散文行 | ☐ |
| T4 | 分区交付 | Phase B1 支持 element/elements/text/region/page/artifact/file 七种 | 散文行 | ☐ |

## 5. 已知风险（实施时必看）

| 风险 | 说明 | 依据 |
|---|---|---|
| 多工作区串读 | artifact id 与 version **都不是全局唯一**；只按 id+version 建索引会让大文件从**另一个工作区**读出来 ⇒ 路径必须带 `wsKey` | patch_ws.py 证据 |
| 死代码 | `filterNoiseArtifacts` 存在死代码，需清理后再接线 | 散文行 |
| 守卫窗口期 | 守卫若在任务开始才安装，「先发消息、再点选区」的窗口里 bash 是放行的 ⇒ 必须会话创建时即装 | patch_doc.py 证据 |
| 正则漏依赖 | 若用自写正则解析 src/href/import，漏掉时表现为「页面看起来正常、依赖没进包」 | §21.14 表格 |

## 6. 待补（原文档 §1–§20 丢失，需另行补齐）

- 背景与目标、调研对比（部分由元文档 `ARTIFACTS_META_DESIGN.md` 覆盖）
- 总体架构图与模块边界
- 分阶段计划与里程碑
- 风险登记册与回滚方案
