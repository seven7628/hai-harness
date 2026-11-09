# P4-04b 设计定稿：Hub 搜索 v1 + 冷归档 v2

> 产品决定（2026-11-09，主会话按产品/经验定稿，不再等待拍板）
> P4-04b 原为 deferred；现拆为 04b-1（搜索）与 04b-2（归档）。

## 1. 搜索 v1（04b-1）

### 范围

- **人类**：搜索所有群 + 所有 DM，包含 UI 未加载的历史与未来的冷归档；默认 `all`，可切 `group` / `dm`。
- **智能体**：只能搜自己参与的群和 DM；不提供跨群全局搜索工具。现有 `group_history` 的 `query` 保留。
- **owner-only**：人类可见并标“仅群主可见”；智能体只得到占位。
- **默认排除**：`routing_receipt`、`round_summary`、`approval_stub`。
- 搜索字段：消息正文；任务命中附带 task title/ID，artifact 命中附带 name/path；不搜 reasoning/未落盘草稿。

### 结果与 UI

- newest-first，不做相关度排序；每会话最多 50、全局最多 200。
- 结果字段：`scope`、`group_id`/`agent_id`、会话名、seq、snippet、sender、kind、created_at、owner_only。
- 点击结果：打开对应群/DM，加载命中 seq 附近一页并高亮；不能只打开最新页。
- 桌面和手机共用 `hub_search` 命令；输入 debounce 250ms，Escape 关闭。

### 后端接口

- Store 增加：
  - `SearchMessagesHuman(ctx, gid, query, beforeSeq, limit)`：人类可见（含 owner-only），排除系统噪声。
  - `SearchDirectMessages(ctx, agentID, query, beforeSeq, limit)`：DM 人类搜索。
  - 保留现有 `SearchMessages`（agent-safe）不变，避免影响 `group_history`。
- Bridge 增加 `hub_search`（人类命令，加入手机白名单）：
  - payload：`query`（必填非空）、`scope`（all|group|dm）、`group_id`/`agent_id`、`before` 游标、`limit`。
  - `all` fan-out 遍历 `ListGroups` + `ListAgents`，跳过 deleted；合并后按 `(created_at, id)` 倒序，返回 `next_before`。
  - 不允许智能体调用；命令层只接受人类客户端。
- 读取时避免长时间持全局 `s.mu`；v1 允许每会话 fan-out，但要有上限与短缓存；1 万条消息 p95 目标 <150ms。
- 不引入 FTS/新数据库；若实测超目标再加轻量索引。

### 验收

- 人类群/DM 全量搜、owner-only 标记、系统噪声排除、scope 过滤、cursor 翻页；
- agent 不能跨群、不能看 owner-only；
- 点击旧命中能加载并高亮；
- 桌面+手机可用；
- 搜索不阻塞 append，不泄漏不可见消息。

## 2. 冷归档 v2（04b-2，暂不实现）

- **消息默认永久保留，不自动删除。**
- 触发：单会话热日志 > 20,000 条或 > 50MB；后台归档较早的 70%，热日志至少保留最近 2,000 条。
- 归档不可变段：`archive/<scope>/<id>/<startSeq>-<endSeq>.jsonl` + manifest；读取/搜索透明合并热冷。
- 顺序：写 archive → fsync → manifest → 再从热日志移除；任何失败保留原文，只能重复归档，不能丢消息。
- 删除是独立动作，默认关闭；开启需显式保留期 + 导出/二次确认。
- artifact 独立治理：默认不随消息删除；2GB 提示清理，自动清理默认关闭。

## 3. 非目标

- 不做相关度排序、向量搜索、FTS（v1）。
- 不做自动删除、跨设备同步。
- 不把搜索做成智能体的全局越权工具。
