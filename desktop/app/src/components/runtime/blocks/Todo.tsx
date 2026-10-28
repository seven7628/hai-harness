import { memo, useMemo, useState } from 'react'
import type { TodoItem } from '../../../store/useAppStore'
import { useT } from '../../../i18n'
import './todo.css'

/* The agent's own checklist, drawn once per turn instead of once per call.

   The tool's contract is "one call = the whole list", so a call is not a row in
   the transcript — it is a *snapshot*. Four calls in a row are four photographs
   of one sheet, and drawing four cards is showing the reader the same list four
   times. So consecutive writes to `todo_*` fold into one block: the last
   snapshot is the list, and the change against the previous one is a 2px bar in
   the left gutter — a diff this component computes, not something the model
   says.

   Nothing here has a percentage. A todo list has no progress bar; the only two
   numbers worth printing are how many are done and how many times the sheet was
   rewritten. */

const CHECK = 'M2.5 6.3l2.3 2.3L9.5 3.7'
const DOT = { cx: 6, cy: 6, r: 2.1 }
const PAUSE = 'M4.6 3.1v5.8M7.4 3.1v5.8'

export interface TodoWrite {
  /** the snapshot this call returned; absent on a call that only started */
  items?: TodoItem[]
  running?: boolean
}

type St = 'completed' | 'in_progress' | 'blocked' | 'pending'

/* One in-progress step at a time is a real constraint of the tool, not a
   display choice: the first item that is not done and not blocked is the one
   being worked on. */
function statesOf(items: TodoItem[]): St[] {
  let active = -1
  for (let i = 0; i < items.length; i++) {
    const it = items[i]
    if (it && !it.done && !(it.blockedBy && it.blockedBy.length)) {
      active = i
      break
    }
  }
  return items.map((it, i): St => {
    if (it.done) return 'completed'
    if (it.blockedBy && it.blockedBy.length) return 'blocked'
    return i === active ? 'in_progress' : 'pending'
  })
}

/* A delta is only meaningful against the write before it: a new id, or a
   changed state. The first write of a turn has nothing to compare against, so
   it gets no bars at all rather than a column of "new". */
function deltasOf(prev: TodoItem[] | undefined, items: TodoItem[]): Record<string, 'new' | 'changed'> {
  const out: Record<string, 'new' | 'changed'> = {}
  if (!prev) return out
  const before: Record<string, boolean> = {}
  prev.forEach((it) => {
    before[it.id] = it.done
  })
  items.forEach((it) => {
    if (!(it.id in before)) out[it.id] = 'new'
    else if (before[it.id] !== it.done) out[it.id] = 'changed'
  })
  return out
}

/* 待办标题是模型写的纯文本，里面的 `反引号` 指的是代码/路径 —— 拆出来给 mono，
   其余原样（这里不渲染 Markdown：标题里的 ** 或 - 就是字符本身，不是格式）。 */
const inlineCode = (text: string) =>
  text.split('`').map((part, i) => (i % 2 ? <code key={i}>{part}</code> : <span key={i}>{part}</span>))

function Mark({ s }: { s: St }) {
  if (s === 'completed') {
    return (
      <svg viewBox="0 0 12 12" aria-hidden="true">
        <path d={CHECK} fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    )
  }
  if (s === 'in_progress') {
    return (
      <svg viewBox="0 0 12 12" aria-hidden="true">
        <circle cx={DOT.cx} cy={DOT.cy} r={DOT.r} fill="currentColor" />
      </svg>
    )
  }
  if (s === 'blocked') {
    return (
      <svg viewBox="0 0 12 12" aria-hidden="true">
        <path d={PAUSE} fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
      </svg>
    )
  }
  return null
}

export const TodoBlock = memo(function TodoBlock({ writes }: { writes: TodoWrite[] }) {
  const t = useT()
  const [open, setOpen] = useState(true)
  const snapshots = useMemo(() => writes.filter((w) => w.items && w.items.length).map((w) => w.items!), [writes])
  const last = snapshots[snapshots.length - 1]
  const prev = snapshots.length > 1 ? snapshots[snapshots.length - 2] : undefined
  const running = Boolean(writes[writes.length - 1]?.running)

  const states = useMemo(() => (last ? statesOf(last) : []), [last])
  const deltas = useMemo(() => (last ? deltasOf(prev, last) : {}), [prev, last])
  const counts = useMemo(() => {
    const c = { completed: 0, in_progress: 0, blocked: 0, pending: 0 }
    states.forEach((s) => {
      c[s] += 1
    })
    return c
  }, [states])

  if (!last) {
    // the tool has not returned a list yet: the row says it is working, and
    // draws nothing else — an empty checklist is not a checklist
    return (
      <div className="rt-td" data-s={running ? 'run' : 'idle'}>
        <div className="rt-td-head">
          <span className="rt-td-sum">{running ? t('plan.title') + ' …' : t('plan.title')}</span>
        </div>
      </div>
    )
  }

  /* A clause that is zero is dropped rather than printed as 0 — the line gets
     shorter as the list settles. */
  const clauses: string[] = [t('todo.doneOf').replace('{done}', String(counts.completed)).replace('{all}', String(last.length))]
  if (counts.in_progress) clauses.push(t('todo.inFlight').replace('{n}', String(counts.in_progress)))
  if (counts.blocked) clauses.push(t('todo.blockedN').replace('{n}', String(counts.blocked)))
  if (!counts.completed && !counts.in_progress && !counts.blocked) {
    clauses.push(t('todo.pendingN').replace('{n}', String(counts.pending)))
  }

  return (
    <div className="rt-td" data-s={running ? 'run' : 'done'} data-open={open ? '1' : undefined}>
      <button className="rt-td-head" type="button" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
        <span className="rt-td-sum">
          {clauses.map((c, i) => (
            <span key={i}>
              {i ? <span className="sep">{'\u00b7'}</span> : null}
              {c}
            </span>
          ))}
        </span>
        <span className="rt-td-writes">{t('todo.updatedN').replace('{n}', String(snapshots.length))}</span>
        <svg className="rt-td-chev" viewBox="0 0 12 12" aria-hidden="true">
          <path d="M4.2 2.6L8.6 6.8l-4.4 4.2" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>
      <div className={'rt-td-fold' + (open ? ' open' : '')}>
        <div>
          <div className="rt-td-list" role="list">
            {last.map((it, i) => {
              const st = states[i]
              const d = deltas[it.id]
              return (
                <div className="rt-td-row" data-s={st} data-delta={d} role="listitem" key={it.id}>
                  <span className="rt-td-delta" aria-hidden="true" />
                  <span className="rt-td-mark" aria-hidden="true">
                    <Mark s={st} />
                  </span>
                  <div className="rt-td-t">
                    {inlineCode(it.title)}
                    {st === 'blocked' && it.blockedBy?.length ? (
                      <span className="rt-td-wait">
                        <span className="w">{t('todo.waiting')}</span> {it.blockedBy.join(' ')}
                      </span>
                    ) : null}
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      </div>
    </div>
  )
})
