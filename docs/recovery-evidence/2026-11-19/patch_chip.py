# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

rep('''type SelectionRefItem = {
  refId: string
  artifactId: string
  version: string
  selector: string
  label: string // 例：button「立即购买」
}''','''type SelectionKind = 'element' | 'elements' | 'text' | 'comment' | 'region' | 'page' | 'artifact' | 'file'

type SelectionRefItem = {
  refId: string
  kind: SelectionKind
  artifactId: string
  version: string
  selector: string
  label: string // 例：button「立即购买」/ styles.css:40-60 / 整页
}

// chip 上的类型标记：文案短、可翻译；图标复用既有（不新增一套 SVG）。
const KIND_LABEL: Record<SelectionKind, string> = {
  element: '元素', elements: '多元素', text: '文字', comment: '批注',
  region: '区域', page: '整页', artifact: '产物', file: '文件',
}''')

rep('''    <span className="doc-chip-name">{s.label}</span>
    <span className="doc-chip-size">{s.version.slice(0, 9)}</span>''','''    <span className="sel-kind">{KIND_LABEL[s.kind]}</span>
    <span className="doc-chip-name">{s.label}</span>
    <span className="doc-chip-size">{s.version ? s.version.slice(0, 9) : '—'}</span>''')

# tests
rep('''| `TestSelectionRefDedup` | 同 artifact+selector+version 两次引用 → 一枚 chip |''',
'''| `TestSelectionRefDedup` | 同 artifact+selector+version 两次引用 → 一枚 chip |
| `TestSelectionKindsAccepted` | 8 种 kind 各自 Put/Get 成功；`kind` 缺失按 `element` 兜底 |
| `TestSelectionUnknownKindRejected` | `kind:"dom"` → 400，不落表 |
| `TestSelectionFileWithoutArtifact` | `kind:file` + `selection.path` → 通过（version 可空）；缺 path → 400 |
| `TestSelectionElementsTruncated` | 框选 30 个 → payload 截到 20 + `truncated:true` |''')
open(p,'w').write(s)
print('ok')
