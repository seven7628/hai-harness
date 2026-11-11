# 归属（owner）模型：hooks 信任与 IM 启用/路由的隔离

> 状态：已落地（分支 `implicit-state-fix`）。对应审计条目 **BR-21**（隐式状态审计 §6.1 item 0）。
> 产品决策：**按智能体（agent）/ 群组（group）隔离，不是按 workspace、也不是全局**；
> 普通项目会话保留 `workspace:<path>`。同一 agent 的 DM 与群聊**不共享**（最安全默认）。

## 1. 归属编码（唯一来源）

Go：`owner/owner.go`；前端：`desktop/app/src/lib/owner.ts`（**语义一致**，展示文案可以不同 ——
`ownerLabel` 是给人看的，判定永远用 owner 字符串本身）。

**两侧一致性靠测试钉住，不靠"看起来一样"**：`desktop/app/src/lib/owner-test.ts` 覆盖
DM/群/普通会话/形状不对的 hub id；`desktop/bridge/*_test.go` 覆盖同一批输入的服务端派生。

**trim 策略（两侧一致）**：
- 前端在**编码时** trim 标识（`workspaceOwner/agentOwner/groupOwner` 都 `trim()`，空 → 空串）；
- 服务端在**读 payload 时** trim（`ownerFromPayload` 对 `owner`/`session_id`/`workspace` 先 trim）；
- `owner.Kind/Valid` 本身**不** trim、不归一化：拿到什么判什么。理由 —— 目录名和 agent id
  都是大小写敏感的身份，静默 trim/折叠大小写会把「另一个目录/另一个 agent」变成同一个归属
  （宁可多确认一次，不可少确认一次）。

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

### `FromSessionID` 的已知边界（有意不猜）

`hub.<agent>.g.<group>` 按**第一个** `.g.` 切分（`group` 取切分点之后的全部，因此群 id
里含 `.g.` 仍能正确还原）；`.dm` 只认**结尾**后缀。由此带来的边界：

| session id | 结果 | 说明 |
|---|---|---|
| `hub.a.dm` | `agent:a` | 正常 |
| `hub.a.g.g1` | `group:g1` | 正常 |
| `hub.a` | `""` | 形状不对 → 未绑定（不猜 workspace） |
| `hub.a.g.` | `""` | 群 id 为空 → 未绑定 |
| `hub..dm` | `""` | agent id 为空 → 未绑定 |

**未做的校验（缺口，记录在此）**：这里**不校验** agenthub 的 ID 词法（不查字符集/长度，
是否真实存在）。后果：`hub.A.dm` 与 `hub.a.dm` 是两个归属（大小写敏感，符合"不归一化"），
而形状合法但 id 不存在的会话也会派生出一个归属（不会投递，写信任时会落一条无人读的条目）。
真正的校验属于 agenthub 的会话创建路径（那里才知道 agent/group 是否存在）——
桥侧刻意**不**去查磁盘/注册表，否则 `hooks_list` 这类只读命令会依赖运行态（违反契约 §2）。
若后续要收紧，应该复用 `agenthub` 的同一套 ID 校验函数，而不是在 owner 包里再抄一份。

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
- `hooks_list` 的 `owner` **显式非法**（如 `global`）→ 命令直接报错，不再 200 + 空归属：
  后者会让面板把确认按钮接到空/别的归属上（"显示了但写错人"比"明确报错"更糟）。
  不带 owner（只读聚合）仍成功，归属显示为「未绑定」，此时写操作被拒。
- 设置面板：钩子页显示「当前归属」与「确认给谁」；归属非法时**禁用**确认按钮并提示，
  而不是等用户点了才看到服务端报错。

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
  配置不会重置所有人刚设的归属状态。判「没带」按 **`len(owners)==0`**（不是 `nil`）：
  前端序列化可能给出 `owners: {}`，只判 nil 会把显式 `false` 清空（关掉的归属又自己开了）。
  真要清空账本需要独立命令（未做，当前 UI 不提供）。
- 绑定确认（`im_bind_confirm` 批准）新增一条项目路由后同样调用 `MigrateOwners()`：
  新路由的归属立刻有显式 `enabled=true`，不依赖"有路由即启用"的隐式默认。

### 管理面

- `plugin_list` / `plugin_enable` / `plugin_disable`：带 `owner`；IM 的条目状态取
  `implugin.Plugin.OwnerView(owner)`（`ownerEnabled/ownerRoutes/masterEnabled/sharedConnected`），
  连接状态只作展示。工作区级 `registry.Close` 不会停共享连接（`ClosePreserve`），
  进程退出才统一收尾（`closeIMPlugin`）。
- `im_status` / `im_route_list` / `im_route_set` / `im_route_del`：带 `owner`；
  列表按归属过滤、写入后回显归属；删除按归属收窄。
- 设置页：插件 tab 的 IM 卡片、IM 配置页顶部都显示「作用范围」与**当前归属的启用状态**，
  并可只对当前归属启停（`plugin_enable/disable` + owner）；连接数只作展示（"由所有归属共享"）。
  `plugin_status` 事件是连接面事实，前端合并时保留归属字段（不让 UI 退回「全局启用」）。
- 路由编辑器：目标三态（workspace / agent / group）都可见可编辑；编辑只改用户实际编辑的
  维度（改群 id 不丢群执行者）；列表对目标缺失/不完整的行明确标注且只允许删除 ——
  **不会**把 agent/群路由静默改写成 workspace 路由（消息改投项目会话是数据损坏级别的回归）。

## 4. 未覆盖 / 后续

- `hooks_set`（写配置）仍是**层级**维度（user / project），不是归属维度：配置文件本身就
  分层，归属只决定「谁确认了」。Hub 用户级 hooks 的确认落在当前 agent/group 上。
- IM 群路由要求显式 `agent`（群内执行者）；不指定就是配置错误，不做「猜一个成员」。
- 前端归属由 `lib/activeOwner.ts` 统一派生：Hub 打开时读 `useHubStore` 的
  `open/sid/activeGroupId`（DM → `agent:`、群 → `group:`），否则按 app store 的
  `activeSessionId + workspace`。`App.tsx` 单点把 Hub 上下文写进模块，`send()`（非 React
  路径）与设置页组件读同一份派生；Hub 打开但未选中目标 → 空归属（不猜 workspace）。
  **历史坑（B1）**：只读 `useAppStore.activeSessionId` 时 Hub DM/群 的确认会被写成
  `workspace:<工作区>`，而派发按 `agent:/group:` 查 —— 确认了却不生效。源码守卫
  （`scripts/test-owner-wiring.mjs`）禁止设置页再各自调用 `sessionOwner`。
- 运行时并发：`implugin.Runtime` 的配置与连接表由同一把锁保护，只经
  `ConfigSnapshot/SetConfig/Gateway/GatewaySnapshot/PutGateway/DeleteGateway/TakeGateways`
  访问（Gateway 回调 goroutine 与命令循环并发读写，裸字段就是 -race UB）；
  `manager.imBridge` 是 `atomic.Pointer`（装配在命令路径、读取在回调/cron）。
- 信任库落盘走 `hooks.writeFileAtomic`（临时文件 0600 + rename）：半截 JSON 会让下次加载
  按空库处理（此前所有确认静默作废）。
- renderer-send 守卫（BR-04）目前只匹配直接调用；跨行/别名变体（`const wc = …; wc.send(`）
  仍是基线遗留，未在本轮扩展（详见该测试文件的注释）。
