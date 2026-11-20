# Artifacts V2 · 2026-11-19

> **本文是系统级 Artifacts 的唯一完整设计文档**，自洽、不依赖任何其他文档。
> 内容整合自：通用调研设计、从事故中恢复的历史设计（§21–§24 逐字内容）、
> 当前代码的实际实现状态、以及一轮全面评审的结论（已并入正文，不再单列）。
>
> **参考**：Claude Code Artifacts（HTML/Markdown → 私有交互页）、Claude Design（以结构约束换可编辑性）。
>
> **标注约定**
> - `【现状】` 已在当前代码中逐条验证，附文件:行号
> - `【历史】` 历史设计中逐字恢复的内容
> - `【调研】` 通用调研设计的结论
> - `【方案】` 本文的决策（含对前两者的修订）
>
> **版本** v2.4 · 2026-11-19 · 状态：决策已定（含交互契约）
>
> **三项关键决策（§24）**：存储 = 文件 + 三层索引（不是 SQLite）；传输 = 统一 HTTP（不做自定义协议双通道）；
> 编辑落盘 = Phase 1 仅静态资源，`.tsx/.jsx` 明确返回 `UNSUPPORTED`。

---

## 目录

- 第一部分 总纲
  1. 背景：现状与五个硬伤
  2. 目标、范围与非目标
  3. 系统边界
  4. 总体架构
  5. 数据模型
- 第二部分 协议
  6. A 层：Agent Tool 协议
  7. B 层：页面协议
  8. C 层：生命周期钩子与守卫
  9. D 层：上下文注入
  10. 预览服务协议
  11. CodeMode 编排
- 第三部分 实现
  12. 详细代码改动
  13. 产物判定
  14. 预览与选区的实现细节
  15. 自检闭环
  16. 导出规范
  17. 画布与 Design type
- 第四部分 保障
  18. 成本控制
  19. 安全与边界
  20. 测试
- 第五部分 交付
  21. 实施计划
  22. 风险登记册
  23. 兼容与迁移
  24. 决策记录（存储/传输/编辑已定）
  25. 术语表

---

# 第一部分 总纲

## 1. 背景：现状与五个硬伤

### 1.1 现状能做什么 `【现状】`

一次 Agent 运行结束后，产物以**卡片**形式出现。完整链路：

| 环节 | 位置 | 行为 |
|---|---|---|
| 工具钩子收集 | `agents/artifacts.go:77 observe` | 包住 `events.ToolResponse`，从 `write_file`/`edit_file` 提取落盘结果 |
| 同路径合并 | `:146 mergeArtifactDeltas` | 多次编辑累加 `Added`/`Removed` |
| 噪声过滤 | `:183 filterNoiseArtifacts`、`:173` 噪声目录、`:178` 噪声后缀（`.tmp/.bak/.log/.swp`）、`:200 isNoiseArtifactPath` | 直接丢弃 |
| 单轮上限 | `:32 maxArtifacts = 50` | 超出置 `ArtifactsTruncated` |
| 挂到事件 | `events/agent.go:94 AgentEnd.Artifacts`、`agents/agent_loop.go:1479` | 单点发出 |
| Reducer 投影 | `view_reducer.go:276 Artifacts`、`:309 ArtifactRef`、`:561 blockKindArtifacts`、`:3071 appendArtifactsBlockLocked` | 稳定 schema，14 个用例覆盖 |
| 卡片渲染 | `desktop/app/src/components/ArtifactCard.tsx:130-215` | 「预览 / 打开」两个动作 |

**「预览」实际打开方式**：HTML 走 `useAppStore.ts:3715 openHtmlInBrowser` → 右栏内嵌浏览器
**`file://`**；office 类走 `previewXlsx/Docx/Pptx`（bridge 转 HTML 后同样 `file://`）。

### 1.2 五个硬伤

| # | 硬伤 | 后果 |
|---|---|---|
| 1 | **`file://` 起不来** | ES Module、`fetch`、跨域资源全被拦。带 css/js/图片的真实产物打开就是白屏 |
| 2 | **只按文件列举** | 一个页面 = `index.html` + `app.js` + `style.css` + 3 图 → 卡片 **6 条**，用户不知道哪个是入口 |
| 3 | **没有版本** | 只有单轮 `SHA256`（`core/tool.go:181`），改一版即覆盖，无法回滚、无法按版本导出 |
| 4 | **模型不能主动声明** | 全靠自动汇总；bash/Python 产出的文件钩子看不到 |
| 5 | **页面只能看不能选** | 没有选区协议，用户想说"把这个按钮改大"只能自己描述 |

### 1.3 三条目标

1. **打开就能看** —— 入口页在可控环境下完整渲染
2. **能选能引用** —— 页面元素/文字可选中，作为**数据**（非指令）引用回对话
3. **改的是源码** —— Agent 改的始终是磁盘源文件，预览只是副本

---

## 2. 目标、范围与非目标

### 2.1 一句话目标

> 一次运行结束后，用户**不用自己找文件**就看到最终页面；页面**可点、可选、可引用回对话**；
> 改完自动刷新且不丢状态；Agent 改的始终是源码。

### 2.2 本期范围（Phase 1 = 下面的 A、B 阶段）

- 产物判定四层 + 版本库（内容哈希）
- 预览宿主（注入 + 热刷新）
- 工具面 `artifact_publish` / `promote` / `export`
- 选区协议（8 种引用）+ `<selection>` 注入 + 写守卫
- 自检闭环（最多 2 轮）
- 导出：HTML / PDF / Markdown

### 2.3 非目标（明确后置）

| 项 | 为什么后置 |
|---|---|
| 流式预览（每 ~300ms 前缀容错解析 + morphdom 增量） | 需要 parse5 + morphdom 双依赖与复杂时序；先把"结束后可看"做扎实 |
| 画布 / 多变体同屏对比 | 依赖 Design type 的结构约束先落地 |
| 协作 / 分享链接 / Yjs 共编 | 无多用户场景 |
| PPTX 导出 | 仅幻灯片类产物需要，受众窄 |
| 对话模式（无文件系统的 `artifact_create/read`） | 本项目是桌面 + 文件系统模式，无此场景 |

> 后置 ≠ 忽略：第 17 章给出 Design type 的结构约束草案，第 6 章给出流式预览的接入点，
> 避免将来接入时推翻本文的结构。

---

## 3. 系统边界

### 3.1 两套产物系统，本文只做 A

| | **A 系统级产物（本文目标）** | **B agenthub 群组产物（排除）** |
|---|---|---|
| 代码 | `core.Artifact` + `agents/artifacts.go` | `agenthub.Artifact`（`agenthub/store_types.go:94`） |
| 语义 | 一次运行的产出，给**当前用户**看 | 登记到**群组**、带可见性，给其他 agent/人看 |
| 归属 | 无 Owner/Group | `OwnerAgentID` / `GroupID` / `Visibility` |
| 存储 | **无**（只活在事件/checkpoint） | `FileStore` 持久化 |
| 命令 | 无 | `hub_artifact_get`（`command_registry.go:117`）、`hub_task_artifact_preview`（`:90`） |

**硬约束**：不合并、不共享存储、命名不冲突。A 的工具一律 `artifact_*`；B 保持 `hub_artifact_*`。

### 3.2 与既有预览能力的关系

| 能力 | 现状 | 处置 |
|---|---|---|
| HTML 内嵌浏览器 `file://` | `useAppStore.ts:3715` | **替换**为新预览宿主；旧路径保留为降级 + 打点观测 |
| `` ```html `` 纯展示（未落盘） | `:3716 openHtmlContentInBrowser`（data URL） | **不动** |
| xlsx / docx / pptx | `previewXlsx/Docx/Pptx` | **不动**（office 转换是另一条链路） |
| 群组任务产物图片 | `hub_task_artifact_preview.go:42` | **不动** |

### 3.3 为什么不用 JSON-RPC `【调研】的偏离`

调研文档的进程模型是「Agent 作为 sidecar，经 JSON-RPC 连 Main」。本项目 Agent 运行在 Go 进程内，
与 bridge 同进程，已有成熟的 `command_registry` + IPC 命令模式（`command_registry.go`、`hub_commands.go`）。
**结论**：复用既有命令通道，不引入 JSON-RPC。代价是协议描述不够通用；收益是零新增依赖、零序列化层。

---

## 4. 总体架构

### 4.1 分层图（含自检闭环）

```
┌──────────────┐   tools schema   ┌───────────────────────────────────────┐
│  Agent Loop  │◄────────────────►│ tools/builtin/artifact.go (新增)       │
│ agent_loop.go│                  │ artifact_publish / promote / export    │
└──────┬───────┘                  └───────────────┬───────────────────────┘
       │ AgentStart / AgentEnd                    │ 声明
       ▼                                          ▼
┌──────────────────────────┐   ChangeSet  ┌────────────────────────────────┐
│ ChangeTracker            │─────────────►│ Classifier（四层判定）           │
│ 钩子 ✅ agents/artifacts  │              │ 1 声明 2 目录约定 3 评分 4 用户纠正│
│ 快照 ➕ snapshot diff     │              └──────────────┬─────────────────┘
└──────────┬───────────────┘                             ▼
           │                            ┌────────────────────────────────┐
           └───────────────────────────►│ Version Store ➕（内容哈希）     │
                                        └──────────────┬─────────────────┘
                                                       ▼
                                        ┌────────────────────────────────┐
                                        │ Verifier ➕（自检闭环，≤2 轮）    │
                                        │ 错误收集 + 双视口截图 + 溢出检查  │
                                        └──────────────┬─────────────────┘
                                         有问题→回灌 Agent│ 通过
                                                       ▼
                                        ┌────────────────────────────────┐
                                        │ Preview Host ➕（注入 data-src）  │
                                        │ 沙箱 WebView / 内嵌浏览器         │
                                        └──────┬───────────────┬─────────┘
                                               ▼               ▼
                                        ┌────────────┐  ┌────────────────┐
                                        │ Inspector  │  │ ArtifactCard    │
                                        │ (选区)     │  │ ✅ 已存在        │
                                        └─────┬──────┘  └────────────────┘
                                              │ selection / comment
                                              ▼
                                        ┌────────────────────────────────┐
                                        │ Selection Refs ➕ → <selection>  │
                                        └────────────────────────────────┘
```

### 4.2 模块职责

| 模块 | 归属 | 职责 | 状态 |
|---|---|---|---|
| ChangeTracker·钩子半 | `agents/artifacts.go` | 从工具响应提取产出 | ✅ 已有 |
| ChangeTracker·快照半 | 同上（扩展） | AgentStart/End 目录 diff，兜住 bash/Python | ➕ |
| Classifier | 同上（扩展） | 四层判定、评分、入口选择 | ➕ |
| Version Store | 新包 `artifacts/` | 版本目录、blob 去重、GC | ➕ |
| **Verifier** | `desktop/bridge/artifact_verify.go` | 加载产物、收集错误、截图、回灌 | ➕ |
| Preview Host | `desktop/bridge/preview_host.go` | 静态服务、签名、注入、SSE | ➕ |
| Inspector | `desktop/bridge/artifact_inspector.js`（`go:embed`） | 元素拾取、选区上报 | ➕ |
| Selection Refs | `desktop/bridge/selection_refs.go` | 引用表、校验、清理 | ➕ |
| 写守卫 | `desktop/bridge/selection_guard.go` | `beforeToolCall` 拦截 | ➕ |
| 卡片 | `ArtifactCard.tsx` | 展示、预览、设为产物、纠正 | ⚙️ 改造 |
| Reducer | `view_reducer.go` | 投影稳定 schema | ⚙️ 扩展 |

### 4.3 一次运行的完整时序

```
User 发消息
 ├─ AgentStart ─► ChangeTracker 拍目录快照（mtime+size+小文件 hash，忽略噪声目录）
 ├─ loop
 │   ├─ write_file/edit_file ─► 钩子记 delta（✅ 已有）
 │   └─ bash/python ──────────► 钩子看不到 ⟵ 这就是必须有快照 diff 的原因
 ├─ AgentEnd
 │   ├─ ChangeSet = 钩子 delta ∪ (End 快照 − Start 快照)
 │   ├─ Classifier：声明 → 目录约定 → 评分 → 候选
 │   ├─ 依赖闭包：解析入口 HTML 的 src/href/import
 │   ├─ Version Store：写 v<N>，VersionID = sha256(排序后 path→hash)
 │   ├─ Verifier：加载 → 收集 console/pageerror/失败请求 → 双视口截图
 │   │    └─ 有问题且轮次 < 2 → verify.report 回灌 Agent → 回到 loop
 │   └─ 通过 → artifact.ready（url 按当前入口重算）→ SSE 通知
 ├─ 前端：卡片（入口 + 依赖数 + 版本）
 │   └─「预览」→ 沙箱 WebView / 内嵌浏览器
 └─ 用户选中 ─► selection.commit ─► Selection Refs
                 └─► 下一轮请求携 <selection>（数据）
                       └─► Agent 调 artifact_edit（带 base_version）→ 新版本
```

---

## 5. 数据模型

### 5.1 事件侧结构（不改动）

```go
// 【现状】core/tool.go:181；被 view_reducer.go:309 ArtifactRef 与卡片、14 个用例依赖
type Artifact struct {
    Name    string `json:"name"`
    Path    string `json:"path"`      // 工作区内为相对路径；工作区外为绝对路径
    Kind    string `json:"kind"`      // created | modified
    Added   int    `json:"added,omitempty"`
    Removed int    `json:"removed,omitempty"`
    Size    int64  `json:"size,omitempty"`
    Lines   int64  `json:"lines,omitempty"`
    SHA256  string `json:"sha256,omitempty"` // 前 16 字节 hex；超大文件为空
}
```

### 5.2 产物与版本（新增）

```go
type ArtifactID string   // "art_<8hex>"，由 wsKey + 入口相对路径派生，稳定可复现

type Artifact struct {
    ID         ArtifactID `json:"id"`
    WSKey      string     `json:"ws_key"`       // ⚠️ 必带：见 §22 R1
    Title      string     `json:"title"`
    Entry      string     `json:"entry"`        // 相对路径，如 artifacts/landing/index.html
    Latest     int        `json:"latest"`
    DeclaredBy string     `json:"declared_by"`  // tool | manifest | convention | score | user
    Provenance *Provenance `json:"provenance,omitempty"`
    CreatedAt  time.Time  `json:"created_at"`
    UpdatedAt  time.Time  `json:"updated_at"`
}

// 生成脚本不是产物，但记来源，支持「重新生成」
type Provenance struct {
    Script  string `json:"script"`   // 如 scripts/make_chart.py
    Cmdline string `json:"cmdline,omitempty"`
}

type ArtifactVersion struct {
    ArtifactID ArtifactID        `json:"artifact_id"`
    Version    int               `json:"version"`
    VersionID  string            `json:"version_id"`  // sha256(排序后 path→hash)
    Entry      string            `json:"entry"`
    Files      map[string]string `json:"files"`       // relPath → blob hash
    TotalBytes int64             `json:"total_bytes"`
    Kind       string            `json:"kind"`        // snapshot | stamp（Design type 用）
    CreatedAt  time.Time         `json:"created_at"`
}
```

### 5.3 目录语义：产物区 vs 版本库（**必须分清**）

| 目录 | 语义 | 可见性 | 谁写 |
|---|---|---|---|
| `<ws>/artifacts/<name>/` | **产物区**：源码所在地，路径即意图 | 用户可见 | Agent（write_file） |
| `<ws>/.scratch/` | **草稿区**：不进版本快照，保留 N 天后清理 | 隐藏 | Agent |
| `<ws>/.artifacts/` | **版本库**：快照与 blob，内容寻址 | 隐藏 | 系统 |

```
<ws>/.artifacts/
  index.json                         # 快照：全局索引（周期性压缩产物）
  index.journal                      # 追加日志：每次写入一行，启动时回放
  idx/
    bypath/<sha1(rel)%256>.json      # 反向索引分片：产物区相对路径 → ArtifactID
  <artifactID>/
    meta.json                        # Artifact 元信息（单产物单文件，读写隔离）
    v1/manifest.json                 # ArtifactVersion
    v2/manifest.json
  blobs/<h[0:2]>/<h>                 # 内容寻址（前 2 位十六进制分片）
  scratch/                           # 草稿区索引（不进版本快照，7 天 GC）
```

> **常见误解**：把产物区写成隐藏目录会让"目录约定"这一判定层失效——因为"路径即意图"依赖
> 用户和模型都能看见并遵守 `artifacts/`。版本库隐藏是为了不干扰工作区浏览。

### 5.6 索引与检索性能（**文件存储不能慢在这里**）

文件存储最容易踩的坑是"每次查询都 `filepath.Walk`"。本设计用**三层**规避：

| 层 | 结构 | 复杂度 | 用途 |
|---|---|---|---|
| L1 内存索引 | `map[ArtifactID]*Artifact` + `map[wsKey][]ArtifactID` + `map[wsKey+"\x00"+relPath]ArtifactID` | **O(1)** | 所有热路径查询（列表、按路径找产物、按 wsKey 过滤） |
| L2 反向索引分片 | `idx/bypath/<sha1(wsKey+"\x00"+rel)%256>.json` | O(1) 文件读 | 冷启动/跨进程只查单个分片，**不扫全目录** |
| L3 追加日志 | `index.journal`（每行一条 JSON） | O(1) append | 写放大恒定；启动时「快照 + 回放」重建 L1 |

> ⚠️ **键必须含 `wsKey`**：`artifacts/index.html` 在所有工作区里都叫这个名字。
> 只用 `relPath` 做键会让后打开的工作区覆盖前一个——这正是风险 R1 在**进程内**的形态。
> 分隔符用 `\x00` 而非 `/`，避免与路径里的字符冲突。

**写入路径**（publish / promote / 新版本）：

```
1. 写 blobs/（内容寻址，同 hash 已存在则跳过写入）
2. 写 <id>/v<N>/manifest.json（原子：tmp + rename）
3. append index.journal 一行（fsync 批量：每 200ms 或 64 条一次）
4. 更新 L1 内存索引（同进程立即可见）
5. 若 journal 行数 > 4096 → 后台异步压缩成 index.json 快照并清空 journal
```

**读取路径**（预览、导出、卡片）：

| 查询 | 路径 | 耗时量级 |
|---|---|---|
| 按 `ArtifactID` 取 meta | L1 命中 → 否则读 `<id>/meta.json` | O(1)，~0.1ms |
| 列某 wsKey 产物 | L1 `map[wsKey][]ArtifactID` | O(k)（k=产物数） |
| 按产物区路径找产物 | L1 `map[relPath]` → 否则读 1 个分片 | O(1) |
| 取某版本文件 | `<id>/v<N>/manifest.json` → `blobs/<h[0:2]>/<h>` | 2 次磁盘读 |
| 预览热资源 | 内存 LRU（**64MB**，按 blob hash 缓存） | 命中 0 磁盘读 |

**明确的禁令**（写进代码评审清单）：

1. **禁止**在请求路径上调用 `filepath.Walk` / `os.ReadDir` 递归。
2. **禁止**把 `blobs/` 放进任何"列目录"实现（10 万 blob 会让目录枚举退化）。
3. **禁止**每次写都重写 `index.json`（O(n) 写放大）——只追加 journal。
4. **禁止**在 HTTP handler 里做 GC 或压缩；GC/压缩一律后台 goroutine、单次 ≤200ms 预算、可中断。

**规模预估**（验证不会慢）：

| 项 | 1,000 产物 | 10,000 产物 |
|---|---|---|
| `index.json` 快照 | ~200KB | ~2MB |
| L1 内存 | ~1MB | ~10MB |
| 启动加载（快照+回放 4k 行） | ~30ms | ~120ms |
| 单次 publish 写放大 | ≤ 3 个小文件 + 1 行 append | 同左（**与总量无关**） |

> 结论：文件方案在 1 万产物量级下，启动 ≤150ms、查询 O(1)、写入恒定写放大——
> 满足"检索不能慢"，且无 SQLite 的依赖与迁移成本（见 §24 D1）。

### 5.4 选区引用

```go
type SelectionRef struct {
    RefID     string          `json:"ref_id"`
    WSKey     string          `json:"ws_key"`
    SessionID string          `json:"session_id"`
    Artifact  ArtifactID      `json:"artifact"`
    Version   int             `json:"version"`
    VersionID string          `json:"version_id"` // 失效判定基准
    Kind      string          `json:"kind"`       // 8 种之一
    Payload   json.RawMessage `json:"payload"`    // ≤2KB 裁剪后
    Label     string          `json:"label"`      // chip 文案
    CreatedAt time.Time       `json:"created_at"`
}
```

### 5.5 版本哈希（顺序无关、可复现）

```go
func versionID(files map[string]string) string {
    keys := make([]string, 0, len(files))
    for k := range files { keys = append(keys, k) }
    sort.Strings(keys)
    h := sha256.New()
    for _, k := range keys { fmt.Fprintf(h, "%s\x00%s\n", k, files[k]) }
    return hex.EncodeToString(h.Sum(nil))
}
```

---

# 第二部分 协议

## 6. A 层：Agent Tool 协议

### 6.1 工具清单（9 个；Phase 1 交付 8 个）

| 工具 | 读写 | 挂载条件 | 入参 | 成功返回 | 失败码 |
|---|---|---|---|---|---|
| `artifact_publish` | 写 | 文件系统模式 | `{entry, title}` | `{ok, artifact_id, version, version_id, files, total_bytes}` | `ENTRY_MISSING` `ENTRY_OUTSIDE` `TOO_LARGE` |
| `artifact_promote` | 写 | 文件系统模式 | `{from, title}` | 同上 | `NOT_IN_SCRATCH` `TARGET_EXISTS` |
| `artifact_edit` | 写 | 总是 | `{artifact_id, base_version, scope?, edits[]}` | `{ok, version, version_id}` | `VERSION_CONFLICT` `OLD_STR_NOT_UNIQUE` `EDITS_TOO_MANY` |
| `artifact_export` | 读 | 总是 | `{artifact_id, version?, format}` | `{ok, path, warnings[]}` | `UNSUPPORTED` `TOO_LARGE` `VERSION_GONE` |
| `source_get` | 读 | 有选区时 | `{path, line, context_lines≤20}` | `{ok, snippet}` | `OUT_OF_RANGE` |
| `source_find` | 读 | 有选区时 | `{query}` | `{ok, candidates[≤5]}` | — |
| `preview_screenshot` | 读 | 总是 | `{artifact_id, version?, viewport}` | `{ok, blob_ref, bytes≤1MB}` | `RENDER_TIMEOUT` |
| `preview_inspect` | 读 | 有选区时 | `{selector, want_outer_html≤8KB}` | `{ok, node}` | `NODE_GONE` |
| `designsystem_get` | 读 | Design type | `{name}` | `{ok, token\|component}` | `NOT_FOUND` |

> 表中 9 行是因为 `designsystem_get` 只在 Design type 挂载；**Phase 1 实际交付 8 个**（不含它），
> 与历史设计「CodeMode 下 9 个工具」的差 1 即此。

### 6.2 三条通用约定

1. **线上名一律下划线**：`artifact_publish`，不是 `artifact.publish`。
2. **永不抛异常**：失败统一返回 `{ok:false, code, message}`；`message` 必须是给人看的一句话。
3. **结构化结果**：给模型的是对象；给 CodeMode 脚本的也是对象（不是 JSON 字符串）。

### 6.3 `artifact_publish` 的完整语义

```
1. 校验 entry 在产物区（前缀 artifacts/）且文件存在       → 否则 ENTRY_OUTSIDE / ENTRY_MISSING
2. 解析依赖闭包（见 §13.4），任一依赖逃出工作区 → ENTRY_OUTSIDE
3. 计算 VersionID = versionID(files)（见 §5.5）
3.5 幂等短路：若 VersionID == 该 artifact 的 Latest.VersionID
        → 直接返回现有 {version, version_id}，**不新建版本**（避免脚本重复 publish 刷版本号）
4. 若同 ArtifactID 已存在：Version = Latest + 1，否则 Version = 1
5. 写 blobs/（同 hash 已存在则跳过）→ 写 v<N>/manifest.json（tmp + rename 原子）
6. append index.journal → 更新 L1 内存索引
7. 返回 artifact_id / version / version_id
8. 触发 SSE reload（若该 artifact 的预览正打开）
```

**失败原子性**（任一步失败都不能留下半成品）：

| 失败点 | 回滚动作 |
|---|---|
| 步骤 5 写 blob 失败 | 已写的 blob 保留（内容寻址，可被后续引用），**不建版本目录** |
| manifest 写失败 | 删除 `v<N>/` 临时目录，`Latest` 不变 |
| journal append 失败 | 版本已落盘但索引未更新 → 下次启动回放时由**目录扫描补偿**（仅启动时，不在请求路径） |

> 目录扫描补偿是 O(n) 的，所以**只在启动时且仅当 journal 校验失败**才触发；
> 请求路径永远不扫目录（§5.6 禁令 1）。

### 6.4 `artifact_edit` 的冲突语义

```
base_version != artifact.Latest
  ├─ 且 artifact 的当前 VersionID == 计算出的 base 的 VersionID → 内容未变，放行
  └─ 否则 → 返回 VERSION_CONFLICT，{current_version, hint:"调用 preview_inspect 重新确认"}
```

`edits[]` 每条 `{file, old_str, new_str}`：`old_str` 必须在文件中**唯一匹配**，
匹配 0 次 → `OLD_STR_NOT_FOUND`；匹配 >1 次 → `OLD_STR_NOT_UNIQUE`。

> 例外：入口为 `*.tsx/*.jsx` 时不走字符串替换，见 §14.5。

---

## 7. B 层：页面协议（Inspector ↔ Host）

### 7.1 传输

| 项 | 值 |
|---|---|
| 通道 | `MessageChannel` 私有端口（`window.parent.postMessage` 只用于**建链握手**一次） |
| 建链 | Host 在注入脚本里 `new MessageChannel()`，把 `port2` 交给 Inspector |
| 校验 | 首次握手带 `nonce`（Host 生成、注入到 HTML），Inspector 回传同一 nonce；不匹配即丢弃 |
| 编码 | JSON；截图/裁剪图只传 `blob_ref` 字符串 |
| 上限 | 单条消息 ≤ 256KB |

### 7.2 消息表

| 消息 | 方向 | 载荷 | 节流 |
|---|---|---|---|
| `handshake` | 页面 → Host | `{nonce, v}` | 一次 |
| `inspector.enable` / `disable` | Host → 页面 | `{}` | — |
| `hover` | 页面 → Host | `{rect, selector, tag}` | **16ms** |
| `selection.changed` | 页面 → Host | `{kind, payload}` | 100ms |
| `selection.commit` | 页面 → Host | `{refs: [...]}` | — |
| `comment.added` | 页面 → Host | `{anchor, text}` | — |
| `highlight` / `clear` | Host → 页面 | `{ref_id?}` | — |
| `apply_ops` | Host → 页面 | `{base_version, ops[]}` | — |
| `edit.result` | Host → 页面 | `{ok, reason?}` | — |

### 7.3 兼容规则

- 信封：`{v, type, seq, payload}`；`v` 当前 = 1
- **只增字段不改字段**；收到更高 `v` 时忽略未知字段，**不报错**
- `apply_ops.base_version` 与当前不一致 → 丢弃并回 `edit.result{ok:false, reason:"conflict"}`
- 未知 `type` 静默忽略（便于灰度）

---

## 8. C 层：生命周期钩子与守卫

### 8.1 四个钩子

| 钩子 | 实现位置 | 现状 | 用途 |
|---|---|---|---|
| `onAgentStart` | `agents/agent_loop.go:995`（collector 创建处） | ⚙️ 扩展 | 拍目录快照 |
| `beforeToolCall` | `tools/engine.go` PreToolUse 链 | ⚙️ 扩展 | 写守卫 |
| `onToolResult` | `agents/artifacts.go:77 observe` | ✅ 已有 | 记录 delta |
| `onAgentEnd` | `agents/agent_loop.go:1478` | ✅ 已有 | 汇总判定 |

### 8.2 写守卫的准确语义（**容易搞反**）

守卫限制的是**写入范围**，不是"选区范围"：

```
选区任务激活（scope.active = true）
  └─ beforeToolCall 收到 write_file / edit_file / run_python
       ├─ 目标路径 ∈ scope.allowedPaths → 放行
       └─ 否则 → 拒绝，reason 作为 tool_result 返回给模型
```

- **allowedPaths 的来源**：选中的 artifact 的 `Version.Files` 全集（相对工作区根）。
- **不是**只有 `artifact_edit` 才校验——`write_file` 也必须过；否则模型绕过守卫直接写。
- **安装时机**：会话创建时即装（`active=false` 零成本放行），不是任务开始时。

---

## 9. D 层：上下文注入

### 9.1 注入格式（逐字）

```
<selection kind="element" version_id="sha256:9c1e2f…" artifact="art_3f9a2c1b" label="Hero 标题">
<div class="hero__title" data-src="artifacts/landing/index.html:14:6">Hello</div>
</selection>

<user_instruction>
把标题改成更正式的措辞
</user_instruction>
```

### 9.2 硬规则

| # | 规则 | 违反后果 |
|---|---|---|
| 1 | 只有 `<user_instruction>` 内是指令；`<selection>` 内一律是**数据** | 页面里的 prompt injection 可直接操纵 Agent |
| 2 | 系统提示必须显式写明第 1 条 | 模型会"帮忙执行"页面里的文字 |
| 3 | 裁剪：`outer_html ≤ 2KB`、`text ≤ 300 字`、`computed` 按需拉取 | 上下文爆炸 |
| 4 | 完整样式不注入 | 同上 |

### 9.3 拼接位置

在 `desktop/bridge/main.go` 的 `CmdAsk` 分支（约 `:3250`，`text := str(c.Payload, "text")` 之后）
把 `<selection>` 块前插到 user 文本之前；同一轮多个引用按 `created_at` 升序拼接。

---

## 10. 预览服务协议

### 10.1 URL 形态

```
http://127.0.0.1:<port>/<wsKey>/<artifactID>/<version>/<sig>/<relPath>
例：http://127.0.0.1:53114/ws_a1b2/art_3f9a2c1b/3/7Kq2mZ…/index.html
```

`sig` 计算（**必须含 wsKey**）：

```go
func previewSig(secret []byte, wsKey, id string, v int, p string) string {
    mac := hmac.New(sha256.New, secret)
    fmt.Fprintf(mac, "%s\x00%s\x00%d\x00%s", wsKey, id, v, p)
    return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:22]
}
```

### 10.2 端点

| 端点 | 方法 | 行为 |
|---|---|---|
| `/<wsKey>/<id>/<v>/<sig>/` | GET | 返回入口页（`Entry`），注入 Inspector + `data-src` |
| `/<wsKey>/<id>/<v>/<sig>/<path>` | GET | 返回 blob 内容；`Content-Type` 按扩展名 |
| `/__events?token=<sig0>` | GET | SSE：`reload` / `artifact.ready` |

响应头固定：`Cache-Control: no-store`、`X-Content-Type-Options: nosniff`。

### 10.3 注入规则（只改响应，不改磁盘）

| 注入项 | 位置 | 内容 |
|---|---|---|
| Inspector 脚本 | `</head>` 前 | `<script src="/__inspector.js?nonce=<n>" data-artifact="…" data-version="…">` |
| `data-src` | 每个元素起始标签 | `data-src="<relPath>:<line>:<col>"`，行号由 tokenizer `Offset()` 换算 |
| `base href` | `<head>` 内首个元素前 | `<base href="/<wsKey>/<id>/<v>/<sig>/">`（解决页面里 `/style.css` 的问题） |

### 10.4 统一走 HTTP（**单一通道，已定**）

桌面内嵌 WebView、右侧浏览器标签、移动端 WebView **全部**走
`http://127.0.0.1:<port>/…`，不再维护 `artifact://` 自定义协议。

**为什么放弃双通道**：

| 理由 | 说明 |
|---|---|
| 一条代码路径 | 双通道意味着两套 URL 生成、两套调试、两套失效排查；而这个功能的收益全在"稳定能开" |
| 移动端本来就用不了自定义协议 | 手机连的是 bridge 的 `/api/cmd` 面，只有 HTTP 可用 → 双通道里有一半永远是死代码 |
| HTTP 的能力短板可以补 | 独立 origin、无 Host 校验这些"自定义协议的天然优势"，用下面的加固措施等价实现 |

**加固措施（逐条，缺一不可）**：

| # | 措施 | 实现位置 |
|---|---|---|
| 1 | 只监听 `127.0.0.1`，端口进程内随机 | `preview_host.go` `net.Listen("tcp","127.0.0.1:0")` |
| 2 | 校验 `Host` 头必须为 `127.0.0.1:<port>` 或 `localhost:<port>` | HTTP 中间件，不匹配 → 403 |
| 3 | 路径签名 HMAC-SHA256，**签名含 wsKey** | `previewSig()`（§10.1） |
| 4 | 签名 TTL 10 分钟，每次取预览 URL 重签 | `CmdArtifactPreview` 返回短期 URL |
| 5 | 响应头 `Cache-Control: no-store` + `X-Content-Type-Options: nosniff` | 固定 |
| 6 | 预览页 CSP：`default-src 'self' data: blob:` | 注入 `<meta http-equiv>` |
| 7 | 内嵌时用**沙箱 iframe / WebContentsView**（`sandbox=1`、`allow-scripts` 不含 `allow-same-origin`） | 前端 `embed.ts` |
| 8 | Inspector 走 MessageChannel + nonce（不依赖 origin 隔离） | §7.1 |

> 第 7 条是自定义协议"独立 origin"的等价物：`sandbox` 无 `allow-same-origin` 时，
> iframe 获得**不透明 origin**，同样无法读写宿主 DOM/存储。
> 代价：不透明 origin 下 `localStorage` 不可用——这正好符合"预览页不持久化任何东西"的定位。
> 连带约束：`postMessage` 的 `targetOrigin` 只能传 `"*"`（不透明 origin 无法匹配具体 origin），
> 因此**安全性完全依赖 MessageChannel 的 nonce 校验**（§7.1），不能再靠 origin 比对。

### 10.5 服务生命周期与硬限制

| 项 | 规则 |
|---|---|
| 端口分配 | `net.Listen("tcp","127.0.0.1:0")`；**冲突不可能**（内核分配），但端口耗尽需重试 3 次后报错 |
| 启动时机 | 首个产物 publish 时懒启动；无产物不占端口 |
| 关闭 | 应用退出 / 工作区关闭时 `Close()`；SSE 连接全部断开，等待 ≤2s 后强制 |
| 单连接超时 | 读写各 15s；SSE 心跳 20s（`: ping`） |
| SSE 并发上限 | 每 artifact 最多 4 条连接，超出返回 429 |
| 单文件响应上限 | **50MB**；超出 413（与 `TOO_LARGE` 对齐） |
| Range 请求 | **Phase 1 不支持**，返回 200 全量；视频/大二进制请走导出 |
| HTML 注入上限 | 单页 **2MB**；超出则跳过 `data-src` 注入（只注入 Inspector），并在响应头 `X-Artifact-Injected: partial` |
| 页面自带 CSP | 若页面 `<meta http-equiv="Content-Security-Policy">` 禁 `script-src 'self'`，注入的 Inspector 会被拦 → 响应头标记 `X-Artifact-Inspector: blocked`，前端禁用选区并提示（**预览本身仍可用**） |

---

## 11. CodeMode 编排

### 11.1 六条要求

| # | 要求 | 落地位置 |
|---|---|---|
| 1 | 可编排广告（工具注解声明可被脚本调用） | `tools/builtin/artifact.go` 的工具定义 |
| 2 | 结构化结果（脚本拿对象） | `tools/builtin/codemode/bridge.go` 的返回值序列化 |
| 3 | 路径语义差异写进描述 | 工具 `Description` |
| 4 | 图片类工具在脚本内降级为元信息 | `preview_screenshot` 的脚本分支 |
| 5 | **per-artifact 串行** | 每个 `ArtifactID` 一把锁 |
| 6 | 目录预算 | 单目录文件数/字节上限，超限截断并标记 |

### 11.2 per-artifact 串行的实现

```go
// artifacts/store.go
type Store struct {
    mu    sync.Mutex            // 保护 index/blobs 元数据
    perID map[ArtifactID]*sync.Mutex // 每个产物的写锁（懒创建）
}

func (s *Store) lockID(id ArtifactID) func() {
    s.mu.Lock()
    if s.perID == nil { s.perID = map[ArtifactID]*sync.Mutex{} }
    l, ok := s.perID[id]
    if !ok { l = &sync.Mutex{}; s.perID[id] = l }
    s.mu.Unlock()
    l.Lock()
    return l.Unlock
}
```

> 为什么必须串行：CodeMode 脚本可以并发调多个 `artifact_publish`，同 id 并发写会让
> `Latest` 自增出现重复版本号，版本目录互相覆盖。

### 11.3 目录预算

| 项 | 上限 | 超限行为 |
|---|---|---|
| 单产物文件数 | 50 | 截断，`warnings` 里说明 |
| 单文件字节 | 50MB（导出）/ 1MB（`artifact_create` 语义，本期不启用） | `TOO_LARGE` |
| 单产物总字节 | 200MB | `TOO_LARGE` |

### 11.4 每个工具的 CodeMode 形态（**全部可编排**）

CodeMode 脚本内可直接调用下列工具。**脚本拿到的是对象，不是 JSON 字符串**（§11.1 #2）。

| 工具 | 脚本内名 | 返回（对象） | CodeMode 专有限制 | 降级行为 |
|---|---|---|---|---|
| `artifact_publish` | `artifact_publish` | `{artifact_id, version, version_id, files, total_bytes}` | 默认 `auto_open=false`（脚本内不抢用户视线） | 失败抛 `PublishError`，脚本可捕获 |
| `artifact_promote` | `artifact_promote` | 同上 | 同上 | 同上 |
| `artifact_edit` | `artifact_edit` | `{version, version_id}` | `base_version` 必填；persistent 写锁 | 冲突抛 `VersionConflict{current, hint}` |
| `artifact_export` | `artifact_export` | `{path, warnings[]}` | 单次 ≤ 50MB；不支持 Range | `UNSUPPORTED` 抛 `ExportError` |
| `source_get` | `source_get` | `{snippet}` | `context_lines ≤ 20` | `OUT_OF_RANGE` 抛错 |
| `source_find` | `source_find` | `{candidates[]}` | 最多 5 条 | 返回空数组，不抛 |
| `preview_screenshot` | `preview_screenshot` | `{blob_ref, bytes, width, height}` | **降级为元信息**（脚本里不返回二进制，只给 `blob_ref` 与尺寸） | 超时抛 `RenderTimeout` |
| `preview_inspect` | `preview_inspect` | `{node}` | `outer_html ≤ 8KB` | `NODE_GONE` 抛错 |

**脚本调用示例**：

```python
# CodeMode 脚本内
r = artifact_publish(entry="artifacts/dashboard/index.html", title="Dashboard v2")
print(r.artifact_id, r.version)          # 对象属性，不是 r["artifact_id"]

# 批量导出：失败可捕获，不中断后续
for fmt in ["html", "pdf"]:
    try:
        e = artifact_export(artifact_id=r.artifact_id, format=fmt)
        log(e.path, e.warnings)
    except ExportError as ex:
        log("skip", fmt, ex.code)
```

**四条 CodeMode 硬约束**：

1. **per-artifact 串行**：同一 `ArtifactID` 上的 publish/edit 由 `Store.perID` 锁串行（§11.2）。
2. **不自动打开**：脚本内 `auto_open` 默认 `false`，避免脚本批量发布时把用户界面刷爆。
3. **错误以异常暴露**：脚本语言需要 `try/except`；错误码在 `ex.code`。
4. **`preview_screenshot` 降级**：脚本不接收二进制，只拿 `blob_ref`（避免脚本上下文塞满 base64）。

> 为什么必须支持 CodeMode：Agent 常需要"生成 5 个页面 → 逐个 publish → 导出 PDF"这类批量流程，
> 走模型逐个工具调用会消耗 10+ 次往返；脚本一次调用即可。

---

# 第三部分 实现

## 12. 详细代码改动

> 本章是本文的落地核心。每条改动给出：**文件:行 → 现状 → 改为 → 理由**。
> 新增文件给出可直接编译的骨架。所有行号基于当前 `main` 分支实测（2026-11-20）。

### 12.1 新增文件（9 个）

| # | 文件 | 行数估算 | 职责 |
|---|---|---|---|
| N1 | `artifacts/model.go` | ~110 | 类型、校验、ArtifactID 派生 |
| N2 | `artifacts/store.go` | ~300 | 版本目录、blob 去重、per-id 锁、GC |
| N3 | `artifacts/store_test.go` | ~180 | 哈希稳定性、去重、wsKey 隔离 |
| N4 | `artifacts/resolve.go` | ~170 | 入口解析 + 依赖闭包（`x/net/html`） |
| N5 | `artifacts/resolve_test.go` | ~150 | 无引号/跨行属性、`url()`、`@import` |
| N6 | `agents/artifact_snapshot.go` | ~140 | 目录快照与 diff |
| N7 | `agents/artifact_classify.go` | ~200 | 四层判定、评分、入口选择 |
| N8 | `tools/builtin/artifact.go` | ~260 | 8 个工具中的 publish/promote/export |
| N9 | `desktop/bridge/artifact_commands.go` | ~180 | 4 条 IPC 命令的 dispatch（与 hub 同构） |

另有 2 个非 Go 新增：

| # | 文件 | 职责 |
|---|---|---|
| N10 | `desktop/bridge/preview_host.go` | HTTP/自定义协议服务、签名、注入、SSE |
| N11 | `desktop/bridge/artifact_inspector.js` | Inspector（`go:embed` 注入） |

### 12.2 修改点（Go，9 处）

#### M1 `agents/artifacts.go:32` —— 增加快照相关常量

```go
// 现状
const maxArtifacts = 50 // 单轮产出上限（超出置 ArtifactsTruncated）

// 改为（在其后追加）
const (
    maxArtifacts = 50 // 单轮产出上限（超出置 ArtifactsTruncated）

    // snapshotSmallFileBytes 小于该值的文件在快照里额外算内容 hash。
    // 原因：只比 mtime+size 会漏掉「同大小的快速改写」。
    snapshotSmallFileBytes = 64 << 10
    // snapshotMaxEntries 单次快照最多统计多少个文件（超出则只统计产物区）。
    snapshotMaxEntries = 20000
)
```

#### M2 `agents/artifacts.go:64-74` —— `wrap` 增加子 agent 产物继承

```go
// 现状
func (c *artifactCollector) wrap(next events.EventHandler) events.EventHandler {
    if c == nil { return next }
    return func(ctx context.Context, e events.Event) {
        if tr, ok := e.(*events.ToolResponse); ok {
            c.observe(tr)
        }
        next(ctx, e)
    }
}

// 改为
func (c *artifactCollector) wrap(next events.EventHandler) events.EventHandler {
    if c == nil { return next }
    return func(ctx context.Context, e events.Event) {
        switch ev := e.(type) {
        case *events.ToolResponse:
            c.observe(ev)
        case *events.AgentEnd:
            // 子 agent 的产出也要进本轮汇总（历史设计 §21.16「产物声明表继承」）。
            // Depth > 0 才是子 agent；主 agent 的 AgentEnd 由 collect() 统一出，不在这里吃。
            if ev.Depth > 0 && len(ev.Artifacts) > 0 {
                c.absorbCore(ev.Artifacts)
            }
        }
        next(ctx, e)
    }
}
```

新增方法（放在 `observe` 之后，`:98` 之后插入）：

```go
// absorbCore 把子 agent 已投影好的 core.Artifact 折算回 delta。
// 注意：子 agent 的 Path 走的是 displayPath（可能是相对），这里按 root 还原成绝对，
// 否则 mergeArtifactDeltas 会与父 agent 的绝对路径分成两条。
func (c *artifactCollector) absorbCore(items []core.Artifact) {
    if c == nil { return }
    c.mu.Lock()
    defer c.mu.Unlock()
    for _, it := range items {
        p := it.Path
        if c.root != "" && !filepath.IsAbs(p) {
            p = filepath.Join(c.root, p)
        }
        c.acc = append(c.acc, artifactDelta{
            path: p, created: it.Kind == core.ArtifactKindCreated,
            added: it.Added, removed: it.Removed,
            size: it.Size, lines: it.Lines, sha256: it.SHA256,
        })
    }
}
```

#### M3 `agents/artifacts.go:183-197` —— 噪声不再丢弃，改为打标

**先纠正一个事实**：历史设计中提到的"`filterNoiseArtifacts` 死代码"在**当前代码里不存在**——
`:184 out := in[:0]` 是正确的原地过滤，逻辑完整。因此本项不是"修 bug"，而是**语义调整**。

```go
// 现状 :183
func filterNoiseArtifacts(in []artifactDelta) []artifactDelta {
    out := in[:0]
    for _, d := range in {
        p := d.path
        lower := strings.ToLower(filepath.ToSlash(p))
        if isNoiseArtifactPath(lower) { continue }
        if strings.HasPrefix(filepath.Base(p), ".") { continue }
        out = append(out, d)
    }
    return out
}

// 改为：保留噪声项，打标后交给评分层（噪声=负分，而非直接消失）
func filterNoiseArtifacts(in []artifactDelta) []artifactDelta {
    out := in[:0]
    for _, d := range in {
        d.noise = isNoiseArtifactPath(strings.ToLower(filepath.ToSlash(d.path))) ||
            strings.HasPrefix(filepath.Base(d.path), ".")
        out = append(out, d)
    }
    return out
}
```

配套在 `:35 artifactDelta` 结构体增加字段：

```go
type artifactDelta struct {
    path    string
    created bool
    added   int
    removed int
    size    int64
    lines   int64
    sha256  string
    noise   bool // 新增：噪声项，进评分（负分），不进 AgentEnd 产出清单
}
```

并在 `:120-122` 截断**之前**过滤掉 `noise`（保证 AgentEnd 语义不变）：

```go
// 现状
if len(merged) > maxArtifacts { merged, truncated = merged[:maxArtifacts], true }

// 改为
kept := merged[:0]
for _, m := range merged { if !m.noise { kept = append(kept, m) } }
merged = kept
if len(merged) > maxArtifacts { merged, truncated = merged[:maxArtifacts], true }
```

#### M4 `agents/artifacts.go:224-233` —— `displayPath` 统一 posix 斜杠

```go
// 现状 :232
return rel

// 改为
return filepath.ToSlash(rel) // 归因必须统一成 posix，否则 Windows 下产物路径与 wsKey 派生不一致
```

#### M5 `agents/agent_loop.go:995-996` —— 采集器之外再拍一次开始快照

```go
// 现状
collector := newArtifactCollector(a.cfg.WorkingDir)
handler = collector.wrap(handler)

// 改为
collector := newArtifactCollector(a.cfg.WorkingDir)
handler = collector.wrap(handler)
snapStart := captureSnapshot(a.cfg.WorkingDir) // 新增：AgentStart 快照
```

#### M6 `agents/agent_loop.go:1478` —— 用快照 diff 兜住 bash / 未声明输出的脚本

```go
// 现状
artifacts, artifactsTruncated := collector.collect()

// 改为
// bash 落盘、run_python 未声明 outputs 的写入都不会经过工具钩子（见 artifacts.go:81-85
// 的白名单），只能靠快照 diff 兜住完整性。
collector.absorbDeltas(diffSnapshot(snapStart, captureSnapshot(a.cfg.WorkingDir)))
artifacts, artifactsTruncated := collector.collect()
```

`absorbDeltas([]artifactDelta)` 为新增方法，体同 M2 的 `absorbCore`，只是入参已是 delta。

#### M7 `events/agent.go:94-97` —— 增加版本投影字段（不改 `core.Artifact`）

```go
// 现状
Artifacts []core.Artifact `json:"artifacts,omitempty"`
ArtifactsTruncated bool `json:"artifacts_truncated,omitempty"`

// 改为（在其后追加）
Artifacts []core.Artifact `json:"artifacts,omitempty"`
ArtifactsTruncated bool `json:"artifacts_truncated,omitempty"`

// ArtifactVersions 产物 → 版本的映射（Writer 落地后才有；旧日志为空）。
// 与 Artifacts 分开：Artifacts 是「本轮改了哪些文件」，这里是「哪些产物有了第几版」。
ArtifactVersions []ArtifactVersionRef `json:"artifact_versions,omitempty"`

// ArtifactVersionRef 是版本投影（checkpoint 稳定 schema）。
type ArtifactVersionRef struct {
    Path       string `json:"path"`        // 入口相对路径，与 core.Artifact.Path 对得上
    ArtifactID string `json:"artifact_id"`
    Version    int    `json:"version"`
    VersionID  string `json:"version_id"`
}
```

#### M8 `desktop/bridge/command_registry.go:117` —— 新增 4 条命令常量

```go
// 现状 :117
CmdHubArtifactGet       Command = "hub_artifact_get"

// 改为（在其后插入新块；注意与 hub 前缀分离）
CmdHubArtifactGet Command = "hub_artifact_get"

// —— 系统级产物（Artifacts V2）：本地桌面 UI 专用，不加进 mobile 白名单 ——
CmdArtifactPublish   Command = "artifact_publish"
CmdArtifactPreview   Command = "artifact_preview"   // 生成预览 URL（含签名）
CmdArtifactSelection Command = "artifact_selection" // 提交选区引用
CmdArtifactExport    Command = "artifact_export"
```

#### M9 `desktop/bridge/main.go:2701` —— 挂上产物命令分派（与 hub 同构）

```go
// 现状 :2700-2703
// Hub 命令分派集中在新文件里（命令面会持续变长，不往这个 switch 里再塞 20 个 case）。
if m.dispatchHub(c) {
    return
}

// 改为
if m.dispatchHub(c) {
    return
}
// 系统级产物命令同理集中到 artifact_commands.go，避免主 switch 继续膨胀。
if m.dispatchArtifact(c) {
    return
}
```

#### M10 `desktop/bridge/main.go` 的 `case CmdAsk:`（约 `:3250`，`text := str(c.Payload,"text")` 之后）—— 拼接选区

```go
// 现状（示意）
text := str(c.Payload, "text")

// 改为
text := str(c.Payload, "text")
// 选区引用（Selection Refs）在这里注入：只拼 <selection> 数据块，
// <user_instruction> 由既有 text 承担 —— 二者语义绝不能混。
if block := m.selectionBlockFor(m.currentSessionID(c)); block != "" {
    text = block + "\n\n<user_instruction>\n" + text + "\n</user_instruction>"
}
```

> `selectionBlockFor` 内部按 `created_at` 升序拼多个 `<selection>`，并在发送后**标记已发送**
> （同一引用不重复注入）。

#### M11 `desktop/bridge/main.go` 三处清理点（`:764-800`、约 `:3600`、`:4773-4785`）

```go
// 1) :764-800 的 switch ev := e.(type) → case *events.AgentEnd:
case *events.AgentEnd:
    m.selectionClearScope(sid)   // 新增：一轮结束即释放写守卫

// 2) 约 :3600 的 case CmdAsk / case CmdAskBatch 两分支，提交后清理
m.selectionCommitScope(sid)      // 新增：提交即把守卫范围收窄到本次选区

// 3) :4773-4785 的 case CmdDeleteSession:
m.selectionDropSession(sid)      // 新增：删会话时删引用表 + 守卫
```

### 12.3 修改点（前端，4 处）

#### F1 `desktop/app/src/store/useAppStore.ts:3715` —— 新增产物预览动作

```ts
// 现状 :3715
openHtmlInBrowser(path: string): void // write_file 落盘 .html → 右栏内嵌浏览器 file:// 打开

// 改为（在其后新增；file:// 保留为降级）
openHtmlInBrowser(path: string): void
// artifactPreview 走 Preview Host（不再 file://）：解决 ES Module / fetch 跨域。
// 失败时回落 openHtmlInBrowser，并在控制台打点 —— 用于观测 file:// 残留调用量。
artifactPreview(artifactId: string, version?: number): Promise<boolean>
```

实现插在 `:12720 openHtmlInBrowser` 之后：调用 `bridge?.artifactPreview({artifact_id, version})` 拿
短期 URL，然后按 §14.11 打开/复用 **`kind:"artifact"`** 标签（不是 `web` 标签——
`artifact` 标签只持久化 `{artifactId, version}`，URL 每次重签）。

#### F2 `desktop/app/src/components/ArtifactCard.tsx:149-155` —— 主按钮改走预览宿主

```tsx
// 现状
if (BROWSER_EXTS.has(ext)) {
  return { label: t('art.openBrowser'), run: () => void openHtmlInBrowser(a.path) }
}

// 改为
if (BROWSER_EXTS.has(ext)) {
  const id = a.artifactId // 新增字段（来自 AgentEnd.ArtifactVersions）
  return {
    label: t('art.openBrowser'),
    run: () => {
      if (!id) return void openHtmlInBrowser(a.path)          // 无版本：降级
      void useAppStore.getState().artifactPreview(id).then((ok) => {
        if (!ok) openHtmlInBrowser(a.path)                     // 预览失败：降级
      })
    },
  }
}
```

#### F3 `desktop/app/src/components/ArtifactCard.tsx` 按钮区 —— 新增「设为产物 / 不是产物」

在既有次按钮组（按 `OFFICE_EXTS` 补「默认程序打开」那段）之后追加：

```tsx
<button className="btn btn-xs" onClick={() => void useAppStore.getState().artifactSetUser(a.path, true)}>
  {t('art.setAsArtifact')}
</button>
<button className="btn btn-xs" onClick={() => void useAppStore.getState().artifactSetUser(a.path, false)}>
  {t('art.notArtifact')}
</button>
```

#### F4 `desktop/app/src/lib/bridge.ts` —— 新增 3 个封装

在 `DesktopApi` 类型定义中追加（类型右值由 preload 提供）：

```ts
artifactPreview?: (req: { artifact_id: string; version?: number }) => Promise<{ ok: boolean; url?: string; error?: string }>
artifactSelection?: (req: { refs: unknown[] }) => Promise<{ ok: boolean; error?: string }>
artifactExport?: (req: { artifact_id: string; version?: number; format: 'html' | 'pdf' | 'md' }) => Promise<{ ok: boolean; path?: string; warnings?: string[]; error?: string }>
```

### 12.4 新增文件骨架（关键部分）

#### N6 `agents/artifact_snapshot.go`

```go
package agents

// snapshotEntry 单个文件的快照项。
type snapshotEntry struct {
    mtime int64
    size  int64
    hash  string // 仅小文件（< snapshotSmallFileBytes）计算，其余为空
}

// captureSnapshot 遍历工作区，产出 path → entry 的快照。
// 规则：跳过噪声目录（复用 artifactNoiseDirs）、跳过 .artifacts/ 自身、
// 超过 snapshotMaxEntries 后只统计 artifacts/ 前缀下的文件。
func captureSnapshot(root string) map[string]snapshotEntry

// diffSnapshot 求差集：新增 / 修改（mtime 或 size 变化，或小文件 hash 变化）。
// 返回的 delta 里 created 由「旧快照不存在该路径」判定；added/removed 为 0
// （快照不知道行数，size/lines/sha 由 collect 归一化时按需补）。
func diffSnapshot(oldSnap, newSnap map[string]snapshotEntry) []artifactDelta
```

#### N4 `artifacts/resolve.go` 的核心循环

```go
// resolveDeps 解析入口 HTML 的依赖闭包（相对路径已归一到工作区相对路径）。
func resolveDeps(root, entryRel string) (files map[string]string, err error) {
    files = map[string]string{}
    queue := []string{entryRel}
    seen := map[string]bool{}
    for len(queue) > 0 {
        cur := queue[0]; queue = queue[1:]
        if seen[cur] { continue }
        seen[cur] = true
        abs := filepath.Join(root, filepath.FromSlash(cur))
        b, err := os.ReadFile(abs)
        if err != nil { continue }            // 缺失依赖跳过，不中断
        files[cur] = sha256Hex(b)
        if !isHTML(cur) { continue }
        for _, ref := range htmlRefs(b) {      // tokenizer 提取 src/href/import + <style> url()/@import
            if r, ok := normalizeRef(entryRel, ref); ok && !seen[r] { queue = append(queue, r) }
        }
    }
    return files, nil
}
```

#### N2 `artifacts/store.go` —— 索引与日志（D1 文件方案的核心）

```go
// artifacts/store.go
type Store struct {
    dir    string                    // <ws>/.artifacts
    mu     sync.RWMutex
    byID   map[ArtifactID]*Artifact
    byWS   map[string][]ArtifactID            // wsKey → 产物列表
    byPath map[string]ArtifactID              // wsKey + "\x00" + relPath → id（⚠️ 必须含 wsKey）
    perID  map[ArtifactID]*sync.Mutex         // per-artifact 串行写（§11.2）
    jw     *journalWriter                     // 追加日志，批量 fsync（200ms 或 64 条）
    lru    *lruCache                          // 64MB blob 缓存
    lock   *os.File                           // .artifacts/LOCK（flock，防跨进程并发写）
}

// Open 启动加载：读 index.json 快照 → 回放 index.journal → 必要时目录补偿。
// 只读打开（flock 失败）时 writable=false：预览可用，publish 返回 READONLY。
func Open(dir string) (s *Store, err error)

// Put 写入一个新版本：校验 → 幂等短路 → blob（跳过已存在）→ manifest 原子写 → journal append → 更新 L1。
func (s *Store) Put(wsKey string, v *ArtifactVersion, files map[string][]byte) (Artifact, error)

func (s *Store) Get(id ArtifactID) (*Artifact, bool)            // L1，O(1)
func (s *Store) ByPath(wsKey, rel string) (ArtifactID, bool)    // L1，O(1)
func (s *Store) List(wsKey string) []Artifact                   // L1，O(k)
func (s *Store) Blob(hash string) ([]byte, error)               // 先 LRU，未命中再读 blobs/<h[0:2]>/<h>
func (s *Store) CompactIfNeeded()                               // journal 行数 > 4096 → 快照 + 清空
func (s *Store) GC(retain int)                                  // 后台：保留最近 retain 版 + 被引用版；单次 ≤200ms，可中断
func (s *Store) Close() error                                   // 关 flock、flush journal
```

**journal 记录格式与崩溃恢复**（写一半的行必须能识别）：

```
{"op":"put","id":"art_3f9a2c1b","ws":"ws_a1b2","path":"artifacts/landing/index.html",
 "v":3,"vid":"9c1e…","ts":1795155351067}
```

```
回放算法：
  for each line:
      if !json.Valid(line) → 视为「崩溃时写了一半」，truncate 到上一行末尾并停止回放
      apply(line)
  若 journal 存在但无任何有效行且目录非空 → 触发一次目录补偿（仅启动时，O(n) 可接受）
```

> 三个关键点：**① 键含 wsKey**（防进程内串台）；**② 尾行截断**（崩溃恢复）；
> **③ 只在启动时允许 O(n)**（请求路径永远 O(1)）。

#### N9 `desktop/bridge/artifact_commands.go`

```go
// dispatchArtifact 系统级产物命令分派（与 dispatchHub 同构，main.go:2701 之后调用）。
func (m *manager) dispatchArtifact(c command) bool {
    switch c.Type {
    case CmdArtifactPreview:
        m.respOK(c, m.artifactPreviewURL(c))
        return true
    case CmdArtifactPublish:
        m.respJSON(c, m.artifactPublish(c))
        return true
    case CmdArtifactSelection:
        m.respJSON(c, m.artifactSelectionCommit(c))
        return true
    case CmdArtifactExport:
        m.respJSON(c, m.artifactExport(c))
        return true
    }
    return false
}
```

### 12.5 改动清单速查（供实施勾选）

| # | 文件:行 | 类型 | 一句话 | 依赖 |
|---|---|---|---|---|
| N2a | `artifacts/store.go`（新增） | 新增 | L1/L2/L3 索引 + journal 回放与压缩 | — |
| M1 | `agents/artifacts.go:32` | 新增常量 | 快照阈值 | — |
| M2 | `agents/artifacts.go:64` | 改 + 新方法 | 子 agent 产物继承 | M7 |
| M3 | `agents/artifacts.go:183` | 改语义 | 噪声打标不丢弃 | — |
| M4 | `agents/artifacts.go:232` | 改一行 | posix 归因 | — |
| M5 | `agents/agent_loop.go:995` | 加一行 | AgentStart 快照 | N6 |
| M6 | `agents/agent_loop.go:1478` | 改一行 | 快照 diff 兜底 | N6 |
| M7 | `events/agent.go:97` | 新增字段 | 版本投影 | — |
| M8 | `command_registry.go:117` | 新增常量 | 4 条命令 | — |
| M9 | `main.go:2701` | 加 3 行 | 挂分派 | N9 |
| M10 | `main.go:~3250` | 加 4 行 | 注入 `<selection>` | N10 |
| M11 | `main.go:764/3600/4773` | 各 1 行 | 清理守卫与引用 | N10 |
| F1 | `useAppStore.ts:3715` | 新增动作 | 预览宿主 | N10 |
| F2 | `ArtifactCard.tsx:149` | 改分支 | 主按钮 | F1 |
| F3 | `ArtifactCard.tsx` 按钮区 | 新增 2 按钮 | 用户纠正 | M8 |
| F4 | `lib/bridge.ts` | 新增 3 声明 | IPC 封装 | M8 |

---

## 13. 产物判定

### 13.1 四层

| 层 | 机制 | 实现位置 | 现状 |
|---|---|---|---|
| 1 声明 | `artifact_publish` / `.artifact/manifest.json` | N8 / N7 | ➕ |
| 2 目录约定 | 产物区 `artifacts/`、草稿区 `.scratch/` | N7 | ➕ |
| 3 规则评分 | 对未声明项打分，只产**候选** | N7（复用 M3 的 noise 标记） | ⚙️ |
| 4 用户纠正 | 卡片两个按钮 | F3 | ➕ |

### 13.2 评分表（分值固定）

| 信号 | 分值 | 判定代码 |
|---|---|---|
| 位于 `artifacts/` | **+5** | `strings.HasPrefix(rel, "artifacts/")` |
| 本轮新建/修改的 `html`/`svg`/`pdf`/`md` | **+2** | 扩展名白名单 ∧ `created ‖ modified` |
| 位于 `.scratch/`、`tmp`、`node_modules`、`__pycache__` | **−10** | `isNoiseArtifactPath` ∨ 前缀 |
| 文件名含 `tmp`/`debug`/`test`/`out`/`log` | **−3** | 子串匹配 |
| 空文件 或 二进制超阈值 | **−2** | `size == 0` ∨ (`!isText` ∧ `size > 2MB`) |
| 被另一产物引用 | **不单独计分** | 依赖闭包里出现即并入 |

**阈值**：`score >= 3` → 候选；已声明项**不参与评分**。

### 13.3 入口选择优先级

```
1. .artifact/manifest.json 的 entry
2. agent 调用 artifact_publish 时给的 entry
3. 目录内唯一 index.html
4. 本轮改动量（added+removed）最大的 .html
5. 评分最高的 .html
```
全部落空 → 不建产物，只保留文件清单卡片（即当前行为）。

### 13.4 依赖闭包（必须用 tokenizer）

```go
// htmlRefs 提取一个 HTML 里的全部引用：src / href / import / url() / @import
func htmlRefs(b []byte) []string {
    var refs []string
    z := html.NewTokenizer(bytes.NewReader(b))
    inStyle := false
    for {
        tt := z.Next()
        switch tt {
        case html.ErrorToken:
            return refs
        case html.StartTagToken, html.SelfClosingTagToken:
            name, hasAttr := z.TagName()
            tag := string(name)
            if tag == "style" { inStyle = true }
            if tag == "style" && tt == html.SelfClosingTagToken { inStyle = false }
            for hasAttr {
                k, v, more := z.TagAttr(); hasAttr = more
                if isRefAttr(tag, string(k)) { refs = append(refs, string(v)) }
            }
        case html.EndTagToken:
            name, _ := z.TagName()
            if string(name) == "style" { inStyle = false }
        case html.TextToken:
            if inStyle { refs = append(refs, cssRefs(z.Text())...) }
        }
    }
}
```

`isRefAttr`：`script→src`、`img→src`、`link→href`、`iframe→src`、`source→src`、`a→href`（仅同源相对）。

### 13.5 provenance 与草稿区

| 项 | 规则 |
|---|---|
| 生成脚本（`make_chart.py` 等） | **不是产物**，记入 `Provenance{Script, Cmdline}`，卡片提供「重新生成」 |
| `.scratch/` | 不进版本快照；保留 **7 天**后由 GC 清理；可 `artifact_promote` 提升 |
| 提升/降级 | `promote`：草稿 → `artifacts/<id>/`；降级同理反向 |

---

## 14. 预览与选区的实现细节

### 14.1 `data-src` 注入（逐行）

```go
// injectPreview 把磁盘 HTML 转成「发给浏览器」的副本：注入 Inspector、nonce、data-src、base。
func injectPreview(src []byte, rel, baseURL, nonce string) []byte {
    out := &bytes.Buffer{}
    z := html.NewTokenizer(bytes.NewReader(src))
    line, lineStart := 1, 0
    for {
        tt := z.Next()
        if tt == html.ErrorToken { out.Write(z.Raw()); break }
        raw := z.Raw()
        lineStart += countNL(raw, &line)          // 维护当前 token 的起始行
        if tt == html.StartTagToken {
            name, _ := z.TagName()
            if string(name) == "head" {
                out.Write(raw)
                fmt.Fprintf(out, `<base href=%q><script src="/__inspector.js?nonce=%s"></script>`, baseURL, nonce)
                continue
            }
            // 在起始标签的 ">" 前插入 data-src
            out.WriteString(withAttr(raw, "data-src",
                fmt.Sprintf("%s:%d:%d", rel, line, lineStart)))
            continue
        }
        out.Write(raw)
    }
    return out.Bytes()
}
```

**约束**：注入只发生在**响应**里；`os.ReadFile` 出来的原始字节永不修改。

### 14.2 Inspector 行为（逐条）

| 交互 | 触发 | 行为 | 产出 |
|---|---|---|---|
| 点选 | `click`（capture，`preventDefault`） | 语义提升后上报 | `selection.changed{kind:"element"}` |
| 追加 | `Shift+click` | 加入集合，≤20 | `kind:"elements"` |
| 层叠轮换 | `Alt+click` | `elementsFromPoint` 轮换 | 同上 |
| 框选 | 拖矩形 | 默认「完全包含」；`Alt` 切换「相交」；LCA 判组 | `kind:"region"`（无单一节点时） |
| 文字选区 | `mouseup` 有 `Range` | 记录 `range{start,end}` + 所属节点 | `kind:"text"` |
| 批注 | 右键「批注」+ 输入 | 锚点绑定 selector，位置回退 rect | `comment.added` |
| 盲区 | canvas / 跨域 iframe / closed shadow DOM | 自动落区域框选 + 截图 | `kind:"region"` |
| 高亮 | — | **Shadow DOM** 覆盖层，不污染页面样式 | — |

**语义提升**：跳过纯布局包装（`div/span` 且无背景/内边距/`data-component`）；
遇 `button/li/a/[data-component]` 或带背景/内边距的容器即停。
提升**不改变**原始命中点，面包屑可逐级退回叶子。

### 14.3 坐标换算（画布与滚动容器）

```
屏幕坐标 → 文档坐标： doc = (screen - rect.left + scrollLeft) / scale
画布容器缩放：       rect_display = rect_doc * scale + offset
sticky/fixed 元素：   先判 position，fixed 直接用视口坐标，不叠加 scroll
虚拟列表：            不按 DOM 位置，改按 item 的 data key 绑定
```

### 14.4 降级矩阵

| 条件 | 能力 | 退路 |
|---|---|---|
| 有 `data-nid`（Design type stamping） | 精确定位，行号漂移无影响 | — |
| 仅 `data-src`（本方案默认） | 精确到文件:行:列 | 版本过期时用文本指纹重定位 |
| 无映射 | selector + 文本指纹 | `source_find` 搜源码，结合截图判断 |
| 有 CDP | 精确截图、样式来源、穿透 iframe | 脚本截图，样式只取 computed |

### 14.5 两条修改路径（必须都实现）

| 路径 | 适用 | 是否经 LLM | 实现 |
|---|---|---|---|
| **确定性 patch** | 颜色、字号、文案、间距这类单值改动 | ❌ 不经 | UI 产出 op 列表 → 按 `data-src` 定位 → 直接改文件 → 新版本 |
| **Agent 路径** | 布局、风格、重构、多文件联动 | ✅ 经 | 选区 → `<selection>` → Agent 调 `artifact_edit` |

确定性 patch 的 op 形态：

```jsonc
{ "ops": [
  { "op": "setStyle", "selector": "#hero h1", "prop": "font-size", "value": "32px" },
  { "op": "setText",  "selector": "#hero h1", "text": "你好" }
] }
```

两条路径**共用** `base_version` 校验与版本自增逻辑。

### 14.6 修改落点（按样式来源）

| 样式来源 | 落点 |
|---|---|
| inline `style` | 直接改该属性 |
| class，仅本节点用 | 改对应 CSS 规则 |
| class，多处共用（`shared > 1`） | 新增局部 class 覆盖，或询问「只改这个还是全部」 |
| Tailwind | 改 class 列表，**保持类名顺序** |
| token（CSS 变量） | 优先替换为已有 token，不新增硬编码 |
| 响应式 | 写入 `viewport.w` 对应媒体查询，除非用户选「所有断点」 |

### 14.7 React / TSX 的落盘规则（**Phase 1 不支持，明确拒绝**）

| 情形 | 做法 |
|---|---|
| 入口为 `.html` / `.css` / `.js` / `.svg` / `.md` | `old_str/new_str` 唯一匹配替换（**Phase 1 支持**） |
| 入口为 `.tsx` / `.jsx` | **返回 `UNSUPPORTED`**，提示"请直接编辑源码"（Phase 1 不支持） |
| `instances > 1`（map 列表 / 复用组件） | Phase 1 不涉及（仅静态资源） |
| `dynamic: true` | Phase 1 不涉及 |
| 组件被多处引用 | Phase 1 不涉及 |

**为什么 Phase 1 直接拒绝而不是"尽力替换"**：

字符串替换在 TSX 上会破坏格式化、注释与用户手改，且 `old_str` 极易命中多处 ——
这是**正确性**问题，不是体验问题。半成品实现比明确拒绝更糟：用户会以为改成功了。
AST 改写（`ts-morph`：定位 JSX 字面量/className，改节点后回写）列为 **P8**，届时再开放。

> 拒绝时返回的帮助信息必须包含：文件路径、建议工具（源码面板 / `edit_file`）、
> 以及"该入口类型需 Phase 2 才支持"。

### 14.8 改后验证与回滚

```
应用 patch → 生成新版本 → 对选区区域截图 → 回传 Agent 自检
  ├─ 满意 → 保持
  └─ 不满意 → 回滚到上一版本（版本目录本就不可变，回滚=指回旧 VersionID）
```

### 14.9 选择方式矩阵（8 种触发 → 8 类引用）

用户在预览页上把内容"选出来引用给 Agent"，支持以下方式。**每种方式都有明确的 kind 与手势**：

| # | 交互方式 | 触发 | Inspector 行为 | 产出 kind | Chat 里显示 |
|---|---|---|---|---|---|
| 1 | **单击元素** | 左键单击 | capture 阶段拦截、`preventDefault`；语义提升后上报 | `element` | 图标 `</>` + 标签名或文本前 24 字 |
| 2 | **多选（追加）** | `Shift` + 单击 | 加入集合；≤20，超出 `truncated` | `elements` | 图标 ⧉ + 「N 个元素」 |
| 3 | **层叠循环** | `Alt` + 单击同一点 | `elementsFromPoint` 轮换上层/下层 | `element` | 同上（每次替换当前选中） |
| 4 | **框选（有 DOM）** | 在页面空白或内容上拖矩形 | 默认「完全包含」；`Alt` 拖 = 「相交」；LCA 判组 | `elements` | 图标 ⧉ + 「N 个元素」 |
| 5 | **框选（无 DOM）** | 在 canvas / 跨域 iframe / closed shadow DOM 上拖 | 采样 `elementsFromPoint`，仅存 rect 与候选 | `region` | 图标 ▭ + 区域尺寸 + 截图缩略 |
| 6 | **文字选区** | 拖选文字后松手 | 取 `Range`，记录起止与所属节点 | `text` | 图标 " " + 引用文本前 24 字 |
| 7 | **批注** | 右键 →「批注」→ 输入意见 | 锚点绑定 selector，位置回退 rect | `comment` | 图标 ✎ + 意见文本 |
| 8 | **整页/画板** | 工具栏「引用整页」或 `Cmd/Ctrl+A` | —— | `page` | 图标 ▤ + 页面标题 |

**键盘与快捷键**（与终端保持一致的手感）：

| 键 | 行为 |
|---|---|
| `Enter` | 引用当前选中（覆盖 chip 里的现有同源引用） |
| `Shift` + `Enter` | 追加到已有引用 |
| `Esc` | 取消选中 / 关闭浮动输入框 |
| `Cmd/Ctrl` + `Shift` + `Enter` | 引用当前屏幕（无 DOM 时退化为 `region` 全屏） |
| `↑` / `↓`（选中时） | 面包屑上溯父节点 / 下钻子节点 |
| `Alt`（悬停时） | 显示层叠节点列表 |

**触控（移动端 / 触控板）**：

| 手势 | 行为 |
|---|---|
| 长按 400ms | 同「单击元素」 |
| 双击 | 进入层叠循环（等价 `Alt`+单击） |
| 双指缩放 | 画布缩放（在 `region` 模式下改为框选缩小） |
| 三指下滑 | 引用当前屏幕 |

**上限与去重**：

- 单次发送最多 **8** 个引用；超出 toast「最多引用 8 个选区」
- 去重键 `(artifact_id, version_id, kind, selector|range|rect)`；重复选择**替换**而非叠加
- 单引用 payload ≤ **2KB**（`outer_html` 截断、`text ≤ 300 字`），超出在 chip 上标「已裁剪」

### 14.10 从页面引用到 Agent Chat 的完整链路

```
① 用户在上面任一方式选中
     Inspector → selection.changed（临时高亮，尚未提交）
② 用户按 Enter / 点「引用」
     Inspector → selection.commit{refs}
     Host：建 SelectionRef（裁剪 ≤2KB、带 version_id、生成 label）
③ Chat Composer 出现 chip
     [</> Hero 标题 ×]  [▭ 区域 320×180 ×]
④ 用户输入指令 → 发送
⑤ 发送时（main.go CmdAsk 分支，见 M10）
     chips → 按 created_at 升序拼 <selection> 块 → 前置到 <user_instruction>
⑥ 发送后
     chips 清空；refs 标记 sent（同一版本内不重复注入）
     引用仍留在会话历史，可再次引用（重新生成 ref，新 version_id）
⑦ 版本变化
     旧 ref 标「已过期」→ 发送前提示「将按新版本重定位」或「选区已失效」
```

**chip 的交互细则**：

| 操作 | 行为 |
|---|---|
| hover | 浮层显示来源（产物名 + 版本 + `data-src` 路径）与 payload 预览 |
| 点 `×` | 移除该引用 |
| 「清空全部」 | 移除所有引用（不影响已发送的历史） |
| 点击 chip 主体 | 在预览里**高亮并滚动到**该元素（locate 操作） |
| 引用超 2KB 被裁剪 | chip 尾部标「已裁剪」，hover 显示原始大小 |

> 为什么用 chip 而不是"在输入框里插入文本"：文本插入会让用户误以为可以编辑选区内容，
> 而选区是**不可编辑的数据**；chip 的"不可编辑、可删除"语义更准确（沿用终端引用同一套组件）。

### 14.11 右栏自动展开（publish 触发）

**契约**：`artifact_publish` 成功 → **自动展开右侧边栏 + 切到该产物的预览标签**。

| 环节 | 实现 |
|---|---|
| 触发 | `artifact_publish` 返回前发 `artifact.ready{artifact_id, version, url}` |
| 标签 | 新增标签类型 `kind:"artifact"`（只持久化 `{id, kind, artifactId, version}`；URL 每次重新签名生成，**不落盘**） |
| 复用 | 同 `artifactId` 已有标签 → 原地切到该标签并刷新，**不新建** |
| 展开 | `setRightExpanded(true)`；已展开则只切标签 |
| 不抢焦点 | 不动左栏、不滚动主对话、不抢输入框焦点（用户正在打字时不打断） |
| 用户手动关过 | 同一 artifact 后续版本不再自动开（记 `dismissed: true`，卡片点击可重置） |
| 开关 | `artifact.autoOpenPanel` 默认 `true`；关闭后只发事件、卡片显示"有新版本" |
| 批量 publish（CodeMode） | 默认 `auto_open=false`，只开**最后一次**（见 §11.4） |

> 为什么选"自动展开"：publish 的语义就是"我给用户看这个"。不展开等于把成果藏起来，
> 用户还得自己去找。但**只在应用内展开**，不碰外部浏览器（§19.4）。

### 14.12 预览标签的状态机与兜底

```
idle ──打开──► loading ──ready──► live ──失败/超时──► failed
                  │                 │
                  └──超时(10s)──────┴─► failed（带原因 + 重试按钮）

live 的子状态：
  · inspector-blocked（页面 CSP 拦截注入）：预览可用、选区禁用、底部条提示
  · stale（有新版本未刷新）：标签显示"有新版本"小点，点击刷新
```

| 状态 | UI | 兜底 |
|---|---|---|
| `loading` | 骨架 + 「正在准备预览…」 | 10s 超时 → `failed` |
| `live` | 页面 | — |
| `failed` | 错误文案 + 「重试」+ 「在浏览器打开」 | 重试重签 URL；仍失败提供导出源码路径 |
| `inspector-blocked` | 页面 + 底部条 + 选区按钮置灰 | 提示改用 `source_get` 或源码面板 |
| `stale` | 页面 + 角标 | 点角标或等 SSE 自动刷新 |



---

## 15. 自检闭环（Verifier）

### 15.1 检查项（固定 7 项）

| # | 检查 | 判定 |
|---|---|---|
| 1 | `console.error` | 有任一条 → 失败 |
| 2 | `pageerror`（未捕获异常） | 有任一条 → 失败 |
| 3 | 失败请求（4xx/5xx 或网络错误） | 任一 → 失败 |
| 4 | 空白页 | `document.body.innerText.trim() == "" && 无可见元素` → 失败 |
| 5 | 横向溢出 | `documentElement.scrollWidth > clientWidth + 2` → 告警（不阻断） |
| 6 | 文字对比度 | 抽样 20 个可见文本节点，`contrast(text, bg) < 4.5:1` 的占比 > 20% → 告警 |
| 7 | 双视口截图 | 桌面 1280×800、移动 390×844，各一张（≤1MB） |

> 对比度只做**告警**不做失败：设计类产物本就可能用低对比配色，硬拦会误伤。
> 检测方式：读 `getComputedStyle` 的 `color`/`backgroundColor`，向上回溯到第一个非透明背景。

### 15.2 循环规则

```
AgentEnd 且本轮**有 versioned artifact**
  ├─ 全通过 → artifact.ready
  ├─ 有失败 且 round < 2 → verify.report 回灌 Agent → 回到 loop（Agent 自动修）
  └─ round == 2 仍有失败 → 仍发 artifact.ready，但带 warnings（不静默丢弃）

AgentEnd 但本轮**无 artifact**（只有文件清单卡片 / 未达判定阈值）
  └─ 跳过自检（无入口页可加载），行为与现状一致
```

### 15.3 实现位置与依赖

- 文件：`desktop/bridge/artifact_verify.go`（新增）
- 加载方式：桌面内嵌 `WebContentsView` 加载预览 URL，附加 `webContents.debugger` 监听
  `Runtime.consoleAPICalled` / `Runtime.exceptionThrown` / `Network.loadingFailed`
- 截图：`webContents.capturePage()`
- 超时：单次加载 10s；截图 3s；**总预算 25s**，超出按失败处理（避免拖住 run 收尾）

### 15.4 为什么必须有

没有它，「产物打开是白屏/报错」会被当成成功交付——用户看到的第一个页面就是坏的，
而 Agent 以为完成了。这是本方案里**唯一能自我纠错**的环节。

---

## 16. 导出规范

### 16.1 矩阵

| 产物类型 | HTML | PDF | Markdown |
|---|---|---|---|
| html | 单文件，资源内联 | `printToPDF` | 有损转换 + `warnings` |
| markdown | 渲染 + 内置样式 | 经 HTML 渲染 | 原文 |
| react 项目 | 打包成单文件静态快照，交互不保证 | 渲染后 PDF | 仅文本 + `warnings` |
| slides | 单文件带翻页 | 每页一页 | 按 `---` 分页 |
| design | 按画板分节 | 每画板一页 | `UNSUPPORTED`，建议 PNG/PDF |

### 16.2 约束（逐条）

| # | 约束 |
|---|---|
| 1 | 导出**只基于版本快照**，不读工作区、不修改 artifact |
| 2 | 导出的 HTML **剥离** `data-src`、`data-nid`、Inspector 脚本 |
| 3 | Markdown 有损 → 必须返回 `warnings`；图片复制到 `<name>_assets/` 并改相对路径 |
| 4 | 文件名 `<title>-<versionShort>.<ext>` |
| 5 | 单文件上限 **50MB**，超出 `TOO_LARGE` |
| 6 | PDF 前等待字体与图片加载完成，冻结动画；渲染窗口 sandbox，不执行 Node 能力 |
| 7 | 导出前先跑自检闭环；失败则写进 `warnings`，**不静默输出空文件** |
| 8 | 输出目录固定为 `<ws>/.artifacts/exports/`，文件名 `<title>-<versionShort>.<ext>`；返回**绝对路径**给调用方 |

---

## 17. 画布与 Design type（结构约束先行，实现后置）

### 17.1 Design 是 Artifact 的一种 type

| 层 | 内容 | 适用 |
|---|---|---|
| 共用底座 | 存储、版本、预览宿主、Agent 工具、选区协议、导出 | 所有 artifact |
| 通用编辑层 | 选区、批注、patch、撤销 | 所有 artifact |
| **Type 层** | 结构约定、id stamping、画布、属性面板、设计系统 | design、slides |

### 17.2 结构约束（写进生成规范，不是事后修）

| 约束 | 规则 |
|---|---|
| 画板文件 | `boards/<boardId>.html`，`boardId` 匹配 `^[a-z0-9-]{1,32}$` |
| 可编辑元素 | 必须有 `data-nid`，格式 `^n_[0-9a-z]{6}$`，同 artifact 内唯一，**删除后不复用** |
| 缺失 id | 由 Host stamping 补齐，生成独立版本 `kind:"stamp"` |
| 颜色/间距/字号 | 必须走 CSS 变量；lint 扫硬编码，不在 tokens 里 → 回灌 Agent |
| 画板脚本 | 不得增删 DOM 结构（保证选区稳定）；动态内容用 `data-dynamic` 标注 |

### 17.3 画布坐标

每画板一个沙箱 iframe；外层 `transform: translate() scale()` 做平移缩放；
选区矩形换算 `rect × scale + offset`；变体对比 = 同 artifact 多版本同屏。

---

## 18. 成本控制

| 手段 | 具体做法 |
|---|---|
| edit 代替整文件重写 | `artifact_edit` 的 `old_str/new_str`，不重发整文件 |
| 选区上下文裁剪 | `outer_html ≤ 2KB`、`text ≤ 300 字`、computed 按需拉取 |
| 设计系统摘要 | ≤2K token 摘要进系统提示固定前缀，细节按 `designsystem_get` 取 |
| 截图缩略图 | 回传 ≤1MB，必要时降采样 |
| prompt caching | 系统提示 + 设计系统摘要作为**固定前缀**，不随轮次变化 |
| 自检上限 | 最多 2 轮，防止死循环 |
| 确定性 patch | 单值改动不经 LLM（见 §14.5） |

**观测指标**：单轮 token、单轮延迟、自检轮次分布、回滚率。

---

## 19. 安全与边界

### 19.1 访问控制

| 项 | 规则 |
|---|---|
| 监听 | 仅 `127.0.0.1`（HTTP 通道）|
| 端口 | 随机，进程内记录 |
| 鉴权 | 路径签名（含 `wsKey`）+ 校验 `Host` 头 |
| TTL | 签名 URL 有效期 10 分钟，过期 403 |
| 只读 | 服务不执行工作区任何代码 |

### 19.2 路径安全

```go
abs, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
if err != nil || !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
    return errOutside // 挡住 ../ 与符号链接逃逸
}
```

### 19.3 内容不可信

| 风险 | 措施 |
|---|---|
| 页面 prompt injection | `<selection>` 声明为数据 + 系统提示硬规则 + 工具描述里再强调一次 |
| 外联 | 预览页默认加 CSP：`default-src 'self' data: blob:` |
| 绝对路径资源 | 注入 `<base href>`；若失效，改用**每会话子域** `<token>.localhost:<port>`（Safari 需实测） |
| 需要构建的项目（Vite/Next） | **不自动起 dev server**，先让用户确认；只自动打开纯静态产物 |
| 远程 / 容器内 Agent | 本地服务不可达：要么端口转发，要么把变更文件同步到本机再起服务 |

### 19.4 打开策略（区分「应用内」与「外部」）

| 场景 | 行为 | 可配置 |
|---|---|---|
| **publish 成功 → 应用内右栏** | 自动**展开右侧边栏**并切到该产物的预览标签（同一 artifact 复用已有标签） | `artifact.autoOpenPanel`，默认 `true` |
| 外部浏览器打开 | 默认 **不自动**，卡片上点「在浏览器打开」 | `artifact.autoOpenExternal`，默认 `false` |
| 一次运行多个产物 | 只自动打开**最后一个 publish 的**入口；其余留在卡片里 | — |
| 入口变化（同 id 新版本） | 已有标签原地刷新（SSE），**不新建标签、不抢焦点** | — |
| 用户手动关闭了该标签 | 同一 artifact 后续版本**不再自动打开**（尊重用户意图，直到卡片再次点击） | — |
| 无头 / CI | 跳过所有自动打开 | — |

> 关键区分：**右栏展开是应用内行为**（用户的注意力本来就在应用里，展开是"把成果递到手边"）；
> **外部浏览器是会抢焦点的行为**（可能打断用户在别的窗口的工作），所以默认关。

### 19.5 交互反馈（用户始终知道发生了什么）

| 时刻 | 用户看到 | 实现 |
|---|---|---|
| publish 开始 | 右栏展开 + 标签显示 loading（`正在准备预览…`） | `artifact_preview` 返回前先插标签 |
| 预览就绪 | loading 消失，页面可交互；标签标题 = `title` | SSE `ready` |
| 自检失败 | 页面照常显示 + 顶部细条「自检发现 N 个问题，已自动修复」/「仍有问题」 | `verify.report` 摘要 |
| 注入被 CSP 拦 | 底部条「页面限制了脚本注入，选区暂不可用」+ 选区按钮置灰 | `X-Artifact-Inspector: blocked` |
| 选区成功 | Composer 出现 chip（带 kind 图标 + 标签 + 来源） | §14.10 |
| 引用超限 | toast「最多引用 8 个选区」 | 上限保护 |

---

## 20. 测试

### 20.1 单元（必写）

| 目标 | 用例 |
|---|---|
| `versionID` | 顺序无关；同内容同 hash；单文件变动即变 |
| `wsKey` 隔离 | 两工作区同名同内容 → 不串读（R1 回归） |
| 签名 | 改 `wsKey`/`id`/`version`/`path` 任一 → 403 |
| 评分 | 各信号边界；`.scratch` −10 优先；被引用并入 |
| 入口选择 | manifest > publish > index.html > 改动量 > 评分 |
| 依赖解析 | 无引号属性、跨行、`<style>url()`、`@import`、相对路径归一 |
| 快照 diff | 同大小快速改写（mtime 变）能被发现；仅内容变而 mtime/size 不变（小文件 hash 兜底） |
| 噪声打标 | 打标不丢弃；`AgentEnd` 产出清单与原行为一致 |

### 20.2 集成

| 场景 | 断言 |
|---|---|
| 预览 200 | 入口 + css + js + 图片全部 200 |
| 预览 403 | 错签名 / 过期 / 跨工作区 |
| 注入 | `data-src` **只在响应**出现；磁盘源码不含 |
| SSE | 新版本落盘 → 客户端收到 `reload` |
| 依赖闭包 | 6 文件产物 → 1 个 artifact，不是 6 条文件 |
| 自检 | 注入一个 `throw` → 检出 pageerror → 回灌 1 轮 → 第 2 轮通过 |

### 20.3 端到端

| 流程 | 断言 |
|---|---|
| 发布 → 卡片 | 卡片显示入口 + 版本 + 文件数 |
| 预览 → 选中 → 引用 | 下一轮请求含 `<selection kind="element" version_id=…>` |
| 引用 → 编辑 | `artifact_edit` 带 `base_version`，成功后版本 +1 |
| 版本过期 | 明确提示「选区已失效」，不改错节点 |
| 守卫 | 选区任务期间 `bash` 写文件被拒，`reason` 出现在 tool_result |
| 导出 | HTML 不含 `data-src`；Markdown 带 `warnings` |

### 20.4 回归（必须全绿）

| 项 | 依据 |
|---|---|
| 产物块 reducer 14 个用例 | `desktop/bridge/artifacts_reducer_test.go` |
| `hub_artifact_get` / `hub_task_artifact_preview` | 群组产物不受影响 |
| office 预览（xlsx/docx/pptx） | 另一条链路 |
| `` ```html `` 纯展示 | `openHtmlContentInBrowser` |
| `go build ./...` / `go vet ./agents/ ./artifacts/ ./tools/builtin/ ./desktop/bridge/` | 编译与静态检查 |

### 20.5 失败语义

每个错误码至少一条用例。**没有测试的改动不得合并**。

---

## 21. 实施计划

### 21.1 与四阶段路线（A/B/C/D）的映射

| 阶段 | 内容 | 对应本文 | 检查点 |
|---|---|---|---|
| **A 基座** | 判定三层、版本快照、AgentEnd 预览与热刷新、自检 | §13 §5 §10 §15 | 结束后能看到正确页面 |
| **B 选择改稿** | 点选、批注、结构化选区、edit 式 patch | §7 §9 §14 | 选中能引用并能改 |
| **C 画布与设计系统** | 多画板、变体对比、tokens lint、属性面板 | §17 | 画板可精确编辑 |
| **D 交接与协作** | 导出、handoff、分享、共编 | §16 | 能交付出去 |

### 21.2 任务分解

| # | 交付 | 依赖 | 验收 | 预估 |
|---|---|---|---|---|
| P0 | N1+N2+N3（版本库 + **索引与日志**：L1/L2/L3 + journal 回放与压缩） | — | 哈希稳定；`(wsKey,relPath)` 隔离；**崩溃回放**（写一半的 journal 行被丢弃）；写放大不随总量增长 | 4d |
| P1 | N10+N11+N12（预览宿主） | P0 | 入口页与全部依赖 200；注入只在响应；端口冲突自动重试 | 4d |
| P2 | N8+N9 + M7+M8+M9（工具面与命令） | P0 | `artifact_publish` 后版本 +1 | 3d |
| P3 | N6+N7 + M1/M3/M5/M6（快照与判定） | P0 | bash 产出能被发现；评分边界齐 | 3d |
| P4 | N13+N14 + M10/M11（选区与守卫） | P1 | 引用注入正确；越界写入被拒 | 5d |
| P5 | Verifier（§15） | P1 | 注入错误能被检出并回灌 | 3d |
| P6 | `artifact_edit` + §14.5/§14.6 | P4 | 版本冲突与漂移处理正确 | 3d |
| P7 | CodeMode 编排（§11） | P2 | 脚本内可调、结果结构化、per-id 串行 | 3d |
| P8 | Design type（§17） | P6 | 画板 stamping + tokens lint | 后续 |

**关键路径**：P0 → P1 → P4 → P6；P0 → P2 → P7；P1 → P5。

---

## 22. 风险登记册

| # | 风险 | 后果 | 对策 | 验证方式 |
|---|---|---|---|---|
| R1 | artifact id / version 非全局唯一 | 大文件从另一个工作区读出 | 路径、签名、索引一律带 `wsKey` | 单测：两工作区同内容不串读 |
| R2 | 守卫装晚 | 「先发消息再点选区」窗口内 bash 放行 | 会话创建时即装，`active=false` 零成本 | 集成：窗口期内写入被拒 |
| R3 | 正则解析依赖遗漏 | 页面正常但依赖没进包 | `x/net/html` tokenizer | 单测：无引号/跨行/`url()` |
| R4 | 页面内容 prompt injection | Agent 被页面文本操纵 | `<selection>` 声明为数据 + 系统提示 | 端到端：注入指令不生效 |
| R5 | 版本存储膨胀 | 磁盘占用 | 内容寻址去重 + 保留 10 版 + GC | 观测：`.artifacts` 体积 |
| R6 | ~~`filterNoiseArtifacts` 死代码~~ | — | **已核实不存在**：`:184 out := in[:0]` 逻辑完整，本项作废 | — |
| R7 | 预览服务暴露 | 端口被滥用 | 127.0.0.1 + 签名 + Host 校验 + 10 分钟 TTL | 安全测试：错 Host 403 |
| R8 | 与 agenthub 产物串台 | 改错系统、命名冲突 | §3.1 硬边界；`artifact_*` vs `hub_artifact_*` | 回归：hub 用例全绿 |
| R9 | `file://` 降级路径残留 | 旧代码仍走 file:// | F1 保留降级 + 打点；观测调用量后移除 | 观测 |
| R10 | `ArtifactsBlock` 只改渲染 | 「只有 bundle」的轮次整块消失 | 空判同时看 `bundles?.length` | 组件测试 |
| R11 | 快照只看 mtime+size | 同大小快速改写被漏 | 小文件补内容 hash | 单测：mtime 变即捕获 |
| R12 | 归因路径未归一 | 产物来源全部落成 shell | `displayPath` 统一 `filepath.ToSlash` | 单测：Windows 分隔符 |
| R13 | **跨进程并发写**（两个应用实例打开同一工作区） | journal 行交错、版本号重复 | `.artifacts/LOCK` 文件锁（`flock`）：首个实例独占写；第二个实例**只读打开**（预览可用，publish 返回 `READONLY`） | 双进程集成测试 |
| R14 | 页面自带 CSP 阻断 Inspector 注入 | 选区不可用 | 响应头 `X-Artifact-Inspector: blocked` → 前端禁用选区并提示；**预览本身不受影响** | 带 CSP 的 fixture 页 |
| R15 | 入口改名/移动 | `ArtifactID` 变化，旧产物残留 | 文档化为既定行为（改名即新产物）；旧产物仍可打开，不自动迁移 | 改名后 publish 生成新 id |

---

## 23. 兼容与迁移

| 项 | 策略 |
|---|---|
| `core.Artifact` | **不改结构**；事件 JSON、卡片、14 个 reducer 用例零影响 |
| `AgentEnd` 新字段 | `ArtifactVersions` 缺省为空，旧日志与旧客户端忽略即可 |
| 旧 `file://` 预览 | 保留为降级 + 打点观测 |
| 历史产物 | 无版本可迁移（从未持久化）→ 从下一次 `AgentEnd` 开始建版本 |
| 群组产物 | 完全不动 |
| 产物目录约定 | 首次引入；对既有工作区**不做迁移**，新约定只对 `artifacts/` 生效 |

---

## 24. 决策记录（已定，不再待拍）

| # | 决策项 | 结论 | 理由与补偿 |
|---|---|---|---|
| **D1** | 存储 | **文件 + 内容寻址**（不用 SQLite） | 仓库无 SQLite 依赖，引入驱动/迁移/事务成本高；用 §5.6 的三层索引保证"检索不慢"：L1 内存 O(1)、L2 按 sha1 分片的反向索引、L3 追加日志（写放大恒定）；1 万产物量级启动 ≤150ms |
| **D2** | 传输 | **统一 HTTP**（单一通道） | 一份代码路径；移动端本就用不了自定义协议；用 §10.4 的 8 条加固（127.0.0.1 + 随机端口 + Host 校验 + 含 wsKey 的 HMAC + 10min TTL + CSP + 沙箱 iframe 的不透明 origin + MessageChannel nonce）等价实现隔离 |
| **D3** | 编辑落盘 | **Phase 1 只支持静态资源**：`.html/.css/.js/.svg/.md` 走 `old_str` 唯一匹配替换；**`.tsx/.jsx` 返回 `UNSUPPORTED`**（引导改用源码编辑） | 字符串替换在 TSX 上会破坏格式化、注释与用户手改，且 `old_str` 极易命中多处 —— 这是正确性问题，不能用"能跑就算"掩盖。AST 改写（`ts-morph`）列为 P8，届时再开放 |

### 24.1 其他已定方案（本文直接决定，避免实施期再讨论）

| 项 | 结论 | 理由 |
|---|---|---|
| 索引形态 | 内存 L1 + 分片 L2 + 追加日志 L3（§5.6） | 免全目录扫描；写放大恒定 |
| journal 压缩阈值 | 行数 > 4096 触发 | 启动回放 ≤ 4k 行（~120ms 上限） |
| blob 分片 | 前 2 位十六进制（256 目录） | 单目录条目数可控，避免大目录枚举退化 |
| 预览缓存 | 内存 LRU **64MB**，按 blob hash | 热刷新零磁盘读 |
| 版本保留 | 最近 **10** 版 + 被选区引用的版本；GC 后台、单次 ≤200ms、可中断 | 空间与可回滚性平衡 |
| 草稿区 TTL | **7 天** | 足够复核，不至于堆积 |
| 签名 TTL | **10 分钟**，每次取预览 URL 重签 | 短窗口 + 不落日志 |
| 自检闭环 | 默认开启；最多 **2** 轮；总预算 **25s**；失败**不阻断**交付，只带 `warnings` | 自愈但绝不卡住 run 收尾 |
| 打开策略 | 默认 `ask`（不自动开浏览器）；一次运行最多自动开 **1** 个标签 | 避免打断用户当前阅读 |
| 判分阈值 | `score >= 3` 为候选，候选**不自动打开** | 降低误报代价 |
| 依赖解析 | `golang.org/x/net/html` tokenizer | 属性可无引号/跨行、`url()`/`@import` 形态多，正则必漏 |
| 产物区 | `artifacts/<name>/`（可见） | "路径即意图"，对 bash/脚本同样有效 |
| 版本库 | `<ws>/.artifacts/`（隐藏） | 不干扰工作区浏览 |

> 需要变更上述任一项时，走评审并在本表更新；实现与本文不一致处必须回写本文。

---

## 25. 术语表

| 术语 | 含义 |
|---|---|
| 产物（Artifact） | 入口 + 依赖闭包的集合，有版本、可预览 |
| 入口（Entry） | 用户直接打开的页面，通常 `index.html` |
| 依赖闭包 | 入口引用的 css/js/图片/数据的递归集合 |
| 产物区 | `artifacts/<name>/`，源码所在地，路径即意图 |
| 草稿区 | `.scratch/`，不进版本快照，7 天后清理 |
| 版本库 | `.artifacts/`，快照与 blob，内容寻址 |
| VersionID | 版本的内容哈希（`path → hash` 排序后求 sha256） |
| 选区（Selection） | 用户在预览里选中并引用回对话的范围，**是数据不是指令** |
| 守卫（Guard） | 选区任务期间限制**可写范围**的 `beforeToolCall` 拦截器 |
| provenance | 产物的生成来源（脚本与命令行），支持「重新生成」 |
| stamping | Design type 给元素补 `data-nid` 的过程，产出 `kind:"stamp"` 版本 |
| 自检闭环（Verifier） | 加载产物、收集错误、截图、失败回灌 Agent，最多 2 轮 |

---

_文档结束。实现与本文不一致处，必须回写本文对应章节。_

---

## 26. 修订记录（含本轮自审修复）

| 版本 | 变更 | 触发 |
|---|---|---|
| v2.0 | 首版：五硬伤、架构、协议 A/B/C/D、判定四层、P0–P8 | 初始需求 |
| v2.1 | 收敛为唯一文档：吸收元设计、历史 §21–§24、实施清单、代码片段；补自检闭环 / 导出规范 / 画布与 Design type / 两条修改路径 / 落点表 / React 规则；§12 改为文件:行级 | 「只保留 V2 一份」+「不要含糊、要文件行号」 |
| v2.2 | 决策落定：D1 文件存储（新增 §5.6 三层索引）、D2 统一 HTTP（§10.4 八条加固）、D3 Phase 1 仅静态资源；另定 13 项实施参数（§24.1） | 用户拍板 |
| v2.3 | **本轮自审修复 10 处**（见下表） | 「再来一次 Review，确保细节到位」 |

| v2.4 | **补齐四类交互契约**：① 用户交互（§14.9 八种选择方式矩阵 + §14.10 引用到 Chat 全链路 + §14.12 标签状态机 + §19.5 反馈）；② 与 Agent 交互（8 种选择 → 8 类 kind 的映射与快捷键/触控手势）；③ publish 自动展开右栏（§14.11 + §19.4 区分应用内/外部）；④ 新工具全部可进 CodeMode（§11.4 逐工具形态 + 四条硬约束） | 用户追加要求 |

### 26.1 v2.3 修复清单

| # | 问题 | 严重度 | 修复 |
|---|---|---|---|
| 1 | **L1/L2 索引键只有 `relPath`** | 🔴 真 bug | 改为 `wsKey + "\x00" + relPath`（同一进程多工作区会串台，是 R1 的进程内形态） |
| 2 | 工具清单标题写「8 个」但表内 9 行 | 🟠 不一致 | 标题改「9 个；Phase 1 交付 8 个」 |
| 3 | 自检缺**文字对比度**（元设计明确要求） | 🟠 漏项 | 加为第 6 项（告警级，不阻断），共 7 项 |
| 4 | P0 未包含索引/日志与崩溃回放 | 🔴 计划缺口 | P0 加「L1/L2/L3 + journal 回放与压缩」，验收加「崩溃回放、写放大不随总量增长」，3d→4d |
| 5 | `artifact_publish` 重复调用会刷版本号 | 🟠 语义缺失 | 加步骤 3.5 幂等短路（同 `VersionID` 直接返回现有版本） |
| 6 | 失败原子性未定义（可能留半版本） | 🔴 正确性 | 新增失败回滚表；明确「目录补偿只在启动时」 |
| 7 | 预览服务生命周期与硬限制缺失 | 🟠 实施会卡 | 新增 §10.5（端口/懒启动/关闭/超时/SSE 上限/50MB/Range/2MB 注入上限/页面自带 CSP） |
| 8 | 沙箱 iframe 不透明 origin 下 `postMessage` 约束未写 | 🟠 易踩 | §10.4 补明：`targetOrigin` 只能 `"*"`，安全性完全依赖 MessageChannel nonce |
| 9 | 跨进程并发写、CSP 阻断注入、入口改名未登记 | 🟠 未覆盖 | §22 新增 R13/R14/R15 及其验证方式 |
| 10 | 导出输出目录与命名未定 | 🟠 实施会随意发挥 | §16.2 第 8 条：固定 `<ws>/.artifacts/exports/`，返回绝对路径 |
| 11 | 无 artifact 时是否跑自检未定义 | 🟡 边界 | §15.2 明确：无 versioned artifact 则跳过 |

### 26.2 结论

- **一致性**：章节编号无重复；交叉引用（§5.5/§5.6/§10.4/§13.4/§15.1/§24）均存在；无「待拍板」「待评审冻结」等悬空状态。
- **可实施性**：§12 每条改动均给出 `文件:行 → 现状 → 改为`，新增文件给可编译骨架（含索引与 journal 的崩溃恢复算法）。
- **已知剩余风险**：性能数字（启动 ≤150ms、写放大 ≤3 文件）是按结构推导的量级估算，**未做压测**；
  P0 落地后应以真实 1k/1w 产物数据集复测，若偏差 > 2 倍则回到本节记录并调整索引策略。
