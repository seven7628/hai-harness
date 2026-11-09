# Agent Hub 计划与进度

这个目录是 Agent Hub（多智能体工作台）的**计划与进度**单一入口。

## 打开看板

双击 `PROGRESS.html`（无需服务器，自包含）。

- 顶部：总进度 / 各期进度 / 状态统计
- 左侧阶段卡片：点击过滤
- 中间：任务列表（搜索、状态筛选）
- 点任务：右侧详情（验收、文件、测试、commit、Review、notes）
- 进度存本机 `localStorage`；换机用「导出 / 导入」

## 文件

| 文件 | 作用 |
|---|---|
| `../../AGENT_HUB_TECH_PLAN.md` | 技术方案（需求、架构、数据模型、协议、UX、代码方案、Review 流程、任务拆分） |
| `PROGRESS.html` | 唯一进度看板（自包含，直接打开） |
| `tasks.json` | 任务定义（看板数据源） |
| `P4-RETRO.md` | P4 最终复盘：交付矩阵、回归/性能/成本、已知限制、waiver |
| `sync_progress.py` | 改完 `tasks.json` 后，把数据同步进 `PROGRESS.html` |

## 工作流（每个任务）

```
todo → in_progress → review（必须已有 commit）→ done（Review passed）
```

- 每个任务**一个 commit**，message 带任务 ID，例如 `feat(hub-p0-01): ...`。
- 提交后做一次 Review（自检 + 自动测试 + 独立 Review），结论写进看板。
- Review 不通过：追加 `fix(hub-...)` commit，不要 amend 已 Review 的 commit。
- 看板会在 `done` 缺 commit / Review 未通过时标红。

详细定义见技术方案 §18（提交与 Review 工作流）、§19（任务拆分）。

## 修改任务

1. 改 `tasks.json`。
2. 跑 `python3 docs/agent-hub/sync_progress.py`。
3. 刷新 `PROGRESS.html`。

看板加载时按 `todo < in_progress < review < done` **只前进**合并仓库 progress：
任务从进行中推进到待 Review/已完成会自动覆盖旧的本机状态，本地 `notes` 保留。
同阶段会同步最新 commit/review/reviewNote；若本机已手工标成更后阶段则不会被仓库回退。
