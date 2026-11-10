# 归属（owner）模型：hooks 信任与 IM 启用/路由的隔离

> 状态：已落地（分支 `implicit-state-fix`）。对应审计条目 **BR-21**（隐式状态审计 §6.1 item 0）。
> 产品决策：**按智能体（agent）/ 群组（group）隔离，不是按 workspace、也不是全局**；
> 普通项目会话保留 `workspace:<path>`。同一 agent 的 DM 与群聊**不共享**（最安全默认）。

## 1. 归属编码（唯一来源）

Go：`owner/owner.go`；前端：`desktop/app/src/lib/owner.ts`（逐字对应，有单测钉住）。

| 归属 | 含义 | 从哪来 |
|---|---|---|
| `agent:<agentID>` | Hub 智能体 DM 会话 | 会话 id `hub.<agent>.dm` |
| `group:<groupID>` | Hub 群聊会话 | 会话 id `hub.<agent>.g.<group>` |
| `workspace:<abs path>` | 项目会话（含该项目派生的 cron 会话） | 工作区路径 |
| `legacy` | 迁移前的全局信任（**只读**） | 旧 `hooks-state.json` 的顶层条目 |

- `Valid()` 只放行前三种；写路径拒绝 `legacy` / 空 / 未知前缀。
- 不做继承：`group:g` 不继承 `agent:a` 的任何状态，反之亦然（产品决策）。
- hub 前缀但形状不对（如 `hub.x`）→ 空归属（显式「无法归属」），**不**回落成
  `workspace:<hub home>`（那会把智能体 home 当成一个项目）。

## 2. hooks 信任库（`~/.go-code/hooks-state.json`）

**实例**是进程级一份（一个文件、`hooks_trust` 写完即刻生效，契约 §5.5），
**条目**按归属分表。

```json
{
  "version": 2,
  "entries": {
    "agent:release":   { "echo hi": { "hash": "sha256:…", "scope": "project", "reason": "…" } },
    "group:team":      { "echo hi": { "hash": "sha256:…" } },
    "workspace:/proj": { "echo hi": { "hash": "sha256:…" } },
    "legacy":          { "old cmd": { "hash": "sha256:…" } }
  },
  "managed": []
}
```

判定 `TrustStore.Check(owner, command)`：

1. `managed` 名单 → `managed`；
2. 该 owner 下的记录 → `trusted` / `modified`（内容哈希不符）；
3. **仅当 owner 是 `workspace:*`** 时回退到 `legacy` 条目；
4. 其余一律 `untrusted`（不再执行）。

派发侧：`Runner.Dispatch` 用 `Payload.Owner`（由 `Dispatcher.ToolHooksFor(cwd, sid, owner)`
绑定；旧入口 `ToolHooks` 自动按 `workspace:<cwd>` 派生，**不再是全局**）。

### 迁移规则（v1 → v2）

- 旧文件（无 `version`，命令平铺在顶层）**不会**被套到所有 agent/group —— 那正是要消除的跨 owner 越权；
- 条目落到 `legacy`，且**只对 workspace 维兼容生效**：升级后项目会话的既有确认照常工作，
  Hub 智能体/群聊必须各自重新确认（面板会提示「有 N 条只来自迁移前记录」）；
- 加载**不改写文件**；下一次 `hooks_trust` 写入时落成 v2，且保留 `legacy` 段（不删历史）；
- 坏文件 → 空库（全部未信任），不阻断启动。

### 命令面

- `hooks_trust`：`owner`（显式）→ `session_id`（Hub 会话派生）→ `workspace`；**三者都没有就报错**，
  绝不默认写成全局。
- `hooks_list`：回 `owner/ownerKind/ownerLabel`（当前会话归属）、`legacyTrust`、`owners`；
  每个来源带自己的 `owner` + `trust`（项目来源按 `workspace:<该项目>` 判定）。
- 设置面板：钩子页显示「当前归属」与「确认给谁」，信任按钮回传来源的 owner。

## 3. IM（`~/.go-code/im-config.json`）

**连接**共享（一条配置 = 一个账号长连接，绝不为每个 owner 各起一条），
**启用/路由**按归属。

```json
{
  "enabled": true,
  "gateways": [{
    "id": "feishu-main", "type": "feishu", "enabled": true,
    "routes": [
      { "chat": {"gateway":"feishu","chat_id":"dm-1"},  "agent": "release" },
      { "chat": {"gateway":"feishu","chat_id":"grp-1"}, "agent": "release", "group": "team" },
      { "chat": {"gateway":"feishu","chat_id":"old"},   "workspace": "/proj/x" }
    ]
  }],
  "owners": { "agent:release": {"enabled": true}, "group:team": {"enabled": false} }
}
```

- `RouteConfig.Owner` 留空时**由投递目标派生**：`group` + `agent` → `group:<g>`；
  `agent` → `agent:<a>`；`workspace` → `workspace:<path>`。显式填写必须与目标一致
  （否则「关 A」会停掉 B 的投递）。不造第二套真相：owner 不另存一份。
- 投递：`group` → `hub.<a>.g.<g>`（`hubGroupSession`）；`agent` → `hub.<a>.dm`
  （`hubDMSession`）；`workspace` → 既有 `im-<gateway>-<chat>` 会话。
- 闸门：`OnInMessage` 在路由命中后判 `Config.OwnerEnabled(route.EffectiveOwner())`，
  否则审计 `owner_disabled` 并丢弃 —— 连接在，投递按归属放行。
- 启用状态：`owners[owner].enabled` 三态（nil = 未设置）。未设置时**有路由即启用**
  （升级默认：旧配置只有路由、没有 owners 段，升级后不丢消息）。
- 关掉最后一个归属 → 主开关一并关、连接停止；重启不回连（`WasEnabled` 会检查归属）。

### 迁移规则

- 旧配置（无 `owners`）：按第 3 条推导，投递照旧（不静默丢消息）；
- 首次保存配置 / 设置路由时调用 `MigrateOwners()`，为已有路由的归属补 `enabled=true`
  （显式化，管理面能看出「这是升级默认值」）；幂等；
- 编辑整份配置（`im_config_set`）时，payload 没带 `owners` 就沿用磁盘上的 —— 保存一次
  配置不会重置所有人刚设的归属状态。

### 管理面

- `plugin_list` / `plugin_enable` / `plugin_disable`：带 `owner`；IM 的条目状态取
  `implugin.Plugin.OwnerView(owner)`（`ownerEnabled/ownerRoutes/masterEnabled/sharedConnected`），
  连接状态只作展示。工作区级 `registry.Close` 不会停共享连接（`ClosePreserve`），
  进程退出才统一收尾（`closeIMPlugin`）。
- `im_status` / `im_route_list` / `im_route_set` / `im_route_del`：带 `owner`；
  列表按归属过滤、写入后回显归属；删除按归属收窄。
- 设置页：插件 tab 的 IM 卡片、IM 配置页顶部都显示「作用范围」；
  `plugin_status` 事件是连接面事实，前端合并时保留归属字段（不让 UI 退回「全局启用」）。

## 4. 未覆盖 / 后续

- `hooks_set`（写配置）仍是**层级**维度（user / project），不是归属维度：配置文件本身就
  分层，归属只决定「谁确认了」。Hub 用户级 hooks 的确认落在当前 agent/group 上。
- IM 群路由要求显式 `agent`（群内执行者）；不指定就是配置错误，不做「猜一个成员」。
- 前端归属由 `activeSessionId + workspace` 计算；切换会话后插件/钩子面板需重新拉取
  （面板打开时刷新）。
