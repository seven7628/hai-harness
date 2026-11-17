import { useCallback, useState } from 'react'
import { mainRunActive, useAppStore } from '../store/useAppStore'
import type { TodoItem } from '../store/useAppStore'
import { useT } from '../i18n'
import { pickTodoList } from '../lib/todoScope'
import { useFocusTodos } from './useFocusTodos'
import { useScopedApproval, useScopedQuestion } from './sessionScope'
import { AgentRunningBar } from './AgentRunningBar'
import { ChevronDown } from './icons'

// 输入框上沿的那一条：**左槽 = 运行状态，右槽 = 待办**（用户 2026-11 拍板的口径）。
//
// 为什么把运行状态也搬进来：它原本是会话框正上方一条居中的文字，待办页签又要在同一侧，
// 两行叠着就成了「一条居中的字 + 右下角一个页签」—— 视线要先读完中间再跳到右边。
// 现在它们同占一行：谁在跑看左边，还要不要做事看右边，阅读顺序 = 从左到右一次。
//
// 三种形态（同一个组件，靠 open 切换）：
//   · 收起：右槽一枚**页签**（待办 + 一格一条的进度 + 3/8），贴着会话框上沿；
//   · 展开：页签消失，换成**比会话框窄的卡片**（左右各缩 36px），下沿钻进会话框**背后**
//     14px —— 连接处压一层黑色渐变，读起来是「卡片在输入框后面」，不是「又叠了一张卡」；
//   · 全完成：收成一条 20px 薄唇（8/8 全部完成），点一下才展开。
//
// 三条让位纪律（都是踩过的坑）：
//   · 零待办 → 只留运行状态，不画空壳；
//   · 审批 / 提问（阻塞 dock）在等 → 待办整块让位，那一格归审批（运行状态照旧在左槽）；
//   · 会话框**不动**：不改它的圆角、边框、阴影，也不给它加负边距以外的任何东西 ——
//     卡片是「钻到它背后」，不是「和它拼成一个整体」。

/* 展开态是**每会话各自的**（用户 2026-11 报障：在 A 会话展开，切到 B 还是展开的）。
   旧实现是一个全局 localStorage 布尔，切换会话时组件不卸载 → 状态跟着走。
   现在按 `<sessionId>|<scope>` 存一张表：没开过的会话默认收起，回到 A 仍是你上次的样子。 */
const OPEN_KEY = 'todo.dock.open.v2'
type OpenMap = Record<string, boolean>
const readOpenMap = (): OpenMap => {
  try {
    const raw = localStorage.getItem(OPEN_KEY)
    const v = raw ? (JSON.parse(raw) as unknown) : null
    if (!v || typeof v !== 'object' || Array.isArray(v)) return {}
    const out: OpenMap = {}
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) if (val === true) out[k] = true
    return out
  } catch {
    return {}
  }
}
const writeOpenMap = (m: OpenMap) => {
  try {
    localStorage.setItem(OPEN_KEY, JSON.stringify(m))
  } catch { /* 隐私模式写不了：保持内存态即可 */ }
}

export default function TodoPanel() {
  const storeTodos = useAppStore((s) => (s.focusedAgent ? EMPTY : s.todos))
  const focus = useFocusTodos()
  // 取值规则单一来源（lib/todoScope.pickTodoList）：agent 作用域绝不回退会话清单
  const todos = pickTodoList(focus?.scope ?? 'main', storeTodos, focus?.todos)
  // 运行态只用来决定「要不要占左槽的高度」——真正那条状态条自己会判断跑没跑。
  const running = useAppStore((s) => mainRunActive(s.activeSessionId ? s.views[s.activeSessionId] : undefined))
  const approval = useScopedApproval()
  const question = useScopedQuestion()
  // 跟随**当前会话 + 作用域**：切会话不卸载组件，所以状态不能是组件级的裸布尔
  const sessionId = useAppStore((s) => s.activeSessionId ?? '')
  const openKey = `${sessionId}|${focus?.scope ?? 'main'}`
  const [openMap, setOpenMap] = useState<OpenMap>(readOpenMap)
  const open = openMap[openKey] === true
  const t = useT()

  const toggle = useCallback(() => {
    setOpenMap((m) => {
      const next: OpenMap = { ...m }
      if (m[openKey]) delete next[openKey]
      else next[openKey] = true
      writeOpenMap(next)
      return next
    })
  }, [openKey])

  // 待办让位的两种情形（运行状态不受影响，它在左槽）
  const yielding = Boolean(approval || question)
  const showTodo = todos.length > 0 && !yielding
  const done = todos.filter((item) => item.done).length
  const total = todos.length
  const allDone = total > 0 && done === total

  return (
    <div className="composer-strip">
      <div className="composer-strip-row">
        <AgentRunningBar inline />
        <span className="sp" aria-hidden="true" />
        {showTodo && !open && (
          // 完成与否只改颜色、不改形态：同一枚页签，全绿 = 全做完（用户 2026-11：
          // 「完成态也这样，只不过全绿」—— 不再为完成态换一套「薄唇」）
          <button
            type="button"
            className={`todo-hd${allDone ? ' done' : ''}`}
            aria-expanded={false}
            aria-label={`${t('todo.title')} ${done}/${total}`}
            title={t('todo.expand')}
            onClick={toggle}
          >
            {running && <span className="todo-live" aria-hidden="true" />}
            <span className="todo-title">{t('todo.title')}</span>
            <Segments todos={todos} />
            <span className="todo-count mono">{done}/{total}</span>
            <ChevronDown size={13} className="todo-chev" />
          </button>
        )}
      </div>
      {showTodo && open && (
        <section className="todo-card" aria-label={t('todo.title')}>
          <button type="button" className="todo-hd open" aria-expanded onClick={toggle} title={t('todo.collapse')}>
            <span className="todo-title">{t('todo.title')}</span>
            <span className="todo-count mono">{done}/{total}</span>
            <span className="sp" aria-hidden="true" />
            {/* 折叠按钮钉在右上角（用户 2026-11）；进度条不在这里 —— 它要占**整宽**，见下面那条 */}
            <ChevronDown size={13} className="todo-chev" />
          </button>
          {/* 整宽的进度：橘黄底 = 还没做的，绿色 = 已完成的占比（用户 2026-11 口径） */}
          <span className="todo-progress" aria-hidden="true">
            <i style={{ width: `${Math.round((done / total) * 100)}%` }} />
          </span>
          <ul className="todo-list">
            {todos.map((todo) => {
              const blocked = !todo.done && todo.blockedBy && todo.blockedBy.length > 0
              const state = todo.done ? 'done' : blocked ? 'run' : ''
              return (
                <li key={todo.id} className={`todo-item ${state}`} title={todo.id}>
                  <span className="todo-st" aria-hidden="true">{todo.done ? '✓' : blocked ? '▶' : '○'}</span>
                  <span className="todo-txt">{todo.title}</span>
                  {blocked && <span className="todo-blk mono">{t('todo.blockedBy').replace('{ids}', todo.blockedBy!.join(','))}</span>}
                </li>
              )
            })}
          </ul>
        </section>
      )}
    </div>
  )
}

// 一格一条的进度：条数多时不换行、不加宽，格子自己收细 —— 「还差几条」靠长度看，不靠读数字。
// 超过 12 条只画 11 格 + `+N`，再细就看不清了。
function Segments({ todos }: { todos: TodoItem[] }) {
  const max = 12
  const shown = todos.slice(0, max)
  const rest = todos.length - shown.length
  return (
    <span className="todo-segs" aria-hidden="true">
      {shown.map((td) => <i key={td.id} className={td.done ? 'f' : ''} />)}
      {rest > 0 && <span className="todo-seg-rest mono">+{rest}</span>}
    </span>
  )
}

const EMPTY: TodoItem[] = []
