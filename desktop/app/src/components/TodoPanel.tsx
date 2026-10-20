import { useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { TodoItem } from '../store/useAppStore'
import { useT } from '../i18n'
import { pickTodoList } from '../lib/todoScope'
import { useFocusTodos } from './useFocusTodos'

// 待办面板：右栏顶部常驻小结（默认折叠成一行进度条+计数，点开展开列表；不悬浮在叙述通道上，
// 避免遮挡实时流式输出）。
//
// **作用域跟随「你在看谁」**（用户 2026-10 报障：子智能体里显示了主会话的 TODO；
// 以及随后拍板的「子智能体等同于切换个 session」）：
//   · 看主会话 → 会话待办（主 agent 的清单，数据源 = store.todos）
//   · 看子会话 → 该子智能体**自己的**清单（它卡片上的 todos 快照 —— 子 agent 是独立
//                上下文，因此是独立 TODO）
// 两者**绝不互相回退**：子 agent 没有清单时整块不渲染，而不是显示主会话的清单。
// 回退会让「这个子智能体没有计划」被显示成「它有一份和主会话一样的计划」，是更难发现的错。
//
// 作用域由面板**自己**解析（不靠调用方传参）：两个调用点各传一份，迟早漂移成
// 「面板在这边显示 A、角标在那边数 B」。判定收在 useFocusTodos + lib/todoScope.pickTodoList。
export default function TodoPanel() {
  const storeTodos = useAppStore((s) => (s.focusedAgent ? EMPTY : s.todos))
  const focus = useFocusTodos()
  // 取值规则单一来源（lib/todoScope.pickTodoList）：agent 作用域绝不回退会话清单
  const todos = pickTodoList(focus?.scope ?? 'main', storeTodos, focus?.todos)
  const [collapsed, setCollapsed] = useState(true)
  const t = useT()

  if (todos.length === 0) return null
  const done = todos.filter((item) => item.done).length
  const pct = Math.round((done / todos.length) * 100)

  return (
    <section className={`todo-panel ${collapsed ? 'collapsed' : ''}`} aria-label={t('todo.title')}>
      <div
        className="todo-hd"
        onClick={() => setCollapsed((v) => !v)}
        role="button"
        aria-expanded={!collapsed}
        tabIndex={0}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            setCollapsed((v) => !v)
          }
        }}
      >
        <span className="todo-title">{t('todo.title')}</span>
        <span className="todo-count mono">{done}/{todos.length}</span>
        <span className="todo-bar" aria-hidden="true">
          <span className="todo-fill" style={{ width: `${pct}%` }} />
        </span>
        <span className="todo-chev" aria-hidden="true">{collapsed ? '▸' : '▾'}</span>
      </div>
      {!collapsed && (
        <ul className="todo-list">
          {todos.map((todo) => {
            const blocked = !todo.done && todo.blockedBy && todo.blockedBy.length > 0
            return (
              <li key={todo.id} className={`todo-item ${todo.done ? 'done' : ''}`}>
                <span className="todo-st" aria-hidden="true">{todo.done ? '✓' : '○'}</span>
                <span className="todo-id mono">{todo.id}</span>
                <span className="todo-txt" title={todo.title}>{todo.title}</span>
                {blocked && <span className="todo-blk mono">{t('todo.blockedBy').replace('{ids}', todo.blockedBy!.join(','))}</span>}
              </li>
            )
          })}
        </ul>
      )}
    </section>
  )
}

const EMPTY: TodoItem[] = []
