# Artifacts V2 · 2026-11-19

> **文档性质**：全新设计的系统级 Artifacts 方案（非恢复稿、非修订稿）。
> 设计参考 **Claude Code Artifacts** 与 **Claude Design**，并结合本项目已恢复的历史设计内容
> 与当前代码实现重新设计。
>
> **来源标注**
> - `【现状】` —— 已在当前代码中逐条验证，附文件:行号
> - `【历史】` —— 从 2026-11-19 误删事故中逐字恢复的历史设计（`ARTIFACTS_AGENT_DESIGN.md` §21–§24）
> - `【元设计】` —— 用户提供的通用设计（`ARTIFACTS_META_DESIGN.md`）
> - `【外部参考】` —— Claude Code / Claude Design 的公开行为
> - `【方案】` —— 本文的新决策
>
> **版本**：v2.0 · 2026-11-19 · 状态：待评审

---

## 目录

1. 为什么要有 V2
2. 调研与借鉴
3. 系统边界
4. 总体架构
5. 数据模型
6. 协议
7. 详细代码改动
8. 产物判定
9. 预览与选区实现
10. 安全
11. 性能与成本
12. 评估
13. 测试
14. 实施计划
15. 风险登记册
16. 兼容与迁移
17. 附录

---

## 1. 为什么要有 V2

### 1.1 现状能做什么 `【现状】`

一次 Agent 运行结束后，产物会以**卡片**形式出现：

| 环节 | 位置 |
|---|---|
| 从 `write_file` / `edit_file` 收集 | `agents/artifacts.go:77 observe`（包 `ToolResponse`） |
| 同路径合并（`Added`/`Removed` 累加） | `:146 mergeArtifactDeltas` |
| 噪声过滤（`.tmp/.bak/.log/.swp`、噪声目录） | `:183 filterNoiseArtifacts`、`:173`、`:178`、`:200` |
| 单轮上限 50，超出置 `ArtifactsTruncated` | `:32 maxArtifacts` |
| 挂到 `AgentEnd` 单点发出 | `events/agent.go:94`、`agents/agent_loop.go:1479` |
| Reducer 投影成稳定 schema | `view_reducer.go:276`、`:309 ArtifactRef`、`:561 blockKindArtifacts` |
| 渲染成卡片 | `desktop/app/src/components/ArtifactCard.tsx` |

**卡片的「预览」是什么？** 对 HTML 走 `openHtmlInBrowser` → 右栏内嵌浏览器 **`file://`**
（`useAppStore.ts:3715`）；office 类走 `previewXlsx/Docx/Pptx`（bridge 转 HTML 后同样 `file://`）。

### 1.2 现状的五个硬伤

| # | 硬伤 | 后果 |
|---|---|---|
| 1 | **`file://` 起不来** | ES Module、`fetch`、跨域资源全被拦。真实产物页（带 css/js/图片）打开就是白屏 |
| 2 | **只按文件列举** | 一个页面 = `index.html` + `app.js` + `style.css` + 3 张图 → 卡片里 **6 条**，用户不知道哪个才是入口 |
| 3 | **没有版本** | 只有单轮 `SHA256`（`core/tool.go`），改一版就覆盖，**无法回滚、无法按版本导出** |
| 4 | **模型不能主动声明** | 全靠自动汇总；脚本（bash/Python）产出的文件**收集器根本看不到** |
| 5 | **页面只能看不能选** | 没有选区协议，用户想说「把这个按钮改大」只能自己描述 |

### 1.3 V2 要达成的三件事

1. **打开就能看**：入口页在本地 HTTP 下完整渲染（不再 `file://`）
2. **能选能引用**：页面元素/文字可以选中，作为**数据**（不是指令）引用回对话
3. **改的是源码**：Agent 改的始终是磁盘上的源文件，预览只是副本

---

## 2. 调研与借鉴

### 2.1 Claude Code Artifacts（外部参考）

`【外部参考】` Claude Code 的 `Artifact` 工具语义（据其官方文档）：

- **形态**：把一个 HTML 或 Markdown 文件发布成 **claude.ai 上的私有交互页**
- **分享**：可生成公开链接；Team / Enterprise 组织内可共享，公开分享需 Owner 开启
- **前置条件**：需要 Pro / Max / Ultra / Team / Enterprise 计划 + `/login` 认证
- **能力边界**：托管在云端，产物是"页面"，**不可选中元素回注到对话**

**可借鉴**：① 一个入口而不是一堆文件；② 内容即页面（HTML/Markdown 双形态）；
③ 私有优先、分享显式。
**本项目不照搬**：我们**本地优先**（不依赖账号与云），且额外提供**选区回注**。

### 2.2 Claude Design（外部参考）

`【外部参考】+【元设计】` Design 不是一个独立产品，而是 **Artifact 的一种 type**：
用**生成期的结构约束**换取**事后可精确编辑**。

| 层 | 内容 |
|---|---|
| 共用底座 | 存储、版本、预览宿主、分享、Agent 工具、选区协议、导出 |
| 通用编辑层 | 选区、批注、patch、撤销（可靠性分级） |
| **Type 层** | 结构约定、id stamping、画布与属性面板、设计系统、type 专属指令 |

**结构约束三条**（`【元设计】`）：

- 每个画板一个文件：`boards/<boardId>.html`，`boardId` 匹配 `^[a-z0-9-]{1,32}$`
- 可编辑元素必须有 `data-nid`，格式 `^n_[0-9a-z]{6}$`，同 artifact 内唯一、删除不复用，缺失由 Host stamping 补齐
- 颜色/间距/字号走 CSS 变量（tokens），不允许硬编码，lint 不通过则回灌 Agent

**可借鉴**：`data-nid` stamping 让"选中→改"从**猜测**变成**精确定位**。
**本项目策略 `【方案】`**：V2 先做**通用编辑层**（`data-src` 服务时注入），
`data-nid` stamping 留到 Design type 阶段（P8+），避免 V2 一步跨太大。

### 2.3 与元设计的对齐与三处修订

`【元设计】` 的通用设计整体采纳，但按本项目事实修订三处：

| 元设计 | 本项目 | 理由 |
|---|---|---|
| `artifact.publish`（点号） | `artifact_publish`（**下划线**） | `【历史】§22.1` 线上名一律下划线 |
| 通信 JSON-RPC 2.0 | 复用既有 **`command_registry` + IPC** | `【现状】command_registry.go:90/117` 已有成熟模式 |
| 守卫挂在选区任务开始 | **会话创建时即装**（`active=false` 零成本） | `【历史】` 实测：`selection.commit` 可能晚于 run goroutine 启动 |

---

## 3. 系统边界

### 3.1 两套产物系统，本项目只做 A

| | **A 系统级产物（V2 目标）** | **B agenthub 群组产物（排除）** |
|---|---|---|
| 代码 | `core.Artifact` + `agents/artifacts.go` | `agenthub.Artifact`（`agenthub/store_types.go:94`） |
| 语义 | 一次运行的产出，给**当前用户**看 | 登记到**群组**、带可见性，给其他 agent/人看 |
| 归属 | 无 | `OwnerAgentID` / `GroupID` / `Visibility` |
| 存储 | **无**（只活在事件/checkpoint） | `FileStore` 持久化 |
| 命令 | 无 | `hub_artifact_get`（`command_registry.go:117`）、`hub_task_artifact_preview`（`:90`） |

**硬约束**：两套**不合并、不共享存储、命名不冲突**。V2 工具一律 `artifact_*`；
群组级保持 `hub_artifact_*`，**不得改名或复用**。

### 3.2 与既有预览能力的关系

| 能力 | 现状 | V2 处置 |
|---|---|---|
| HTML 内嵌浏览器 `file://` | `useAppStore.ts:3715 openHtmlInBrowser` | **替换为** Preview Host 的 `http://127.0.0.1:…`（保留旧路径作降级） |
| `` ```html `` 纯展示（未落盘） | `:3716 openHtmlContentInBrowser`（data URL） | **不动**（无文件系统产物，走 data URL 合理） |
| xlsx/docx/pptx | `previewXlsx/Docx/Pptx` → bridge 转 HTML | **不动**（office 转换是另一条链路） |
| 群组任务产物图片 | `hub_task_artifact_preview.go:42` | **不动** |

---

## 4. 总体架构

### 4.1 分层图

```
┌──────────────┐    tools schema    ┌─────────────────────────────────────────┐
│  Agent Loop  │◄──────────────────►│ tools/builtin/artifact_*.go (新增)       │
│ agent_loop.go│                    │ publish / promote / export              │
└──────┬───────┘                    └───────────────┬─────────────────────────┘
       │ AgentStart / AgentEnd                      │ 声明
       ▼                                            ▼
┌──────────────────────────┐   ChangeSet   ┌──────────────────────────────────┐
│ ChangeTracker            │──────────────►│ Classifier（四层判定）             │
│ ✅ 钩子: agents/artifacts │               │ 1 声明 2 目录约定 3 评分 4 用户纠正 │
│ ➕ 快照: snapshot diff    │               └──────────────┬───────────────────┘
└──────────┬───────────────┘                              ▼
           │                              ┌──────────────────────────────────┐
           └─────────────────────────────►│ Version Store（新增）              │
                                          │ .artifacts/<id>/v<N>/ + blobs     │
                                          └──────────────┬───────────────────┘
                                                         ▼
                                          ┌──────────────────────────────────┐
                                          │ Preview Host（新增）               │
                                          │ 127.0.0.1:rand + 签名 + data-src   │
                                          │ + SSE reload                      │
                                          └──────┬───────────────┬───────────┘
                                                 ▼               ▼
                                          ┌────────────┐  ┌──────────────────┐
                                          │ 内嵌浏览器  │  │ ArtifactCard      │
                                          │ +Inspector │  │ ✅ 已存在          │
                                          └─────┬──────┘  └──────────────────┘
                                                │ selection / comment
                                                ▼
                                          ┌──────────────────────────────────┐
                                          │ Selection Refs（新增）             │
                                          │ → <selection> 注入下一轮           │
                                          └──────────────────────────────────┘
```

### 4.2 模块职责

| 模块 | 归属 | 职责 | 状态 |
|---|---|---|---|
| `ChangeTracker` 钩子半 | `agents/artifacts.go` | 从工具响应提取产出 | ✅ 已有 |
| `ChangeTracker` 快照半 | `agents/artifacts.go`（扩展） | AgentStart/End 目录快照 diff，兜住 bash/Python | ➕ 新增 |
| `Classifier` | `agents/artifacts.go`（扩展） | 四层判定、评分、入口选择 | ➕ 新增 |
| `Version Store` | `agenthub/…`？**否** → 新包 `artifacts/` | 版本目录、blob 去重、GC | ➕ 新增 |
| `Preview Host` | `desktop/bridge/preview_host.go` | HTTP 服务、签名、`data-src` 注入、SSE | ➕ 新增 |
| `Inspector` | `desktop/bridge/artifact_inspector.js`（`go:embed`） | 元素拾取、选区上报 | ➕ 新增 |
| `Selection Refs` | `desktop/bridge/selection_refs.go` | 会话级引用表、校验、清理 | ➕ 新增 |
| 写守卫 | `desktop/bridge/selection_guard.go` | `beforeToolCall` 拦截 | ➕ 新增 |
| UI 卡片 | `ArtifactCard.tsx` | 展示、预览、设为产物 | ⚙️ 改造 |
| Reducer | `view_reducer.go` | 投影稳定 schema | ⚙️ 扩展 |

### 4.3 一次运行的完整时序

```
User: 发消息
  │
  ├─ AgentStart ──► ChangeTracker 拍目录快照（mtime+size 哈希，忽略 .git/node_modules）
  │
  ├─ loop: 工具调用
  │    └─ write_file/edit_file ──► 钩子记录 delta（✅ 已有）
  │    └─ bash/python ───────────► 钩子看不到（❗这就是为什么要快照 diff）
  │
  ├─ AgentEnd
  │    ├─ ChangeSet = 钩子 delta ∪ (End 快照 − Start 快照)
  │    ├─ Classifier：声明优先 → 目录约定 → 评分 → 候选项
  │    ├─ 依赖闭包：解析入口 HTML 的 src/href/import（x/net/html）
  │    ├─ Version Store：写入 v<N>，VersionID = sha256(path→hash)
  │    └─ Preview Host：开放 /{wsKey}/{id}/{v}/{sig}/…
  │
  ├─ 前端：产物卡片（入口 + 依赖 + 版本号）
  │    └─ 「预览」→ 内嵌浏览器 http://127.0.0.1:…（不再 file://）
  │
  └─ 用户选中元素 ──► selection.commit ──► Selection Refs
                        └─► 下一轮请求携带 <selection>（数据）
                              └─► Agent 调 artifact_edit（带 base_version）
```

---

## 5. 数据模型

### 5.1 保留的既有结构（不动）

```go
// 【现状】core/tool.go:181 —— 事件侧最小集合，前端卡片直接消费
type Artifact struct {
    Name    string `json:"name"`
    Path    string `json:"path"`
    Kind    string `json:"kind"`      // created | modified
    Added   int    `json:"added,omitempty"`
    Removed int    `json:"removed,omitempty"`
    Size    int64  `json:"size,omitempty"`
    Lines   int64  `json:"lines,omitempty"`
    SHA256  string `json:"sha256,omitempty"`
}
```

> **不修改它**：`view_reducer.go:309 ArtifactRef` 与 14 个 reducer 测试、前端卡片全部依赖该形状。

### 5.2 新增：产物与版本

```go
// 【方案】新包 artifacts/（不放 agenthub，避免与群组产物混淆）
type ArtifactID string   // 形如 "art_<8hex>"，由 wsKey+入口路径派生，稳定

type Artifact struct {
    ID         ArtifactID `json:"id"`
    WSKey      string     `json:"ws_key"`      // ⚠️ 必带：见 §15 R1
    Title      string     `json:"title"`
    Entry      string     `json:"entry"`       // 相对路径，如 index.html
    Latest     int        `json:"latest"`      // 最新版本号
    DeclaredBy string     `json:"declared_by"` // "" | "tool" | "manifest" | "score" | "user"
    CreatedAt  time.Time  `json:"created_at"`
    UpdatedAt  time.Time  `json:"updated_at"`
}

type ArtifactVersion struct {
    ArtifactID ArtifactID        `json:"artifact_id"`
    Version    int               `json:"version"`
    VersionID  string            `json:"version_id"` // sha256(排序后 path→hash 拼接)
    Entry      string            `json:"entry"`
    Files      map[string]string `json:"files"`      // relPath → blob hash
    TotalBytes int64             `json:"total_bytes"`
    CreatedAt  time.Time         `json:"created_at"`
}
```

### 5.3 磁盘布局

```
<workspace>/.artifacts/
  index.json                          # ArtifactID → Artifact 元信息
  <artifactID>/
    v1/
      manifest.json                   # ArtifactVersion（含 files 映射）
    v2/
      manifest.json
  blobs/
    <sha256 前 2 位>/<sha256>          # 内容寻址，跨版本/跨产物去重
```

> **为什么内容寻址**：同一张 logo 在 v1..v9 里不变，只存一份；
> 版本目录里只存 `path → hash`，天然支持回滚与按版本导出。

### 5.4 选区引用

```go
// 【历史】§23.2 selection_refs.go
type SelectionRef struct {
    RefID     string          `json:"ref_id"`
    WSKey     string          `json:"ws_key"`
    SessionID string          `json:"session_id"`
    Artifact  ArtifactID      `json:"artifact"`
    Version   int             `json:"version"`
    VersionID string          `json:"version_id"`  // 失效判定基准
    Kind      string          `json:"kind"`        // 8 种之一
    Payload   json.RawMessage `json:"payload"`     // ≤2KB 裁剪后
    Label     string          `json:"label"`       // chip 文案
    CreatedAt time.Time       `json:"created_at"`
}
```

### 5.5 状态机（前端 pane）

`【历史】` 已还原的状态机，V2 沿用：

```
idle → creating → connecting → replaying → live → exited
                     └──────────┴──────────► failed
```

> 注：这是**右栏终端** pane 的机器；产物预览 pane 用简版
> `idle → loading → live → failed`，共享同一套 `reset/gap/exit` 语义。

---

## 6. 协议

### 6.1 A 层：Agent Tool 协议

#### 6.1.1 工具清单

| 工具 | 读写 | 挂载 | 关键约束 | 错误码 |
|---|---|---|---|---|
| `artifact_publish` | 写 | 文件系统模式 | entry 必须在产物区且存在；同 id 再调即新版本 | `ENTRY_MISSING` / `NOT_IN_PRODUCT_AREA` / `TOO_LARGE` |
| `artifact_promote` | 写 | 文件系统模式 | `from` 必须在草稿区；目标 id 不存在则新建 | `NOT_IN_SCRATCH` / `TARGET_EXISTS` |
| `artifact_export` | 读 | 总是 | 基于版本快照；返回路径 + `warnings` | `UNSUPPORTED` / `TOO_LARGE` / `VERSION_GONE` |
| `source_get` | 读 | 有选区时 | `context_lines ≤ 20` | `OUT_OF_RANGE` |
| `source_find` | 读 | 有选区时 | 最多 5 条候选 | — |
| `preview_screenshot` | 读 | 总是 | ≤ 1MB | `RENDER_TIMEOUT` |
| `preview_inspect` | 读 | 有选区时 | `outer_html ≤ 8KB` | `NODE_GONE` |
| `designsystem_get` | 读 | Design type | 按名取，不返回全量 | `NOT_FOUND` |

#### 6.1.2 入参/出参示例

```jsonc
// artifact_publish
{ "entry": "artifacts/landing/index.html", "title": "Landing v2" }
// →
{ "ok": true, "artifact_id": "art_3f9a2c1b", "version": 3,
  "version_id": "sha256:9c1e…", "files": 6, "total_bytes": 41233 }

// artifact_edit（P6）
{ "artifact_id": "art_3f9a2c1b", "base_version": 3,
  "scope": { "kind": "element", "selector": "#hero h1" },
  "edits": [{ "file": "artifacts/landing/index.html",
              "old_str": "<h1 class=\"t\">Hello</h1>",
              "new_str": "<h1 class=\"t\">你好</h1>" }] }
// →
{ "ok": true, "version": 4, "version_id": "sha256:44aa…" }
```

#### 6.1.3 通用约定

- **线上名一律下划线** `【历史】§22.1`
- **永不抛异常**：失败统一 `{ok:false, code, message}`，面板显示一句人话
- 返回给**模型**的结构化；返回给**脚本**（CodeMode）的是对象而非 JSON 字符串 `【历史】§22.3`

### 6.2 B 层：页面协议（Inspector ↔ Host）

**信封**（MessageChannel 私有端口，不走 `window.postMessage` 裸通道）：

```jsonc
{ "v": 1, "type": "selection.changed", "seq": 42, "payload": { … } }
```

| 消息 | 方向 | 说明 |
|---|---|---|
| `inspector.enable` / `disable` | Host → 页面 | 开关拾取模式 |
| `hover` | 页面 → Host | **节流 16ms**，只报 `rect` + 轻量 selector |
| `selection.changed` | 页面 → Host | 点选/框选/文字选中的当前值 |
| `selection.commit` | 页面 → Host | 用户点「引用」后提交 |
| `comment.added` | 页面 → Host | 批注 pin |
| `highlight` / `clear` | Host → 页面 | 高亮控制 |
| `edit.result` | Host → 页面 | 应用结果或 `conflict` |

**约束**：

- 单条消息 ≤ **256KB**；截图只传 blob 引用
- 协议**只增字段不改字段**；收到更高 `v` 时忽略未知字段，不报错
- 版本冲突：`apply_ops.baseVersion ≠ 当前` → 丢弃并回 `edit.result{ok:false,reason:"conflict"}`

### 6.3 C 层：生命周期钩子

| 钩子 | 时机 | 用途 |
|---|---|---|
| `onAgentStart` | run 开始 | 拍目录快照 |
| `beforeToolCall` | 每次工具调用前 | **写守卫**（选区任务期间限制范围） |
| `onToolResult` | 每次工具返回 | ✅ 已有：`agents/artifacts.go:77` |
| `onAgentEnd` | run 结束 | 快照 diff → ChangeSet → 判定 → 版本 |

钩子**不进** `tools` schema；`beforeToolCall` 拒绝时把 `reason` 作为 `tool_result` 回给模型，
让它改用正确的工具 `【元设计】`。

### 6.4 D 层：上下文注入

```
<selection kind="element" version_id="sha256:9c1e…">
  <div class="hero"><h1 class="t">Hello</h1></div>
</selection>

<user_instruction>
把标题改成更正式的措辞
</user_instruction>
```

**铁律**：只有 `<user_instruction>` 是指令；`<selection>` 内一律是**数据**，
并在系统提示里写明这条 `【历史】§23.3`。

裁剪规则 `【历史】`：`outer_html ≤ 2KB`、`text ≤ 300 字`、`computed` 按需拉取。

### 6.5 预览 HTTP + SSE

| 端点 | 方法 | 说明 |
|---|---|---|
| `/{wsKey}/{artifactID}/{version}/{sig}/{path...}` | GET | 静态资源；`sig = HMAC(secret, wsKey\|id\|version\|path)` |
| `/{wsKey}/{artifactID}/{version}/{sig}/` | GET | 入口页（自动补 `Entry`） |
| `/__events?token=…` | GET | SSE：`reload`（版本更新）、`artifact.ready` |

响应头：`Content-Type` 按扩展名；`Cache-Control: no-store`（预览不缓存，避免旧版本残留）。

**`data-src` 注入**：只在返回 HTML 时注入 `<script src="/__inspector.js">` 与
`data-src="file:line:col"` 属性——**磁盘源码保持干净**，Agent 改的永远是源码。

### 6.6 CodeMode 编排 `【历史】§22`

| 要求 | 内容 |
|---|---|
| 可编排广告 | 工具注解声明可被脚本调用 |
| 结构化结果 | 脚本拿**对象**，不是 JSON 字符串 |
| 路径语义 | 脚本路径 vs 模型路径的差异写进工具描述 |
| 图片降级 | 图片类工具在脚本内降级为元信息 |
| 并发 | **per-artifact 串行**（同一产物不并发写） |
| 目录预算 | 单目录产出上限，超限截断并标记 |

---

## 7. 详细代码改动

### 7.1 新增文件（14 个）

| # | 文件 | 职责 | ~行数 |
|---|---|---|---|
| N1 | `artifacts/model.go` | `Artifact`/`ArtifactVersion`/`ArtifactID` 及校验 | ~120 |
| N2 | `artifacts/store.go` | 版本目录读写、blob 内容寻址、GC | ~320 |
| N3 | `artifacts/store_test.go` | 版本哈希稳定性、去重、GC | ~200 |
| N4 | `artifacts/resolve.go` | 入口解析 + 依赖闭包（`x/net/html`） | ~180 |
| N5 | `artifacts/resolve_test.go` | 无引号属性、跨行、`url()`/`@import` | ~160 |
| N6 | `agents/artifact_snapshot.go` | AgentStart/End 目录快照 diff | ~150 |
| N7 | `agents/artifact_classify.go` | 四层判定 + 评分 + 候选 | ~220 |
| N8 | `agents/artifact_classify_test.go` | 评分边界、声明优先级 | ~180 |
| N9 | `tools/builtin/artifact.go` | `artifact_publish` / `promote` / `export` | ~280 |
| N10 | `tools/builtin/artifact_test.go` | 失败语义、越界、同 id 升版 | ~200 |
| N11 | `desktop/bridge/preview_host.go` | HTTP 服务、签名、注入、SSE | ~360 |
| N12 | `desktop/bridge/preview_host_test.go` | 200/403、签名含 wsKey、注入只出现在响应 | ~220 |
| N13 | `desktop/bridge/selection_refs.go` | 引用表、校验、清理 | ~200 |
| N14 | `desktop/bridge/selection_guard.go` | `beforeToolCall` 守卫（常驻安装） | ~120 |

> `【历史】` 原始清单为 24 项（含前端、脚本、eval），上表是 V2 Phase 1 的 14 项；
> 其余 10 项（前端 Inspector、卡片改造、`artifact_eval.py`、`artifact-verify.ts` 等）见 §7.3。

### 7.2 修改文件（Go 侧，8 处）

| # | 文件:位置 `【历史】行号，以当前代码为准` | 改什么 |
|---|---|---|
| M1 | `agents/artifacts.go:183 filterNoiseArtifacts` | **先清死代码**（R6），再扩展为评分层 |
| M2 | `agents/artifacts.go` | 新增快照 diff 入口，`collect()` 合并两路来源 |
| M3 | `agents/agent_loop.go:1479` `AgentEnd` | 携带 `ArtifactID`/`Version`/`VersionID`（新增字段，旧字段不动） |
| M4 | `desktop/bridge/command_registry.go`（`:90/:117` 同文件） | 新增 `CmdArtifactPublish` / `CmdArtifactPreview` / `CmdArtifactSelection` |
| M5 | `desktop/bridge/main.go` ~`:3250`（`case CmdAsk:` 里 `text := str(...)` 之后） | 解析 `selection_refs`，拼进 `<selection>` 块 |
| M6 | `desktop/bridge/main.go` `:764-800`（`switch ev := e.(type)` 的 `case *events.AgentEnd:`） | `emit` 时清选区作用域 |
| M7 | `desktop/bridge/main.go` ~`:3600`（`CmdAsk`/`CmdAskBatch` 两分支） | 清选区作用域 |
| M8 | `desktop/bridge/main.go` `:4773-4785`（`case CmdDeleteSession:`） | 会话删除时清引用表 + 守卫 |

### 7.3 修改文件（前端，5 处）

| # | 文件 | 改什么 |
|---|---|---|
| F1 | `desktop/app/src/components/ArtifactCard.tsx:130-215` | 「预览」改为走 Preview Host；新增「设为产物 / 不是产物」 |
| F2 | `desktop/app/src/store/useAppStore.ts:3715 openHtmlInBrowser` | 增加 `openArtifactPreview(artifactId, version)`；`file://` 保留为降级 |
| F3 | `desktop/app/src/lib/bridge.ts` 末尾（与 `embedOpenTarget` 同族） | 新增 `artifactPreview` / `artifactSelection` / `artifactExport` 封装 |
| F4 | `desktop/app/src/components/Narrative.tsx` | 把 `bundles` 传进产出卡（`E-Narrative.tsx-1`） |
| F5 | `desktop/app/src/components/ArtifactsBlock.tsx` | `bundles` prop + **渲染位置**（`:126` 空判、`:150` 卡片根节点，两处都要改） |

> `【历史】` 特别提示：`ArtifactsBlock` 只改渲染会让「本轮只有 bundle、没有文件清单」的
> 轮次整块消失——空判必须同时考虑 `bundles?.length`。

### 7.4 关键实现片段

#### 7.4.1 版本哈希（稳定、与顺序无关）

```go
func versionID(files map[string]string) string {
    keys := make([]string, 0, len(files))
    for k := range files { keys = append(keys, k) }
    sort.Strings(keys)                       // ← 顺序无关
    h := sha256.New()
    for _, k := range keys {
        fmt.Fprintf(h, "%s\x00%s\n", k, files[k])
    }
    return hex.EncodeToString(h.Sum(nil))
}
```

#### 7.4.2 签名（含 wsKey，防跨工作区）

```go
func previewSig(secret []byte, wsKey, id string, v int, p string) string {
    mac := hmac.New(sha256.New, secret)
    fmt.Fprintf(mac, "%s\x00%s\x00%d\x00%s", wsKey, id, v, p) // wsKey 必须在签名里
    return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:22]
}
```

#### 7.4.3 守卫常驻安装

```go
// 会话创建时即装（不是任务开始时）
scope := newSelectionScope()                 // active=false 时零成本放行
s, err := session.NewSession(id, loop, store,
    session.WithToolGuard(scope.guard),      // ← PreToolUse
    …)
bs = &bridgeSession{id: id, runtime: ws, s: s, selectionScope: scope}
```

#### 7.4.4 依赖闭包（用 tokenizer，不用正则）

```go
z := html.NewTokenizer(r)
for {
    tt := z.Next()
    if tt == html.ErrorToken { break }
    if tt != html.StartTagToken && tt != html.SelfClosingTagToken { continue }
    name, hasAttr := z.TagName()
    switch string(name) {
    case "script", "img", "link", "iframe":
        for hasAttr {
            k, v, more := z.TagAttr(); hasAttr = more
            if isRefAttr(string(name), string(k)) { refs = append(refs, string(v)) }
        }
    }
}
// 另需处理 <style> 内的 url() 与 @import（单独一轮 tokenizer）
```

---

## 8. 产物判定

### 8.1 四层

| 层 | 机制 | V2 现状 |
|---|---|---|
| 1 声明 | `artifact_publish` 或 `.artifact/manifest.json` | ➕ 新增（优先级最高） |
| 2 目录约定 | 产物区 `artifacts/`、草稿区 `.scratch/` | ➕ 新增（对 bash/Python 同样有效） |
| 3 规则评分 | 对未声明文件打分，只生成**候选** | ⚠️ 部分（已有噪声过滤，缺正分与阈值） |
| 4 用户纠正 | 卡片「设为产物 / 不是产物」 | ➕ 新增（记入会话偏好） |

### 8.2 评分表

| 信号 | 分值 |
|---|---|
| 位于 `artifacts/` | +5 |
| 本轮新建/修改的 html / svg / pdf / md | +2 |
| 位于 `.scratch/` / tmp / node_modules / `__pycache__` | −10 |
| 文件名含 tmp / debug / test / out / log | −3 |
| 空文件，或超阈值的二进制 | −2 |
| 被另一个产物引用（css/js/图片/数据） | 并入该产物，不单独计分 |

**≥ 3 分记为候选**：卡片提示，**不自动打开**。已声明的不参与评分。

### 8.3 入口选择优先级

1. `.artifact/manifest.json` 的 `entry`
2. `artifact_publish` 的 `entry`
3. 目录内唯一 `index.html`
4. 本轮修改量最大的 `.html`
5. 评分最高者

### 8.4 噪声规则（复用并修正）

`【现状】agents/artifacts.go:173 artifactNoiseDirs`、`:178 artifactNoiseSuffixes`、
`:200 isNoiseArtifactPath` 已实现一半。V2 需：

- 清掉 `filterNoiseArtifacts` 里的**死代码**（R6）
- 把"排除"升级为"负分"，让噪声参与评分而非直接丢弃（用户仍可手动设为产物）

---

## 9. 预览与选区实现

### 9.1 `data-src` 注入

```go
// 只改发给浏览器的副本
func injectSourceMap(htmlSrc []byte, rel string) []byte {
    // 1) <script src="/__inspector.js" data-artifact=… data-version=…>
    // 2) 给每个元素补 data-src="rel:line:col"
    //    行号由 tokenizer 的 z.Offset() 换算；粗粒度到"元素起始行"即可
}
```

> `【历史】` 明确：**服务时注入**，不是入库时注入。因为 bash/Python 生成的页面
> 不经过任何入库钩子；若在入库时注入，磁盘源码会被污染。

### 9.2 Inspector 行为

| 交互 | 行为 |
|---|---|
| 点选 | capture 阶段拦截 `click`，`preventDefault`；高亮层放 **Shadow DOM**（不污染页面样式） |
| Shift 点 | 追加选中（`elements`） |
| Alt + 点 | `elementsFromPoint` 轮换层叠节点 |
| 框选 | 拖矩形；默认"完全包含"，LCA 判断成组，上限 20 |
| 文字选区 | `selection` API 取 `Range`，带 `textRange` |
| 盲区 | canvas / 跨域 iframe / closed shadow DOM → 落 `region` + 截图 |

**语义提升规则** `【元设计】`：忽略纯布局包装与行内元素；遇到 `data-component`、`button`、
`li`、`a`、有背景或内边距的容器即停；提升不改变原始命中点，面包屑始终可退回叶子。

### 9.3 失效与漂移

```
发送时校验 ref.version_id == 当前 artifact 的 version_id？
  ├─ 是 → 正常注入
  └─ 否 → 用文本指纹在新版本里重新定位
           ├─ 找到 → 替换 payload，标注「已按新版本重定位」
           └─ 找不到 → 明确提示「选区已失效」，不静默套用
```

### 9.4 写守卫

```
选区任务激活（scope.active = true）
  └─ beforeToolCall 校验：写入路径是否落在 scope 允许范围内
       ├─ 是 → 放行
       └─ 否 → 拒绝，reason 作为 tool_result 回给模型（让它改用 artifact_edit）
```

**安装时机**：会话创建时（`active=false`）。原因见 §15 R2。

---

## 10. 安全

| 面 | 措施 |
|---|---|
| 访问控制 | Preview Host 只监听 `127.0.0.1`，随机端口，校验 `Host` 头 |
| 路径安全 | 先 `realpath` 再比较前缀，挡住 `../` 与符号链接逃逸 |
| 令牌 | 签名含 `wsKey`；`expires` 短时；预览 URL 不写日志 |
| 内容不可信 | 产物是模型生成的页面：沙箱 WebView 打开，CSP 限制外联 |
| Prompt injection | `<selection>` 声明为数据 + 系统提示硬规则 + 模型侧不得把页面文本当指令 |
| 导出 | 导出内容剥离 `data-src` / `data-nid` / Inspector 脚本 |
| 绝对路径资源 | 页面里 `/style.css` 不落在令牌前缀下 → 注入 `<base href>` 或 cookie 携带令牌 |
| 需要构建的项目 | Vite/Next 不自动起 dev server，先让用户确认；只自动打开纯静态产物 |
| 打开策略 | 一次运行最多自动打开一个标签页；`autoOpen: always \| ask \| never`；无头环境跳过 |

---

## 11. 性能与成本

| 项 | 目标 | 手段 |
|---|---|---|
| 预览首字节 | < 100ms（本地） | 直接读 blob，不经过桥的 JSON 序列化 |
| `data-src` 注入开销 | O(HTML 大小)，单次 < 10ms（≤2MB） | tokenizer 单遍，不构建 DOM |
| 版本存储 | 增量 | 内容寻址去重；同 blob 只存一份 |
| GC | 保留最近 N 版（默认 10）+ 被引用版本 | 超期且无引用的 blob 清理 |
| 上下文成本 | 选区 ≤ 2KB、`text ≤ 300 字` | 完整 HTML 不注入，按需 `preview_inspect` |
| 自检 | 最多 2 轮 | 防死循环 |

---

## 12. 评估

### 12.1 技术选型对比

| 决策点 | 选项 | 结论 | 理由 |
|---|---|---|---|
| 预览传输 | `file://` / 本地 HTTP / 自定义协议 | **本地 HTTP** | `file://` 起不来 ES Module 与 fetch（现状硬伤 1） |
| 依赖解析 | 自写正则 / `x/net/html` tokenizer | **tokenizer** | `【历史】§21.14`：属性可无引号跨行、`<style>` 里 `url()`/`@import` 形态多；正则漏掉时表现为"页面正常、依赖没进包" |
| 版本标识 | 自增 int / 内容哈希 | **两者并存** | int 给人看，hash 用于去重与失效判定 |
| 选区注入时机 | 入库时 / 服务时 | **服务时** | bash/Python 产物不经过入库钩子；入库注入会污染源码 |
| 高亮层 | 直接插 DOM / Shadow DOM | **Shadow DOM** | 不污染页面样式 |
| Inspector 通信 | `window.postMessage` / MessageChannel | **MessageChannel** | 私有端口，页面脚本无法伪造 |
| 存储位置 | `~/.go-code` / 工作区 `.artifacts/` | **工作区** | 产物属于工作区；随工作区迁移/删除，天然隔离 |
| 与 agenthub 产物 | 合并 / 独立 | **独立** | 语义、归属、可见性完全不同（§3.1） |

### 12.2 依赖影响

- 新增：`golang.org/x/net/html`（Go 官方模块，无传递依赖树）
- `go.mod` 唯一变化；`go.sum` 预计 +2 行
- 前端：无新依赖（Inspector 用原生 DOM API；如需截图复用现有 Electron `capturePage`）

### 12.3 容量估算

| 场景 | 产物数/轮 | 版本数/会话 | 存储估算 |
|---|---|---|---|
| 轻量（单页 + 3 依赖） | 1 | 3–5 | ~1–3 MB |
| 中等（多页 + 资源） | 2–3 | 10 | ~10–30 MB |
| 重（大图/视频） | 1–2 | 5 | 受 `TOO_LARGE` 上限保护（单文件 50MB） |

### 12.4 成功指标

| 指标 | 定义 | 目标 |
|---|---|---|
| 首次可看率 | 运行结束后用户能直接打开入口页的比例 | > 95% |
| 入口准确率 | 自动选中的入口 = 用户认可的入口 | > 90% |
| 误报率 | 用户点「不是产物」的比例 | < 10% |
| 漏检率 | 用户手动「设为产物」的比例 | < 15% |
| 选区解析成功率 | 选中元素能正确定位到源码 | > 90% |
| 回滚率 | 用户回滚到上一版本的比例 | 观测项 |

---

## 13. 测试

### 13.1 单元

| 目标 | 用例要点 |
|---|---|
| `versionID` | 顺序无关；同内容同 hash；单文件变动即变 |
| `wsKey` 隔离 | 两个工作区同名同内容产物 → **不串读**（R1 回归） |
| 签名 | 改 `wsKey`/`id`/`version`/`path` 任一 → 403 |
| 评分 | 各信号边界；`.scratch` 优先负分；被引用并入 |
| 入口选择 | manifest > publish > index.html > 改动量 > 评分 |
| 依赖解析 | 无引号属性、跨行、`<style>url()`、`@import`、相对路径归一 |
| 噪声规则 | `.tmp/.bak/.log/.swp`、噪声目录；**清理死代码后**的等价性 |

### 13.2 集成

| 场景 | 断言 |
|---|---|
| 预览 200 | 入口页、css、js、图片全部 200 |
| 预览 403 | 错签名、过期签名、跨工作区 |
| `data-src` 注入 | **只出现在响应里**；磁盘源码不含 |
| SSE | 新版本落盘后客户端收到 `reload` |
| 依赖闭包 | 6 文件产物 → 1 个 artifact，不是 6 条 |

### 13.3 端到端

| 流程 | 断言 |
|---|---|
| 发布 → 卡片 | 卡片显示入口 + 版本号 + 文件数 |
| 预览 → 选中 → 引用 | 下一轮请求含 `<selection kind="element" version_id=…>` |
| 引用 → 编辑 | `artifact_edit` 带 `base_version`，成功后版本 +1 |
| 版本过期 | 明确提示「选区已失效」，不改错节点 |
| 守卫 | 选区任务期间 `bash` 写文件被拒，`reason` 出现在 tool_result |

### 13.4 回归（必须全绿）

- `artifacts_reducer_test.go` 全部 14 个用例（产物块 JSON 形状、幂等、checkpoint 往返、子 agent 归并）
- `hub_artifact_get` / `hub_task_artifact_preview`（群组产物**不受影响**）
- office 预览（xlsx/docx/pptx）链路
- `openHtmlContentInBrowser`（`` ```html `` 纯展示）

### 13.5 失败语义

**所有新工具/新命令必须配「失败语义 + 测试用例」**`【历史】`：没有测试的改动不应合并。
每个错误码至少一条用例。

---

## 14. 实施计划

| 阶段 | 交付 | 验收 | 预估 |
|---|---|---|---|
| **P0** 版本库 | N1–N5 | 单测绿；`wsKey` 隔离测试通过 | 3d |
| **P1** 预览服务 | N11–N12 | 任意入口页可开；依赖全 200；注入只出现在响应 | 4d |
| **P2** 工具面 | N9–N10 + M3/M4 | `artifact_publish` 后卡片出现、版本 +1 | 3d |
| **P3** 判定四层 | N7–N8 + M1/M2 | 误报/漏检可统计；候选不自动打开 | 3d |
| **P4** 选区协议 | N13 + N14 + M5/F1–F3 | 选中 → 引用 → 模型收到 `<selection>` | 5d |
| **P5** 写守卫 | M6/M7/M8 | 选区期间越界写入被拒 | 2d |
| **P6** `artifact_edit` | 工具 + 漂移 | 版本过期明确提示 | 3d |
| **P7** CodeMode 编排 | 注解 + 结构化结果 | 脚本内可调、结果为对象 | 3d |
| **P8** Design type（后续） | `data-nid` stamping + tokens lint | 画板可精确编辑 | — |

**依赖关系**：P0 → P1 → P2 → P3；P1 → P4 → P5 → P6；P2 → P7。

---

## 15. 风险登记册

| # | 风险 | 后果 | 对策 |
|---|---|---|---|
| R1 | artifact id / version **非全局唯一** | 大文件从**另一个工作区**读出 | 路径、签名、索引一律带 `wsKey`（`【历史】`实测坑） |
| R2 | 守卫装晚 | 「先发消息再点选区」窗口内 bash 放行 | 会话创建时即装，`active=false` 零成本 |
| R3 | 正则解析依赖遗漏 | 页面正常但依赖没进包（最难查） | `x/net/html` tokenizer |
| R4 | 页面内容 prompt injection | Agent 被页面文本操纵 | `<selection>` 声明为数据 + 系统提示硬规则 |
| R5 | 版本存储膨胀 | 磁盘占用 | 内容寻址去重 + 保留 N 版 + GC |
| R6 | `filterNoiseArtifacts` 死代码 | 接线后行为不可预期 | `agents/artifacts.go:183` 先清理（P3 前置） |
| R7 | 预览服务暴露 | 本地端口被滥用 | 127.0.0.1 + 签名 + Host 校验 + 短 TTL |
| R8 | **与 agenthub 产物串台** | 改错系统、命名冲突 | §3.1 硬边界；`artifact_*` vs `hub_artifact_*` |
| R9 | `file://` 降级路径残留 | 旧代码仍可能走 `file://` | F2 保留降级但加告警日志，观测调用量后移除 |
| R10 | `ArtifactsBlock` 只改渲染 | 「只有 bundle」的轮次整块消失 | 空判同时看 `bundles?.length`（`【历史】`） |

---

## 16. 兼容与迁移

| 项 | 策略 |
|---|---|
| `core.Artifact` | **不改结构**（事件 JSON、卡片、14 个 reducer 测试零影响） |
| 历史事件回放 | `ArtifactRef` 投影不变；新字段缺省即可 |
| 旧 `file://` 预览 | 保留为降级；新路径优先；观测残留调用量 |
| 已有产物数据 | 无历史版本可迁移（从未持久化）→ 从下一次 `AgentEnd` 开始建版本 |
| agenthub 产物 | 完全不动 |

---

## 17. 附录

### 17.1 术语

| 术语 | 含义 |
|---|---|
| 产物（Artifact） | 入口 + 依赖闭包的集合，有版本 |
| 入口（Entry） | 用户直接打开的页面，通常是 `index.html` |
| 依赖闭包 | 入口引用的 css/js/图片/数据的递归集合 |
| 草稿区（`.scratch/`） | 不进版本快照的临时产出，N 天后清理 |
| 版本（Version） | 一次快照；`VersionID` 是其内容哈希 |
| 选区（Selection） | 用户在预览里选中并引用回对话的范围，**是数据不是指令** |
| 守卫（Guard） | 选区任务期间限制可写范围 |

### 17.2 证据索引

| 文件 | 内容 |
|---|---|
| `docs/ARTIFACTS_AGENT_DESIGN.md` | 历史设计逐字恢复（§21–§24 + 元设计 20 条对照） |
| `docs/ARTIFACTS_META_DESIGN.md` | 用户提供的通用元设计（355 行） |
| `docs/ARTIFACTS_IMPLEMENTATION_CHECKLIST.md` | 11 个 `E-*` 改动点清单 |
| `docs/ARTIFACT_CODE_FRAGMENTS.md` | 177 段可编译代码块 |
| `docs/ARTIFACT_SECTIONS_EXTRA.md` | 21 整节正文 + 121 段零散片段 |
| `docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md` | 结构化恢复稿（骨架/表格/散文） |
| `docs/recovery-evidence/2026-11-19/` | 恢复证据归档（108 文件 + sha256） |

### 17.3 参考

- Claude Code 文档 · Artifacts（`code.claude.com/docs/en/artifacts`）：HTML/Markdown → 私有交互页、
  公开链接分享、组织内共享、需 Pro/Max/Ultra/Team/Enterprise
- Claude Code 文档 · Tools reference：`Artifact` 工具的设备与计划限制
- 本项目元设计《Agent 产物系统完整设计》（`ARTIFACTS_META_DESIGN.md`）

---

_文档结束。规格变更请走评审；实现与本文不一致处必须回写本节。_
