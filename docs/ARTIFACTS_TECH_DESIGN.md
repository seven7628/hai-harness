# 系统级 Artifacts 技术方案（整合版）

> **本文性质**：基于**当前代码现状 + 恢复出的历史设计文档 + 通用元设计**重新整合的技术方案，
> 不是任何单一文档的副本。可直接据以开发。
>
> **来源标注**：`【现状】` 已在代码中验证；`【历史】` 从误删事故中逐字恢复
> （`ARTIFACTS_AGENT_DESIGN.md` §21–§24）；`【元设计】` 用户提供的通用设计
> （`ARTIFACTS_META_DESIGN.md`）；`【方案】` 本文的新决策。
>
> 生成日期：2026-11-20

---

## 一、先划清边界：本方案只做「系统级产物」

代码里存在**两套互不相干的产物系统**，本方案只涉及前者。

| | **A. 系统级产物（本文目标）** | **B. agenthub 群组产物（不在范围内）** |
|---|---|---|
| 代码 | `core.Artifact`（`core/tool.go:181`）+ `agents/artifacts.go` | `agenthub.Artifact`（`agenthub/store_types.go:94`） |
| 语义 | 一次 Agent 运行**产出的文件**，给**当前用户**看 | 登记到**群组**、带可见性的交付物，给**其他 agent/人**看 |
| 归属 | 无 Owner/Group | `OwnerAgentID` / `GroupID` / `Visibility` |
| 存储 | **无独立存储**（只活在事件 / checkpoint / UI 块里） | `FileStore`：`SaveArtifact` / `LoadArtifact` / `ListArtifacts` |
| 版本 | 只有 `SHA256` 内容指纹 | `Version int` 自增 |
| 命令 | 无（模型不能主动声明） | `hub_artifact_get` / `hub_task_artifact_preview` |
| 前端 | `ArtifactCard.tsx` | 群组面板 |

> **硬约束 `【方案】`**：两套**不合并、不共享存储、命名不冲突**。
> 系统级工具一律 `artifact_*`；群组级已有 `hub_artifact_*`，不得改名或复用。
> 历史设计里那张"工具替换表"针对的也是系统级，别把它误接到 agenthub。

---

## 二、现状盘点（系统级）

### 2.1 已经有的（比预想的多）

| 能力 | 位置 | 说明 |
|---|---|---|
| **变更收集（钩子式）** | `agents/artifacts.go:64 wrap` + `:77 observe` | 包住 `events.ToolResponse`，从 `write_file` / `edit_file` 的落盘结果里提取 |
| **同路径合并** | `:146 mergeArtifactDeltas` | 同一路径多次编辑累加 `Added`/`Removed` |
| **噪声过滤** | `:183 filterNoiseArtifacts` + `:173 artifactNoiseDirs` + `:178 artifactNoiseSuffixes`（`.tmp/.bak/.log/.swp`） + `:200 isNoiseArtifactPath` | **已有第一层噪声规则**（对应元设计评分表的负分项） |
| **单轮上限** | `:32 maxArtifacts = 50` | 超出置 `ArtifactsTruncated` |
| **内容指纹** | `core.Artifact.SHA256`（前 16 字节 hex） | 已有，可作版本哈希的基础 |
| **展示路径** | `:224 displayPath` | 工作区内 → 相对路径；工作区外 → 绝对路径 |
| **事件面** | `events/agent.go:94 AgentEnd.Artifacts` | 挂在 `AgentEnd` 单点发出 |
| **UI 块** | `desktop/bridge/view_reducer.go` + `artifacts_reducer_test.go`（14 用例） | `kind:"artifacts"` 块；JSON 形状 / 幂等 / checkpoint 往返 / 子 agent 归并均有测试 |
| **卡片** | `desktop/app/src/components/ArtifactCard.tsx`（22 KB） | 「预览 / 打开」两个动作 |

### 2.2 明确没有的（就是本方案要补的）

| 缺口 | 后果 |
|---|---|
| **持久化存储** | 产物只活在事件流与 checkpoint 里；会话重开后拿不到历史版本 |
| **版本库** | 只有单轮 `SHA256`，跨轮无版本历史，无法回滚、无法按版本导出 |
| **入口 + 依赖闭包** | 只按**文件**列举。一个页面 = index.html + css + js + 图片，现在会被拆成 4 条产物，用户看到 4 张卡 |
| **HTML 预览服务** | 卡片只能「打开」（交给系统应用），没有本地只读服务、没有 `data-src` 注入、没有热刷新 |
| **选区协议** | 完全没有。不能选中页面元素引用回对话 |
| **Inspector / 写守卫** | 完全没有 |
| **工具面** | 模型**无法主动声明**"这是产物"，只能靠收集器被动汇总 |
| **判定四层** | 只有噪声过滤一层，没有声明、目录约定、评分、用户纠正 |

---

## 三、目标与范围

### 3.1 一句话目标

> 一次运行结束后，用户**不用自己找文件**就看到最终页面；这个页面**可点、可选、可引用回对话**，
> 而 Agent 改的始终是**源码**。

### 3.2 本期做（Phase 1）

1. 系统级产物**存储与版本库**（入口 + 依赖闭包，内容哈希）
2. **预览服务**（本地只读 HTTP + 签名路径 + `data-src` 注入 + SSE 热刷新）
3. **工具面**：`artifact_publish` / `artifact_promote` / `artifact_export`
4. **判定四层**（复用已有噪声过滤作第 3 层的基础）
5. **选区协议**：8 种引用类型 + `<selection>` 注入 + 写守卫

### 3.3 本期不做

画布 / 多 artboard / 协作分享 / 流式预览 / PPTX 导出 / **agenthub 群组产物**的任何改动。

---

## 四、目标架构

```
┌──────────────┐  tools schema   ┌────────────────────────────────────┐
│  Agent Loop  │◄───────────────►│ artifact_publish / promote / export │
│ agent_loop.go│                 └──────────────┬─────────────────────┘
└──────┬───────┘                                │
       │ AgentStart/End                          ▼
       ▼                          ┌────────────────────────────────────┐
┌──────────────────────┐          │ Classifier（四层判定）              │
│ artifactCollector    │ ChangeSet│  1 声明 / 2 目录约定                │
│ ✅ 已存在            │─────────►│  3 评分（复用噪声规则） / 4 用户纠正 │
│ 钩子 observe()       │          └──────────────┬─────────────────────┘
│ 合并 / 噪声 / 上限50 │                         ▼
└──────────┬───────────┘          ┌────────────────────────────────────┐
           │ 快照 diff（新增，兜底）│ Version Store（新增）              │
           └─────────────────────►│  core.Artifact + ArtifactVersion    │
                                  └──────────────┬─────────────────────┘
                                                 ▼
                                  ┌────────────────────────────────────┐
                                  │ Preview Host（新增，本地只读 HTTP）  │
                                  │  签名路径含 wsKey + data-src 注入    │
                                  └───────┬──────────────────┬─────────┘
                                          ▼                  ▼
                                  ┌──────────────┐   ┌──────────────────┐
                                  │ 沙箱 WebView │   │ ArtifactCard     │
                                  │ Inspector    │   │ ✅ 已存在         │
                                  └──────┬───────┘   └──────────────────┘
                                         │ selection / comment
                                         ▼
                                  ┌────────────────────────────────────┐
                                  │ selection_refs.go（新增）           │
                                  │  → <selection> 注入下一轮           │
                                  └────────────────────────────────────┘
```

**复用而非重写 `【方案】`**：元设计里的 "ChangeTracker（钩子 + 快照）" 中，**钩子这一半已经存在**
（`artifactCollector.wrap/observe`），只需补**快照 diff** 作完整性兜底——因为 bash / Python 产生的
文件不经过任何工具钩子，收集器看不到它们。

---

## 五、数据模型演进

### 5.1 现状 vs 目标

| 项 | 现状 `【现状】` | 目标 `【方案】` |
|---|---|---|
| 产物标识 | 无 ID（只有 path） | 稳定 `ArtifactID`（按入口路径 + wsKey 派生） |
| 版本 | 单轮 `SHA256` | `ArtifactVersion`：`path → hash` 映射 + 内容寻址 blob |
| 依赖 | 无 | 入口 + 依赖闭包（`x/net/html` 解析 + 预览期实际加载记录） |
| 入口声明 | 无 | `DeclaredEntry`（`artifact_publish` 或 `.artifact/manifest.json`） |
| 存储 | **无** | 工作区下 `.artifacts/`（版本目录 + blob 去重） |

### 5.2 新增类型

```go
// 【方案】系统级产物版本；与 agenthub.Artifact 无关，独立存储
type ArtifactVersion struct {
    ArtifactID ArtifactID        `json:"artifact_id"`
    Version    int               `json:"version"`
    VersionID  string            `json:"version_id"`  // sha256(排序后的 path→hash)
    Entry      string            `json:"entry"`       // 入口相对路径，如 index.html
    Files      map[string]string `json:"files"`       // relPath → blob hash
    WSKey      string            `json:"ws_key"`      // ⚠️ 必带，见 R1
    CreatedAt  time.Time         `json:"created_at"`
}
```

### 5.3 兼容性

- `core.Artifact` **结构不动**（事件 JSON、前端卡片、14 个 reducer 测试全部不受影响）
- 新增的版本库是**旁挂**的：`core.Artifact` 仍走事件流；版本库负责持久化与预览
- `SHA256` 语义明确为"入口文件指纹"，与 `VersionID` 不冲突

---

## 六、协议设计

### 6.1 引用类型（8 种）`【历史】§23.1b`

| 层 | kind | 用户动作 | payload 关键字段 | 模型拿到什么 |
|---|---|---|---|---|
| 节点级 | `element` | 点选一个元素 | `selector, tag, text(≤300), html(≤2KB), rect, styles, viewport, theme` | 一个节点 + 它长什么样 |
| | `elements` | Shift 追加 / 框选 | `selector, items[{selector,tag,text,rect}], truncated` | 一组节点 + 共同祖先 |
| | `text` | 选中文字 → 引用 | `anchor{selector,tag}, text, range{start,end}, context{before,after}` | 一段文字 + 它属于哪个节点 |
| | `comment` | 批注 pin + 写意见 | `anchor{selector\|rect}, text` | 一条意见 + 锚点 |
| 区域级 | `region` | 拖矩形 / 截图上画框 | `rect{x,y,w,h}, elements[≤8], shot_id?, viewport` | 区域 + 候选元素；**没有单一节点** |
| 整体级 | `page` | 引用整页 | `entry, title, viewport, shot_id?` | 整个页面 |
| | `artifact` | 从卡片/列表引用 | `entry, files[≤50], latest_version` | 这一版交付了什么 |
| | `file` | 源码面板/文件树引用 | `path, line_range?, sha256?` | 源码位置，不经预览 |

**降级规则** `【历史】`：canvas / 跨域 iframe / closed shadow DOM → 落 `region`；框选 >20 → 截断并
`truncated:true`；半行文字 → `text` 带 `range`，**不**升级成整节点；版本过期 → 走失效流程。

### 6.2 注入格式 `【历史】§23.3`

```
<selection kind="element" version_id="sha256:…">
  （页面文本 / outerHTML 片段 / 截图引用）
</selection>

<user_instruction>
  把这段文字改成更正式的措辞
</user_instruction>
```

**铁律**：只有 `<user_instruction>` 是指令，`<selection>` 内一律是**数据**。
系统提示必须显式写明——否则页面里的 prompt injection 可直接操纵 Agent。

### 6.3 写守卫 `【历史】§23.6`

- 挂在 **`beforeToolCall`**，不只校验 `artifact_edit`
- **必须常驻安装**（会话创建时即装，`active=false` 零成本放行）
  原因：`selection.commit` 可能在 run goroutine 已启动**之后**才到——用户先发消息、再点选区。
  装晚一步，那道窗口里 bash 是放行的。

### 6.4 失效与漂移 `【历史】§23.5`

选区带 `version_id`；版本过期用文本指纹在新版本重定位，找不到就明确提示「选区已失效」，
**不要静默套到错误节点上**。

---

## 七、工具面 `【历史】§22 + 【元设计】`

| 工具 | 读写 | 挂载条件 | 关键约束 |
|---|---|---|---|
| `artifact_publish` | 写 | 文件系统模式 | entry 必须在产物区且存在；同 id 再调即新版本 |
| `artifact_promote` | 写 | 文件系统模式 | from 必须在草稿区；目标 id 不存在则新建 |
| `artifact_export` | 读 | 总是 | 基于 version 快照，返回路径与 `warnings` |
| `source_get` / `source_find` | 读 | 有选区时 | `context_lines ≤ 20`；find ≤ 5 条候选 |
| `preview_screenshot` / `preview_inspect` | 读 | 总是 / 有选区时 | 截图 ≤ 1MB；`outer_html ≤ 8KB` |
| `designsystem_get` | 读 | Design type | 按名取，不返回全量；读**当前**设计系统而非版本内 |

**命名铁律** `【历史】§22.1`：线上名**一律下划线**。元设计写的是 `artifact.publish`（点号），
本项目定为 `artifact_publish`，不要混用；也**不要**与群组级的 `hub_artifact_get` 混淆。

**CodeMode 编排要求** `【历史】§22`：可编排广告、结构化结果（脚本拿对象非 JSON 串）、
路径语义差异写进描述、图片工具降级、**per-artifact 串行**、目录预算。

---

## 八、预览服务

| 项 | 决策 | 理由 |
|---|---|---|
| 协议 | 本地 HTTP，仅 `127.0.0.1`，随机端口 | `file://` 遇 ES Module / fetch 跨域限制 |
| 鉴权 | 路径带签名：`/{wsKey}/{artifactID}/{version}/{sig}/path` | 比 cookie 简单；**签名含 wsKey**（防跨工作区） |
| 只读 | 是，不执行工作区代码 | 安全 |
| 注入 | **服务时注入 `data-src`**，只改发给浏览器的副本 | bash/Python 产出的页面不经过任何入库钩子 |
| 热刷新 | SSE `reload` | 不 `location.reload()`，避免丢滚动位置 |
| 版本切换 | 新 URL（版本在路径里） | 无需缓存失效逻辑 |

> **与 agenthub 图片预览的关系**：`hub_task_artifact_preview.go:42` 是**群组任务产物**的图片预览，
> 属 B 类，与本方案**两条独立链路**，不合并、不改动。

---

## 九、产物判定四层 `【元设计】+【现状】`

| 层 | 机制 | 现状 | 待补 |
|---|---|---|---|
| 1 声明 | `artifact_publish` | ❌ 无 | 新增工具 |
| 2 目录约定 | 产物区 `artifacts/<id>/`、草稿区 `.scratch/` | ❌ 无（`artifactNoiseDirs` 只是排除噪声） | 新增约定与文档 |
| 3 规则评分 | AgentEnd 对未声明文件打分 | ⚠️ **部分**：已有噪声过滤（`filterNoiseArtifacts`），无正分与阈值 | 补正分、阈值 ≥3、候选态 |
| 4 用户纠正 | 卡片「设为产物 / 不是产物」 | ❌ 无 | 新增卡片动作 + 会话偏好 |

评分表 `【元设计】`：`artifacts/` +5；本轮新建的 html/svg/pdf/md +2；`.scratch`/tmp/node_modules −10；
文件名含 tmp/debug/test/out/log −3；空文件或超大二进制 −2；被其他产物引用则并入、不单独计分。

**产物定义**：**入口 + 依赖闭包**。解析入口 HTML 的 `src/href/import`，或在预览服务里记录实际
被加载的文件。`【历史】§21.14` 明确用 **`golang.org/x/net/html` tokenizer**（唯一新依赖），
不用自写正则——正则漏掉时表现是「页面看着正常、依赖没进包」，最难发现。

---

## 十、实施顺序

| 阶段 | 内容 | 依赖 | 验收 |
|---|---|---|---|
| **P0** | 版本库 + 内容哈希 + `wsKey` 隔离 | 无 | 单测：同内容不同工作区不串读 |
| **P1** | 预览服务（HTTP + 签名 + `data-src` + SSE） | P0 | 任意入口页可开，静态依赖全 200 |
| **P2** | 工具面 `artifact_publish/promote/export` | P0 | 调用后卡片出现、版本 +1 |
| **P3** | 判定四层（评分/候选/用户纠正） | P2 | 误报、漏检可统计 |
| **P4** | 选区协议 + Inspector + `<selection>` 注入 | P1 | 选中 → 引用 → 模型收到 `<selection>` |
| **P5** | 写守卫（常驻安装） | P4 | 选区任务期间 bash 写入被拒且有提示 |
| **P6** | `artifact_edit` + 失效漂移 | P4/P5 | 版本过期明确提示，不错改 |
| **P7** | CodeMode 编排 | P2 | 脚本内可调工具、结果为对象 |

---

## 十一、风险登记册

| # | 风险 | 后果 | 对策 |
|---|---|---|---|
| R1 | artifact id / version **非全局唯一** | 大文件从**另一个工作区**读出 | 路径与索引一律带 `wsKey`（`【历史】`实测坑） |
| R2 | 守卫装晚 | 「先发消息再点选区」窗口内 bash 放行 | 会话创建时即装，`active=false` 零成本 |
| R3 | 正则解析依赖遗漏 | 页面正常但依赖没进包 | 用 `x/net/html` tokenizer |
| R4 | 页面内容 prompt injection | Agent 被页面文本操纵 | `<selection>` 声明为数据 + 系统提示硬规则 |
| R5 | 版本快照占空间 | 磁盘膨胀 | 内容寻址去重 + 失效版本 GC |
| R6 | **`filterNoiseArtifacts` 死代码** | 接线后行为不可预期 | `agents/artifacts.go:183` 先清理再扩展为评分层 |
| R7 | 预览服务暴露 | 端口被滥用 | 只监听 127.0.0.1 + 签名 + 校验 Host 头 |
| R8 | **与 agenthub 产物混淆** | 存储/命令/命名串台，改错系统 | 本文 §1 边界为硬约束；工具前缀 `artifact_*` vs `hub_artifact_*` |

---

## 十二、测试策略

| 层 | 测什么 | 参考 |
|---|---|---|
| 单元 | 版本哈希稳定性、`wsKey` 隔离、评分、入口解析 | 新增 |
| 收集器 | 合并、噪声过滤、上限 50、truncated（**已有**） | `agents/artifacts.go` |
| Reducer | 产物块 JSON 形状、幂等、checkpoint 往返、子 agent 归并 | ✅ 已有 14 用例 |
| 协议 | 8 种 ref 序列化、注入格式、失效重定位 | `【历史】§23.8` |
| 集成 | 预览服务 200/403、SSE reload、`data-src` 只在响应里 | 新增 |
| 端到端 | 选中 → 引用 → 请求含 `<selection>` → 改动落到源码 | 新增 |
| 回归 | **agenthub 群组产物不受影响**（`hub_artifact_get`、任务产物图片预览） | 已有测试 |

---

## 十三、与原设计的关系

| 原设计 | 本文处置 |
|---|---|
| `【历史】` §22 CodeMode 全文 | **原样采纳** |
| `【历史】` §23 选区协议全文 | **原样采纳**，本文补落地顺序与风险 |
| `【历史】` §21 文件布局（24 新增 / 10+7 修改） | **修订**：版本改为内容哈希；索引加 `wsKey`；明确与 agenthub 隔离 |
| `【元设计】` 通用设计 | **对齐采纳**，三处修订：命名下划线、通信不用 JSON-RPC、守卫常驻 |
| 元设计 "ChangeTracker" | **半复用**：钩子已存在（`artifactCollector`），只补快照 diff |
| agenthub 群组产物 | **明确排除**，不动 |

---

## 附录：相关文件

| 文件 | 作用 |
|---|---|
| `docs/ARTIFACTS_AGENT_DESIGN.md` | 历史设计逐字恢复（§21–§24 + 元设计 20 条对照） |
| `docs/ARTIFACTS_META_DESIGN.md` | 用户提供的通用元设计（355 行） |
| `docs/ARTIFACTS_IMPLEMENTATION_CHECKLIST.md` | 11 个 `E-*` 改动点 + 协议/CodeMode/验收清单 |
| `docs/ARTIFACT_CODE_FRAGMENTS.md` | 177 段可编译代码块 |
| `docs/ARTIFACT_SECTIONS_EXTRA.md` | 21 整节正文 + 121 段零散片段 |
| `docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md` | 结构化恢复稿 |
| `docs/recovery-evidence/2026-11-19/` | 恢复证据归档（108 文件 + sha256） |
