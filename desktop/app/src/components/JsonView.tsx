import { useState } from 'react'
import { useT } from '../i18n'

// 可读 JSON 查看器：语法高亮（键/字符串/数字/布尔），
// 长字符串与大容器（对象/数组）默认折叠、点击展开——适合审批参数等长内容查看。
// 解析失败时回退原始文本。
export default function JsonView({ text, className }: { text: string; className?: string }) {
  const t = useT()
  let value: unknown
  try {
    value = JSON.parse(text)
  } catch {
    return <pre className={`jv jv-raw ${className || ''}`}>{text}</pre>
  }
  return (
    <div className={`jv ${className || ''}`}>
      <Node value={value} depth={0} t={t} />
    </div>
  )
}

const COLLAPSE_STRING = 120
const COLLAPSE_ITEMS = 8

// 长字符串：默认截断 + 展开全文
function Str({ s, t }: { s: string; t: (k: string) => string }) {
  const [open, setOpen] = useState(false)
  if (s.length <= COLLAPSE_STRING) return <span className="jv-str">{JSON.stringify(s)}</span>
  const shown = open ? s : s.slice(0, COLLAPSE_STRING)
  return (
    <span className="jv-str-wrap">
      <span className="jv-str">"{shown}{!open ? '…' : ''}"</span>
      <button className="jv-toggle" onClick={() => setOpen((v) => !v)} title={open ? t('json.collapse') : t('json.expandFull').replace('{n}', String(s.length))}>
        {open ? t('json.collapse') : t('json.expand').replace('{n}', String(s.length))}
      </button>
    </span>
  )
}

function Node({ value, depth, t }: { value: unknown; depth: number; t: (k: string) => string }) {
  if (value === null || value === undefined) return <span className="jv-null">null</span>
  if (typeof value === 'boolean') return <span className="jv-bool">{String(value)}</span>
  if (typeof value === 'number') return <span className="jv-num">{String(value)}</span>
  if (typeof value === 'string') return <Str s={value} t={t} />
  if (Array.isArray(value)) return <Arr arr={value} depth={depth} t={t} />
  if (typeof value === 'object') return <Obj obj={value as Record<string, unknown>} depth={depth} t={t} />
  return <span className="jv-num">{String(value)}</span>
}

function indent(depth: number) {
  return { marginLeft: depth * 14 }
}

function Arr({ arr, depth, t }: { arr: unknown[]; depth: number; t: (k: string) => string }) {
  const [open, setOpen] = useState(depth < 2 && arr.length <= COLLAPSE_ITEMS)
  if (!open) {
    return (
      <span className="jv-collapsed">
        <span className="jv-punct">[</span>
        <button className="jv-toggle" onClick={() => setOpen(true)}> {t('json.items').replace('{n}', String(arr.length))} </button>
        <span className="jv-punct">]</span>
      </span>
    )
  }
  return (
    <span className="jv-node">
      <span className="jv-punct">[</span>
      <button className="jv-toggle" onClick={() => setOpen(false)}>{t('json.collapse')}</button>
      <div style={indent(depth + 1)}>
        {arr.map((item, i) => (
          <div key={i} className="jv-line">
            <Node value={item} depth={depth + 1} t={t} />
            {i < arr.length - 1 && <span className="jv-punct">,</span>}
          </div>
        ))}
      </div>
      <span className="jv-punct">]</span>
    </span>
  )
}

function Obj({ obj, depth, t }: { obj: Record<string, unknown>; depth: number; t: (k: string) => string }) {
  const keys = Object.keys(obj)
  const [open, setOpen] = useState(depth < 2 && keys.length <= COLLAPSE_ITEMS)
  if (!open) {
    return (
      <span className="jv-collapsed">
        <span className="jv-punct">{"{"}</span>
        <button className="jv-toggle" onClick={() => setOpen(true)}> {t('json.fields').replace('{n}', String(keys.length))} </button>
        <span className="jv-punct">{"}"}</span>
      </span>
    )
  }
  return (
    <span className="jv-node">
      <span className="jv-punct">{"{"}</span>
      <button className="jv-toggle" onClick={() => setOpen(false)}>{t('json.collapse')}</button>
      <div style={indent(depth + 1)}>
        {keys.map((k, i) => (
          <div key={k} className="jv-line">
            <span className="jv-key">"{k}"</span>
            <span className="jv-punct">: </span>
            <Node value={obj[k]} depth={depth + 1} t={t} />
            {i < keys.length - 1 && <span className="jv-punct">,</span>}
          </div>
        ))}
      </div>
      <span className="jv-punct">{"}"}</span>
    </span>
  )
}
