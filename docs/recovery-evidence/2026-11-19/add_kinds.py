# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

# 1) §23.1b taxonomy
anchor = "### 23.2 服务端选区引用表（`desktop/bridge/selection_refs.go`，新增全文）"
addition = r'''### 23.1b 引用类型：节点级 / 区域级 / 整体级（8 种）

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

'''
rep(anchor, addition+anchor)

# 2) selectionRef.Kind + validation
rep('''type selectionRef struct {
	ID       string
	WS       string
	SID      string
	Artifact string
	Version  string
	Entry    string
	Payload  json.RawMessage // 已裁剪（≤2KB）的选区数据，原样来自 inspector
	Label    string          // chip 上显示的文字（tag + 文本前 24 字）
	Created  time.Time
}''','''type selectionRef struct {
	ID       string
	WS       string
	SID      string
	Kind     string // element | elements | text | comment | region | page | artifact | file
	Artifact string // page/artifact/region/element… 都归属一个 artifact；file 可为空
	Version  string
	Entry    string
	Payload  json.RawMessage // 已裁剪（≤2KB）的选区数据，原样来自 inspector
	Label    string          // chip 上显示的文字（tag + 文本前 24 字）
	Created  time.Time
}

// 引用类型白名单 + 每种类型的 payload 上限（元素列表类按条数截断）。
const (
	selElement  = "element"
	selElements = "elements"
	selText     = "text"
	selComment  = "comment"
	selRegion   = "region"
	selPage     = "page"
	selArtifact = "artifact"
	selFile     = "file"

	selMaxElements = 20 // elements 类的条数上限（设计稿：框选上限 20）
)

func validSelectionKind(k string) bool {
	switch k {
	case selElement, selElements, selText, selComment, selRegion, selPage, selArtifact, selFile:
		return true
	}
	return false
}''')

# 3) Put validation
rep('''func (s *selectionRefStore) Put(r *selectionRef) (string, error) {
	if len(r.Payload) == 0 || len(r.Payload) > selectionRefMaxBytes {
		return "", errors.New("selection payload missing or too large")
	}''','''func (s *selectionRefStore) Put(r *selectionRef) (string, error) {
	if !validSelectionKind(r.Kind) {
		return "", errors.New("unknown selection kind: " + r.Kind)
	}
	if len(r.Payload) == 0 || len(r.Payload) > selectionRefMaxBytes {
		return "", errors.New("selection payload missing or too large")
	}''')
open(p,'w').write(s)
print('ok')
