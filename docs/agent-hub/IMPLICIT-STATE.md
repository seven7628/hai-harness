# 隐式状态审计：跨 Go / Bridge / Electron / React 的状态清单

> 基线：`agent-hub` @ `1569313`（三路调研在此版本完成；UI 分支 `93c1a7c` 只改群消息表现与 mock，不影响本文引用的 file:line（已抽样复核））。
> 范围：`agents/ core/ session/ events/ memory/ cron/ im/ agenthub/ provider/ hooks/ mcp/ plugin/ runtime/ sandbox/ subagent/ todo/ tools/ execproc/`、`desktop/bridge/`、`desktop/app/electron/`、`desktop/app/src/`。
> 方法：三路只读静态调研（Go 内核、前端、桥接/环境）+ 已知事故复盘 + 主会话交叉核对；每条带 `file:line` 证据。
> 结论：**93 条高置信记录：P0 17、P1 44、P2 31、P3 1**。它们不是 93 个独立 bug，而是同一组根因在四个面上的投影：**默认值/缺失值承担语义、同一事实多个载体、进程级缓存无 owner、身份/生命周期没有单一真相**。

## 0. 摘要

### 0.1 先看这五件事

1. **身份会串**：`session_id` 的“先删后注入”一旦失配，事件会落到当前打开的会话（BR-03）；前端再用“当前会话”兜底，跨项目串台就从可能变成必然。
2. **默认值会固化**：Electron 读 settings 时返回“默认值 ∪ 磁盘值”，任意一次保存又把并集写回（BR-01）；provider 段还有第二个写者（BR-02），明文 key 也在这份文件里（BR-08）。
3. **价表有影子**：`userPriceTable` 是进程级内存表，`priceFor` 先查影子再查内置表，历史成本会被一次 `loadSettings` 改写（GO-01）；实时刷新只改内存，落盘靠前端自觉（BR-14）。
4. **checkpoint 不是自包含快照**：record、`.blocks`、`.blocks.idx`、`.bak` 靠文件名约定关联；`.bak` 兜底会去找 `.bak.blocks` 空壳，块数为 0（BR-09），而 `blockStoreCache` 又用 size+mtime 判新鲜（GO-02）。
5. **“忙/运行中”是多个布尔**：`running / hubSettling / hubDispatchPending / hubGroupBusy / hubBudgetStop` 分散在 bridge（GO-14）；恢复又只在 `hub_open` 时触发（BR-13），前端再把视图镜像到顶层（FE-02），刷新后“交卷”与直派进运行中会话都从这里来。

### 0.2 数量与分布

| 面 | P0 | P1 | P2 | P3 | 合计 |
|---|---:|---:|---:|---:|---:|
| Go 内核 / 运行时 / 持久化 | 5 | 15 | 7 | 1 | 28 |
| 前端（store / UI / 传输） | 4 | 14 | 17 | 0 | 35 |
| Bridge / Electron / 协议 / 环境 | 8 | 15 | 7 | 0 | 30 |
| **合计** | **17** | **44** | **31** | **1** | **93** |

> 计数口径：原始调研的“建议 P0/P1/P2”优先；同一状态点跨面出现时保留多条记录（例如 settings 默认值 BR-01 与“写侧全量覆盖”BR-29 分开），便于定位不同修法。两处特别口径：GO-28 原始只有 `Low-Medium`，本文记 P3；BR-21 原始写“P0（沙箱）/P2”，本文按沙箱越权记 P0。

### 0.3 怎么读这份文档

- 先读 §4 Top 10 与 §5 五个跨面案例，建立“状态怎么流动”的直觉；
- 再按 §6 全量清单查自己负责的面；
- §7 是**明确接受**的隐式状态，不要一刀切；
- §9 是治理顺序，§10 是本次没覆盖到的区域。

## 1. 什么叫“隐式状态”

本审计把**会影响下一次行为、但没有进入任何显式契约的状态**称为隐式状态。显式契约包括：

- Go 类型定义、接口与构造参数；
- JSON/WS/HTTP 协议字段与 schema；
- 持久化记录、迁移版本与归档格式；
- 配置 schema、命令行参数与环境变量白名单；
- React props/state/store 字段与组件契约；
- 测试 fixture 与启动脚本显式声明的输入。

判定四条：

1. **存在性**：真实保存在进程、文件、DOM、宿主环境或时序中；
2. **行为性**：会影响下一次请求、渲染、计费、恢复或清理；
3. **契约外**：类型、schema、协议、配置或文档没有完整表达它；
4. **难枚举**：没有统一入口可以列出、重置、导出或断言它。

不属于本审计：纯函数临时变量、每次重算且不影响结果的派生值、显式配置项本身、只用于调试且不参与行为的日志。

### 1.1 为什么本仓特别多

这不是“注释写得少”。恰好相反，仓库注释密度很高，很多历史事故就写在 `var`/字段旁边。隐式状态多的结构性原因有四条：

- **事件日志 + checkpoint 双读路径**：同一份转录既可从事件重建，也可从 record/sidecar 恢复；两条路径的版本、覆盖字节、去重游标必须严格一致。
- **Provider 矩阵**：模型、端点、字段名、思考文本、max tokens、价表都存在“端点指纹”和兼容回退；回退本身是状态。
- **多进程/多窗口**：Electron 主进程、bridge、手机端、renderer reload 各有生命周期；id 空间、HOME、端口、文件写入者靠约定错开。
- **前端“视图↔顶层”镜像**：`views[sid]` 是真相源，约 50 个字段同时镜像到 AppState 顶层；新增字段漏一处就静默丢状态（FE-02）。

## 2. 分类法

| 代号 | 类别 | 典型载体 | 主要风险 |
|---|---|---|---|
| G | 进程级全局与单例 | 包级变量、`init()`、`sync.Once`、全局 registry、默认 client、全局锁 | 跨会话污染、测试顺序依赖、不可重置 |
| C | 缓存与派生索引 | 价表、模型表、任务/会话索引、标题缓存、Markdown/memo、能力指纹 | 失效不及时、旧值覆盖新值、内存无界 |
| S | 未建模/未序列化字段 | `json:"-"`、`omitempty`、额外 map、前端类型缺字段、内存专用字段 | 重启/恢复后语义改变、字段丢失、三态坍缩 |
| D | 默认值/零值/缺失语义 | `??` / 逻辑或回退、默认 settings、缺失 policy、0 与“不限”、空字符串 | “没设置”被写成“设为默认”、0 被当成缺失 |
| L | 生命周期/运行位 | `live`、`runEnded`、`turnRunning`、`running`、pending、checkpoint、重试、压缩、归档、定时器 | 刷新后“交卷”、轮次/账目错位、后台任务泄漏 |
| F | 身份/归属/当前选择 | `session_id`、`run_id`、`task_id`、`agent_id`、当前会话/智能体/Provider/Model、群成员 | 跨项目串台、卡片/记录挂错、重复插入 |
| Q | 并发/队列/锁/顺序 | 全局/每目标锁、队列、去重、事件序号、goroutine 生命周期 | 死锁、超时、乱序、取消后仍写盘 |
| H | 路径/环境/宿主 | `HOME`、cwd、配置目录、env、CLI flag、端口、时区、locale、OS、sandbox、插件目录 | 本机污染、换机失效、端口冲突、凭证泄漏 |
| I | 协议/序列化/兼容回退 | JSON key shadowing、字段别名、SSE/WS 帧、`thinking_format`、`max_completion_tokens`、错误分类 | 静默丢字段、截断、错误重试、跨模型污染 |
| U | UI 交互/手势/焦点/滚动 | gesture intent、滚动锚点、focus、selection、草稿、预览、hover、折叠、主题、DOM 缓存 | 切会话跳动、折叠态被重建吃掉、类名覆盖 |
| M | 迁移/备份/压缩/生成物 | schema version、`.bak`、compact、sidecar、归档、生成文档、构建产物 | 旧格式不可读、备份不可用、生成物与源不一致 |
| T | 测试/宿主隔离 | 全局价表、临时 `HOME`、settings、端口、时间/随机、测试顺序、未入库 fixture | 假绿/假红、干净检出编译不过、CI 只在 tag 跑 |

三个横切维度：**安全/权限**（审批、sandbox、token）、**成本**（计价、usage、预算）、**可观测性**（日志、指标、转储）。

## 3. 风险分级与总览

| 级别 | 判据 | 典型后果 | 处置时限 |
|---|---|---|---|
| P0 | 可静默造成数据丢失/覆盖、跨会话串台、错误计费/授权、恢复后状态撒谎，且无自愈 | 用户信任受损、需人工清理、可能不可恢复 | 立即修 + 回归测试 |
| P1 | 可造成错误行为/错误归属/明显性能退化，需重启或手动操作恢复 | 单功能错误、可绕过、影响范围有限 | 本迭代修 + 测试 |
| P2 | 局部视觉/交互/性能/测试波动，有明确 workaround | 体验下降、噪声、偶发 | 排期修或记录豁免 |
| P3 | 纯一致性/可维护性问题，不影响当前行为 | 认知负担、未来回归风险 | 随重构处理 |

**风险 = 影响面 × 触发概率 × 可观测性 × 恢复难度**。“没有报错”不降低风险；静默错误通常应上调一级。本清单里 P0 的共性是：用户看不到错误，只能看到“历史没了 / 别人的消息 / 数字不对 / 一直转圈”。

### 3.1 类别分布（主类计数）

| 类别 | 数量 | 代表 |
|---|---:|---|
| C 缓存/派生索引 | 16 | 价表、blockStoreCache、TaskIndex、Markdown cache |
| U UI/手势/滚动 | 14 | 滚动锚点、右栏布局、AgentEditor 快照、主题顺序 |
| L 生命周期/运行位 | 12 | busy 标志、checkpoint live、cron running、看门狗 |
| Q 并发/锁/顺序 | 12 | 任务锁、全局 q.mu、id 代际、响应乱序 |
| G 进程级全局 | 9 | priceTable、registry、hidden、HTTPClient |
| S 未建模字段 | 6 | `omitempty`、`json:"-"`、视图镜像 |
| I 协议/兼容回退 | 6 | session_id、id 空间、thinking 字段、max tokens |
| D 默认值/缺失 | 5 | settings 默认值、cron enabled、MCP exposure |
| H 路径/环境 | 5 | HOME、workspaceKey、sidecar 路径 |
| M 迁移/备份/压缩 | 4 | `.bak`、manifest、归档、生成物 |
| T 测试隔离 | 3 | appDataDirOverride、全局价表、mock/localStorage |
| F 身份/归属 | 1 | 无会话发送缓冲 |

> 一条记录可以有两个类（如 `C/Q`）；上表只按**第一个代号**计主类，合计 93。

## 4. Top 10

| # | 状态链 | 主要证据 | 为什么排在这里 | 最小处置 |
|---:|---|---|---|---|
| 1 | settings 默认值 → 双写者 → 明文 key | BR-01、BR-02、BR-08、BR-29 | 一次保存就能固化默认值/覆盖 key；用户看到“已保存”，下次启动 401 | 单写者 + 白名单 patch + key 与 settings 分离 |
| 2 | `session_id` 重复键 → 空串胜出 → 当前会话兜底 | BR-03、BR-15 | 跨项目串台，最伤信任；日志里完全合法 | 结构化注入 + 每行断言仅一个 `session_id` |
| 3 | 全局价表影子 → 历史成本重算 | GO-01、BR-14 | 计费/预算/熔断都依赖它；无事件无日志 | 价表随 provider 配置对象传递；`cost_source` 显式 |
| 4 | checkpoint 四件套 → `.bak.blocks` 空壳 → blockStoreCache | GO-02、BR-09 | 本该“回退到上一份可用快照”，实际会话空白 | 自包含 generation 目录 + covered 字节强校验 |
| 5 | 多个 busy 布尔 → 恢复只在 `hub_open` → 前端镜像 | GO-14、BR-13、FE-02、FE-18 | 刷新后交卷、直派进运行中会话、任务终态被覆盖 | 单一 `busyState{state,reason}`；恢复挂启动序列 |
| 6 | 无会话发送缓冲 / 跨会话 docRefs / 派发去重表 | FE-01、FE-05、BR-12 | 用户消息静默消失或进错会话；A 的文档喂给 B；重启后重复执行 | `ScopeKeyMap<T>` + 幂等账本 + 可见“待发送 N 条” |
| 7 | 内存 seq/queue/cron 游标 | GO-04、GO-05、GO-12、GO-13 | seq 复用导致消息 ID 串；queued 永久滞留；cron 停在 running | `next_seq` 落盘；queue TTL；启动折叠超期 running |
| 8 | 只增不减的 registry / 锁表 | GO-03、GO-10、GO-25 | 历史事故：65 万条 / 2.5GB / 75 分钟锁等待 | 引用计数 + `Stats()` + 上限告警 |
| 9 | 事件/命令无全局 seq、三套 id 空间 | BR-05、BR-06、BR-07、BR-19、BR-28 | 丢一条 `agent_end` 就永久“运行中”；迟到响应污染全局错误 | `seq + ts + origin + epoch + attempt` 信封 |
| 10 | 测试写真实 HOME / 全局状态不还原 | BR-27、GO-19、GO-24、GO-26、FE-32 | 假绿、假红、跑完测试用户数据变了 | `internal/testenv` + `GO_CODE_HOME` + 端口 0 |

> 并列候选：FE-11（AgentEditor 持旧对象，保存算出错误 diff）、BR-21（预览器/信任库进程级单例跨工作区共享，P0 安全）、GO-11（cron 缺字段 = 全关）。它们没有进前十是因为爆炸半径更局部，但都应在 P0/P1 批次处理。

## 5. 五个跨面案例

### 5.1 刷新/重启后的“运行位”

**状态怎么流动**：bridge 用 `running / hubSettling / hubDispatchPending / hubGroupBusy / hubBudgetStop` 等多个原子布尔描述“忙”（GO-14）；前端把 `views[sid]` 的会话字段镜像到顶层（FE-02），恢复时用 `snapshotPending/preBuf` 缓冲（FE-18）；checkpoint 恢复路径只在 `hub_open` 时触发（BR-13）。

**触发**：用户刷新/重启、切换到后台会话、子 agent 的 `agent_end` 与根 run 的 `agent_end` 混在同一 emit、恢复失败后闩住。

**后果**：前端把仍在跑的 run 收敛成“已完成”，轮尾账提前结算，压缩 signal 被移出 Working 盒；或者反过来，把已完成任务抢成 canceled。用户看到“它说完成了，但其实还在跑”或“历史不出来”。

**修法**：一个 `busyState{state, reason, run_id}` 作为唯一真相，随会话列表输出；恢复在 `serve()` 启动序列执行，`hub_open` 只刷新；checkpoint 显式携带 `live/run_id/turn_id`；前端只消费服务端状态，不用 `false` 默认值推断。

### 5.2 一次后台任务摘出，为什么消息会到另一个项目

**状态怎么流动**：`events.TaskPromoted` 的 JSON 自带 `session_id:""`（没有 `omitempty`）；`jsonwire` 先注入宿主 `session_id`，但“先删后注入”失败时原键还在；JSON 解析器对重复键取后者 → 空串胜出。前端收到空 `session_id` 后回退“当前会话”（BR-03）。

**后果**：A 项目的任务/用量/转录出现在 B 项目；这是最严重的归属错误，且 stdout 行本身是合法 JSON，日志里看不出问题。

**修法**：在事件 marshal 前清空/禁止 SDK 自带路由键，或注入改为“解码 → 覆盖 → 重编码”；每行 golden test 断言 `session_id` 恰好出现一次；前端对空/不匹配的归属**丢弃并记录**，绝不回退当前会话。

**现状（P5-03b）**：emit 侧已闭环（`Envelope` + 行级/emit 级断言）；但前端兜底**仍是旧行为**（`getSid` 回退 `activeSessionId`、空 sid 建 `views['']`），`writeLine` 直写行也不走信封 —— 两项分别是 §6.1 第 1 条的 **N7** / **N8** 残留，别把 BR-03 当成全闭环。

### 5.3 改一个主题，为什么 provider key 没了

**状态怎么流动**：Electron `readSettingsResult` 深拷贝 `DEFAULT_SETTINGS` 再逐段合并 raw（BR-01）；`settings:set` 用 `{...cur, ...patch}` 把整份并集写回（BR-29）；bridge 的 OAuth 路径又把整个 `provider` 对象重写回去（BR-02）；`provider:save` 把 `api_key` 明文放进 settings（BR-08）。

**后果**：用户只改主题，settings.json 从几行变成 200+ 行；新版本默认值对老用户永不生效；OAuth 与设置面板并发时，bridge 的快照覆盖刚保存的 key；key 还会被注入 bridge 环境，被所有 MCP/插件子进程继承。

**修法**：settings 只由 Electron 写；写路径只接受白名单键的 patch（复用 `patchJSONFile` 语义）；读侧合并默认值、写侧不写默认值；**存量按 §9.3 的 v1→v2 规则迁移**；key 只进 `safeStorage`/`keys.json`（0600），`bridgeEnv` 只注入当前活跃 provider 的一把 key。

### 5.4 价表为什么有三份“真相”

**状态怎么流动**：`provider[].prices`（settings）→ `setUserPrice` 写进程级 `userPriceTable`（GO-01）→ `priceFor` 先查影子表再查内置常量表；实时刷新 `price_refresh` 只改内存、响应里 `persisted:false`，落盘靠前端再调 `providerSave`（BR-14）。

**后果**：历史成本、`MaxCost` 熔断、`cost_usd` 注入全部按当前内存价重算；删除 provider 的 prices 会静默回退内置价；刷新成功但前端没落盘，重启后回到编译期快照。另一个口径坑：`cost_usd=0` 与“没有价格”是两件事，报表不能混。

**修法**：一份权威价表（独立 `prices.json` 或 provider config 对象），读取时整表 replace 并清理删除项；usage 里记录 `cost_source`（upstream / table / unpriced）；前端明确区分“本次运行生效”和“已保存”。

### 5.5 checkpoint 的四件套与“回退到空”

**状态怎么流动**：`<sid>.record.json` + `.blocks` + `.blocks.idx` + `.bak` 靠文件名关联（BR-09）；`blockStoreCache` 用 `(size,mtime)` 判 sidecar 是否仍当前（GO-02）；`FileStore` 的 `seq/taskIdx` 又从日志扫描重建（GO-04）。

**后果**：主 record 校验失败时回退 `.bak`，却去找 `.bak.blocks`（8 字节空壳，0 块）→ 会话空白；外部 `cp -p` 替换同 size/mtime 的 `.blocks` 会命中陈旧索引；日志被删除/截断（非归档）后 `rebuildIndex` 从 0 起算，`msg_1` 重号；全量归档路径已有 manifest 补偿，但该补偿缺回归测试。

**修法**：`<sid>.gen<N>/{record,blocks,idx,manifest}` 目录式快照，版本自包含；sidecar 记录 `covered` 字节并强校验；`.bak` 命名统一由一处常量生成；`next_seq` 落进 `group.json`/`hub.json`，启动取 `max(scan, persisted)`。

## 6. 全量清单（93 条）

> 排序：P0 → P1 → P2 → P3；同一风险内按 Go → Bridge → 前端、原编号数字升序。P0 段同样升序（处置批次见 §6.1，不改变表序）。`类` 见 §2；`证据` 是节选，完整上下文见原始调研（task-24z9jt7z / task-agn8vh3q / task-fa93d6sx）。
> 标记 `P0` 的条目不是“建议尽快”，而是“已具备静默破坏正确性的路径”；先按 §9.3 的阶段 0 处理。


| 统一编号 | 原编号 | 类 | 风险 | 证据（file:line，节选） | 一句话影响 |
|---|---|---|---|---|---|
| GO-01 | G1 | C/D | P0 | desktop/bridge/metrics.go:30 | 所有会话的历史成本重算（costUsd 回退路径、overrideRegistryLocked 写 info.Cost）立刻按新/旧价记账 |
| GO-02 | G2 | C/Q | P0 | session/block_store.go:273–276 | 读到错位块 / 半份数据（文件头注释 :23 声明"绝不返回半份数据"依赖三层校验，而索引命中路径不读 sidecar，见 :325 的取舍表） |
| GO-10 | G10 | G/L | P0 | subagent/registry.go:392 tasks | 注册表只增不减 → 曾涨到 65 万条/2.5GB，List 在锁上等 75 分钟，桌面端“所有工作区会话数都是 0” |
| GO-11 | G11 | D | P0 | desktop/bridge/providers.go:244–268 | 功能静默关闭 |
| GO-14 | G14 | L/Q/F | P0 | desktop/bridge/main.go:401 lastUsed | 任务并发派进正在跑的会话（并批/串话） |
| BR-01 | B1 | D/S | P0 | desktop/app/electron/main.ts:471-518; main.ts:562-575; main.ts:1446-1486 | ① 老配置被钉死在旧默认值上，新版本改默认（新模型、新 reminder_rounds、新 provider 预置）对老用户永不生效，且无法区分“用户选的 30”和“默认的 30” |
| BR-02 | B2 | S/Q | P0 | desktop/bridge/config_patch.go:1-24; config_patch.go:36-40; desktop/bridge/oauth.go:150-181 | bridge 那份 providers 快照会覆盖 Electron 刚写入的 api_key / label / prices / usage_input_includes_cache → 界面显示“已保存”但 key 丢失（详见 raw B2） |
| BR-03 | B3 | I/F | P0 | desktop/bridge/jsonwire/jsonwire.go（Envelope/ForceKeys/RemoveField）; desktop/bridge/main.go:653-680（emit 漏斗，含 extras 信封）; 残留：useAppStore 的 getSid 兜底、main.go 的 writeLine（§6.1 N7/N8） | 跨会话串台——最严重的一类：消息/任务/用量被记到错误会话，用户看到“别人的内容出现在我的对话里”（emit 行已闭环，前端归属兜底未闭环） |
| BR-04 | B4 | L/H | P0 | main.ts:806; main.ts:945; main.ts:964 | ① mainWindow 为 null 时该行触发 TypeError，抛在 readline 的 'line' 监听里 → 主进程未捕获异常（轻则丢这一行，重则崩溃，取决于 Electron 对 uncaughtException 的处置；详见 raw B4） |
| BR-08 | B8 | H/I | P0 | main.ts:1521-1528; main.ts:263-300; main.ts:875-880 | ① 凭据随“配置文件”外泄 |
| BR-09 | B9 | M/S | P0 | session/block_store.go:776-778; session/store_record.go:315-325; session/store_record.go:381 | 历史丢失：本应“回退到上一份可用快照”，实际得到 0 块 → 会话空白（正是用户看到的“历史不出来”） |
| BR-21 | B21 | G/H | P0 | desktop/bridge/main.go:1294-1340; main.go:1288-1293; main.go:6573-6584 | 跨工作区越权读取/写入（沙箱根错配） |
| BR-27 | B27 | T/H | P0 | desktop/bridge/appdata.go:28-30; main.go:6578; mobile_pair.go:53-88 | 测试写真实 ~/.go-code（覆盖用户 hub 账本、metrics、devices.json） |
| FE-01 | F01 | F/L | P0 | store/useAppStore.ts:2516-2517 | 用户发出的消息静默消失或投进错误会话 |
| FE-02 | F02 | S | P0 | store/useAppStore.ts:1197 | 漏回写 → 切回会话读到旧历史/旧右栏 tab |
| FE-03 | F03 | Q/L | P0 | store/useAppStore.ts:3354 | 迟到响应被丢弃且不报错——表现是「历史不出来、骨架屏超时后空白」，代码注释多次记录该现象（8841-8844、8868-8873） |
| FE-05 | F05 | S/F | P0 | components/Composer.tsx:260 | 在 B 发送时 refsLine（1073）把 A 的文档引用拼进 B 的消息 → 用户以为只发了文本，实际把 A 的资料喂给了 B 的模型（且不可见于发送前 UI 之外） |
| GO-03 | G3 | G/Q | P1 | agenthub/ledger.go:51 | Hub 长跑 + 大量任务 → 内存线性增长 |
| GO-04 | G4 | C/Q | P1 | agenthub/store.go:136–141 | seq 复用 → msg_<seq> 消息 ID 重复 → 前端按 ID 去重/挂载会串消息、msg_id 幂等命中错行 |
| GO-05 | G5 | G/L | P1 | agenthub/queue.go:34 | 任务永久滞留 queued（用户看到"目标忙碌"的假象） |
| GO-06 | G6 | G/L | P1 | tools/engine.go:71 | (a) 策略失效——换 persona 后策略外工具重新可见（P1-01 复审记录：包装法会丢掉 ApprovalRequired 等可选接口 → "审批、时限、只读判定全部静默失效"） |
| GO-07 | G7 | L/S | P1 | session/session.go:201 persistedCount | 历史重复/丢失（同一条工具结果出现两次 → 上下文膨胀与 UI 重复块） |
| GO-08 | G8 | G/L | P1 | session/approval.go:57 | (a) 重启后"挂着的审批"变成僵尸 UI 行，工具 goroutine 已随进程消失 |
| GO-09 | G9 | G/F | P1 | session/hub.go:53 | 跨节点投递把"本机未注册"报成 ErrTargetNotFound（用户无法区分"会话不存在"与"没恢复"） |
| GO-12 | G12 | L/M | P1 | cron/store.go:58 next | 审计/计费追溯断裂 |
| GO-13 | G13 | L/M | P1 | desktop/bridge/cron.go:91 | cron 运行中途崩溃 → 账本停在 running（永不 Finish）、会话内容不可恢复、LastStatus 永远显示"执行中"（cron/store.go:308-311 MarkFinished 的注释正是这条） |
| GO-15 | G15 | G/H | P1 | tools/builtin/bash.go:52 | 相对路径写入错误目录（rm -rf dist/ 命中的是别人的 dist） |
| GO-17 | G17 | C/M | P1 | tools/builtin/codemode/store.go:30 StoreFileName | 跨会话重启后脚本状态消失或"看起来在、其实只在本进程" |
| GO-18 | G18 | C/H | P1 | provider/auth/refresh.go:76–84 | 缓存不失效 → 连续 401 直到 TTL 到期 |
| GO-21 | G21 | D | P1 | mcp/manager.go:43 | 模型看不到也调不到任何 MCP 工具，且没有任何报错——注释原文："等价于把 MCP 功能对模型整体关闭（既不在 ToolParams、也没有任何东西能把它加载回来）" |
| GO-22 | G22 | M/H | P1 | hooks/trust.go:32–39 | 用户"配了 hooks 却不生效" |
| GO-24 | G24 | H/T | P1 | desktop/bridge/appdata.go:31 | 会话历史"丢失"（其实是分裂成两份） |
| BR-05 | B5 | Q/I | P1 | desktop/app/src/store/useAppStore.ts:2505; useAppStore.ts:3605; useAppStore.ts:3647-3668 | pending[id] 被新页面的同号命令覆盖 → 旧命令的迟到响应会当成新命令的响应被消费（useAppStore.ts:4645-4649：const meta = pending[r.id]; delete pending[r.id]；详见 raw B5） |
| BR-06 | B6 | Q/I | P1 | main.ts:105; main.ts:1007; useAppStore.ts:3605 | 主进程超时/迟到响应以负 id 落到 renderer 无主响应，污染全局 bridgeError |
| BR-07 | B7 | Q/I | P1 | desktop/bridge/mobile_server.go:586-605; main.go:2160-2169; mobile_server.go:569-585 | 第二条响应被静默丢弃（buffer 已满时），且桌面端也收不到——属于“事件黑洞”，比串台更难查 |
| BR-10 | B10 | Q/H | P1 | agenthub/store.go:130; main.ts:1862-1875; main.ts:121,267 | ① Hub 账本/群日志并发追加 → 交错行、seq 重复、manifest 与日志不一致 |
| BR-11 | B11 | H/F | P1 | desktop/bridge/appdata.go:33-57; appdata.go:60-70; agenthub/queue.go:78-90 | “重启后历史丢失 / 项目重复出现”（appdata.go:38-42 注释即为此事故） |
| BR-12 | B12 | C/F | P1 | desktop/bridge/main.go:1212-1213; hub_workspace.go:345-368; hub_workspace.go:451-476 | ① 重启后同一任务被执行两次（副作用重复：发消息、写文件、派子 agent） |
| BR-13 | B13 | L/F | P1 | desktop/bridge/hub_commands.go:41-44; hub_recovery.go:24-52; hub_recovery.go:93-130 | ① queued 任务永不入队 → “提交了但永远不跑” |
| BR-15 | B15 | I/Q | P1 | desktop/app/src/store/events.ts:34-78（runSettleTraces）; useAppStore.ts 的 settleRunNodes / settleSyncSubRuns / syncSubRunIds / agent_end / session_run_error / session_closed / task_end / user_inputs_consumed; desktop/bridge/view_reducer.go 的 applyAgentStartLocked（RunNode.TaskID）; 回归：scripts/test-store-queue.mjs 场景 25–41 | 丢一条 agent_end 就永久“运行中”常亮（同步子 run / 异步子 run / 会话终态各自有证据，见 §6.1 第 1 条） |
| BR-18 | B18 | I/T | P1 | desktop/app/src/transport/index.ts:8-16; desktop/app/src/transport/mock.ts; electron.ts:20-29 | ① 测试/演示通过而真机失败（mock 不发 command_response 的真实错误码/超时语义） |
| BR-19 | B19 | I/Q | P1 | useAppStore.ts:3647-3668; electron.ts:82-83; main.ts:999-1003 | 乐观 UI 无法回滚或被回滚错（合成失败后再来真响应，第二个 ok:true 被 delete pending[id] 后当无主响应处理） |
| BR-20 | B20 | L/I | P1 | useAppStore.ts:3623-3640; useAppStore.ts:3615-3619; useAppStore.ts:3627-3631 | ① pendingRestoreAttempt 永不清理（每会话一条，长会话列表下单页内存缓升） |
| BR-22 | B22 | H | P1 | main.ts:265-267; main.ts:121; desktop/bridge/appdata.go:24-27 | npx/npm（MCP server 启动方式）、git、gh、ssh、python venv 全部看不到真实 HOME → MCP 服务器认证失败、git 提交者身份丢失、缓存全冷（首次启动明显变慢） |
| BR-28 | B28 | I/Q | P1 | desktop/bridge/main.go:6629/6640/6648; main.go:6670-6696; useAppStore.ts:3605 | 用户看到“发送无反应”（pending[id] 永不结算，直到 10s 兜底） |
| BR-29 | B29 | S/I | P1 | main.ts:1450; main.ts:471-518 | ① 拼错字段名被永久写入 settings.json（theme:'darkk' 之类），UI 读回后行为未定义 |
| BR-30 | B30 | L/Q | P1 | main.ts:1903-1905; desktop/bridge/main.go:4232-4262; main.go:6680-6685 | ① 会话 record 的写入可能被截断（.bak 兜底因此变得关键，见 B9） |
| FE-04 | F04 | Q/L | P1 | store/useAppStore.ts:3337 | interrupting/promoted 乐观态永久停留 |
| FE-06 | F06 | D/L | P1 | store/useAppStore.ts:2768 | ①「模型串 session」类问题（注释 2795-2802 记录 provider 反查的补丁） |
| FE-07 | F07 | U/S | P1 | store/useAppStore.ts:1225-1226 | 用户把折叠当布局偏好，实际是「每会话的布局记忆」 |
| FE-08 | F08 | U/Q | P1 | store/useAppStore.ts:3417 | B 里刚写的提交信息被无声清空 |
| FE-10 | F10 | U/C | P1 | components/RightPanel.tsx:892 | ①tab id 复用/重排若遗漏，滚动位置与 md/csv 模式会贴到另一个文件上 |
| FE-11 | F11 | S/L | P1 | hub/useHubStore.ts:355-356 | 保存时算出错误的 diff —— 要么漏发改动（服务端仍是旧值），要么把 A 的字段写进 B（如果 prop 换成 B） |
| FE-12 | F12 | U/S | P1 | hub/useHubStore.ts:91 | 语义分裂（有的干净、有的残留） |
| FE-16 | F16 | U/L | P1 | store/useAppStore.ts:4461-4462 | 用户在跑子智能体/后台任务时，主会话会莫名重播历史渐入 |
| FE-18 | F18 | L/Q | P1 | store/useAppStore.ts:7705-7706 | 视图冻结或只剩残缺尾部 |
| FE-23 | F23 | C/H | P1 | lib/localAsset.ts:104-105 | base64 图片常驻内存直至刷新，长会话内存持续上涨 |
| FE-30 | F30 | U/H | P1 | lib/appearance.ts:198 | 亮色自定义底色在深色主题下泄漏（代码注释记录过该 bug） |
| FE-32 | F32 | T/G | P1 | transport/index.ts:5 | 测试间串状态（设置、key 存在性、输入历史） |
| FE-33 | F33 | T/I | P1 | store/useAppStore.ts:3406-3412 | 新功能在浏览器（mock）里 100% 挂，在 Electron 里看起来正常（或反之），排错成本极高 |
| FE-34 | F34 | U/L | P1 | main.tsx:56 | ①所有「挂载时拉一次」的 effect 在 dev 下跑两遍（hub/HubSidebar.tsx:77-80 的 refresh()+loadGroups() 就是双请求） |
| GO-16 | G16 | M/H | P2 | tools/engine.go:46 | 临时目录无限增长（长命令 / 大输出） |
| GO-19 | G19 | G/T | P2 | provider/auth/oauth/util.go:121–124 | 任何测试替换后不还原 → 后续所有 OAuth flow 用错 transport/TLS 配置 |
| GO-20 | G20 | C/T | P2 | provider/registry_json.go:89 | 模型/价表/上下文窗口全部按旧注册表 |
| GO-23 | G23 | H/F | P2 | memory/config.go:45–53 | 模型"忘事"或重复记忆 |
| GO-25 | G25 | Q/G | P2 | runtime/python.go:625–640 | 锁等待超时被报成"另一实例正在 bootstrap"（可能其实是同进程另一协程） |
| GO-26 | G26 | C/Q | P2 | session/compact_scheduler.go:26–28 | 阈值被测试改动泄漏 → 生产语义变成"1MiB 就压缩"（与"太小的文件重写不划算"相反） |
| GO-27 | G27 | Q/M | P2 | agenthub/artifact.go:25 | 并发/越权路径下产物版本冲突、预览指向被覆盖内容 |
| BR-14 | B14 | C/D | P2 | desktop/bridge/price_refresh.go:23-31 | 重启后价表回退到编译期快照 → 成本/MaxCost 熔断/cost_usd 注入全部按 0 或缺价计算 |
| BR-16 | B16 | L/I | P2 | desktop/bridge/main.go:6616; useAppStore.ts:4611-4616; useAppStore.ts:4650 | embed/浏览器自举等服务依赖该信号 → reload 后进入“等待就绪”分支，只能靠第一条命令响应或超时解脱 |
| BR-17 | B17 | I/F | P2 | desktop/bridge/mobile_hub.go:85-112; mobile_hub.go:113-140 | 手机端看不到“bridge 重启/告警/定时任务变更”等跨会话状态 → 表现为“手机端一直显示运行中，桌面端已经结束” |
| BR-23 | B23 | D/S | P2 | main.ts:1544-1555; main.ts:1561-1563; main.ts:1497-1499 | ① 被删 provider 的明文 api_key 继续留在文件里（main.ts:1553 显式保留） |
| BR-24 | B24 | S/Q | P2 | main.ts:1578-1597; desktop/bridge/mcp_projects.go:244 | bridge 侧对用户层 mcpServers 的任何写入（或另一实例的保存）被整段抹掉 |
| BR-25 | B25 | M | P2 | agenthub/archive.go:19-25; archive.go:52-100; desktop/bridge/hub_archive.go:21-41 | 孤儿段永久占盘（未见 GC 路径） |
| BR-26 | B26 | L/M | P2 | desktop/bridge/main.go:6610; main.go:6683-6685; main.go:4026-4050 | 停机期间到点的任务重启首个 tick 只触发一次（NextRun 已落盘）；间隔内多次折叠，无 catch_up 标记 |
| FE-09 | F09 | U | P2 | components/RightPanel.tsx:381 | 折叠再展开后 commit 草稿/展开分区/过滤词消失 |
| FE-13 | F13 | C/L | P2 | hub/groupStream.ts:10 | 幽灵流式气泡长期停留 |
| FE-14 | F14 | C/F | P2 | hub/useHubStore.ts:203 | 左栏未读/排序卡住不再更新 |
| FE-15 | F15 | U/L | P2 | hub/useHubStore.ts:209-216 | 2s 内新消息不贴底 |
| FE-17 | F17 | Q/L | P2 | store/useAppStore.ts:4464-4495 | 顺序敏感的状态机（llm_start→llm_end→tool）在极端时序下错序 |
| FE-19 | F19 | U/C | P2 | components/Composer.tsx:223 | 内存随会话数增长（图片 base64） |
| FE-20 | F20 | U | P2 | components/LeftPanel.tsx:83 | 多工作区同时展开，活跃项高亮但不再自动展开 |
| FE-21 | F21 | U/M | P2 | components/LeftPanel.tsx:100 | 同类「浏览位置」在不同栏有 3 套生命周期（本地/模块 Map/持久化），任何「记住我在哪」的需求都会得到 3 种答案 |
| FE-22 | F22 | C/U | P2 | components/markdown.tsx:953 | 全局节流是跨组件的——一个巨大的代码块会把所有其它代码块的高亮挤后 250ms |
| FE-24 | F24 | C/L | P2 | store/useAppStore.ts:1797 | 一个会话首次自动打开过可视文件后，该会话内不再自动打开（包括上下文已完全不同） |
| FE-25 | F25 | C/Q | P2 | store/useAppStore.ts:2081 | 5s 内同一文件不能重试（表现为「刚切会话，预览不刷新」） |
| FE-26 | F26 | Q/L | P2 | store/useAppStore.ts:3361 | web tab 永久「拉取中…」（代码注释 8900-8904 记录了该成因） |
| FE-27 | F27 | Q/F | P2 | store/useAppStore.ts:2588-2589 | 翻页结果被当作「替换」而非「追加」（或反之），历史列表错乱/重复 |
| FE-28 | F28 | C/Q | P2 | store/useAppStore.ts:8416 | 图表按错误窗口归日（近一年热力图 vs 7 天视图串数据） |
| FE-29 | F29 | L/Q | P2 | store/useAppStore.ts:2531-2560 | 压缩态最多挂 5m15s |
| FE-31 | F31 | U/C | P2 | i18n.ts:3720-3724 | 性能退化 + 语言切换瞬间的 toast/错误文案混语言 |
| FE-35 | F35 | U/H | P2 | lib/narrativeSkin.ts:12-27 | 皮肤切换会让组件走不同 DOM 分支，而这些分支的本地 state（GroupView 的 view/displayMode、Composer 的 menu/cmdMenu）不会重置 → 同一状态在两套皮肤下渲染成不同位置（详见 raw F35） |
| GO-28 | G28 | Q | P3 | session/record_lock.go:33–35 | 对照项：引用计数 + 可观测 + 显式 nil 三态，风险低（用作 G3/G25 的样板） |

### 6.1 P0 处置顺序（建议）

0. **BR-21 + BR-04 + GO-11 + FE-03**：`xlsxPV/docxPV/pptxPV/hookTrust/imPlugin` 按 workspace 分槽；主进程所有 `webContents.send` 走判空包装；`cronSettings` 改 `*bool` / 先赋默认再 Unmarshal；前端导航代际丢弃补 trace 并区分“过期/不一致”。这四条是 §6.1 原先漏掉的 P0。
1. **BR-03 + BR-15**：先把身份/事件信封做对；否则后面所有恢复/计费都建立在错归属上。
   **批次 1（P5-03 / P5-03b）进度**：

   *BR-03 —— emit 行闭环*：`jsonwire.Envelope` 把「删同名键 + 注入宿主键」合成一个入口（结构异常回退 `ForceKeys`，再失败丢弃并告警，绝不带重复键上线）。行级测试对 stdout 断言顶层 `session_id`/`workspace` 恰好一份（空串/重复键/缺字段/嵌套对象/值里的 JSON 字符串，负控：重复键能数到 2、嵌套同名键不计）；宿主**附加字段**（`cost_usd`/`todos`/`diff`）走同一个信封，`TestEmitTodoSnapshotExactlyOnce` 断言 `ToolResponse{todo_write, Todos:[...]}` 的 stdout 行顶层 `todos` 恰好一份且为宿主值（负控：只注入不删会数到 2）。

   *BR-03 残留（N7/N8，**本批明确不做**，别再宣称闭环）*：
   - **N7 前端归属兜底**：`useAppStore` 的 `getSid` 在 `session_id` 缺失时仍回退 `activeSessionId`，空 sid 还会建 `views['']`。也就是说，只要有一行**真的**漏了 `session_id`（旧日志、`writeLine` 直写行、未来新增的写入点），事件仍会落到「当前打开的会话」，跨项目串台的可能**没有消失**。要闭环得改成「sid 缺失 / 不在会话表 → 丢弃并记诊断」，这是跨全部事件分支的语义改动（恢复重放、`agent_send`、hub 群事件都依赖当前兜底），单列一批做。
   - **N8 `writeLine` 直写行不走信封**：`out.writeLine`（`command_response` / `bridge_ready` / `sandbox_status` / `mesh_status` / `cron_changed` …）直接写 map，不经过 `Envelope`。这些行大多显式带 `session_id`/`workspace`，风险低于事件行，但**没有行级断言兜着**；后续入口：让 `writeLine` 也走同一个信封（或在 `out` 层统一注入），并为每类行补计数断言。

   *BR-15 —— 运行位收敛（P5-03d 收口）*：运行位在这些终止事实上收敛，每一笔都写进 `events.ts` 的有界诊断（`runSettleTraces`，测试读 `__runSettles`）：`session_run_error`（主 run 终态 + 同步子 run，并冻结残留流式块）、`session_closed`（run / tool / agent 卡片 / `async_task` 占位全量落地，并冻结残留流式块）、`task_end`（按卡片 `taskId→runId` 与工具节点 `taskId` 收敛**执行方**；**不用** `TaskEnd.RunId` —— 它是发起方）、新批次开口（`injected=false` 证明上一轮持久化那段**被落地的 run 树**结束 → 顶层 run + 该树下的同步子 run 一起收敛，并冻结上一轮残留的流式块；`injected=true` 是运行中插话，**不是**终止证据）、`agent_end` 收尾**当前顶层 run** 时顺带收敛该树下的同步子 run、无 `run_id` 的 `agent_end`（收敛唯一顶层 run）。三条「父 run 终止」证据（`agent_end` / 新批次开口 / `session_run_error`）共用同一个 helper（`settleSyncSubRuns`），口径只写一次；**作用域也由同一个参数定** —— 三条路径都必须给出「本次真正落地的根 run」（`roots`），`syncSubRunIds` 只从这些根沿 `parentId` 做后代闭包，**遇到带 `task_id` 的节点停止下沉**。子 run 的三种形态，各自的**判据**与**终态证据**：
   - **同步子 run**（in-loop，无 `task_id`）：**在被落地的那棵 run 树下、且从根到它的路径上没有 `task_id`** 时，父 run 收尾**一并收敛** —— run 节点落脚、agent 卡片 `abandoned`（卡片可能在 `blocks`，也可能被分页到 `agentDetached`，两处都扫）。它是父 run 里的阻塞调用，父 run 结束它不可能还在跑；这是它丢了自己的 `agent_end` 之后唯一可证明的终止事实。**这句话的前提是作用域**：只对被收敛的 run 树的后代成立，不是「全视图里所有无 `task_id` 的子 run」。注意 `subagent` 的 `maxSpawnDepth = 2` 只是**深度上限**，不表示 depth-2 一定同步：当前桌面端所有 spawn 工具都是 `Background: true`（`subagent/agent_tools.go` / `subagent/subagent.go` / `desktop/bridge/main.go`），后台派发必写 `AgentStart.TaskId`；无 `task_id` 的 `runSync` 只在宿主注册同步 spawn 工具时可达，当前桌面端不注册。因此「主 run → 异步后台任务（depth-1，带 `task_id`）→ 它自己派出的 in-loop 子 run（depth-2，无 `task_id`）」在当前线上不可达，场景 40/41 是**防御性覆盖**（给未来同步 spawn 宿主或其他调用方兜底），不是今天的线上形态。即使如此，作用域仍按「被落地 run 树」收窄：depth-2 那条的父是异步 run，**不归父 run 收尾管**（父 run 的 `agent_end` 不证明别人派出去的后台任务里的阻塞调用也结束了），终态证据仍是它自己的 `agent_end` / `task_end`。闭包遇 `task_id` **停止下沉**，异步节点自己也不算同步子 run；`roots` 为空（本次没有真正落地任何根）时一个都不收。代价是：根已经落地后**迟到**的同步子 run，三条证据都不会再收它（`session_closed` 才兜底）；当前桌面端无同步 spawn，故同样不可达。P5-03d 之前这里按「全视图里无 `task_id`」收，depth-2 被父 run 的 `agent_end` 收敛成 `done` / 卡片 `abandoned`，而 `settleRunNodes` 只动 running 节点 —— 误收敛**永久**，真实 `agent_end` 到了也改不回来（复审 blocking）。状态**不继承**父 run 的 `runStatus`：父 run abort 才是 `interrupted`（阻塞调用被一起打断），`stop` / `max_iterations` / `error` 一律 `done` —— 那几个 `finish_reason` 说的是**父 loop** 的结局，父 run 达轮数上限时那个正常返回的子 agent 不该被画成 interrupted / failed。
   - **异步子 run**（带 `task_id`）：可以晚于父 run 收尾，终态证据是它自己的 `agent_end` / `task_end`，**绝不**随父 run 收敛；后代闭包遇到它**停止下沉**，所以它派出去的 depth-2 in-loop 子 run 同样不在父 run 收尾的范围里（P5-03d，见上一条）。这句保证的**前提**是判据本身成立 —— 判据有两个来源，缺一不可：① `RunNode.TaskID`（`agent_start.TaskId`；P5-03c 起 Go 侧 checkpoint 的 `run_nodes` 也持久化它，暖恢复读得回来 —— 但**旧 checkpoint 的节点没有**它）；② 卡片（`agent` / `async_task`）的 `taskId`，来源是 `view.blocks` **加** `view.agentDetached`（200 块窗口外的卡片只活在这里，那不是缓存副本）。**没有“节点是唯一事实”这回事**：暖恢复 + 卡片被分页出去时只剩 ②；P5-03c 之前两个来源同时缺，父 run 的 `agent_end` 会把仍在跑的后台子 run 误杀（评审 B-1）。
   - **既无 `task_id` 又不阻塞**：线上不存在（后台派发必带 `AgentStart.TaskId`，in-loop 子 agent 必阻塞）。测试 fixture 若这样写就是与线上不同形，已改正（场景12 的后台子 run 补 `task_id`）。
   - 找不到归属的 `agent_end`（迟到旧 run）只进诊断、不改当前 run 的结算位。
   - **诊断口径**：父 run 收尾顺带收敛的子 run 记同一条记录，`runIds` 只含**被顺带收敛**的 run（不含事件自己那个 run）；没有实际收敛就**不写**诊断（不许假装收敛过）。
   - **验证入口**：`desktop/app/scripts/test-store-queue.mjs` 场景 25–41（`npm run test:store`；35–36 是**暖恢复 × 卡片在 `blocks` / 只在 `agentDetached`**，37–38 是同步子的另外两条父 run 终止证据，39 是同步子状态映射，**40 是 depth-2 反例**：「主 run → 异步后台任务（带 `task_id`）→ 它派出的 in-loop 子 run」在父 run `agent_end` 后必须**全部仍 running**（节点与卡片），`busy` 仍真、无收敛诊断，随后它自己的 `agent_end` 能真正落地（证明没被误锁），**41 是作用域的正反两面**：新批次开口 / `session_run_error` 同样不下沉进异步子树，同时根自己的同步子 run 照旧收敛，工具节点不进入闭包（N-6））+ 变异实验（删 `injected` 判断 / `task_end` 兼认 `run_id` / 关掉同步子 run 收敛 / 去掉 `async_task` 分支 / 去掉流式块冻结 / **删 `agentDetached` 来源** / **删卡片来源** / **删节点 `taskId` 来源** / `session_closed` 不冻结 / 同步子继承父 `runStatus` / **把 `syncSubRunIds` 退回「全视图里无 `task_id` 的子 run」**，各自让对应场景定向变红）。
2. **BR-01 + BR-02 + BR-08 + BR-29**：settings 单写者、patch 白名单、key 分离；这是“改一个主题丢 key”的根因。
3. **GO-01 + BR-14**：价表单一权威源 + `cost_source`；先把账算对。
4. **BR-09 + GO-02 + GO-04**：checkpoint 自包含 + sidecar 强校验 + seq 落盘；先把历史恢复对。
5. **GO-14 + BR-13 + FE-02/FE-18**：运行位单一状态机 + 启动恢复；先把“刷新后交卷”修掉。
6. **FE-01 + FE-05 + BR-12**：发送归属/草稿 scope/幂等账本；先把用户输入不丢、不串。
7. **GO-10 + GO-03 + GO-25**：只增不减的 registry/锁表加 Stats 与回收；历史事故不再复现。
8. **BR-27 + GO-19/24/26 + FE-32**：测试隔离；否则以上修复都可能被假绿掩盖。

## 7. 明确接受 / 非缺陷

不是所有隐式状态都要消灭。以下属于“可以保留，但必须有边界”：

| 项 | 为什么可接受 | 需要补的条件 |
|---|---|---|
| GO-28 `recordLockTable` 引用计数 + `bgCompact` 显式 nil | 有回收、有观测点、三态明确；是 G3/G25 的样板 | 把同一模式推广到 ledgerTaskLocks/runtime 锁表 |
| FE-22 Markdown/highlight cache | 有 LRU/字符预算、纯派生、可重建 | key 加主题/密度；预览卡绕过节流的问题单独修 |
| FE-09/FE-21 组件本地 UI 态 | 折叠/分区/过滤是临时展示态，不需要跨会话 | 在源码标注 `ephemeral/session/persistent`；commitMsg 这类用户输入提升到 store |
| FE-35 `data-theme/data-skin` 显式 DOM 契约 | 主题/皮肤本来就该由属性驱动 | 切皮肤时重置跨皮肤歧义的本地态；避免结构分支依赖它 |
| BR-25 归档 manifest 高版本 fail-fast | 避免旧代码误读新格式，方向正确 | 降级时退化为“只读热日志 + 告警”，不要整段空白 |
| 测试/调试用 localStorage（FE-32） | 接受的是 mock/浏览器回退这一媒介；状态污染本身仍要修 | `resetTransport()` 同时清 mock 状态与 4 个命名空间键；FE-32 按 P1 处理 |

## 8. 已知事故对照

这张表把项目记忆中已经发生过的事故映射到本次清单；不是新增证据，而是用来判断“清单是否覆盖了真实疼痛”：

| 已发生事故 | 对应条目 | 覆盖判断 |
|---|---|---|
| settings.json 默认值被当成已存值写回，provider 丢失 | BR-01、BR-29 | 直接覆盖 |
| 价表三份拷贝 / 宿主 priceTable 遮盖 / 缺价报 0 | GO-01、BR-14 | 直接覆盖 |
| checkpoint 30s 超时、406x 写放大、全局锁等待 | GO-02、BR-09、BR-10、GO-04 | 机制覆盖；性能数字需单独 benchmark |
| 暖恢复漏 `live` → 刷新后 Working 变 Worked | GO-14、BR-13、FE-02、FE-18 | 组合覆盖（运行位/恢复/镜像） |
| `task_promoted` 的 `session_id` 空串覆盖宿主注入 | BR-03 | 直接覆盖 |
| 子智能体续跑：task_id 复用/多卡/计时两把尺子 | GO-06、GO-10、GO-07、FE-16 | 部分覆盖；初始 prompt 未发事件需下一轮补 |
| 切会话滚动定位/手势残留 | FE-10、FE-07、FE-15 | 部分覆盖；建议加 e2e 手势断言 |
| 测试污染真实 HOME/价表/端口 | BR-27、GO-19、GO-24、GO-26、FE-32 | 直接覆盖 |
| runtime 皮肤组件内定义组件、`data-open` 当 live | FE-22、FE-34、FE-35 | 部分覆盖；属性能实现债，单独 benchmark |

> 未在本次三路报告中单列的已知项：`after_run` 的 `omitempty` 三态与记录落位（记忆中有完整复盘）。它属于 S/L 类，建议作为下一轮第一个补审项，不要因为本表没有行就认为不存在。

## 9. 治理建议

### 9.1 七条原则

1. **能显式就显式**：进入类型、schema、协议或配置；不能显式化的，必须有名字、所有者、生命周期、失效条件。
2. **默认值不落盘**：区分“未设置”“显式设为默认”“显式设为 0/空”；持久化只写用户意图。
3. **身份不猜**：事件必须带完整归属；归属不明时丢弃并记录，不回退“当前会话/当前智能体”。
4. **生命周期单源**：运行位由状态机或服务端快照唯一决定；前端只消费，不自行推断。
5. **缓存可失效**：每个缓存声明 key、TTL/版本、失效触发与内存上限。
6. **环境可注入**：路径、端口、时间、随机、Provider、价表在测试中可替换；生产默认值集中管理。
7. **状态可观测**：启动打印 effective config（脱敏）；关键状态变更带 ID 与原因；提供转储/诊断入口。

### 9.2 工程护栏

- **状态登记表**：所有包级 `var`、`sync.Once`、全局 `Map`、落盘文件、`localStorage` key、`Getenv`、`listen` 在 PR 里登记 `owner / scope / lifetime / reset / metric`。可先做成 `docs/agent-hub/STATE-REGISTRY.md`，再考虑 lint。
- **持久化 schema 版本与未知字段保留**：settings、archive manifest、checkpoint、Hub 账本都加 `schema_version`；迁移测试覆盖旧版本。
- **事件 envelope 强类型**：`seq`（每进程单调）、`ts`、`workspace`、`session_id`、`run_id`、`origin`、`epoch`、`attempt`；`command_response` 回显 `origin/epoch`；每行 `session_id` 恰好一次。
- **单一 selector 取代双真相**：`views[sid]` 与顶层镜像改成 `selectViewField(sid, key)` 或生成镜像；新增字段只写一处。
- **测试环境统一入口**：`internal/testenv.WithHome(t)` / `WithPriceTable(t)` / `WithAppDataDir(t)` / `WithPort0(t)`；禁止测试直接读真实 `~/.go-code`；CI 加“干净检出编译门”。
- **降级必须可观测**：`codemodeStore` 退化内存、`cron.Ledger` 写失败、`StateStore.nilSafe`、`LoadTrust` 空库、`respTap=nil` 都要发事件/告警，不能只返回字符串。
- **前端 local state 分档**：`ephemeral`（关面板即丢）/ `session`（跨会话保留）/ `persistent`（落盘）；`closeHub`、`resetView`、卸载路径按档位声明清理。

### 9.3 分阶段落地

| 阶段 | 目标 | 具体动作 | 验收 |
|---|---|---|---|
| 0（1–2 周） | 止血 P0 | settings 单写者+patch+key 分离 + **存量 v1→v2 迁移**；session_id 结构化注入；价表单一源；checkpoint generation 目录 + `.bak` 单测；busyState + 启动恢复；BR-21/BR-04/GO-11/FE-03；测试 HOME 守卫 | 新装/迁移后 settings diff 仅目标键；未迁移机器只保证不再恶化；事件行 `session_id` 计数=1；`.bak` 恢复块数>0；刷新不误报完成；`go test` 后用户数据 mtime 不变 |
| 1（2–4 周） | 去结构性债 | Hub `next_seq` 落盘、queue TTL、cron running 折叠；registry/锁表引用计数 + Stats；事件 seq/origin/epoch；前端 ScopeKeyMap + 单 selector + docRefs 分槽 | 重启后同 client_msg_id 不重复执行；queued 超期自动 interrupted；registry size 有上限；切会话不串 docRefs |
| 2（持续） | 可观测与防回归 | effective config 脱敏输出；缓存/registry/queue 指标；丢弃分支 trace；季度状态清点；ADR 固化生命周期 | 每次“历史不出来/一直转圈”都能在日志里定位到具体丢弃分支；新增全局状态有登记 |

**v1→v2 迁移规则（阶段 0 的前置）**：以发布时的 `DEFAULT_SETTINGS` 快照为基线，逐键删除与默认值**完全相同**的顶层键（删除优先）；`provider` 段只保留显式 `api_key/label/prices/usage_input_includes_cache` 与 `deleted` 标记；未知键原样保留。值等于默认但用户曾显式选择的键无法与“被固化的默认”区分，统一按删除处理并接受其未来跟随新默认——这是 v1 不可恢复的信息；v2 起 UI 写入时落 `explicit` 标记，后续迁移不再丢失用户意图。迁移前写 `settings.json.bak`，失败回滚；迁移幂等，完成后写 `schema_version: 2`。

### 9.4 建议的验收指标

- `settings.json`：新装/迁移后一次主题保存 diff ≤ 1 行；未迁移机器只要求“不再新增固化键”（存量 200+ 键由 v2 迁移清理）；`provider.*.api_key` 永不出现在 settings；`schema_version` 存在。
- 事件：`grep -c '"session_id"'` 每行 ≤ 1；空/不匹配归属的丢弃计数可查。
- 计费：每条 usage 有 `cost_source`；`cost_usd=0` 与“无价”分开统计；价表变更可追溯到来源。
- 恢复：启动日志输出 `recovery: queued=N interrupted=M notified=K`；`.bak` 恢复测试块数 > 0。
- 运行位：`busyState` 只有枚举值；前端不出现“默认 false 即空闲”的分支。
- 测试：干净 HOME + 无 settings + `-shuffle=on` 下目标包全绿；测试结束真实 HOME 无写入。
- 缓存：每个缓存有 size/evict 指标；只增不减的 map 在 CI 里有上限断言。

## 10. 覆盖与限制

### 10.1 覆盖

- **Go**：约 8.9 万行非测试代码（按本次范围粗略口径：含注释/空行的 `wc -l` 约 116k，剔空行/注释约 86k），覆盖 `session/ agents/ core/ events/ memory/ cron/ im/ agenthub/ provider/ hooks/ mcp/ plugin/ runtime/ sandbox/ subagent/ todo/ tools/ execproc/` + `desktop/bridge/*.go`。
- **前端**：`store/useAppStore.ts`（约 11k 行，精读约 1.5k 行）、`hub/*`、`components/*`、`lib/*`、`transport/*`、App/main/i18n/CSS 开关。
- **桥接/环境**：Electron `main.ts` 1905 行、preload/transport、bridge 的 settings/hub/archive/recovery/mobile/jsonwire/config_patch 等。
- **方法**：枚举包级 `var`/`sync.Once`/`sync.Map`/`json:"-"`，反查测试是否快照还原，读历史事故注释；只报有代码证据的项。

### 10.2 未覆盖 / 待复核

1. **Go**：`agents/llm_compressor.go` 的 registry/压缩阈值；`im/gateway.go` 重连与 mirror；`execproc` spool 生命周期；`sandbox/isolatedEnv`；`plugin/browser|computer` 的 CDP seq；`desktop/bridge` 的 `checkpoint.go`、`compress_*`、`context_*`、`billing_commands.go`、`hub_notify.go`、`mobile_*`、`git.go`。
2. **前端**：`ProviderPage`、`WorkspaceFiles`、`SettingsModal`、`MCPTab/HooksTab`、`CronPage` 的未保存草稿/tab/scroll/轮询；`RightPanel` 的 browser/embed 段；`NeedsYouPanel` 的 answers 草稿；Electron 主进程 `embedSetSession`、`input-history`、深链队列。
3. **Bridge/环境**：`embed/fs-ops/chrome-import/node-path` 的 scope 校验；mock 与真 bridge 的错误码/时序对账；`cron` 的补跑逻辑；`uncaughtException` 是否注册；`ipcMain` 全量入参白名单。
4. **另需补**：mesh 跨机 `session_list` 身份冲突；`hub_background` spawn/WaitGroup 与 `hubArchiveRunning` 复位；`.blocks.idx` 崩溃一致性；`docs/agent-hub` 文档 vs 代码对账。
5. **未做**：全量 `go test -race`、运行时 heap/goroutine dump、真实配额下的 localStorage 失败路径。因此部分“测试泄漏”结论来自测试代码注释与手写快照模式，需实跑复现后再定级。

### 10.3 复现 / 验证命令

```bash
# 前端门禁
cd desktop/app
npx tsc --noEmit
npm run test:p4
npm run test:css

# Go 门禁（干净 HOME，避免价表/设置污染）
HOME=$(mktemp -d) GO_CODE_HOME=$(mktemp -d) go test ./desktop/bridge -count=1
go test ./agenthub/... ./session/... -count=1

# 事件重复键（BR-03）的静态判据：任何 stdout 行 session_id 计数应 ≤ 1
# 运行一次后台任务摘出，抓 bridge stdout：
#   ... | python3 -c 'import sys,json; [print(json.loads(l).get("session_id","")) for l in sys.stdin]'
```

## 11. 附录

### 11.1 三路调研任务

> task_id 来自本会话后台任务记录（`~/.go-code/sessions/.../agents/<task_id>.jsonl`）；不随仓库分发。

| 面 | task_id | 覆盖 |
|---|---|---|
| Go 内核/运行时/持久化 | task-24z9jt7z | 28 条（GO-01…GO-28） |
| 桌面前端 | task-agn8vh3q | 35 条（FE-01…FE-35） |
| Bridge/Electron/协议/环境 | task-fa93d6sx | 30 条（BR-01…BR-30） |

### 11.2 术语

- **live / runEnded / turnRunning**：前端运行位；恢复时必须来自服务端快照，不能用 `false` 默认值推断。
- **checkpoint**：SessionRecord + view_checkpoint + blocks/sidecar；不是单一 JSON。
- **sidecar**：`.blocks`（块内容）与 `.blocks.idx`（索引），必须与 record 同代。
- **effective config**：默认值合并后的运行配置；打印时必须脱敏。
- **cost_source**：`upstream` / `table` / `unpriced`；用来区分“上游报了 0”和“没有价格”。
- **ScopeKeyMap**：以 workspace/session/agent 为 key 的显式容器，禁止裸模块级 `Map` 承载跨会话状态。

### 11.3 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-10 | 初稿：三路调研合并、93 条清单、Top 10、五个跨面案例、治理路线 |
| 2026-11-10 | P5-03（§6.1 批次 1）：BR-03 emit 信封收口（Envelope/ForceKeys + 行级测试）；BR-15 运行位收敛（session_run_error / session_closed / task_end / 新批次开口 / 未匹配 agent_end 诊断），残留见 §6.1 第 1 条 |
| 2026-11-11 | P5-03b（复审 CHANGES_REQUESTED 修复）：`agent_end` 收「当前顶层 run」时顺带收敛**同步子 run**（无 task_id，节点落地 + 卡片 abandoned，诊断 runIds 只记被顺带收敛者，异步子 run 不动）；`user_inputs_consumed(injected=false)` 与 `session_run_error` 收敛后冻结残留流式块并落 `runEnded`；`session_closed` 补 `async_task` 占位；`task_end` fixture 补「run_id = 发起方」并加反向场景；测试场景 25–34 + 五个变异实验。**BR-03 残留单列为 N7（getSid 兜底 / 空 sid 建 `views['']`）与 N8（writeLine 直写行不走 Envelope），不再宣称全闭环** |
| 2026-11-11 | P5-03c（复审 B-1 / N-1 / N-2 / N-3 / N-6 修复）：① **B-1 暖恢复误杀异步子 run** —— `syncSubRunIds` 的卡片源补 `agentDetached`、`abandonAgentCards` 两处都扫，Go `applyAgentStartLocked` 给 run 节点补 `TaskID`（JSON `omitempty`，格式不变）+ checkpoint 落盘/读回断言；② **N-1** 「同步子收敛 + 卡片 abandon」抽成 `settleSyncSubRuns`，新批次开口与 `session_run_error` 也调用（只收无 task_id 的子 run）；③ **N-2** `session_closed` 复用 `freezeStreamingAssistants` 冻结残留流式块；④ **N-3** 同步子 run 状态改 `aborted ? interrupted : done`，不再继承父 run 的 `runStatus`；⑤ **N-6** 节点判定显式 `kind==='run'`。测试场景 35–39 + 变异实验（删 `agentDetached` 来源 / 删卡片来源 / 删节点 `taskId` 来源 / 两条 N-1 路径回退 / 不冻结 / 继承 `runStatus`）。N7/N8 仍是 BR-03 残留，本批不做 |
| 2026-11-11 | P5-03d（复审 blocking 修复）：**收敛作用域 = 被落地的 run 树**。`syncSubRunIds(v, roots)` 从本次真正落地的根沿 `parentId` 做后代闭包、**遇 `task_id` 停止下沉**（异步节点自己也不算同步子 run），根节点自己不算，`roots` 为空时一个都不收；`settleSyncSubRuns` 接收并透传 `roots`，三个调用点分别给「本次被收敛的根」—— `agent_end`（`run_id` 存在传 `[id]`，缺 `run_id` 传当前视图的顶层 run 列表）/ 新批次开口（`stale.settled`）/ `session_run_error`（`runs.settled`）。修的是 depth-2 误杀（**防御性覆盖**：`maxSpawnDepth = 2` 只是深度上限；当前桌面端 spawn 全为 `Background: true`，depth-2 也带 `task_id`，无 `task_id` 的 `runSync` 在桌面端不可达，见复审 N-1）：此前按「全视图里无 `task_id` 的子 run」收，父 run 的 `agent_end` 会把仍在跑的 depth-2 收敛成 `done` / 卡片 `abandoned`，且 `settleRunNodes` 只动 running 节点 → **永久**误收敛（真实 `agent_end` 到了也改不回来）。测试场景 40–41（40 = depth-2 反例 + 它自己的 `agent_end` 仍能落地；41 = 新批次开口 / `session_run_error` 不下沉进异步子树 + 根自己的同步子仍收敛 + 工具节点不进闭包），场景 25–39 断言未改。N7/N8 仍是 BR-03 残留，本批不做 |

---

**一句话总结**：这份清单里最危险的从来不是“某个变量没清空”，而是**同一个事实有多个载体、多个写入者、多个默认值**。先把身份、运行位、配置、价表、checkpoint 五个“单一真相”钉住，剩下的 P2 才不会反复长回来。
