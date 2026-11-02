import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { BUILTIN_SLASH, useAppStore } from '../../../store/useAppStore'
import type { MsgBlock } from '../../../store/useAppStore'
import { useT } from '../../../i18n'
import { FileIcon, FolderIcon } from '../../icons'
import { Markdown } from '../../markdown'
import { fmtClock } from '../../ui'
import './user-block.css'

type UserMsg = Extract<MsgBlock, { kind: 'user' }>

/** `/token` in the prompt is a chip only when it names something that exists —
 *  a skill that is enabled, or a built-in command. Anything else is a path
 *  (`/usr/bin`), and painting that as a command would be a lie. */
function tokensOf(text: string, skills: Set<string>) {
  const out: { kind: 'cmd' | 'skill'; label: string }[] = []
  if (!text || !text.includes('/')) return out
  const seen = new Set<string>()
  for (const raw of text.split(/\s+/)) {
    const m = /^\/([A-Za-z0-9_.-]+)$/.exec(raw)
    if (!m) continue
    const kind = skills.has(m[1]) ? 'skill' : BUILTIN_SLASH.includes(raw) ? 'cmd' : null
    if (!kind || seen.has(raw)) continue
    seen.add(raw)
    out.push({ kind, label: raw })
  }
  return out
}

/* The prompt as a record: a rail, then the four things that travelled with it.
   The label is not a heading and the time is not a line of its own — the turn's
   own head carries the clock (design .cy-head), so here it is only a title. */
export function UserBlock({ b }: { b: UserMsg }) {
  const t = useT()
  const skills = useAppStore((s) => s.skills)
  const [open, setOpen] = useState<{ src: string; cap: string } | null>(null)
  const closeRef = useRef<HTMLButtonElement | null>(null)

  const enabled = new Set(skills.filter((s) => s.enabled).map((s) => s.name))
  const toks = tokensOf(b.text, enabled)
  const imgs = (b.contents ?? []).filter((c) => c.type === 'image')
  const refs = b.refs ?? []
  const mid = (imgs.length - 1) / 2

  useEffect(() => {
    if (!open) return
    const prev = document.documentElement.style.overflow
    document.documentElement.style.overflow = 'hidden'
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(null)
    }
    document.addEventListener('keydown', onKey)
    closeRef.current?.focus()
    return () => {
      document.documentElement.style.overflow = prev
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div className="msg user rt-you" data-injected={b.injected ? '1' : undefined}>
      <span className="rt-who" title={b.ts != null ? fmtClock(b.ts) : undefined}>
        {t('narrative.user')}
        {/* 运行中插话：它出现在 Working 盒子内部，读起来不能像是这一轮的开口。
            一行灰字说明「这是在它跑的时候补的」，与盒子里的工具行同一层级。 */}
        {b.injected ? <span className="rt-who-mid">{t('narrative.interjected')}</span> : null}
      </span>
      {/* 用户输入按 **Markdown** 渲染（用户 2026-10 报障两件事：换行没了、
          自己打的 Markdown 不生效）。此前这里是手写的「只认行内 code」渲染器，
          于是 `1. a\n2. b` 折成一段、`**重点**` 原样显示、`- 列表` 变成一个小横线。
          换行的兜底在 CSS（.rt-you .md 的 white-space: pre-wrap）：remark 把单换行
          留在段落文本里，浏览器默认折叠它 —— pre-wrap 让「用户按了 ⏎」原样回来。 */}
      {b.text ? (
        <div className="rt-body">
          <Markdown text={b.text} />
        </div>
      ) : null}
      {toks.length + refs.length > 0 ? (
        <div className="rt-toks">
          {toks.map((x) => (
            <span
              className="rt-tok"
              data-kind={x.kind}
              key={x.kind + x.label}
              title={x.kind === 'skill' ? t('composer.refs.skill') : t('composer.refs.cmd')}
            >
              {x.label}
            </span>
          ))}
          {refs.map((r, i) => {
            const bad = r.status !== 'loaded'
            return (
              <span
                className="rt-ref"
                data-dir={r.kind === 'dir' ? '1' : undefined}
                data-state={bad ? r.status : undefined}
                key={'r' + i + r.path}
                title={r.resolved || r.path}
              >
                {r.kind === 'dir' ? <FolderIcon size={11} /> : <FileIcon size={11} />}
                {r.path}
                {bad ? (
                  <span className="rt-ref-state">
                    {' '}
                    {r.status === 'blocked' ? t('narrative.refBlocked') : t('narrative.refMissing')}
                  </span>
                ) : null}
              </span>
            )
          })}
        </div>
      ) : null}
      {imgs.length > 0 ? (
        <div className="rt-imgs" data-n={imgs.length} style={{ '--mid': mid } as React.CSSProperties}>
          {imgs.map((c, i) => (
            <button
              className="rt-im"
              type="button"
              key={i}
              style={{ '--i': i } as React.CSSProperties}
              aria-label={t('narrative.zoomImage').replace('{n}', String(i + 1))}
              onClick={() => setOpen({ src: c.content, cap: t('narrative.attachAlt') })}
            >
              <img src={c.content} alt="" draggable={false} />
            </button>
          ))}
        </div>
      ) : null}
      {open
        ? createPortal(
            <div
              className="rt-lb"
              role="dialog"
              aria-modal="true"
              onClick={(e) => {
                if (e.target === e.currentTarget) setOpen(null)
              }}
            >
              <figure>
                <img
                  src={open.src}
                  alt=""
                  onLoad={(e) => {
                    // A small picture opened "full size" is the same picture: it
                    // gets one step up, not a wall of pixels.
                    const el = e.currentTarget
                    if (el.naturalWidth && el.naturalWidth < 420) {
                      el.style.width = Math.round(el.naturalWidth * 1.75) + 'px'
                    }
                  }}
                />
                <figcaption>{open.cap}</figcaption>
                <button
                  className="rt-lb-x"
                  type="button"
                  ref={closeRef}
                  aria-label={t('narrative.zoomClose')}
                  onClick={() => setOpen(null)}
                >
                  ×
                </button>
              </figure>
            </div>,
            document.body,
          )
        : null}
    </div>
  )
}
