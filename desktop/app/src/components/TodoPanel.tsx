import { useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'

// 待办面板：右栏顶部常驻小结（默认折叠成一行进度条+计数，点开展开列表；不悬浮在叙述通道上，
// 避免遮挡实时流式输出）。数据源 = 桥接层在 todo_* 工具结束时随事件带的 todos 字段
// （SDK todo.Item：id/title/done/blocked_by）。已完成 ✓ 灰显删除线，未开始 ○，
// 被依赖阻塞显示「阻塞 tN」标签。
export default function TodoPanel() {
  const todos = useAppStore((s) => s.todos)
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
