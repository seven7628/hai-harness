import { useState } from 'react'
import type { ArtifactItem } from '../../../store/useAppStore'
import { useT } from '../../../i18n'
import { fmtDuration } from '../../../lib/duration'
import { EyeIcon, FolderOpenIcon } from '../../icons'
import './artifacts.css'

/* 产出卡（artifacts）—— DOM、文案位置与交互都照 parts/artifacts.js 的
   cardHTML / mainHTML / rowHTML / wire 搬过来，只把「字符串拼 HTML」换成 React。

   两条设计约束原样保留（它们才是这张卡的样子，不是样式细节）：
     1. **主卡**是「最重要的那个文件」：优先在新建里挑，按改动行数、再按体积取最大；
        其余按 payload 顺序列在「另外 N 个」下，不重排。
     2. 「另外 N 个」只先显示 4 个，展开是把 hidden 去掉并把按钮文案换成「收起」——
        行本身一直在 DOM 里（设计稿的 wire() 就是这么做的），不是重新挂载。

   动作是宿主的事：组件只派发 onPreview / onReveal，由调用方决定开编辑器还是 Finder。 */

const MORE_LIMIT = 4

/* 类型是**写出来**的，不是染色的：按扩展名给一套颜色语言会和页面的语义色
   （绿=落地、强调=可操作、琥珀=注意、红=失败）撞车。最多 4 个字符。 */
const EXT: Record<string, string> = {
  py: 'PY', go: 'GO', rs: 'RS', java: 'JAVA',
  ts: 'TS', tsx: 'TS', js: 'JS', jsx: 'JS', mjs: 'JS', cjs: 'JS',
  css: 'CSS', scss: 'CSS', html: 'HTML', vue: 'VUE',
  md: 'MD', mdx: 'MD', txt: 'TXT', json: 'JSON', yaml: 'YML', yml: 'YML',
  toml: 'TML', sql: 'SQL', sh: 'SH', bash: 'SH', mod: 'MOD', lock: 'LOCK',
  csv: 'CSV', xml: 'XML', proto: 'PROTO',
  png: 'IMG', jpg: 'IMG', jpeg: 'IMG', gif: 'IMG', webp: 'IMG', svg: 'IMG',
  ico: 'IMG', pdf: 'PDF', zip: 'ZIP',
}

const num = (v: unknown) => (Number.isFinite(Number(v)) ? Number(v) : 0)
const changed = (a: ArtifactItem) => num(a.added) + num(a.removed)

function badgeOf(name: string): string {
  const dot = name.lastIndexOf('.')
  const ext = dot > 0 ? name.slice(dot + 1).toLowerCase() : ''
  if (!ext) return 'FILE'
  return EXT[ext] || ext.slice(0, 4).toUpperCase()
}

function bytesText(n: unknown): string {
  const b = num(n)
  if (b <= 0) return ''
  if (b < 1024) return b + ' B'
  if (b < 1048576) return (b / 1024).toFixed(1) + ' KB'
  return (b / 1048576).toFixed(1) + ' MB'
}

/** 主卡：新建优先，改动行数最多（并列比体积）。 */
function headlineOf(list: ArtifactItem[]): ArtifactItem {
  const created = list.filter((a) => a.kind === 'created')
  const pool = created.length ? created : list
  let best = pool[0]
  for (const a of pool) {
    if (changed(a) > changed(best) || (changed(a) === changed(best) && num(a.size) > num(best.size))) best = a
  }
  return best
}

export function ArtifactsBlock({
  items,
  truncated,
  wall,
  onPreview,
  onReveal,
}: {
  items: ArtifactItem[]
  truncated?: boolean
  /** 这一轮的用时（毫秒）；没有就不画 */
  wall?: number
  onPreview?: (a: ArtifactItem) => void
  onReveal?: (a: ArtifactItem) => void
}) {
  const t = useT()
  const [open, setOpen] = useState(false)
  const list = items.filter(Boolean)
  // 没有产出是一个真实结果（纯对话的一轮）：不画空框
  if (!list.length) return null

  const total = list.length
  const add = list.reduce((n, a) => n + num(a.added), 0)
  const del = list.reduce((n, a) => n + num(a.removed), 0)
  const head = headlineOf(list)
  const others = list.filter((a) => a !== head)
  const shown = Math.min(MORE_LIMIT, others.length)
  const rest = others.length - shown

  const linesText = (a: ArtifactItem) => {
    const l = num(a.lines)
    if (l > 0) return t('artifacts.lines').replace('{n}', String(l))
    return bytesText(a.size) ? t('artifacts.binary') : ''
  }
  const metaOf = (a: ArtifactItem) => [bytesText(a.size), linesText(a)].filter(Boolean).join(' · ')

  const Diff = ({ a }: { a: ArtifactItem }) => {
    const p = num(a.added)
    const m = num(a.removed)
    if (!p && !m) return null
    return (
      <span className="ar-df">
        {p ? <span className="p">+{p}</span> : null}
        {m ? <span className="m">−{m}</span> : null}
      </span>
    )
  }

  const Actions = ({ a }: { a: ArtifactItem }) => (
    <>
      <button className="ar-btn" type="button" onClick={() => onPreview?.(a)}>
        <EyeIcon size={12} />
        {t('artifacts.preview')}
      </button>
      <button className="ar-btn" type="button" onClick={() => onReveal?.(a)}>
        <FolderOpenIcon size={12} />
        {t('artifacts.reveal')}
      </button>
    </>
  )

  const Dir = ({ a }: { a: ArtifactItem }) => {
    const i = a.path.lastIndexOf('/')
    return <>{i < 0 ? '.' : a.path.slice(0, i) || '/'}</>
  }

  const wallText = (() => {
    const v = num(wall)
    if (!Number.isFinite(v) || v <= 0) return ''
    return fmtDuration(v)
  })()

  return (
    <div className="ar" data-trunc={truncated ? '1' : '0'} role="group" aria-label={t('artifacts.aria').replace('{n}', String(total))}>
      <div className="ar-h">
        <span className="ar-h-t">
          {t('artifacts.head')} <b>{total}{truncated ? '+' : ''}</b> {t('artifacts.files')}
        </span>
        {add || del ? (
          <span className="ar-h-df">
            {add ? <span className="p">+{add}</span> : null}
            {del ? <span className="m">−{del}</span> : null}
          </span>
        ) : null}
        {wallText ? <span className="ar-h-w">{wallText}</span> : null}
      </div>
      {truncated ? (
        <div className="ar-note">
          <span className="k">{t('artifacts.truncNote').replace('{n}', String(total))}</span>
          <span>{t('artifacts.truncBody')}</span>
        </div>
      ) : null}

      {/* 主卡（唯一有框的那一个） */}
      <div className="ar-main" data-k={head.kind}>
        <span className="ar-ico" aria-hidden="true">{badgeOf(head.name)}</span>
        <div className="ar-m-b">
          <div className="ar-m-n">
            <span className="ar-nm" title={head.path}>{head.name}</span>
            <span className="ar-badge">{head.kind === 'created' ? t('artifacts.created') : t('artifacts.modified')}</span>
            <Diff a={head} />
          </div>
          <div className="ar-m-p">
            <span className="ar-p" title={head.path}>
              <span className="ar-p-h">{head.path.slice(0, head.path.lastIndexOf('/') + 1)}</span>
              <span className="ar-p-t">{head.path.slice(head.path.lastIndexOf('/') + 1)}</span>
            </span>
            {metaOf(head) ? <span className="ar-m-meta">{metaOf(head)}</span> : null}
          </div>
        </div>
        <div className="ar-m-a">
          <Actions a={head} />
        </div>
      </div>

      {others.length ? (
        <>
          <div className="ar-cap">
            {t('artifacts.others')} <b>{others.length}</b> {t('artifacts.files')}
          </div>
          <div className="ar-list">
            {others.map((a, i) => (
              <div className="ar-f" data-k={a.kind} key={a.path + i} hidden={!open && i >= shown}>
                <div className="ar-f-r">
                  <span className="ar-ico ar-ico-sm" aria-hidden="true">{badgeOf(a.name)}</span>
                  <span className="ar-f-n" title={a.path}>{a.name}</span>
                  <Diff a={a} />
                </div>
                {/* 目录与两个动作已经在 DOM 里：指针移上去之前不占高度、不显形（见 CSS） */}
                <div className="ar-f-x">
                  <div className="ar-f-xin">
                    <div className="ar-f-xrow">
                      <span className="ar-dir" title={a.path}><Dir a={a} /></span>
                      <span className="ar-f-a"><Actions a={a} /></span>
                    </div>
                  </div>
                </div>
              </div>
            ))}
          </div>
          {rest ? (
            <div className="ar-more">
              <span className="ar-more-k">
                {t('artifacts.showing')} <b>{open ? others.length : shown}</b> / {others.length}
              </span>
              <button
                className="ar-btn ar-more-b"
                type="button"
                aria-expanded={open}
                onClick={() => setOpen((v) => !v)}
              >
                {open ? t('artifacts.collapse') : t('artifacts.expand').replace('{n}', String(rest))}
              </button>
            </div>
          ) : null}
        </>
      ) : null}

      <div className="ar-foot">
        <span className="ar-foot-k">
          {truncated ? t('artifacts.listed') : t('artifacts.total')} <b>{total}</b> {t('artifacts.files')}
        </span>
        <button className="ar-btn" type="button" onClick={() => onReveal?.(head)}>
          <FolderOpenIcon size={12} />
          {t('artifacts.revealAll')}
        </button>
      </div>
    </div>
  )
}
