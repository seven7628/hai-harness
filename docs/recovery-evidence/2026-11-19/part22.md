## 22. CodeMode 支持（脚本内调用这 9 个工具）

> 结论先说：**9 个工具默认都能在 codemode 脚本里调**（`ExposureDirect` = 可声明 + 可编排），
> 但「能调用」不等于「好用」。要让脚本真的用得起来，必须补三件事：**名字合法**、
> **结果结构化**、**语义分层（脚本路径 vs 模型路径）**。这一章给的就是这三件事的落点。

### 22.1 命名：线上名一律下划线

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

### 22.2 暴露档位、注解与「可编排」广告

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

### 22.3 结构化结果：脚本拿到对象，不是 JSON 字符串

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

### 22.4 脚本路径 vs 模型路径（隐式差异必须写进设计）

| 维度 | 模型直调（Depth=0） | 脚本内调用（Depth>0） | 对本方案的影响 |
|---|---|---|---|
| 结果截断 | 20 KB 头尾截断 + 中段落盘 | ≤1 MiB 零损耗；超限整条落盘 | `source_get` 的 16KB 上限在脚本里**不是**引擎截断，是工具自己的契约 |
| 结构化结果 | 不消费 sink | 消费 sink → 对象 | §22.3 的 `emit` |
| 图片 | 进 ImageSink → 图片块 | **脚本拿不到图片** | §22.5 截图降级 |
| 审批 | `runBatch → preApprove` | `ExecuteOne → executeApproved → preApprove`（逐次） | 有 `ApprovalRequired` 的工具在脚本里会**弹窗**；artifact 工具都不声明审批 |
| PreToolUse | ✅ 走 `runTools` 的 hook 链 | ❌ **不走**（`tools/nested.go` 的负向测试钉住） | 选区守卫必须封死 codemode（§10.3 已加） |
| 异步摘离 | BatchController 可摘离 | 无摘离入口 | `artifact_export` 别指望脚本里摘离；超时就超时 |
| `searchTools` | 不适用 | 可检索（目录 = 可编排工具） | 描述要可检索（含关键词 publish/preview/selection） |

### 22.5 图片类工具在脚本里的降级

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

### 22.6 并发：per-artifact 串行

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

### 22.7 目录预算

| 项 | 数值 |
|---|---|
| 单个工具描述 | ≤ 240 字符（首行是摘要，codemode 只取首行） |
| 9 个工具总增量 | ≈ 1.1k token（`estimateTokens` 口径：字符/4） |
| codemode 目录上限 | 见 `descriptions.go` 的预算常量；超了会按 `cands` 截断 |
| 降级方案（Phase C） | 4 个辅助工具改 `deferred`，靠 `tool_search` 现查；核心 5 个保持 direct |

### 22.8 测试

| 用例 | 断言 |
|---|---|
| `TestArtifactToolsCallableFromCodemode` | 脚本里 `tools.artifact_publish → artifact_edit → source_get` 全链路成功；`structured` 对象字段可读（不是字符串） |
| `TestArtifactToolsNotCallableWhenHidden` | `HideTool("artifact_publish")` → 脚本调用返回 "not in this call's codemode tool table" |
| `TestArtifactToolErrorsReject` | `artifact_edit` 传过期 base → 脚本 `try/catch` 捕获；`result` 不是对象 |
| `TestPreviewScreenshotScriptPathNoImage` | 脚本路径：`ImageSink` 为空、结构化有 `console_errors` |
| `TestArtifactEditSerializedPerArtifact` | 两个 goroutine 同时 edit 同一 artifact → 一个成功、一个 `version conflict`；最终版本只有一次写入 |
| `TestCodemodeCatalogHasArtifactTools` | `BuildCatalog` 的描述包含 9 个名字，且不含点号 |

---

## 23. 选区 → Agent Chat：引用而不是直接发

> 设计稿的默认动作是「选完就发一条改稿任务」。真实使用里，用户更多是**先引用、再自己写
> 一句话**（「这个按钮改成主色」「这块文案太长了」）。所以 Phase B 必须有两个动作，
> 且「引用」要能复用 Composer 已有的 chip 机制，而不是再造一个输入框。

### 23.1 两个动作

| 动作 | 触发 | 结果 | 守卫 |
|---|---|---|---|
| **引用到对话**（默认） | 浮动框「引用」/ `Cmd+Enter` | 选区变成 Composer 里的一枚 chip；用户继续编辑文字，发送时一起进消息 | **不激活**写守卫（用户可能只是问「这块为什么这样」） |
| **直接改** | 浮动框「直接改」 | 立刻 `Ask` 一条改稿任务（含选区数据） | 激活写守卫（§10），只允许改这个 artifact |

### 23.2 服务端选区引用表（`desktop/bridge/selection_refs.go`，新增全文）

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

### 23.3 发送时注入：`<selection>` 是数据，不是指令

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

### 23.4 Composer：复用已有 chip，不新增输入框

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

### 23.5 失效与漂移

| 情况 | 行为 |
|---|---|
| refId 过期（>30min） | `ask` 返回 `selection_ref_expired`，Composer 保留文字、chip 标灰，提示「选区已过期，请重新选择」 |
| 版本已被删（用户手动清版本库） | `selection_ref_stale`：附最新版本号，chip 上给「用最新版本」按钮 |
| 版本还在、但页面元素选择器失效 | 不阻断发送：把 `stale:true` 写进 `<selection>`，模型会先 `source_get` 再改 |
| 同一元素被引用两次 | 前端按 `artifactId+selector+version` 去重，不产生两枚 chip |

**重新定位**（服务端，发送时执行一次）：selector 命中失败 → 用 `text` 指纹在最新版本里找
（`source_get` 同一套归一化），找到就更新 `Version` 并标记 `relocated:true`；找不到就
`stale:true`，**不猜**。

### 23.6 与写守卫的关系（容易搞反）

- **引用 → 不激活守卫**：用户可能问「这块为什么这样」或让模型改**别的**文件；
- **直接改 → 激活守卫**（§10），并限定 10 分钟 / 单 artifact；
- 用户引用后又点「直接改」：复用同一条 ref，把 `mode` 从 `reference` 改成 `edit`。

这是两条**独立**的状态：`selectionRefStore` 管「引用」，`selectionScope` 管「写范围」。
不要把它们合并成一个结构 —— 生命周期不同（引用 30 分钟、守卫 10 分钟且 AgentEnd 即清）。

### 23.7 抽出来的公共能力

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

### 23.8 测试

| 用例 | 断言 |
|---|---|
| `TestSelectionRefPutGetOwner` | 跨会话 refId → 拒绝；过期 → 拒绝 |
| `TestSelectionRefSweep` | 30 分钟后 Sweep 清空；未过期不动 |
| `TestBuildSelectionMessage` | 指令在前、每个 ref 一个 `<selection>`、版本号在内 |
| `TestAskWithExpiredRef` | 返回 `selection_ref_expired`，不产生 run |
| `TestSelectionRefDedup` | 同 artifact+selector+version 两次引用 → 一枚 chip |
| e2e `verify_selection_ref.mjs` | 选元素 → 引用 → chip 出现 → 发送 → 消息里含 `<selection>` → 模型可读 |

---

## 24. Review 修正清单（本轮全面复核）

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

