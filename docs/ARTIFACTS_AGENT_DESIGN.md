# Artifacts + Agent Design（重建版）

> **本文不是原文档的恢复件，而是按原文档的模块骨架重新装订的新文档。**
>
> 原文档（`docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md`）随 2026-11-19 误删事故丢失。
> 穷尽扫描本机残留后，恢复出：**§22–§24 全文 580 行**、**28 条章节标题**、
> **82 行设计表格**、**261 条散文行**、**177 段代码块**。§1–§20 的标题与正文不可恢复。
>
> 本文按 **协议 / 架构 / 技术方案 / 评估 / 测试** 五模块重新组织：
> `【原文】` = 逐字恢复；`【重建】` = 据证据重述；`【待补】` = 需元文档补齐。

_重建于 2026-11-20 01:10。_

## 模块状态总表

| 模块 | 状态 | 来源 |
|---|---|---|
| 协议 · 引用类型与数据模型 | ✅ 表格逐字 | §23 |
| 协议 · 选区引用表 / 注入格式 / 失效漂移 | ✅ 全文 | §23.2/§23.3/§23.5 |
| 协议 · CodeMode 工具编排 | ✅ 全文 285 行 | §22 |
| 架构 · 文件布局与改动点索引 | ✅ 标题 + 表格 | §21 |
| 技术方案 · 代码级改动 | ✅ 177 段代码块 | 补丁脚本 rep() |
| 评估 · 依赖选型 / Review 清单 | ✅ 表格 + 全文 | §21.14 / §24 |
| 测试 · CodeMode / 选区 | ✅ 全文 | §22.8 / §23.8 |
| 调研对比（元文档） | ✅ 元设计 + 对照表 | `ARTIFACTS_META_DESIGN.md` + §0.2 |

---

---

# 第零部分 · 元设计基线（调研与通用设计）

> 来源：用户提供的元文档《Agent 产物系统完整设计：预览、判定、选区改稿与画布》
> （全文见同目录 `ARTIFACTS_META_DESIGN.md`，355 行）。它是不依赖本项目的通用设计，
> 下面只摘出与本项目落地直接相关的骨架，并给出**逐条对照**。

## 0.1 元文档的核心结论（摘录）

| 主题 | 元文档结论 |
|---|---|
| 变更检测 | **钩子负责归因，快照 diff 负责完整性**；以快照结果为准 |
| 产物判定 | 四层：**工具声明是契约 → 目录约定覆盖 bash/Python → 规则评分兜底 → 用户纠正** |
| 产物定义 | **产物 = 入口 + 依赖闭包**；生成脚本不是产物但记 provenance |
| 协议分层 | A Agent Tools / B 页面协议 / C 生命周期钩子 / D 上下文注入，四类各有传输与是否进模型 |
| 选区 | **服务时注入 `data-src`**，磁盘源码保持干净；选区必须带 versionId |
| 安全 | 选中页面文本是**不可信数据**，必须放 `<selection>` 并声明为数据 |
| 守卫 | 选区任务激活时，范围守卫挂在 `beforeToolCall`，不只校验 `artifact.edit` |
| 存储 | SQLite + 内容哈希；版本只存 path → hash |
| 落点规则 | 样式来源决定落点（inline / class / tailwind / token / 媒体查询） |
| 降级 | 有 nid → 有 data-src → selector+指纹 → CDP，四级降级 |

## 0.2 元文档 → 本项目落地对照

> 依据来自恢复出的 §21（文件布局/改动点）、§22（CodeMode）、§23（选区协议）与 177 段代码块。
> 「落地」列的每一项都能在恢复材料中找到出处。

| # | 元文档设计 | 本项目落地 | 一致/偏离 | 证据 |
|---|---|---|---|---|
| 1 | `artifact.publish` 工具 | `artifact_publish`（**线上名一律下划线**） | 一致（命名改了下划线） | §22.1 |
| 2 | `artifact.promote` / `edit` / `export` | 同名工具，同样下划线 | 一致 | 工具替换表 |
| 3 | `artifact.edit` 必带 `base_version` | `base.version(…)/base.lock(id)/base.readVersionFile` | 一致（做成公共能力） | 工具替换表 |
| 4 | 预览服务：本地只读 + 随机令牌 + 校验 Host | `artifactPageRef{Workspace,SessionID,ArtifactID,Version}` + 路径带签名 `h.sign(id,version)` | 一致（改为签名路径而非 cookie） | `patch_ws.py` 代码块 |
| 5 | 路径安全：realpath 后比对前缀 | 见「路径里必须带 wsKey」——artifact id 与 version **都不是全局唯一**，只按 id+version 建索引会让大文件跨工作区串读 | **本项目更严**（这正是元文档没提到的坑） | `patch_ws.py` |
| 6 | 变更检测：钩子 + 快照 diff | `agents/agent_loop.go-5`：**收尾合并快照 + 声明 + 分类** | 一致 | §21 标题 |
| 7 | 产物判定四层 | `filterNoiseArtifacts`（**有死代码待清理**）+ `subagent/subagent.go` 产物声明表**继承** | 一致 + 新增「子 agent 继承声明表」 | §21.16 / 散文行 |
| 8 | 选区协议 `selection.commit` | `desktop/bridge/selection_refs.go`（新增全文）+ `selection_guard.go` | 一致 | §23.2 |
| 9 | 8 种引用类型（节点/区域/整体） | element / elements / text / comment / region / page / artifact / file | 一致（元文档为设计稿，本项目定型为 8 种） | §23.1b 表格 |
| 10 | `<selection>` 是数据不是指令 | §23.3 同名章节：`<user_instruction>` 才是指令 | 一致 | §23.3 |
| 11 | 守卫挂 `beforeToolCall` | `newSelectionScope()` **常驻安装**（active=false 时零成本放行），避免「先发消息再点选区」的窗口期 | **本项目更严** | `patch_doc.py` 代码块 |
| 12 | 失效与漂移：用 fingerprint 重定位 | §23.5 失效与漂移 | 一致 | §23.5 |
| 13 | `designsystem.get` 按需取 | `designsystem_get` 读的是**当前**工作区设计系统，**不是某个版本内** | 一致 + 显式澄清作用域 | 工具替换表 |
| 14 | 页面协议：MessageChannel + 来源校验 | `artifact_embed.go`：`//go:embed artifact_inspector.js` 注入 + `artifact_commands.go`(~230 行) | 实现方式不同（embed 注入而非 CDP） | N14 / N14b |
| 15 | 通信：JSON-RPC 2.0 | 本项目走 **command_registry + IPC 命令**（`CmdAsk`/`CmdAskBatch` 等），非 JSON-RPC | **明确偏离** | §21.10 |
| 16 | 导出：PDF/PNG/PPTX/Markdown | `artifact_export`，基于 version 快照 | 一致 | 工具替换表 |
| 17 | CodeMode 下工具可编排 | §22 全文：9 个工具在脚本内可调、结构化结果、per-artifact 串行、目录预算 | **元文档未覆盖，本项目新增** | §22 |
| 18 | `autoOpen: always/ask/never` | 恢复材料中未出现 | 【待补】 | — |
| 19 | 流式预览（parse5 + morphdom） | 恢复材料中未出现 | 【待补】 | — |
| 20 | 画布与多 artboard | 恢复材料中未出现（元文档列为 Type 层能力） | 【待补】 | — |

## 0.3 由对照得出的三条结论

1. **主体一致**：协议分层、判定四层、选区即数据、守卫挂 beforeToolCall —— 本项目与元文档同构，
   说明当初就是照这份元设计落地的。
2. **两处本项目更严**：多工作区路径必须带 `wsKey`（否则跨工作区串读）；选区守卫常驻安装（消除窗口期）。
   这两条是元文档没有、实践踩出来的。
3. **一处明确偏离**：通信层没有采用 JSON-RPC 2.0，而是复用既有 `command_registry` + IPC。
4. **两处本项目独有**：CodeMode 工具编排（§22 全节）；子 agent 产物声明表继承（§21.16）。

---

# 第一部分 · 协议

## 1.1 引用类型与取选模式（【原文】表格）

| 模式 | 进入方式 | 产出 kind |
|---|---|---|
| 元素（默认） | 直接点 | `element`；Shift 点 = `elements` |
| 区域 | 工具条「区域」或按住 `R` 拖 | `region` |
| 文字 | 直接拖选文本（自动，无需切模式） | `text` |
| 整页 | 工具条「整页」 | `page` |
| 层 | kind | 用户动作 | 选择范围 | payload 关键字段 | 模型拿到什么 |
|---|---|---|---|---|---|
| **节点级** | `element` | 点选一个元素 | 单个节点（语义提升后） | `selector, tag, text(≤300), html(≤2KB), rect, styles, viewport, theme` | 一个节点 + 它长什么样 |
| | `elements` | Shift 追加 / 框选 | 2–20 个节点（LCA 判断成组） | `selector, items[{selector,tag,text,rect}], truncated` | 一组节点 + 共同祖先 |
| | `text` | 选中文字 → 「引用」 | 一个 `Range` | `anchor{selector,tag}, text, range{start,end}, context{before,after}` | 一段文字 + 它属于哪个节点 |
| | `comment` | 批注 pin + 写意见 | 锚点（节点或矩形） | `anchor{selector|rect}, text` | 一条意见 + 锚点位置 |

## 1.2 选区 → Agent Chat 协议（【原文】§23 全文）

### 23. 选区 → Agent Chat：引用而不是直接发

> 设计稿的默认动作是「选完就发一条改稿任务」。真实使用里，用户更多是**先引用、再自己写
> 一句话**（「这个按钮改成主色」「这块文案太长了」）。所以 Phase B 必须有两个动作，
> 且「引用」要能复用 Composer 已有的 chip 机制，而不是再造一个输入框。

#### 23.1 两个动作

| 动作 | 触发 | 结果 | 守卫 |
|---|---|---|---|
| **引用到对话**（默认） | 浮动框「引用」/ `Cmd+Enter` | 选区变成 Composer 里的一枚 chip；用户继续编辑文字，发送时一起进消息 | **不激活**写守卫（用户可能只是问「这块为什么这样」） |
| **直接改** | 浮动框「直接改」 | 立刻 `Ask` 一条改稿任务（含选区数据） | 激活写守卫（§10），只允许改这个 artifact |

#### 23.2 服务端选区引用表（`desktop/bridge/selection_refs.go`，新增全文）

```go
package main

// 选区引用：页面选中 → 服务端存一份 → 渲染层只拿 refId。
//
// 为什么不把选区数据直接塞进 Composer：
//   1. 页面/渲染层是不可信输入（页面是模型生成的），2KB 的 html 里可以塞任何东西；
//   2. 发送时要用**当前最新版本**重新校验（选区可能已经过期）；
//   3. chip 只需要一个短 id，checkpoint 也不必存两份。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

type selectionRef struct {
	ID       string
	WS       string
	SID      string
	Artifact string
	Version  string
	Entry    string
	Payload  json.RawMessage // 已裁剪（≤2KB）的选区数据，原样来自 inspector
	Label    string          // chip 上显示的文字（tag + 文本前 24 字）
	Created  time.Time
}

const (
	selectionRefTTL      = 30 * time.Minute
	selectionRefPerSess  = 20
	selectionRefMaxBytes = 2 << 10
)

type selectionRefStore struct {
	mu   sync.Mutex
	refs map[string]*selectionRef
	now  func() time.Time
}

func newSelectionRefStore() *selectionRefStore {
	return &selectionRefStore{refs: map[string]*selectionRef{}, now: time.Now}
}

func newSelectionRefID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "sel_" + hex.EncodeToString(b[:])
}

// Put 存一条引用；同一会话超过上限时丢最旧的（选区是瞬时意图，不值得报错）。
func (s *selectionRefStore) Put(r *selectionRef) (string, error) {
	if len(r.Payload) == 0 || len(r.Payload) > selectionRefMaxBytes {
		return "", errors.New("selection payload missing or too large")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID = newSelectionRefID()
	r.Created = s.now()
	s.refs[r.ID] = r
	s.enforceLocked(r.WS, r.SID)
	return r.ID, nil
}

func (s *selectionRefStore) enforceLocked(ws, sid string) {
	var oldest *selectionRef
	n := 0
	for _, r := range s.refs {
		if r.WS != ws || r.SID != sid {
			continue
		}
		n++
		if oldest == nil || r.Created.Before(oldest.Created) {
			oldest = r
		}
	}
	if n > selectionRefPerSess && oldest != nil {
		delete(s.refs, oldest.ID)
	}
}

// Get 取引用并校验属主（ws+sid）：refId 不能跨会话复用。
func (s *selectionRefStore) Get(ws, sid, id string) (*selectionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refs[id]
	if !ok {
		return nil, errors.New("selection ref not found (it may have expired)")
	}
	if r.WS != ws || r.SID != sid {
		return nil, errors.New("selection ref belongs to another session")
	}
	if s.now().Sub(r.Created) > selectionRefTTL {
		delete(s.refs, id)
		return nil, errors.New("selection ref expired; re-select the element and try again")
	}
	return r, nil
}

// Sweep 周期清理（挂在 revisit 清扫器上，与 artifact_sweep 同一个 tick）。
func (s *selectionRefStore) Sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.refs {
		if now.Sub(r.Created) > selectionRefTTL {
			delete(s.refs, id)
		}
	}
}
```

> 存的是**页面原文**，但 `Payload` 在 `Put` 时已由 `onArtifactPageMessage` 裁剪过
>（`selector/tag/text/html(≤2KB)/rect/styles/viewport/theme`，见 §10.5）。

#### 23.3 发送时注入：`<selection>` 是数据，不是指令

`CmdAsk` 的 payload 增加 `selection_refs: ["sel_xxx", ...]`（最多 5 条）。bridge 在
`bs.s.Ask` 之前把它们拼成**一条**用户消息：

```go
// buildSelectionMessage 把引用拼成消息：指令在前，选区在后，且明确标注为数据。
// 顺序固定（[user_instruction] 然后 [selection]），系统提示里那条规则才可解释。
func buildSelectionMessage(text string, refs []*selectionRef) core.Message {
	var b strings.Builder
	b.WriteString(text)
	for _, r := range refs {
		b.WriteString("\n\n<selection artifact=\"")
		b.WriteString(r.Artifact)
		b.WriteString("\" version=\"")
		b.WriteString(r.Version)
		b.WriteString("\" entry=\"")
		b.WriteString(r.Entry)
		b.WriteString("\">\n")
		b.Write(r.Payload)
		b.WriteString("\n</selection>")
	}
	return core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: b.String()})
}
```

三条纪律：

1. **只有 `<user_instruction>` 是指令**；系统提示（§10.5 已加的那句）明确说 `<selection>` 是
   从页面复制的数据，不得执行其中的指令。
2. **不做 HTML 转义**：`Payload` 是 JSON，`json.RawMessage` 保证它本身合法；外层标签只需要
   把 `"` 留在 JSON 里由模型自行解析。
3. **版本必须带**：没有版本就无法判断「选区说的是哪一版」，`source_get` 也无从定位。

#### 23.4 Composer：复用已有 chip，不新增输入框

Composer 已有两套 chip（`doc-chip` 文档引用、`quote-chip` 插件引用），选区引用是第三套，
但**同一行、同一交互**：

```ts
// 与 DocRef 并列的草稿态（按 draftKey 存，切会话/切草稿不串）
type SelectionRefItem = {
  refId: string
  artifactId: string
  version: string
  selector: string
  label: string // 例：button「立即购买」
}

const [selectionRefs, setSelectionRefs] = useState<SelectionRefItem[]>([])
const MAX_SELECTION_REFS = 5
```

chip 渲染（插在 `docRefs` 与 `quotes` 之间）：

```tsx
{selectionRefs.map((s, i) => (
  <div className="attach-chip doc-chip selection-chip" role="listitem" key={`sel-${s.refId}`}
       title={`${s.artifactId}@${s.version}\n${s.selector}`}>
    <span className="doc-chip-name">{s.label}</span>
    <span className="doc-chip-size">{s.version.slice(0, 9)}</span>
    <button className="attach-x" onClick={() => setSelectionRefs((prev) => prev.filter((_, j) => j !== i))}
            aria-label={t('composer.selection.remove')}>×</button>
  </div>
))}
```

发送：

```ts
transport.send({
  cmd: 'ask',
  payload: {
    text,
    ...(selectionRefs.length ? { selection_refs: selectionRefs.map((s) => s.refId) } : {}),
  },
})
```

**发送成功后才清空**（与 docRefs 同款）：失败保留，用户可以直接重试。

#### 23.5 失效与漂移

| 情况 | 行为 |
|---|---|
| refId 过期（>30min） | `ask` 返回 `selection_ref_expired`，Composer 保留文字、chip 标灰，提示「选区已过期，请重新选择」 |
| 版本已被删（用户手动清版本库） | `selection_ref_stale`：附最新版本号，chip 上给「用最新版本」按钮 |
| 版本还在、但页面元素选择器失效 | 不阻断发送：把 `stale:true` 写进 `<selection>`，模型会先 `source_get` 再改 |
| 同一元素被引用两次 | 前端按 `artifactId+selector+version` 去重，不产生两枚 chip |

**重新定位**（服务端，发送时执行一次）：selector 命中失败 → 用 `text` 指纹在最新版本里找
（`source_get` 同一套归一化），找到就更新 `Version` 并标记 `relocated:true`；找不到就
`stale:true`，**不猜**。

#### 23.6 与写守卫的关系（容易搞反）

- **引用 → 不激活守卫**：用户可能问「这块为什么这样」或让模型改**别的**文件；
- **直接改 → 激活守卫**（§10），并限定 10 分钟 / 单 artifact；
- 用户引用后又点「直接改」：复用同一条 ref，把 `mode` 从 `reference` 改成 `edit`。

这是两条**独立**的状态：`selectionRefStore` 管「引用」，`selectionScope` 管「写范围」。
不要把它们合并成一个结构 —— 生命周期不同（引用 30 分钟、守卫 10 分钟且 AgentEnd 即清）。

#### 23.7 抽出来的公共能力

| 层 | 能力 | 落点 | 谁用 |
|---|---|---|---|
| Go | 版本解析 / 版本内读文件 / 结构化返回 / per-artifact 锁 | `artifact_toolbase.go`（§22.3） | 9 个工具 |
| Go | 宿主 RPC（截图 / inspect / verify） | `hostRPC`（§9.6） | `preview_*` 工具 + 自检闭环 |
| Go | 选区数据裁剪 + 版本校验 + 重定位 | `selection_refs.go` + `selection_guard.go` | 引用、直接改、`source_get` |
| Go | 版本提交 + 热刷新 | `artifactService.commit` + `Notify` | publish / edit / promote |
| 前端 | 打开/预览/导出/提升/拒绝 | `useAppStore` 的 `openArtifactPreview` 等 action | 卡片、画布、选区 |
| 前端 | chip（文档/引用/选区） | `attach-chip` 样式 + `doc-chip` 结构 | Composer、ArtifactsBlock |
| 前端 | 产物卡与包卡 | `ArtifactsBlock` / `BundlesBlock` | 叙述区、子 agent 卡 |

> 抽公共能力的判据：**同一段「校验 + 读 + 格式化」出现第二次就抽**。上面这些都已经在
> 方案里出现 ≥3 次（9 个工具、3 个 chip 场景、2 条预览路径），不抽就是 9 份路径校验。

#### 23.8 测试

| 用例 | 断言 |
|---|---|
| `TestSelectionRefPutGetOwner` | 跨会话 refId → 拒绝；过期 → 拒绝 |
| `TestSelectionRefSweep` | 30 分钟后 Sweep 清空；未过期不动 |
| `TestBuildSelectionMessage` | 指令在前、每个 ref 一个 `<selection>`、版本号在内 |
| `TestAskWithExpiredRef` | 返回 `selection_ref_expired`，不产生 run |
| `TestSelectionRefDedup` | 同 artifact+selector+version 两次引用 → 一枚 chip |
| e2e `verify_selection_ref.mjs` | 选元素 → 引用 → chip 出现 → 发送 → 消息里含 `<selection>` → 模型可读 |

---

---

# 第二部分 · 架构

## 2.1 文件布局（【原文】标题）

- `### 21.1 新增文件（24 项，含测试与脚本）`
- `### 21.2 修改文件总表（10 个 Go + 7 个前端/Electron）`
- `### 21.10 desktop/bridge/main.go（12 处）+ command_registry.go（1 处）`
- `### 21.11 前端 store（checkpoint.ts 2 处、useAppStore.ts 6 处）`
- `### 21.14 go.mod / 依赖`
- `### 21.16 subagent/subagent.go（1 处，产物声明表继承）`

## 2.2 改动点索引（【原文】标题）

- `#### E-command_registry.go-1：命令名常量`  <!-- patch11.py -->
- `#### E-main.go-7：命令分发`  <!-- patch11.py -->
- `#### E-bridge.ts-1：渲染进程的 IPC 封装`  <!-- patch13.py -->
- `#### E-main.go-9：`emit` 清选区作用域（AgentEnd）`  <!-- patch14.py -->
- `#### E-main.go-10：`CmdAsk` / `CmdAskBatch` 清选区作用域`  <!-- patch14.py -->
- `#### E-Narrative.tsx-1：把 bundles 传进产出卡`  <!-- patch16.py -->
- `#### E-ArtifactsBlock.tsx-1：`bundles` prop`  <!-- patch16.py -->
- `#### E-ArtifactsBlock.tsx-2：渲染位置`  <!-- patch16.py -->
- `#### E-agents/agent_loop.go-5：收尾合并快照 + 声明 + 分类`  <!-- patch2.py -->
- `#### E-main.go-11：会话删除时清两张表`  <!-- patch_final.py -->
- `#### E-main.go-12：`CmdAsk` / `CmdAskBatch` 解析 `selection_refs``  <!-- patch_ref2.py -->

## 2.3 已知新增文件（据表格还原）

| 编号 | 文件 | 职责 | ~行数 | 依据 |
|---|---|---|---|---|
| N14 | `desktop/bridge/artifact_embed.go` | `//go:embed artifact_inspector.js` | ~16 | §8.6 |
| N14b | `desktop/bridge/artifact_commands.go` | handleArtifactOpen/List/Promote/Reject/Verify/Export/Canvas/Selection | ~230 | §8.7 |
| N23 | `scripts/artifact_eval.py` | 指标评估 | ~40 | §18.3 |
| N25 | `desktop/app/electron/artifact-verify.ts` | verifyArtifact / screenshotArtifact / inspectArtifact | ~120 | §11.7.1 |
| N26 | `desktop/app/src/lib/bridge.ts`（修改） | artifactVerify / artifactScreenshot / artifactInspect | +12 | §11.7 |

---

# 第三部分 · 技术方案细节

## 3.1 CodeMode 支持（【原文】§22 全文）

### 22. CodeMode 支持（脚本内调用这 9 个工具）

> 结论先说：**9 个工具默认都能在 codemode 脚本里调**（`ExposureDirect` = 可声明 + 可编排），
> 但「能调用」不等于「好用」。要让脚本真的用得起来，必须补三件事：**名字合法**、
> **结果结构化**、**语义分层（脚本路径 vs 模型路径）**。这一章给的就是这三件事的落点。

#### 22.1 命名：线上名一律下划线

设计稿写的是 `artifact.publish` / `source.get` / `preview.screenshot`，**不能直接当 wire 名**：

| 约束 | 证据 |
|---|---|
| 函数名不许带点 | OpenAI `function.name` 与 Anthropic `tool.name` 都只允许 `[a-zA-Z0-9_-]`；本仓 `provider/openai.go:1025` 与 `provider/anthropic*.go` 都是**原样透传**，不做替换 |
| 脚本里不是合法标识符 | `tools.artifact.publish(...)` 是语法错误；只能写 `tools["artifact.publish"]`，模型几乎不会主动这么写 |
| codemode 自己会归一 | `tools/builtin/codemode/descriptions.go:412` 的 `normalize()` 把非 `[字母/数字/_/$]` 一律变 `_`，目录里的脚本名本来就是 `artifact_publish` |

**最终 wire 名**（本文已全文替换；设计稿的点号只在散文里作别名出现）：

| 设计稿 | wire 名 | 脚本内 |
|---|---|---|
| `artifact.publish` | `artifact_publish` | `tools.artifact_publish({...})` |
| `artifact.promote` | `artifact_promote` | `tools.artifact_promote({...})` |
| `artifact.edit` | `artifact_edit` | `tools.artifact_edit({...})` |
| `artifact.export` | `artifact_export` | `tools.artifact_export({...})` |
| `artifact.create` / `artifact.read` | `artifact_create` / `artifact_read` | 对话模式专用 |
| `source.get` / `source.find` | `source_get` / `source_find` | `tools.source_get({...})` |
| `preview.screenshot` / `preview.inspect` | `preview_screenshot` / `preview_inspect` | 见 §22.5（截图在脚本里降级） |
| `designsystem.get` | `designsystem_get` | `tools.designsystem_get({...})` |

> 迁移成本为零：这些工具是新加的，没有历史 wire 名要兼容。文档里保留「`artifact.publish`」
> 这种写法只是为了和设计稿对照，**代码里出现点号就是 bug**。

#### 22.2 暴露档位、注解与「可编排」广告

| 工具 | Exposure | ReadOnly | Destructive | Idempotent | `AdvertiseCodemode()` | 超时声明 |
|---|---|---|---|---|---|---|
| `artifact_publish` | direct | — | — | ✅（内容寻址） | ✅ 批量发布是脚本的主场 | 10s |
| `artifact_promote` | direct | — | ✅（移动文件） | — | ❌ | 30s |
| `artifact_edit` | direct | — | — | — | ✅ 多处 patch 一次提交 | 30s |
| `artifact_export` | direct | ✅ | — | — | ❌ | 120s（PDF） |
| `source_get` / `source_find` | direct | ✅ | — | ✅ | ✅ 批量读源码 | 10s |
| `preview_screenshot` | direct | ✅ | — | — | ❌（脚本里没有图，见 §22.5） | 60s |
| `preview_inspect` | direct | ✅ | — | ✅ | ✅ 批量取节点 | 30s |
| `designsystem_get` | direct | ✅ | — | ✅ | ✅ | 10s |

实现方式（每个工具都加这三段）：

```go
func (t *artifactPublishTool) Exposure() tools.ToolExposure { return tools.ExposureDirect }
func (t *artifactPublishTool) Annotations() tools.ToolAnnotations {
	return tools.ToolAnnotations{IdempotentHint: true, OpenWorldHint: false}
}
func (t *artifactPublishTool) AdvertiseCodemode() bool { return true }
```

> `ExposureDirect` 是**零声明默认值**，这里显式写出来是为了让「这个工具可以被编排」成为
> 评审可见的事实，而不是靠 implementer 记得默认值。
>
> 为什么不把辅助工具设成 `deferred` 省 token：`deferred` 不进 codemode 描述、也不声明给模型，
> 只能靠 `tool_search` 现查。选区任务里模型**必须先看到** `source_get`/`preview_inspect`
> 才会用；省下的 ~400 token 换一次「模型不知道有这个工具」不值得。Phase C 若实测目录过大，
> 再把 4 个辅助工具降级（§22.7）。

#### 22.3 结构化结果：脚本拿到对象，不是 JSON 字符串

本仓的契约（`docs/CODEMODE_CALL_CONTRACT_2026-10-28.md` §2）：**有 `Structured` 给对象，
只有文本给 string**。MCP 适配器已经踩过「脚本对字符串做 `r.count` 恒 undefined」的坑；
artifact 工具必须一开始就做对。

**统一结果形状**（每个工具自己一份，字段名固定）：

| 工具 | 成功结果（对象） |
|---|---|
| `artifact_publish` | `{ok:true, artifact:{id,name,title,entry,version,source,openable,score,files:[{path,role,size}],truncated}}` |
| `artifact_promote` | `{ok:true, artifact:{...}}` |
| `artifact_edit` | `{ok:true, artifact_id, base_version, version, entry, changed:[path], warnings:[]}` |
| `artifact_export` | `{ok:true, path, format, bytes, warnings:[]}` |
| `source_get` | `{ok:true, path, start_line, end_line, total_lines, text}` |
| `source_find` | `{ok:true, hits:[{path,line,col,text}], truncated}` |
| `preview_screenshot` | 模型路径：图片 + 文字；脚本路径：`{ok, width, height, blank, overflow_x, console_errors, page_errors, failed_requests}` |
| `preview_inspect` | `{ok:true, selector, tag, outer_html, styles:{...}, rect:{x,y,w,h}}` |
| `designsystem_get` | `{ok:true, tokens:{...}, components:[...]}` |

**公共实现**（新增 `desktop/bridge/artifact_toolbase.go`，全文）：

```go
package main

// artifactToolBase 是所有 artifact 工具共用的「取版本 / 读版本文件 / 结构化返回」底座。
//
// 为什么要有这一层：9 个工具如果各写一遍
//   resolve(id,version) → 校验 → 读 manifest → 读文件 → 判脚本路径 → 写 sink
// 就会出现 9 份「差不多但不一样」的路径校验（历史上最危险的一类重复）。这里只写一次。
//
// 注意它**不持有** run/session 的可变状态：ws/sid 在注册时固定，scope 从 ctx 取。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/seven7628/hai-harness/tools"
)

type artifactToolBase struct {
	svc  *artifactService
	host *artifactPreviewHost
	ws   string
	sid  string
	// locks 每个 artifact 一把锁：两个并行调用（或脚本里的 Promise.all）同时
	// 对同一 artifact 做 read-modify-write 时，base_version 校验会互相踩。
	locks sync.Map // map[string]*sync.Mutex
}

func (b *artifactToolBase) lock(id string) func() {
	v, _ := b.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// version 取某个版本（空 version = latest），并做归属校验。
func (b *artifactToolBase) version(id, version string) (*artifactVersion, error) {
	if b.svc == nil {
		return nil, errors.New("artifact system disabled")
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("artifact_id is required")
	}
	return b.svc.Version(b.ws, id, version)
}

// readVersionFile 从**版本快照**读文件（不读工作区）：选区/审计必须看不可变内容。
// External 大文件没进版本，退回工作区读（与 artifact_edit 的降级口径一致）。
func (b *artifactToolBase) readVersionFile(v *artifactVersion, rel string) ([]byte, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return nil, fmt.Errorf("path escapes artifact: %s", rel)
	}
	var meta *artifactFileMeta
	for i := range v.Manifest.Files {
		if v.Manifest.Files[i].Path == clean {
			meta = &v.Manifest.Files[i]
			break
		}
	}
	if meta == nil {
		return nil, fmt.Errorf("file not in version %s: %s", v.Manifest.Version, rel)
	}
	if meta.External {
		abs, err := artifactSafeJoin(b.ws, meta.Path)
		if err != nil {
			return nil, err
		}
		return readFileBounded(abs, artifactHTMLMax)
	}
	return os.ReadFile(filepath.Join(v.Dir, "files", filepath.FromSlash(meta.Path)))
}

// emit 统一的返回路径：
//   - 脚本调用（codemode 的 tools.x）→ 写结构化槽，返回一行**紧凑**文本（模型最终看到的是
//     脚本自己 text() 的内容，这里只是让脚本可读）；
//   - 模型直调 → 返回同样的 JSON 文本（不写槽：引擎只对脚本路径消费槽）。
//
// 关键纪律：**失败必须走 error**（→ IsError → 脚本 reject），不要返回 {ok:false}。
// 脚本侧 `try/catch` 只认 reject；返回 ok:false 等于把失败变成一个能点字段的对象。
func (b *artifactToolBase) emit(ctx context.Context, v any, text string) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	if tools.IsScriptCall(ctx) {
		if sink := tools.StructuredSinkFrom(ctx); sink != nil {
			sink.Set(raw)
		}
	}
	if text != "" {
		return text, nil
	}
	return string(raw), nil
}

// OutputSchema 所有工具共用的一份「对象」声明（详细字段按工具各自返回）。
// 引擎只把它当「本工具会在脚本路径写结构化槽」的开关；schema 内容供描述生成用。
func (b *artifactToolBase) OutputSchema() any {
	return map[string]any{"type": "object", "additionalProperties": true}
}
```

每个工具改一行：`Call` 的返回从 `fmt.Sprintf(...)` 换成 `return t.emit(ctx, result, "")`。

> **为什么 `emit` 不判断 `IsError`**：Go 侧错误一律用 `error` 返回，引擎转成
> `ToolResult{IsError:true}`；`emit` 只处理成功路径。这与 MCP 适配器「IsError 一律不写槽」
> 是同一条纪律的两个实现。

#### 22.4 脚本路径 vs 模型路径（隐式差异必须写进设计）

| 维度 | 模型直调（Depth=0） | 脚本内调用（Depth>0） | 对本方案的影响 |
|---|---|---|---|
| 结果截断 | 20 KB 头尾截断 + 中段落盘 | ≤1 MiB 零损耗；超限整条落盘 | `source_get` 的 16KB 上限在脚本里**不是**引擎截断，是工具自己的契约 |
| 结构化结果 | 不消费 sink | 消费 sink → 对象 | §22.3 的 `emit` |
| 图片 | 进 ImageSink → 图片块 | **脚本拿不到图片** | §22.5 截图降级 |
| 审批 | `runBatch → preApprove` | `ExecuteOne → executeApproved → preApprove`（逐次） | 有 `ApprovalRequired` 的工具在脚本里会**弹窗**；artifact 工具都不声明审批 |
| PreToolUse | ✅ 走 `runTools` 的 hook 链 | ❌ **不走**（`tools/nested.go` 的负向测试钉住） | 选区守卫必须封死 codemode（§10.3 已加） |
| 异步摘离 | BatchController 可摘离 | 无摘离入口 | `artifact_export` 别指望脚本里摘离；超时就超时 |
| `searchTools` | 不适用 | 可检索（目录 = 可编排工具） | 描述要可检索（含关键词 publish/preview/selection） |

#### 22.5 图片类工具在脚本里的降级

`preview_screenshot` 在脚本里返回图片没有任何意义（脚本不能把图交给模型；`ImageSink` 的
消费者是引擎，而引擎在脚本路径读的是 `structured`）。所以：

```go
func (t *previewScreenshotTool) Call(ctx context.Context, _ string, args string) (string, error) {
	// …解析参数、调宿主 RPC、拿到 shot（png + 指标）…
	if tools.IsScriptCall(ctx) {
		// 脚本路径：不写 ImageSink，只回指标（页面能不能用，脚本真正需要判断的就是这些）
		return t.emit(ctx, shotMetrics(shot), fmt.Sprintf("screenshot ok: %dx%d (script path returns metrics only)",
			shot.Width, shot.Height))
	}
	// 模型路径：图片进 ImageSink，正文给一句话
	if sink := tools.ImageSinkFrom(ctx); sink != nil {
		sink.Add(shot.PNG, "image/png")
	}
	return fmt.Sprintf("screenshot %dx%d (blank=%v overflow_x=%v)", shot.Width, shot.Height, shot.Blank, shot.OverflowX), nil
}
```

> `preview_inspect` 不需要降级：它返回的是文本 + 计算样式，脚本本来就该拿对象。

#### 22.6 并发：per-artifact 串行

codemode 的串行门（`ExecScope.Acquire`）只在**工具自己声明 exclusive** 时生效，artifact 工具
没法从脚本侧声明。而 `artifact_edit` 是 read-modify-write：

```
读 base（版本）→ 改内存 → 校验工作区未变 → 写工作区 → commit
```

两个并行调用（`Promise.all([tools.artifact_edit(...), tools.artifact_edit(...)])`）会同时读到
同一个 `meta.Latest`，后提交的那个把前一个覆盖掉（内容寻址只会让版本号相同/不同，**不会**
让语义正确）。

修法：`artifactToolBase.lock(id)`（§22.3 已给）——所有**写类**工具（publish/promote/edit/export
之外的三类）在进入 read-modify-write 前取锁：

```go
func (t *artifactEditTool) Call(ctx context.Context, _ string, args string) (string, error) {
	// …解析 args…
	unlock := t.base.lock(a.ArtifactID)
	defer unlock()
	// …rest…
}
```

> 这把锁是**进程内**的：bridge 是唯一写者（版本库同进程），所以足够。不要换成文件锁 ——
> 那会把「同进程并发」变成「跨进程串行」，而本仓没有第二个写者。

#### 22.7 目录预算

| 项 | 数值 |
|---|---|
| 单个工具描述 | ≤ 240 字符（首行是摘要，codemode 只取首行） |
| 9 个工具总增量 | ≈ 1.1k token（`estimateTokens` 口径：字符/4） |
| codemode 目录上限 | 见 `descriptions.go` 的预算常量；超了会按 `cands` 截断 |
| 降级方案（Phase C） | 4 个辅助工具改 `deferred`，靠 `tool_search` 现查；核心 5 个保持 direct |

#### 22.8 测试

| 用例 | 断言 |
|---|---|
| `TestArtifactToolsCallableFromCodemode` | 脚本里 `tools.artifact_publish → artifact_edit → source_get` 全链路成功；`structured` 对象字段可读（不是字符串） |
| `TestArtifactToolsNotCallableWhenHidden` | `HideTool("artifact_publish")` → 脚本调用返回 "not in this call's codemode tool table" |
| `TestArtifactToolErrorsReject` | `artifact_edit` 传过期 base → 脚本 `try/catch` 捕获；`result` 不是对象 |
| `TestPreviewScreenshotScriptPathNoImage` | 脚本路径：`ImageSink` 为空、结构化有 `console_errors` |
| `TestArtifactEditSerializedPerArtifact` | 两个 goroutine 同时 edit 同一 artifact → 一个成功、一个 `version conflict`；最终版本只有一次写入 |
| `TestCodemodeCatalogHasArtifactTools` | `BuildCatalog` 的描述包含 9 个名字，且不含点号 |

---

## 3.2 代码级改动（177 段）

见 `ARTIFACT_CODE_FRAGMENTS.md`。

---

# 第四部分 · 评估

## 4.1 依赖决策（【原文】表格）

| 需要的能力 | 本仓现状 | 结论 |
| HTML/CSS/JS 引用解析 | 自写正则（§6.4，~250 行） | 不引入 `golang.org/x/net/html`：只要 src/href/import，DOM 解析器是杀鸡用牛刀，且要多一个依赖 |
| 需要的能力 | 方案 | 结论 |
| HTML/CSS/JS 引用解析 | `golang.org/x/net/html` tokenizer（§6.4） | **唯一新依赖**。为什么不用正则：属性可以无引号/跨行、`<style>` 里的 `url()` 与 `@import` 形态多，正则漏掉时的表现是「页面看起来正常、依赖没进包」——最难发现的一类 bug。x/net 是 Go 官方模块，无额外传递依赖树 |
| `go.mod` | 0（无新依赖） | — | §21.14 |
| `go.mod` / `go.sum` | +1 模块 | A1 | §21.14 |

## 4.2 工具职责收敛（【原文】表格）

| 工具 | 删掉自己写的那段 | 换成 |
| `artifact_publish` | entry 存在性/版本解析 | `base.version`（校验）+ `emit` |
| `source_get` / `source_find` | 自己 resolve + `../` 校验 | `base.version` + `base.readVersionFile` |
| `designsystem_get` | 自己遍历 css/tailwind 文件 | 工作区文件（不是版本内），显式用 `artifactSafeJoin(ws, rel)` —— 它读的是**当前**设计系统，不是某个版本 |

## 4.3 Review 修正清单（【原文】§24 全文）

### 24. Review 修正清单（本轮全面复核）

> 复核口径：① 方案代码与仓库真实 API 是否对得上；② 隐式状态（版本/选区/开关/生命周期）
> 是否自洽；③ 与 Agent 的交互（工具契约、prompt、CodeMode）是否成立；④ 安全边界是否可绕。
> 下表只列**改过**的，全部已在正文/§21 落地。

| # | 严重度 | 位置 | 问题 | 修法 |
|---|---|---|---|---|
| R1 | **P0** | §7.3 `publishArtifactBundles` | 返回了带版本的副本，**调用方丢弃返回值** → 事件/checkpoint 里的 bundle 永远没有 Version，卡片打不开 | `ae.ArtifactBundles = out` 写回；§21.10-5 同步 |
| R2 | **P0** | §8.3 `refs` 索引 | 键只有 `id+version`，两个工作区同名同内容会互相覆盖；`External` 大文件从**另一个工作区**读 | URL 加 `wsKey` 段、签名覆盖 wsKey、refs/subs 键都带 wsKey（§8.3 已改） |
| R3 | **P0** | §10.3 `allowsPath` | 入口在工作区根时 `dir==""` 直接放行任意相对路径 → 守卫形同虚设 | `dir==""` 时只允许 manifest 已有文件 |
| R4 | **P0** | §10.3 守卫白名单 | `codemode` 的嵌套调用**不经过 PreToolUse**；`agent_spawn` 的新 Run 不继承 ToolGuard | 明确拒绝 `codemode`/`agent_spawn`/`agent_resume`/`subagent`/`explore` |
| R5 | **P0** | 全文工具名 | `artifact.publish` 等点号名不是合法 provider 函数名，也不是合法 JS 标识符 | 全部改下划线 wire 名（§22.1） |
| R6 | **P1** | §7.3/§8.3 | AgentEnd 归档后**不 Notify** → 已打开的预览页不刷新（只有 `artifact_edit` 那条支路推） | publish 后按版本变化 Notify（§7.3） |
| R7 | **P1** | §21.9 `ArtifactRef`/前端 | `Deleted` 只加在 `core.Artifact`，投影与解析链没带 → 删除显示成「0 行变更」 | `ArtifactRef`/`CheckpointArtifact`/`ArtifactItem`/`pickArtifacts` 全链补 |
| R8 | **P1** | §21.12 `ArtifactsBlock` | `if (!list.length) return null` 会让「只有 bundle」的一轮整块消失 | 空判加 `&& !bundles?.length` |
| R9 | **P1** | §5.3 | 钩子路径可能是相对、快照是绝对 → `MergeSnapshot` 永远命不中，兜底失效 | 新增 `abs()`，合并键归一（§21.6-3） |
| R10 | **P1** | §5.4 | 方案假设 `WithToolContext(ctx, &ToolContext{...})`，真实 API 是位置参数 `WithToolContextForRunTask` | 改 `WithArtifactRun` setter（§21.5） |
| R11 | **P1** | §5.3 | `filterNoiseArtifacts` 的 `deleted` 分支是死代码 | 删除该分支（§21.6 注） |
| R12 | **P1** | §7.2 `PublishRun` | 子 agent 的 bundle 也需要版本，但 §21.10-5 原稿只归档根 run | 根/子都归档（§21.10-5 已改） |
| R13 | **P2** | §8.3 | `artifactRuns`（声明表 `sync.Map`）从不清 → 长跑进程按会话泄漏 | 会话关闭/删除时 `Delete`（§21.10 附） |
| R14 | **P2** | §9.6 | `preview_screenshot` 在脚本里返回图片无人消费 | 脚本路径降级为指标（§22.5） |
| R15 | **P2** | §9 写类工具 | 并行 `artifact_edit` 会互相覆盖 `base_version` | per-artifact 锁（§22.6） |
| R16 | **P2** | §22 | 工具名/结果形状没有 CodeMode 契约 | 新增 §22 整章 |

**复核未发现问题的部分**（记录一下，避免下次重复怀疑）：

- 版本 id 是**内容寻址**（manifest 的哈希），同内容重复 publish 不产生新版本 —— 幂等成立；
- `serveMsg` 的 method/origin/大小/kind 白名单、`artifactSafeJoin` 的 realpath 前缀校验都是对的；
- `artifact_edit` 的冲突检查（工作区当前内容 vs base）覆盖了「用户手改」与「另一个 run 改」；
- checkpoint 的 `Block.Bundles` 投影不含文件清单，体积可控；
- `Notify` 的 payload 自带完整 URL（含 sig），页面不需要自己拼签名。

---

# 第五部分 · 测试

## 5.1 CodeMode 测试（【原文】§22.8）

| 用例 | 断言 |
|---|---|
| `TestArtifactToolsCallableFromCodemode` | 脚本里 `tools.artifact_publish → artifact_edit → source_get` 全链路成功；`structured` 对象字段可读（不是字符串） |
| `TestArtifactToolsNotCallableWhenHidden` | `HideTool("artifact_publish")` → 脚本调用返回 "not in this call's codemode tool table" |
| `TestArtifactToolErrorsReject` | `artifact_edit` 传过期 base → 脚本 `try/catch` 捕获；`result` 不是对象 |
| `TestPreviewScreenshotScriptPathNoImage` | 脚本路径：`ImageSink` 为空、结构化有 `console_errors` |
| `TestArtifactEditSerializedPerArtifact` | 两个 goroutine 同时 edit 同一 artifact → 一个成功、一个 `version conflict`；最终版本只有一次写入 |
| `TestCodemodeCatalogHasArtifactTools` | `BuildCatalog` 的描述包含 9 个名字，且不含点号 |

---

## 5.2 选区测试（【原文】§23.8）

| 用例 | 断言 |
|---|---|
| `TestSelectionRefPutGetOwner` | 跨会话 refId → 拒绝；过期 → 拒绝 |
| `TestSelectionRefSweep` | 30 分钟后 Sweep 清空；未过期不动 |
| `TestBuildSelectionMessage` | 指令在前、每个 ref 一个 `<selection>`、版本号在内 |
| `TestAskWithExpiredRef` | 返回 `selection_ref_expired`，不产生 run |
| `TestSelectionRefDedup` | 同 artifact+selector+version 两次引用 → 一枚 chip |
| e2e `verify_selection_ref.mjs` | 选元素 → 引用 → chip 出现 → 发送 → 消息里含 `<selection>` → 模型可读 |

---

## 5.3 通用要求（【原文】散文行）

- - 所有新工具/新命令都配了「失败语义 + 测试用例」；没有测试的改动不应合并（§16.2）。
- - 所有新工具/新命令都配了「失败语义 + 测试用例」；没有测试的改动不应合并（§16.2）；

---

# 附录 A · 散文行碎片（逐字）

- **定位**：`desktop/bridge/artifact_commands.go`（§8.7 的命令族），由 `main.go` 的
- `case CmdArtifactSelection:` 调用。
- // handleArtifactSelection 预览页的两种提交：
- //   mode=reference → 只存一条引用，返回 refId（前端放 chip）；
- //   mode=edit      → 校验空闲 → 激活写守卫 → 立刻 Ask 一条改稿任务。
- // 为什么两种模式都先落 ref：直接改也要能被 Composer 记住（用户在等模型时可以再引用一次）；
- // 引用表是**唯一**的选区事实源，页面/渲染层都不保留副本。
- // 版本必须真实存在：页面是模型生成的，它报的 version 不可信；这也是「过期引用」
- // 唯一能确定性拒绝的地方（版本被清理 → 这里 404，而不是发出去让模型猜）。
- // 直接改：忙就拒绝（scope 是会话级，激活会把正在跑的 run 一起拦掉，见 §24 R18）。
- // 顺序不能换：先 set（守卫生效）→ 再 Ask。反过来会留一个「run 已启动、守卫还没生效」的窗口。
- // manifestPaths 版本内全部文件路径（顺序稳定）：写守卫的允许集合。
- // selectorOf / selectionLabel 只从已裁剪的 selection JSON 里取展示字段，不解析 DOM。
- > `selection.changed`（悬停/点选）**不**落 ref：它每次点选都发，落表会把 20 条上限瞬间打满。
- > 只有 `selection.commit`（用户按了「引用」或「直接改」）才进表。
- §8.5 的 inspector 只做了「点选单个元素」。要支撑 §23.1b 的 8 种 kind，浮动条上需要
- **四个模式**（其中两个是自动的）：
- **增量代码**（接在 §8.5 的 `infoOf` / 事件绑定之后；保持同样的 `post()` 与 Shadow DOM 高亮层）：
- // —— 模式状态：'element' | 'region'；text 由 selectionchange 自动识别 ——
- let picked = []          // Shift 多选：按点选顺序（语义提升后的节点）
- let dragRect = null      // 区域拖拽中的矩形（视口坐标）
- // 工具条追加两个按钮与一个提示位（沿用 .bar 的样式）
- tools.innerHTML = '<button class="ghost" data-mode="element">元素</button>' +
- '<button class="ghost" data-mode="region">区域</button>' +
- '<button class="ghost" data-mode="page">整页</button>' +
- // 元素点选：Shift 追加；普通点选清空重来
- // 区域拖拽：mousedown 记起点，mousemove 画框，mouseup 收口。
- // 用 pointer 事件而不是 mouse：触控板/触屏一致。
- // 文字选区：selectionchange 非折叠时提示「引用这段文字」。
- tools.querySelector('.hint').textContent = '引用这段文字？点「引用」'
- // —— payload 构造：每种 kind 只带 §23.1b 列出的字段，绝不多带 ——
- // 提交：两个动作都带 kind；「引用」= reference，「发送」= edit
- > 高亮层与 `selectorOf` / `infoOf` 全部复用 §8.5，不新写一套。`drawPicked` 只是把
- > 多个高亮框一起画；`drawRect` 画区域框。
- > **`document.addEventListener('click')` 要保留 capture 阶段的 `preventDefault`**
- >（设计稿要求「点选不触发页面自己的跳转/提交」）：上面的示例省略了 `true` 参数是为了可读，
- > 实现时必须与原 §8.5 的那一版合并成**一个** listener，不要绑两次。
- 「引用」不是只有「点一个元素」。按**选择粒度**分三层，共 8 种 `kind`——每一种都对应
- 设计稿 §「选择与改稿技术规范」里的一个交互，只是这里只取「引用」这一半（不直接改稿）。
- **边界与降级**（决定「用户选不到元素时会发生什么」）：
- **明确不引用**（避免把上下文撑爆，也避免把不可信内容当指令）：
- - 完整 `outerHTML` 树、全部 computed styles、console 日志、整页截图本体（只存 `shot_id`，
- 模型需要时调 `preview_screenshot`）；
- - `region` 的截图**不进消息**：它只是「空间提示」，真正的像素由工具按需取。
- **与「整体」有关的两个易混点**：
- - `page` ≠ `artifact`：前者是**渲染结果**（一个版本的一页），后者是**交付物**（可能多文件、
- 多入口）。引用 `page` 时模型会去读入口源码；引用 `artifact` 时模型先看文件清单。
- - `file` 可以脱离预览：用户在源码面板里引用 `styles.css:40-60` 也是合法的对话引用，
- 此时不需要 `artifact_open`。
- Payload  json.RawMessage // 已裁剪（≤2KB）的选区数据，原样来自 inspector
- Label    string          // chip 上显示的文字（tag + 文本前 24 字）
- Artifact string // page/artifact/region/element… 都归属一个 artifact；file 可为空
- // 引用类型白名单 + 每种类型的 payload 上限（元素列表类按条数截断）。
- selMaxElements = 20 // elements 类的条数上限（设计稿：框选上限 20）
- **§9 各工具要跟着改的段落**（不改这些，base 就是死代码）：
- > `designsystem_get` 是唯一**不该**走 `readVersionFile` 的：设计系统是工作区级事实，
- > 跨 artifact 共享；从某个版本快照读会拿到旧 tokens。
- // SweepRefs 清掉指向**已不存在版本**的预览索引（周期清扫的一部分）。
- // 不清的话：用户删了版本/清了工作区，旧 ref 还在，URL 仍能生成，但 serveFile 每次 404 ——
- // 表现是「预览白屏」，排查成本很高。
- // RotateIfLarge 账本 > 8 MiB 时轮转成 metrics.jsonl.1（只保留 1 份）。
- // 这是追加型账本，读路径只做统计分析；留全部历史没有意义，还会拖慢清扫时的目录遍历。
- // file 可以脱离 artifact（源码面板引用工作区文件）；其余类型必须有版本 ——
- // 没有版本的选区无法回答「你说的是哪一版」，source_get 也无从定位。
- // 版本要求按类型分档：file 可以脱离 artifact（源码面板引用工作区文件），
- // 其余类型必须有版本 —— 没有版本无法回答「你说的是哪一版」，source_get 也无从定位。
- // 产物归档必须在 reducer.Apply 之前：AgentEnd 进 checkpoint 时 bundle 必须已带
- // Version（否则刷新后卡片有入口却打不开）。失败不阻断：Version 留空。
- // 只对根 run 归档：子 agent 的包归它的卡片，父 run 的 checkpoint 不该替它存版本。
- // **根 run 与子 run 都归档**：子 agent 的包投影在它自己的卡片上（§21.9-5），
- // 没有 Version 那张卡就打不开；多个子 run 写同一个入口时由版本库的内容寻址去重。
- +	CmdHostReply      Command = "host_reply"      // 宿主 RPC 回包（preview.screenshot）
- +	CmdArtifactSelection Command = "artifact_selection" // 选区任务激活/取消（Phase B）
- +	CmdHostReply      Command = "host_reply"      // 宿主 RPC 回包（preview.screenshot/inspect）
- // —— 产物（预览/版本/选区/导出）——
- CmdArtifactOpen      Command = "artifact_open"      // 取某版本的预览 URL
- CmdArtifactList      Command = "artifact_list"      // 工作区产物列表（画布/左栏）
- CmdArtifactPromote   Command = "artifact_promote"   // 用户「设为产物」
- CmdArtifactReject    Command = "artifact_reject"    // 用户「不是产物」
- CmdArtifactVerify    Command = "artifact_verify"    // 客户端自检结果回传
- CmdArtifactExport    Command = "artifact_export"    // 导出（基础 HTML 随 A4；PDF/ZIP 后置）
- CmdArtifactCanvas    Command = "artifact_canvas"    // 画布页（Phase C）
- CmdArtifactSelection Command = "artifact_selection" // 选区任务激活/取消（Phase B）
- CmdHostReply         Command = "host_reply"         // 宿主 RPC 回包（preview.screenshot/inspect）
- **不改。** 逐项核对：
- **只加一个依赖**：`golang.org/x/net`（HTML tokenizer，§6.8 已给 diff）。逐项核对：
- go.mod / go.sum              + golang.org/x/net（唯一新依赖，§6.8）
- > `embed.ts` **不改**：`createEntry` / `openEmbedTarget` 已有 `srcPath` 去重语义
- **定位**：`desktop/app/src/lib/bridge.ts` 末尾（与 `embedOpenTarget` 同族）。
- // 产物预览/自检：渲染进程只拿 URL 与结果，不碰 Chromium。
- **定位**：`desktop/bridge/main.go:764-800` 的 `switch ev := e.(type)` 里 `case *events.AgentEnd:`
- 分支（`bs.resetLive()` 之后）。
- +		// 选区任务结束：恢复常规写面。放在 emit（而不是 publishArtifactBundles）里，
- +		// 因为它必须对所有结束路径生效：正常完成 / 中断 / 失败 / 子运行收尾。
- **定位**：`desktop/bridge/main.go:3600` 一带（`case CmdAsk:` 与 `case CmdAskBatch:` 两个分支）。
- +		// 用户手动发消息 = 离开选区任务：恢复常规写面（否则 bash 会被一直拦着）。
- +		// 放在 m.ask 之前：Ask 入队后 run 可能立刻开始，这里晚一步就有一个窗口。
- > 两个分支都要加（`CmdAskBatch` 同形）。这就是「选区守卫激活晚于 `Ask`」那条修法的落点：
- > 激活必须**先于** `Ask`（在 `onArtifactPageMessage` 的 commit 分支里 `set` →
- > `buildSelectionPrompt` → `Ask`），清除必须**先于**任何新的 `Ask`。
- **定位**：`desktop/bridge/main.go:954` 的 `case *events.AgentEnd:` 分支（在 `switch ev := e.(type)`
- 里，`bs.resetLive()` 之后）。
- **定位**：`desktop/bridge/main.go:3250`（`case CmdAsk:`）与 `3273`（`case CmdAskBatch:`）。
- /** 产物包（有则先渲染 BundlesBlock，再渲染下面的文件清单） */
- /** 这一轮的用时（毫秒）；没有就不画 */
- **定位**
- **定位**：`components/runtime/artifacts/ArtifactsBlock.tsx:108-121`（组件签名与内联 props 类型）。
- **定位**：同文件两处 —— `:126`（空判）与 `:150`（卡片根节点）。两处都要改，**只改渲染会让「本轮只有 bundle」的一轮整块消失**。
- // 只有 bundle、没有文件清单的一轮（publish 了此前已存在且本轮没改的入口）也必须画：
- // 原来只判 list.length，会让这种轮次整块消失（卡片没出现，用户以为没产出）。
- // …（total/add/del/wallText 的计算不动）…
- -- 以上不动 --
- // 收敛结果（BR-15）：在 updater 里算出、setState 之后写诊断
- // …（中间是既有的助手块冻结/工具行收尾/子卡片收敛，一行不动）…
- // 位置：useAppStore.ts 的事件分发 switch（`case 'command_response':` 同层，末尾 default 之前）。
- // 与既有 case 平级，不嵌进任何分支。
- // 回包：bridge 侧等待器按 id 唤醒；失败也回（否则调用方要等超时）
- // 位置：desktop/app/electron/main.ts，`embed:open-target` 注册之后（同一注册区）。
- // 全部是「隐藏窗口加载 URL → 取结果 → 销毁」，不复用主窗口视图池。
- TaskId:             opts.TaskId, // 与 AgentStart 同源：前端终态收敛兜底锚点
- **原文**
- **同一文件、同一函数**，在 `buildLoop` 组装系统提示处（`main.go:6483-6501`，`sysPrompt` 拼完
- MCP 段之后）追加：
- +	// 产物契约：只在产物工具真的注册成功时注入 —— 否则模型会被告知用不存在的工具。
- +	// 固定前缀（不随轮次/时间变化），prompt caching 不被打穿。
- > 为什么不用 `RunOptions.SystemExtra`：那个字段已经被 `session` 的 `systemExtraForRun`
- > 占用（`agents/agent_loop.go:949`），bridge 再覆盖会丢 SessionStart 注入；而这是
- > **loop 级、整会话稳定**的内容，放 `buildLoop` 才对（与 `computerPromptGuide` 同层）。
- +	// selectionScope 当前选区任务的作用域（nil = 无选区任务；见 selection_guard.go）。
- +	// selectionScope 选区写范围守卫（nil = 未启用）；并发安全由它自己的 mu 负责。
- // selectionScope 当前选区任务的作用域（nil = 无选区任务）。emit 的 AgentEnd
- // selectionScope 选区写范围守卫（nil = 未启用）。emit 的 AgentEnd 与 Ask 入口
- selectionScope *selectionScope // 并发安全由 selectionScope.mu 负责
- **同一文件、同一函数**，在构造 `RunOptions` 的地方（不是 loop）加系统提示：
- >           void openArtifactBundle(first.id, first.version!, sid)
- >           void getState().openArtifactPreview(first, { activate: false, quiet: true })
- > `openArtifactBundle` 是 `useAppStore.ts:5700` 一带新增的 action（§11.2 已给全文），
- > `openArtifactPreview` 是 §11.2 已给全文的 store action（`useAppStore.ts:5826` 一带），
- 组合函数见 §6.9。
- +- 以上三条不动（完整文本见 §6.9）
- **改法**：在 §6.9 的 `artifactPromptContract` 常量**末尾**（最后一条 bullet 之后、反引号之前）
- 追加两行，不新增常量、不改注入点：
- > 常量全文见 §6.9（这里只给追加的两行，避免同一段 prompt 在两处各维护一份）。
- // 与 Ask 入口都会清（见 selection_guard.go）。
- // 都会清（见 selection_guard.go）。
- // 这一行是**单行字面量**（现长度 ~600 字符）：按字段名插入，不要重排。
- **第二处**：同一函数末尾的 `bs = &bridgeSession{...}` 是一个**单行**字面量（现长约 600 字符，
- 字段由 `gofmt` 不换行）。不要重排它，只在 `s: s,` 之后插入一个字段：
- > 为什么不做成第二个 hunk：把 600 字符的行拆成多行会产生一片与本次改动无关的 diff，
- > review 时看不出真正加了什么。
- > 状态：**方案定稿，待评审**（2026-11-19）
- > 状态：**方案定稿，待评审**（2026-11-19；同日 v2 增 §21「代码变更全集」）
- 2. 每一处改动的**完整代码**（新文件全文 + 旧文件逐 hunk diff）；
- 2. 每一处改动的**完整代码**（新文件全文；旧文件逐处「定位 / 原文 / 变更后」，见 §21）；
- - 标「全文」的代码块是可直接落盘的完整文件（新文件为主）；
- - 标「核心代码/核心算法」的是关键函数与骨架，接口、结构体、错误路径已给全，样板代码（import 之外的空行/日志）按仓库风格补；
- - `diff` 的行号是 2026-11-19 基线，实施时以 hunk 里的函数名/相邻代码为准；
- - 所有新工具/新命令都配了「失败语义 + 测试用例」；没有测试的改动不应合并（§16.2）。
- - **§21 是实施清单**：每个修改点给出定位（函数 + 基线行号）/ 原文 / 变更后 / 落盘顺序。
- 前面各节的 diff 是设计意图的摘要，**与 §21 冲突时以 §21 为准**（已知修正：钩子路径归一、
- `filterNoiseArtifacts` 的死代码、`WithArtifactRun` 的真实签名、选区守卫的安装时机）；
- - `diff` 的行号是 2026-11-19 基线，实施时以函数名/相邻代码为准（§21 已核对过一轮真实锚点）；
- - 所有新工具/新命令都配了「失败语义 + 测试用例」；没有测试的改动不应合并（§16.2）；
- - §21.15 是编译关卡表：每一步都必须独立可编译，回滚粒度 = 一步。
- // 产物工具：与办公工具同一层（loop 建一次、kit/开关不可用就不注册）。
- // 注册失败不阻断：模型只是看不到这些工具，其余能力照常。
- // 产物工具：与办公工具同一层（每次 buildLoop 注册一次；开关关掉就不注册）。
- // 需要 sid：声明表与指标都要按会话归属（见 artifact_tools.go 的签名）。
- label: string // 例：button「立即购买」
- label: string // 例：button「立即购买」/ styles.css:40-60 / 整页
- // chip 上的类型标记：文案短、可翻译；图标复用既有（不新增一套 SVG）。
- element: '元素', elements: '多元素', text: '文字', comment: '批注',
- region: '区域', page: '整页', artifact: '产物', file: '文件',
- +	// 选区守卫：**每会话一个**（内部有锁 + active 标志，见 selection_guard.go）。
- +	// 为什么创建时就装好、而不是任务开始时再 SetToolGuard：selection.commit 可能在
- +	// run goroutine 已经启动之后才到（用户先发普通消息、再点选区），装晚一步，
- +	// 那道窗口里 bash 是放行的。常驻 + active=false 时零成本放行，没有窗口。
- // 这一行是**单行字面量**（main.go 现长度 ~600 字符）：按字段名插入，不要重排。
- +	// selectionScope 选区改稿的写范围守卫（nil = 未启用）。与 Session 共享同一对象：
- +	// Session.Run 里读它做 PreToolUse，bridge 在 selection.commit/AgentEnd 里改它。
- // 两个 handler 都是「隐藏窗口加载 URL → 取结果 → 销毁」，不复用主窗口视图池。
- // 拖拽：元素模式 = 框选（→ elements），区域模式 = 区域（→ region）。
- // 用 pointer 事件而不是 mouse：触控板/触屏一致；阈值 8px 用来区分「点击」与「拖拽」。
- suppressClick = true // 拖完的这一次 click 要丢掉，否则会把框选覆盖成单选
- // 元素模式：框选 = 区域内的节点，按文档序取前 20（LCA 只用于展示成组提示）
- let suppressClick = false // 拖拽结束后的那次 click 要丢弃
- > **分期**：Phase B1 支持 `element / elements / text / region / page / artifact / file` 七种；
- > `comment`（批注 pin）放 B2 —— 它需要 pin 的放置/编辑 UI 与「多条意见一次提交」的交互，
- > 与只读引用不是同一档工作量。
- **定位**：`desktop/bridge/main.go:4773-4785`（`case CmdDeleteSession:` 调 `m.deleteSession` 处）
- 或 `deleteSession` 函数体末尾（二选一，**不要两处都加**）。
- +		// 两张 per-session 表必须跟着删：长跑 bridge 会按会话无限增长。
- +		//   - artifactRuns：产物声明表（key = ws + "\x00" + sid）
- +		//   - selectionRefs：选区引用（30 分钟 TTL 之外还有会话级上限）
- > 为什么不在 `bridgeSession` 上挂 finalizer/GC：Go 的 sync.Map 没有弱引用语义，
- > 「会话对象没人引用」不会自动清 map 条目；显式删是唯一确定的做法。
- // Sweep 周期清理（挂在 revisit 清扫器上，与 artifact_sweep 同一个 tick）。
- // Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。
- // artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：
- // 内容寻址去重后版本不变时不重复 reload（避免每次都闪一下）。

# 附录 B · 仍缺失

| 缺失项 | 说明 |
|---|---|
| §1–§20 标题与正文 | 补丁脚本从未引用，无残留 |
| 调研对比（元文档） | 需你手上的元文档 |
| 段落级论证 | 只有单行散文 |
| 实现代码 | 文档内嵌代码块在，工作区代码未提交 |

---

# 附录 C · 整节恢复（脚本内联撰写的正文，逐字）

> 有 21 节（13,901 字节）不是通过 `rep(原块,新块)` 修订的，而是脚本用
> `lines.insert(...)` / `L[i] = '''…'''` **整节写入**的。上一轮只提取了 `rep()` 的两个参数，
> 因此这批内容被漏掉——现已补齐。以下按节号排序，标题即原文标题，正文逐字。

> 来源：`patch6.py` · 26 字节

### 21.1 新增文件（24 项，含测试与脚本）

> 来源：`patch_final.py` · 68 字节

### 21.10 `desktop/bridge/main.go`（11 处）+ `command_registry.go`（1 处）

> 来源：`patch_ref2.py` · 68 字节

### 21.10 `desktop/bridge/main.go`（12 处）+ `command_registry.go`（1 处）

> 来源：`patch14.py` · 60 字节

### 21.11 前端 store（`checkpoint.ts` 2 处、`useAppStore.ts` 6 处）

> 来源：`patch_review2.py` · 835 字节

### 21.16 `subagent/subagent.go`（1 处，产物声明表继承）

**定位**：`subagent/subagent.go:243-258`（`opts := &agents.RunOptions{Depth: childDepth, Name: name}` 与 `tc.Hooks` 继承块）。

```go
	opts := &agents.RunOptions{Depth: childDepth, Name: name}
	// …既有字段…
	if tc := events.ToolContextFrom(ctx); tc != nil {
		// 既有：hooks 继承（DisableHookInherit 可关）
		if tc.Hooks != nil && !t.spec.DisableHookInherit {
			opts.PreToolUse = tc.Hooks.PreToolUse
			opts.PostToolBatch = tc.Hooks.PostToolBatch
			opts.PreCompact = tc.Hooks.PreCompact
		}
+		// 产物声明表必须继承：否则子 agent 的 artifact_publish 拿到 nil，
+		// 声明直接丢弃（子卡片永远没有产物包）。与 Hooks 不同，它不受
+		// DisableHookInherit 影响 —— 那是「用户 hooks 不要传染给子 agent」，
+		// 而声明表是**同一次交付**的一部分。
+		opts.ArtifactRun = tc.ArtifactRun
	}
```

> 子 run 的 `AgentEnd` 带着自己的 `RunId`，`Take(subRunId)` 取到的是它自己的声明；
> 父 run 的声明不受影响（表按 RunId 分桶）。

> 来源：`patch_review2.py` · 32 字节

## 22. CodeMode 支持（脚本内调用这 9 个工具）

> 来源：`add_kinds.py` · 1,900 字节

### 23.1b 引用类型：节点级 / 区域级 / 整体级（8 种）

「引用」不是只有「点一个元素」。按**选择粒度**分三层，共 8 种 `kind`——每一种都对应
设计稿 §「选择与改稿技术规范」里的一个交互，只是这里只取「引用」这一半（不直接改稿）。

| 层 | kind | 用户动作 | 选择范围 | payload 关键字段 | 模型拿到什么 |
|---|---|---|---|---|---|
| **节点级** | `element` | 点选一个元素 | 单个节点（语义提升后） | `selector, tag, text(≤300), html(≤2KB), rect, styles, viewport, theme` | 一个节点 + 它长什么样 |
| | `elements` | Shift 追加 / 框选 | 2–20 个节点（LCA 判断成组） | `selector, items[{selector,tag,text,rect}], truncated` | 一组节点 + 共同祖先 |
| | `text` | 选中文字 → 「引用」 | 一个 `Range` | `anchor{selector,tag}, text, range{start,end}, context{before,after}` | 一段文字 + 它属于哪个节点 |
| | `comment` | 批注 pin + 写意见 | 锚点（节点或矩形） | `anchor{selector|rect}, text` | 一条意见 + 锚点位置 |
| **区域级** | `region` | 拖矩形 / 截图上画框 | 一块屏幕区域（**不要求有 DOM**） | `rect{x,y,w,h}, elements[{selector,tag}](≤8), shot_id?, viewport` | 区域坐标 + 区域内采样到的候选元素；**没有单一节点** |
| **整体级** | `page` | 「引用整页」 | 当前版本的一整页 | `entry, title, viewport, shot_id?` | 整个页面（入口 + 版本） |
| | `artifact` | 从产物卡 / 列表引用 | 整个交付物（可多文件） | `entry, files[≤50], latest_version` | 「这一版交付了什么」 |
| | `file` | 源码面板 / 文件树引用 | 一个源文件或行区间 | `path, line_range?, sha256?` | 源码位置，不经过预览 |

**边界与降级**（决定「用户选不到元素时会发生什么」）：

| 场景 | 结果 |
|---|---|
| canvas 内部、跨域 iframe、closed shadow DOM | `element` 选不到 → 自动落到 `region`（rect + `elementsFromPoint` 采样） |
| 框选跨多个组件 | `elements`（≤20，超出截断并 `truncated:true`）；不猜「组件边界」 |
| 用户只选了半行文字 | `text`（带 `range`）；**不**升级成整节点 |
| 页面还没打开预览（只有版本库） | `page` / `artifact` / `file` 仍可引用（不需要渲染） |
| 预览是旧版本 | ref 里带 `version`；发送时校验，过期走 §23.5 |

**明确不引用**（避免把上下文撑爆，也避免把不可信内容当指令）：

- 完整 `outerHTML` 树、全部 computed styles、console 日志、整页截图本体（只存 `shot_id`，
  模型需要时调 `preview_screenshot`）；
- `region` 的截图**不进消息**：它只是「空间提示」，真正的像素由工具按需取。

**与「整体」有关的两个易混点**：

- `page` ≠ `artifact`：前者是**渲染结果**（一个版本的一页），后者是**交付物**（可能多文件、
  多入口）。引用 `page` 时模型会去读入口源码；引用 `artifact` 时模型先看文件清单。
- `file` 可以脱离预览：用户在源码面板里引用 `styles.css:40-60` 也是合法的对话引用，
  此时不需要 `artifact_open`。

> 来源：`add_inspector.py` · 3,719 字节

### 23.1c Inspector：四种取选模式（对 §8.5 的增量）

§8.5 的 inspector 只做了「点选单个元素」。要支撑 §23.1b 的 8 种 kind，浮动条上需要
**四个模式**（其中两个是自动的）：

| 模式 | 进入方式 | 产出 kind |
|---|---|---|
| 元素（默认） | 直接点 | `element`；Shift 点 = `elements` |
| 区域 | 工具条「区域」或按住 `R` 拖 | `region` |
| 文字 | 直接拖选文本（自动，无需切模式） | `text` |
| 整页 | 工具条「整页」 | `page` |

**增量代码**（接在 §8.5 的 `infoOf` / 事件绑定之后；保持同样的 `post()` 与 Shadow DOM 高亮层）：

```js
// —— 模式状态：'element' | 'region'；text 由 selectionchange 自动识别 ——
let mode = 'element'
let picked = []          // Shift 多选：按点选顺序（语义提升后的节点）
let dragRect = null      // 区域拖拽中的矩形（视口坐标）

// 工具条追加两个按钮与一个提示位（沿用 .bar 的样式）
const tools = document.createElement('div')
tools.className = 'row'
tools.innerHTML = '<button class="ghost" data-mode="element">元素</button>' +
  '<button class="ghost" data-mode="region">区域</button>' +
  '<button class="ghost" data-mode="page">整页</button>' +
  '<span class="hint"></span>'
bar.prepend(tools)
tools.addEventListener('click', (e) => {
  const m = e.target && e.target.getAttribute('data-mode')
  if (m === 'page') { void post('selection.commit', commitPayload('page', pagePayload())) ; return }
  if (m) { mode = m; tools.querySelectorAll('button').forEach((b) => b.classList.toggle('on', b.getAttribute('data-mode') === m)) }
})

// 元素点选：Shift 追加；普通点选清空重来
document.addEventListener('click', (e) => {
  if (mode !== 'element') return
  const el = lift(e.target)
  if (!el) return
  e.preventDefault(); e.stopPropagation()
  if (e.shiftKey) {
    if (!picked.includes(el) && picked.length < 20) picked.push(el)
  } else {
    picked = [el]
  }
  drawPicked(picked)
  showBar(picked[picked.length - 1])
})

// 区域拖拽：mousedown 记起点，mousemove 画框，mouseup 收口。
// 用 pointer 事件而不是 mouse：触控板/触屏一致。
addEventListener('pointerdown', (e) => {
  if (mode !== 'region' || e.button !== 0) return
  dragRect = { x: e.clientX, y: e.clientY, w: 0, h: 0 }
  e.preventDefault()
}, true)
addEventListener('pointermove', (e) => {
  if (!dragRect) return
  dragRect.w = e.clientX - dragRect.x
  dragRect.h = e.clientY - dragRect.y
  drawRect(dragRect)
}, true)
addEventListener('pointerup', (e) => {
  if (!dragRect) return
  const r = normalizeRect(dragRect)
  dragRect = null
  if (r.w < 8 || r.h < 8) return // 误触：忽略
  current = { __region: r, __elements: elementsIn(r) }
  showBar(null)
  void post('selection.changed', regionPayload(r))
}, true)

// 文字选区：selectionchange 非折叠时提示「引用这段文字」。
document.addEventListener('selectionchange', () => {
  const sel = getSelection()
  if (!sel || sel.isCollapsed || !sel.toString().trim()) return
  const range = sel.getRangeAt(0)
  const anchor = lift(range.startContainer.parentElement)
  textPick = { text: sel.toString().slice(0, 300), anchor, range: rangeOf(range), context: contextOf(range) }
  tools.querySelector('.hint').textContent = '引用这段文字？点「引用」'
})

// —— payload 构造：每种 kind 只带 §23.1b 列出的字段，绝不多带 ——
function payloadFor(kind) {
  if (kind === 'region') return regionPayload(current.__region)
  if (kind === 'page') return pagePayload()
  if (kind === 'text') return { ...textPick, anchor: infoOf(textPick.anchor) }
  if (kind === 'elements') return { selector: selectorOf(picked[0]), items: picked.map(infoOf), truncated: picked.length >= 20 }
  return infoOf(picked[0])
}

// 提交：两个动作都带 kind；「引用」= reference，「发送」= edit
function commitPayload(kind, payload) {
  return { kind, artifact_id: ARTIFACT_ID, version: VERSION, selection: payload,
           mode: mode === 'region' ? 'reference' : 'reference' }
}
```

> 高亮层与 `selectorOf` / `infoOf` 全部复用 §8.5，不新写一套。`drawPicked` 只是把
> 多个高亮框一起画；`drawRect` 画区域框。
>
> **`document.addEventListener('click')` 要保留 capture 阶段的 `preventDefault`**
>（设计稿要求「点选不触发页面自己的跳转/提交」）：上面的示例省略了 `true` 参数是为了可读，
> 实现时必须与原 §8.5 的那一版合并成**一个** listener，不要绑两次。

> 来源：`add_inspector.py` · 58 字节

### 23.2 服务端选区引用表（`desktop/bridge/selection_refs.go`，新增全文）

> 来源：`add_handler.py` · 3,572 字节

### 23.2b 命令入口：`handleArtifactSelection`

**定位**：`desktop/bridge/artifact_commands.go`（§8.7 的命令族），由 `main.go` 的
`case CmdArtifactSelection:` 调用。

```go
// handleArtifactSelection 预览页的两种提交：
//   mode=reference → 只存一条引用，返回 refId（前端放 chip）；
//   mode=edit      → 校验空闲 → 激活写守卫 → 立刻 Ask 一条改稿任务。
//
// 为什么两种模式都先落 ref：直接改也要能被 Composer 记住（用户在等模型时可以再引用一次）；
// 引用表是**唯一**的选区事实源，页面/渲染层都不保留副本。
func (m *manager) handleArtifactSelection(c command, ws, sid string) {
	bs := m.get(ws, sid)
	if bs == nil {
		m.respCode(c, false, "session not found", "session_not_found", nil)
		return
	}
	id := str(c.Payload, "artifact_id")
	version := str(c.Payload, "version")
	if id == "" || version == "" {
		m.resp(c, false, "artifact_id and version are required", nil)
		return
	}
	sel, err := json.Marshal(c.Payload["selection"])
	if err != nil || len(sel) == 0 || len(sel) > selectionRefMaxBytes {
		m.resp(c, false, "selection payload missing or too large", nil)
		return
	}
	// 版本必须真实存在：页面是模型生成的，它报的 version 不可信；这也是「过期引用」
	// 唯一能确定性拒绝的地方（版本被清理 → 这里 404，而不是发出去让模型猜）。
	v, err := m.artifacts.Version(ws, id, version)
	if err != nil {
		m.respCode(c, false, "selection target version not found", "selection_ref_stale",
			map[string]any{"artifact_id": id})
		return
	}
	ref := &selectionRef{
		WS: ws, SID: sid, Artifact: id, Version: version, Entry: v.Manifest.Entry,
		Payload: sel, Label: selectionLabel(id, v.Manifest.Entry, sel),
	}
	refID, err := m.selectionRefs.Put(ref)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	mode := str(c.Payload, "mode")
	if mode == "" || mode == "reference" {
		m.resp(c, true, "", map[string]any{
			"ref_id": refID, "label": ref.Label, "artifact_id": id,
			"version": version, "selector": selectorOf(sel),
		})
		return
	}
	if mode != "edit" {
		m.resp(c, false, "unknown mode: "+mode, nil)
		return
	}
	// 直接改：忙就拒绝（scope 是会话级，激活会把正在跑的 run 一起拦掉，见 §24 R18）。
	if bs.s.IsRunning() {
		m.respCode(c, false, "session is busy; wait for the current run to finish",
			"session_busy", nil)
		return
	}
	// 顺序不能换：先 set（守卫生效）→ 再 Ask。反过来会留一个「run 已启动、守卫还没生效」的窗口。
	bs.selectionScope.set(id, v.Manifest.Entry, manifestPaths(v))
	instruction := strings.TrimSpace(str(c.Payload, "instruction"))
	if instruction == "" {
		instruction = "Apply the requested change to the selected element."
	}
	if err := bs.s.Ask(context.Background(), buildSelectionMessage(instruction, []*selectionRef{ref})); err != nil {
		bs.selectionScope.clear()
		m.resp(c, false, err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"ref_id": refID, "started": true})
}

// manifestPaths 版本内全部文件路径（顺序稳定）：写守卫的允许集合。
func manifestPaths(v *artifactVersion) []string {
	out := make([]string, 0, len(v.Manifest.Files))
	for _, f := range v.Manifest.Files {
		out = append(out, f.Path)
	}
	return out
}

// selectorOf / selectionLabel 只从已裁剪的 selection JSON 里取展示字段，不解析 DOM。
func selectorOf(raw json.RawMessage) string {
	var s struct {
		Selector string `json:"selector"`
	}
	_ = json.Unmarshal(raw, &s)
	return s.Selector
}

func selectionLabel(id, entry string, raw json.RawMessage) string {
	var s struct {
		Tag  string `json:"tag"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &s)
	tag := s.Tag
	if tag == "" {
		tag = filepath.Base(entry)
	}
	t := strings.TrimSpace(s.Text)
	if len([]rune(t)) > 24 {
		t = string([]rune(t)[:24]) + "…"
	}
	if t == "" {
		return fmt.Sprintf("%s · %s", id, tag)
	}
	return fmt.Sprintf("%s · %s「%s」", id, tag, t)
}
```

> `selection.changed`（悬停/点选）**不**落 ref：它每次点选都发，落表会把 20 条上限瞬间打满。
> 只有 `selection.commit`（用户按了「引用」或「直接改」）才进表。

> 来源：`add_handler.py` · 37 字节

### 23.3 发送时注入：`<selection>` 是数据，不是指令

> 来源：`patch_ref2.py` · 1,094 字节

#### E-main.go-12：`CmdAsk` / `CmdAskBatch` 解析 `selection_refs`

**定位**：`desktop/bridge/main.go:3250`（`case CmdAsk:` 的 `text := str(c.Payload, "text")` 之后）
与 `3273`（`CmdAskBatch` 同形）。

```go
	case CmdAsk:
		// …既有 get/忙检查…
		text := str(c.Payload, "text")
		refs, err := m.selectionRefs.Resolve(ws, sid, strSlice(c.Payload, "selection_refs"))
		if err != nil {
			m.respCode(c, false, err.Error(), "selection_ref_invalid", nil)
			return
		}
		// 有引用时：指令在前、选区在后，拼成**一条**用户消息（§23.3）。
		msg := buildSelectionMessage(text, refs)
		if err := bs.s.Ask(ctx, msg); err != nil { … }
```

`strSlice` 是既有 payload 取值族的补充（`command_registry.go` 或 `main.go` 的 str/int/bool 旁边）：

```go
// strSlice 读 payload 里的字符串数组；缺失/类型不符一律返回 nil（调用方按「没传」处理）。
func strSlice(p map[string]any, key string) []string {
	raw, ok := p[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
```

> `CmdAskBatch` 的 payload 是 `messages` 数组：**第一条**消息带引用（用户一次只能选一次），
> 其余消息不带。不要给每条都拼同一份选区（模型会以为用户选了多次）。

> 来源：`patch14.py` · 1,049 字节

#### E-main.go-9：`emit` 清选区作用域（AgentEnd）

**定位**：`desktop/bridge/main.go:764-800` 的 `switch ev := e.(type)` 里 `case *events.AgentEnd:`
分支（`bs.resetLive()` 之后）。

```go
	case *events.AgentEnd:
		bs.resetLive()
+		// 选区任务结束：恢复常规写面。放在 emit（而不是 publishArtifactBundles）里，
+		// 因为它必须对所有结束路径生效：正常完成 / 中断 / 失败 / 子运行收尾。
+		if bs.selectionScope != nil {
+			bs.selectionScope.clear()
+		}
```

#### E-main.go-10：`CmdAsk` / `CmdAskBatch` 清选区作用域

**定位**：`desktop/bridge/main.go:3600` 一带（`case CmdAsk:` 与 `case CmdAskBatch:` 两个分支）。

```go
	case CmdAsk:
		bs := m.get(ws, sid)
		if bs == nil {
			m.resp(c, false, "session not found", nil)
			return
		}
+		// 用户手动发消息 = 离开选区任务：恢复常规写面（否则 bash 会被一直拦着）。
+		// 放在 m.ask 之前：Ask 入队后 run 可能立刻开始，这里晚一步就有一个窗口。
+		if bs.selectionScope != nil {
+			bs.selectionScope.clear()
+		}
		m.ask(bs, core.Message{Role: "user", Content: text}, ...)
```

> 两个分支都要加（`CmdAskBatch` 同形）。这就是「选区守卫激活晚于 `Ask`」那条修法的落点：
> 激活必须**先于** `Ask`（在 `onArtifactPageMessage` 的 commit 分支里 `set` →
> `buildSelectionPrompt` → `Ask`），清除必须**先于**任何新的 `Ask`。

> 来源：`patch13.py` · 603 字节

#### E-bridge.ts-1：渲染进程的 IPC 封装

**定位**：`desktop/app/src/lib/bridge.ts` 末尾（与 `embedOpenTarget` 同族）。

```ts
// 产物预览/自检：渲染进程只拿 URL 与结果，不碰 Chromium。
export const artifactVerify = (url: string) =>
  window.hai.artifact.verify({ url, sid: useAppStore.getState().activeSessionId ?? '' })
export const artifactScreenshot = (url: string, opts?: { fullPage?: boolean }) =>
  window.hai.artifact.screenshot({ url, opts })
export const artifactInspect = (url: string, selector: string) =>
  window.hai.artifact.inspect({ url, selector })
```

> `embed.ts` **不改**：`createEntry` / `openEmbedTarget` 已有 `srcPath` 去重语义

> 来源：`patch_final.py` · 567 字节

#### E-main.go-11：会话删除时清两张表

**定位**：`desktop/bridge/main.go:4773-4785`（`case CmdDeleteSession:` 调 `m.deleteSession` 处）
或 `deleteSession` 函数体末尾（二选一，**不要两处都加**）。

```go
		m.deleteSession(ws, sid)
+		// 两张 per-session 表必须跟着删：长跑 bridge 会按会话无限增长。
+		//   - artifactRuns：产物声明表（key = ws + "\x00" + sid）
+		//   - selectionRefs：选区引用（30 分钟 TTL 之外还有会话级上限）
+		m.artifactRuns.Delete(ws + "\x00" + sid)
+		if m.selectionRefs != nil {
+			m.selectionRefs.Drop(ws, sid)
+		}
```

> 为什么不在 `bridgeSession` 上挂 finalizer/GC：Go 的 sync.Map 没有弱引用语义，
> 「会话对象没人引用」不会自动清 map 条目；显式删是唯一确定的做法。

> 来源：`patch2.py` · 46 字节

#### E-agents/agent_loop.go-5：收尾合并快照 + 声明 + 分类

> 来源：`patch16.py` · 42 字节

#### E-ArtifactsBlock.tsx-1：`bundles` prop

> 来源：`patch16.py` · 38 字节

#### E-Narrative.tsx-1：把 bundles 传进产出卡

> 来源：`patch11.py` · 34 字节

#### E-command_registry.go-1：命令名常量

> 来源：`patch16.py` · 32 字节

#### E-ArtifactsBlock.tsx-2：渲染位置

> 来源：`patch11.py` · 21 字节

#### E-main.go-7：命令分发

---

# 附录 D · 无标题片段索引（121, 30 KB）

> 这些是零散的代码/散文块（不含标题），全文见 `ARTIFACT_SECTIONS_EXTRA.md`。

| # | 来源脚本 | 字节 | 首行 |
|---|---|---|---|
| 1 | `patch_kind2.py` | 1,384 | `kind := str(c.Payload, "kind")` |
| 2 | `patch_drag.py` | 1,007 | `// 拖拽：元素模式 = 框选（→ elements），区域模式 = 区域（→ region）。` |
| 3 | `patch4.py` | 921 | ````diff` |
| 4 | `patch_kind2.py` | 839 | `id := str(c.Payload, "artifact_id")` |
| 5 | `add_mig.py` | 835 | `**§9 各工具要跟着改的段落**（不改这些，base 就是死代码）：` |
| 6 | `patch2.py` | 789 | `artifacts, artifactsTruncated := collector.collect()` |
| 7 | `patch_pub.py` | 780 | `start := time.Now()` |
| 8 | `patch_guard.py` | 733 | `case "bash", "run_python":` |
| 9 | `patch_drag.py` | 719 | `// 区域拖拽：mousedown 记起点，mousemove 画框，mouseup 收口。` |
| 10 | `patch3.py` | 710 | `**同一文件、同一函数**，在 `buildLoop` 组装系统提示处（`main.go:6483-6501`，`sysPrompt` 拼完` |
| 11 | `patch16.py` | 706 | `if (b.kind === 'artifacts') {` |
| 12 | `patch11.py` | 681 | `// —— 产物（预览/版本/选区/导出）——` |
| 13 | `fix_nested.py` | 663 | `sel, err := json.Marshal(c.Payload["selection"])` |
| 14 | `patch_review2.py` | 632 | `激活前必须确认会话**空闲**（否则会把正在跑的那条 run 一起拦掉）：` |
| 15 | `patch_iface.py` | 599 | ````go` |
| 16 | `patch_doc.py` | 594 | `func (m *manager) newSession(ws *workspaceRuntime, id, restoreID strin` |
| 17 | `patch_doc.py` | 586 | `// 位置：desktop/app/electron/main.ts，`embed:open-target` 注册之后（同一注册区）。` |
| 18 | `fix_nested.py` | 569 | `// file 可以脱离 artifact（源码面板引用工作区文件）；其余类型必须有版本 ——` |
| 19 | `patch_review2.py` | 556 | `\| R16 \| **P2** \| §22 \| 工具名/结果形状没有 CodeMode 契约 \| 新增 §22 整章 \|` |
| 20 | `patch5.py` | 544 | ````diff` |
| 21 | `patch_ref2.py` | 531 | `// Resolve 取一组 refId（发送时调用）：逐条校验属主/过期，并保持用户点选的顺序。` |
| 22 | `patch11.py` | 528 | `case CmdArtifactOpen:` |
| 23 | `patch_chip.py` | 502 | `type SelectionKind = 'element' \| 'elements' \| 'text' \| 'comment' \| 're` |
| 24 | `patch17.py` | 460 | `// 只有 bundle、没有文件清单的一轮（publish 了此前已存在且本轮没改的入口）也必须画：` |
| 25 | `patch_final.py` | 432 | `\| S11 \| N19 + E-session guard（已在 S4）+ E-main.go-7 的 selection 分支 + N16` |
| 26 | `patch4.py` | 418 | ````diff` |
| 27 | `patch16.py` | 417 | `export function ArtifactsBlock({` |
| 28 | `patch5.py` | 407 | `**改法**：在 §6.9 的 `artifactPromptContract` 常量**末尾**（最后一条 bullet 之后、反引号之前` |
| 29 | `patch_chip.py` | 402 | `\| `TestSelectionRefDedup` \| 同 artifact+selector+version 两次引用 → 一枚 chip` |
| 30 | `patch_kind2.py` | 388 | `func selectionLabel(kind, id, entry string, raw json.RawMessage) strin` |
| 31 | `patch4.py` | 374 | `const artifactPromptContract = `## Artifacts` |
| 32 | `patch8.py` | 355 | `- 标「全文」的代码块是可直接落盘的完整文件（新文件为主）；` |
| 33 | `patch_kind2.py` | 352 | `for _, r := range refs {` |
| 34 | `patch10.py` | 346 | `// 产物归档必须在 reducer.Apply 之前：AgentEnd 进 checkpoint 时 bundle 必须已带` |
| 35 | `patch_doc.py` | 344 | `@@ -341,6 +341,10 @@ type bridgeSession struct {` |
| 36 | `patch_meta.py` | 327 | `\| 协作（分享/评论/共编） \| **后置到 Phase D，且先做 handoff 包** \| 本仓是本地单机优先；实时共编需要服务端与 ` |
| 37 | `patch10.py` | 324 | `// 产物归档必须在 reducer.Apply 之前：AgentEnd 进 checkpoint 时 bundle 必须已带` |
| 38 | `patch_iface.py` | 316 | ````go` |
| 39 | `patch_final.py` | 305 | `\| N14c \| `desktop/bridge/artifact_toolbase.go` \| `artifactToolBase` / ` |
| 40 | `patch_kind2.py` | 298 | `for _, r := range refs {` |
| 41 | `patch_final.py` | 278 | `// Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。` |
| 42 | `patch_guard.py` | 248 | `if s.files[p] {` |
| 43 | `patch_pub.py` | 241 | `// artifactSweeper 版本库周期清扫（启动 + 每小时；见 artifact_sweep.go）。` |
| 44 | `patch16.py` | 233 | `return (` |
| 45 | `patch_final.py` | 231 | `// artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：` |
| 46 | `patch7.py` | 223 | ````` |
| 47 | `patch7.py` | 199 | `@@` |
| 48 | `patch_drag.py` | 199 | `> **分期**：Phase B1 支持 `element / elements / text / region / page / arti` |
| 49 | `patch8.py` | 198 | `- 标「全文」的代码块是可直接落盘的完整文件（新文件为主）；` |
| 50 | `patch_chip.py` | 196 | `<span className="sel-kind">{KIND_LABEL[s.kind]}</span>` |
| 51 | `patch_doc.py` | 193 | `// 这一行是**单行字面量**（main.go 现长度 ~600 字符）：按字段名插入，不要重排。` |
| 52 | `patch9.py` | 182 | `m.registerOfficeTools(eng, ws.path)` |
| 53 | `patch_drag.py` | 172 | `document.addEventListener('click', (e) => {` |
| 54 | `patch_kind2.py` | 170 | `func selectionLabel(id, entry string, raw json.RawMessage) string {` |
| 55 | `patch_meta.py` | 166 | `\| `go.mod` / `go.sum` \| +1 模块 \| A1 \| §21.14 \|` |
| 56 | `patch11.py` | 164 | `+	CmdArtifactSelection Command = "artifact_selection" // 选区任务激活/取消（Pha` |
| 57 | `patch4.py` | 160 | `const artifactPromptContract = `## Artifacts` |
| 58 | `patch_meta.py` | 158 | `> 计数口径：**只算生产文件**（不含 `_test.go` / e2e / 评估脚本，那些在 §17 单列）。` |
| 59 | `patch9.py` | 154 | `m.registerOfficeTools(eng, ws.path)` |
| 60 | `patch_guard.py` | 153 | `case "bash", "run_python":` |
| 61 | `patch_drag.py` | 147 | `\| 元素（默认） \| 直接点；Shift 点追加；**拖框** \| `element` / `elements`（拖框 = 框选，按 LCA` |
| 62 | `patch_final.py` | 144 | `// artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：` |
| 63 | `patch_final.py` | 140 | `\| S11 \| N19 + E-session guard（已在 S4）+ E-main.go-7 的 selection 分支 + N16` |
| 64 | `patch11.py` | 139 | `\| N14b \| `desktop/bridge/artifact_commands.go` \| `handleArtifactOpen/L` |
| 65 | `patch_guard.py` | 136 | `if s.files[p] {` |
| 66 | `patch_chip.py` | 135 | `type SelectionRefItem = {` |
| 67 | `patch_pub.py` | 133 | `start := time.Now()` |
| 68 | `patch_final.py` | 123 | `m.artifactPreview = newArtifactPreviewHost(m.artifacts, m.onArtifactPa` |
| 69 | `patch_chip.py` | 119 | `<span className="doc-chip-name">{s.label}</span>` |
| 70 | `patch11.py` | 114 | `+	case CmdArtifactSelection:` |
| 71 | `patch_doc.py` | 98 | `func (m *manager) newSession(ws *workspaceRuntime, id, restoreID strin` |
| 72 | `patch11.py` | 96 | `\| N14 \| `desktop/bridge/artifact_embed.go` \| `//go:embed artifact_insp` |
| 73 | `patch_pub.py` | 96 | `// artifactSweeper 版本库周期清扫（启动 + 每小时；见 artifact_sweep.go）。` |
| 74 | `patch_drag.py` | 90 | `let dragRect = null      // 区域拖拽中的矩形（视口坐标）` |
| 75 | `patch_drag.py` | 86 | `\| 元素（默认） \| 直接点 \| `element`；Shift 点 = `elements` \|` |
| 76 | `patch_meta.py` | 86 | `> 状态：**方案定稿，待评审**（2026-11-19；v2 增 §21 代码变更全集；v3 增 §22 CodeMode、§23 选区引` |
| 77 | `patch_meta.py` | 85 | `\| 协作（分享/评论/共编） \| **后置到 Phase D，且先做 handoff 包** \| 本仓是本地单机优先；实时共编需要服务端与 ` |
| 78 | `patch17.py` | 83 | `**定位**：`components/runtime/artifacts/ArtifactsBlock.tsx:108-121`（组件签名与` |
| 79 | `patch_final.py` | 81 | `m.artifactPreview = newArtifactPreviewHost(m.artifacts, m.onArtifactPa` |
| 80 | `patch11.py` | 80 | `+	CmdHostReply      Command = "host_reply"      // 宿主 RPC 回包（preview.s` |
| 81 | `patch17.py` | 79 | `**定位**：同文件两处 —— `:126`（空判）与 `:150`（卡片根节点）。两处都要改，**只改渲染会让「本轮只有 bundle」的` |
| 82 | `patch_drag.py` | 76 | `document.addEventListener('click', (e) => {` |
| 83 | `add_mig.py` | 75 | `每个工具改一行：`Call` 的返回从 `fmt.Sprintf(...)` 换成 `return t.emit(ctx, result, ` |
| 84 | `patch_chip.py` | 72 | `\| `TestSelectionRefDedup` \| 同 artifact+selector+version 两次引用 → 一枚 chip` |
| 85 | `patch3.py` | 65 | `+	// selectionScope 当前选区任务的作用域（nil = 无选区任务；见 selection_guard.go）。` |
| 86 | `patch3.py` | 62 | `// selectionScope 选区写范围守卫（nil = 未启用）。emit 的 AgentEnd 与 Ask 入口` |
| 87 | `patch3.py` | 61 | `selectionScope *selectionScope // 并发安全由 selectionScope.mu 负责` |
| 88 | `patch_doc.py` | 59 | `+	// selectionScope 选区改稿的写范围守卫（nil = 未启用）。与 Session 共享同一对象：` |
| 89 | `patch_review2.py` | 59 | `\| R16 \| **P2** \| §22 \| 工具名/结果形状没有 CodeMode 契约 \| 新增 §22 整章 \|` |
| 90 | `patch3.py` | 58 | `// selectionScope 当前选区任务的作用域（nil = 无选区任务）。emit 的 AgentEnd` |
| 91 | `patch_meta.py` | 58 | `\| A 基座（检测/判定/版本/预览/产出卡） \| 18 \| 17 \| ~4,600 行 \| 是（4 个 PR） \|` |
| 92 | `patch_final.py` | 57 | `// Sweep 周期清理（挂在 revisit 清扫器上，与 artifact_sweep 同一个 tick）。` |
| 93 | `patch_meta.py` | 57 | `> 计数口径：**只算生产文件**（不含 `_test.go` / e2e / 评估脚本，那些在 §17 单列）。` |
| 94 | `patch3.py` | 55 | `+	// selectionScope 选区写范围守卫（nil = 未启用）；并发安全由它自己的 mu 负责。` |
| 95 | `patch6.py` | 53 | `-	bs = &bridgeSession{id: id, runtime: ws, s: s, ...}` |
| 96 | `patch8.py` | 52 | `2. 每一处改动的**完整代码**（新文件全文；旧文件逐处「定位 / 原文 / 变更后」，见 §21）；` |
| 97 | `patch_final.py` | 52 | `\| `desktop/bridge/main.go` \| 11 \| A3/A4/B \| §21.10 \|` |
| 98 | `patch_ref2.py` | 52 | `\| `desktop/bridge/main.go` \| 12 \| A3/A4/B \| §21.10 \|` |
| 99 | `patch3.py` | 49 | `**同一文件、同一函数**，在构造 `RunOptions` 的地方（不是 loop）加系统提示：` |
| 100 | `patch6.py` | 49 | `-	bs = &bridgeSession{id: id, runtime: ws, s: s,` |
| 101 | `patch8.py` | 49 | `> 状态：**方案定稿，待评审**（2026-11-19；同日 v2 增 §21「代码变更全集」）` |
| 102 | `patch_final.py` | 48 | `\| N14b \| `desktop/bridge/artifact_commands.go` \|` |
| 103 | `patch3.py` | 47 | `+	selectionScope atomic.Pointer[selectionScope]` |
| 104 | `patch6.py` | 47 | `@@ -512,6 +512,9 @@ type bridgeSession struct {` |
| 105 | `patch3.py` | 46 | `selectionScope atomic.Pointer[selectionScope]` |
| 106 | `patch_doc.py` | 45 | `bridgeSession{id: id, runtime: ws, s: s, ...}` |
| 107 | `add_handler.py` | 44 | `docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md` |
| 108 | `patch11.py` | 44 | `+	case CmdHostReply:` |
| 109 | `patch8.py` | 42 | `2. 每一处改动的**完整代码**（新文件全文 + 旧文件逐 hunk diff）；` |
| 110 | `patch_drag.py` | 42 | `let dragRect = null      // 区域拖拽中的矩形（视口坐标）` |
| 111 | `patch_meta.py` | 41 | `\| B 选区改稿 \| 5 \| 9 \| ~1,800 行 \| 是（3 个 PR） \|` |
| 112 | `patch_meta.py` | 41 | `\| B 选区改稿 \| 6 \| 9 \| ~2,000 行 \| 是（3 个 PR） \|` |
| 113 | `patch_ref2.py` | 40 | `// Drop 会话删除时清掉它的全部引用（引用是会话级状态，没有跨会话价值）。` |
| 114 | `patch6.py` | 38 | `// 与 Ask 入口都会清（见 selection_guard.go）。` |
| 115 | `patch9.py` | 34 | `+	if m.artifactsEnabled(ws.path) {` |
| 116 | `patch_drag.py` | 33 | `**明确不引用**（避免把上下文撑爆，也避免把不可信内容当指令）：` |
| 117 | `patch_review2.py` | 33 | `AgentEnd 清 scope（`bs.emit` diff）：` |
| 118 | `patch3.py` | 32 | `+	selectionScope *selectionScope` |
| 119 | `patch6.py` | 30 | `// 都会清（见 selection_guard.go）。` |
| 120 | `patch8.py` | 29 | `> 状态：**方案定稿，待评审**（2026-11-19）` |
| 121 | `patch9.py` | 27 | `+	if m.artifactsEnabled() {` |
