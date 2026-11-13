# 创造模式 V0.1 —— 文档目录

> 一句话：把桌面端右栏从「4 个硬编码标签」改造成 **Surface 能力容器**，并让 agent 能通过受管安装往里面加面板。
> 状态：设计定稿，**未实施**；代码方案为参考实现。日期：2026-11-11。

---

## 阅读顺序

| # | 文件 | 回答什么 | 状态 | 行数 |
|---|---|---|---|---|
| 00 | [`00-conventions.md`](00-conventions.md) | **施工规章**：前置规范（依赖/命名/状态/安全/测试/开关）、CodeMode 7 条契约、PR 8 条硬规则、DoD 五类检查表、观察项 | **活文档 v0.1** | 432 |
| 01 | [`01-design.md`](01-design.md) | 为什么这么做、边界在哪；三态布局、多标签、内嵌编辑、文档/媒体预览、无模式创造；决策登记 D-A…D-N | 定稿 | 752 |
| 02 | [`02-pr-plan.md`](02-pr-plan.md) | 按什么顺序做：24 个 PR（08e 可选）、每个的验收与回滚、门禁 | 可直接开工 | 319 |
| 03 | [`03-code-plan.md`](03-code-plan.md) | 具体写成什么样：类型契约、注册表、布局换算、store 改造、编辑器、预览渲染器、Go 通道与工具、M1/M2 | 参考实现 | 1311 |
| 04 | [`04-review.md`](04-review.md) | 隐式状态登记（20 项）+ 28 条歧义裁决 + 已知取舍 | 评审留痕 | 187 |
| 05 | [`05-preview-media-research.md`](05-preview-media-research.md) | DSH 文档/Office 实现拆解、差距矩阵、音视频方案、**LibreOffice kit 落地设计（§8）**、D-I…D-M | 调研 + 方案 | 333 |
| 06 | [`06-opencode-skills-research.md`](06-opencode-skills-research.md) | OpenCode v2 的"无模式创造"：插件 API + skills；三方对比；D-N 修订 | 调研 + 决策 | 184 |
| — | [`progress.html`](progress.html) | **执行看板**（唯一状态源）：24 轮状态/commit/评审、性能预算 P01-P15、风险与协议 | 活文档 | 442 |

**冲突时以 01 的 §11 决策登记为准**；04 记录每次裁决的理由；00 管施工与 PR 门槛，**实施期可持续修订**（L1/L2 直接改，L3 先裁决）。

---

## 关键决策速览

| 编号 | 决策 |
|---|---|
| D-A | 内容触发时自动展开右栏；手动收起不加锁（自动展开只属于 `openTab`） |
| D-B | 默认 **session 作用域**；`workspace` 只给无进程级视图的看板类 |
| D-C | `⌘S` 只在编辑器聚焦时拦截，全局不绑定 |
| D-D | `activeId` 权威；`rightTab` 保留字段、由 `applyTabs()` 单点写穿，PR-11 删除 |
| D-E | 插件不可声明 `write`；注册期断言 |
| D-F | 编辑器选 CodeMirror 6，不做 LSP/多光标 |
| D-G | 文件写锁提取 `internal/atomicfile`（flock，与 `tools/builtin` 共用） |
| D-H | `ui` 快照只含当前会话标签，其它会话只给计数 |
| D-I | 取消 `office` kind：文档/媒体是 `file{mode:'preview'}` 的渲染器 |
| D-J | Office 保真**沿用 DSH**：LibreOffice kit（可下载、不内置）→ PDF；缺失降级近似 + 横幅；不用 Keynote 脚本 |
| D-K | 表格不引前端依赖，增强服务端 HTML（sheet 导航/冻结/数字格式） |
| D-L | 音视频自研：`hai-media://` 支持 Range + `ffprobe` 探测 + 可选转码 |
| D-M | 转换服务统一限额：并发 2 / 60s / 50-100 MiB / 内容寻址缓存 / 输出保留 24h |
| D-N | **取消 `CreatorMode`**：4 个工具常驻 + `hai-surface-dev` skill 路由 + 安装审批（对齐 OpenCode） |

**状态归属一句话**：注册表只装"活着的"，index 只记"装过的"；dirty 属于标签、聚合值现算；`ui` 快照只给当前会话；`rightTab` 只有一个写者；转换缓存只有转换服务能写。

---

## 怎么开始

第一条竖切（约 3.5 人日，不碰业务逻辑）：

```
PR-00  契约与注册表骨架（无 UI 接入）
PR-01  三态布局 + 删 rightOverHalf
PR-02  activeId 权威 / rightTab 写穿（先改 e2e）
```

跑通后再按 02 的顺序迁 git → file → 编辑器 → web → activity → office。

---

## 目录外的前置文档（都在上一级 `docs/`）

- `../RIGHT_PANEL_TABS_PLAN.md` —— 标签模型（阶段 1-5 已交付，D1-D7 仍有效，**唯一例外**：D6 对 pptx/docx/xlsx 被 `office` provider 取代）
- `../DESKTOP.md` §5.7 —— 三栏布局与聚焦模式（`⌘S` 的旧措辞需按 D-C 更新）
- DSH 源码（调研引用）：`packages/client/ui-sidebar-documentpreview`、`packages/document/office-kit`、`packages/document/tool-office`、`packages/attachment/attachment`

> 文中出现的裸文件名（`DESKTOP.md`、`RIGHT_PANEL_TABS_PLAN.md`）均指上一级 `docs/` 目录。

---

## 维护规则

1. 改设计 → 先改 `04-review.md` 的裁决表与 `01-design.md` §11，再改 `03-code-plan.md`；
2. 新增/重命名本目录文件 → 全目录 `grep -rn "0[1-4]-"` 检查交叉引用；
3. 每个 PR 完成后回写：`02` 的估时/状态、`04` 的新发现（若有）、`03` 的实现偏差；
4. 规范条款要改 → 按 `00` §0.3 分档：澄清直接改、条款调整改本文并 `grep` 交叉引用、硬约束先写 04 裁决；每次改动在 `00` §8 记一行。

---

## 执行状态

进度不在 Markdown 里：见 [`progress.html`](progress.html)（浏览器打开）。
**每一轮 = 实现 → 门禁 → commit → 独立 Review → PASS 才进下一轮**；评审不过则同轮修复（≤3 次）。

---

## 口径表（改文档前先对一遍）

| 口径 | 值 | 权威位置 |
|---|---|---|
| 内置 kind | 4（activity / git / file / web） | 01 §3.1 |
| 决策 | 14 条（D-A…D-N） | 01 §11.1 |
| PR 总数 | 24（08e 可选） | 02 §1 |
| 工作量 | 34.5 人日（含 08e；不做 08e 为 32.5） | 02 §1、03 §6 |
| 评审裁决 | 36 条（F01…F28 + 实施期 F29…F36） | 04 §二、§五 |
| 隐式状态 | 20 项（S1…S20） | 04 §一 |
| 需改文件 | 24 个 | 01 §10.1 |
| 转换限额 | 并发 2 / 60s / 50-100 MiB / 失败不缓存 | 01 §4.7、05 §8.4 |
| 前置规范版本 | v0.2（L1/L2/L3 三档修订；含 §1.12 性能） | 00 §0.3、§8 |
| PR 硬规则 | 8 条（R1…R8） | 00 §3.1 |
| CodeMode 契约 | 7 条（C1…C7）+ 4 个工具的返回契约 | 00 §2 |

改任何数字，四处一起改：01 §11 → 02 §1 → 03 §6 → 本表；规范条款另记 00 §8。
