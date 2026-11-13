import { useMemo, useState } from 'react'
import { useHubStore } from './useHubStore'
import { activeTodos, type HubInlineScope, type HubTodoCard } from '../lib/hubTodoScope'
import './HubInlineTodos.css'

// Hub 单聊/群内的 **Project 待办** 内联卡（场景2）。
//
// 数据来源与分工（改这里之前先读 lib/hubTodoScope.ts 的头部）：
//   · 事件：Project 会话的 `tool_run_end`（自带该 run 的 todos 快照）与 `agent_start`
//     （run → 子智能体名），由 useHubStore.handleEvent 在**所有 sid 闸门之前**收；
//   · 认领：run 的快照按 (session, run) 落表，渲染时用任务表的 target 认领回来；
//   · 本组件只读：不发命令、不写 hub_message / refs，因此待办卡不会进群快照、
//     不会进任何智能体的上下文（与 HubInlineNeeds 同一条约束）。
//
// 空间口径（390 下的硬要求）：卡片区自己限高、可滚动、单卡可折叠。这里**不**把
// hub.css 的 composer 布局改大 — 消息区必须仍有高度（.messages flex:1 min-height:0
// 会被过高的 composer 压成 0）。

function attr(card: HubTodoCard): string {
  const ws = card.workspace
  const project = ws ? `${ws.split('/').filter(Boolean).pop() ?? ws}${card.sessionId ? ` / ${card.sessionId}` : ''}` : card.sessionId
  // 主 run 写「主会话」；见过的子 run 写「子智能体 X」；只认识 run id 时原样写
  // run_xxxx（不知道归属就不冒充子智能体，更不冒充主会话）。
  const sub = card.mainRun
    ? '主会话'
    : card.runKnown && card.runName ? `子智能体 ${card.runName}` : card.runName
  return [card.agentName, project, sub].filter(Boolean).join(' · ')
}

export default function HubInlineTodos({ scope }: { scope: HubInlineScope }) {
  const tasks = useHubStore((s) => s.tasks)
  const snapshots = useHubStore((s) => s.todoSnapshots)
  const runNames = useHubStore((s) => s.runNames)
  // 折叠状态按卡片键记（任务 + run）：同一任务换 run 就是另一张卡，默认展开。
  const [folded, setFolded] = useState<Record<string, boolean>>({})

  const cards = useMemo(
    () => activeTodos(tasks, snapshots, scope, runNames),
    [tasks, snapshots, scope, runNames],
  )

  if (cards.length === 0) return null

  return (
    <section className="hub-inline-todos" aria-label="项目任务待办（只读）">
      {cards.map((card) => {
        const collapsed = folded[card.key] === true
        const pct = card.total > 0 ? Math.round((card.done / card.total) * 100) : 0
        const listID = `hub-inline-todo-list-${card.taskId}`
        return (
          <article className="hub-inline-todo" key={card.key}>
            <header className="hub-inline-todo-head">
              <span className="hub-inline-todo-title">ToDo · {card.done}/{card.total}</span>
              <span className="hub-inline-todo-attr" title={attr(card)}>{attr(card)}</span>
              <button
                type="button"
                className="hub-inline-todo-fold"
                aria-expanded={!collapsed}
                aria-controls={listID}
                title={collapsed ? '展开待办' : '收起待办'}
                onClick={() => setFolded((prev) => ({ ...prev, [card.key]: !collapsed }))}
              >
                {collapsed ? '展开' : '收起'}
              </button>
            </header>
            {/* 进度条只在有清单时出现：0 条的卡片本来就不会渲染。 */}
            <span
              className="hub-inline-todo-bar"
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={card.total}
              aria-valuenow={card.done}
              aria-label={`已完成 ${card.done} / ${card.total}`}
            >
              <span className="hub-inline-todo-bar-fill" style={{ width: `${pct}%` }} />
            </span>
            {collapsed ? null : (
              <ul className="hub-inline-todo-list" id={listID}>
                {card.todos.map((item) => (
                  <li
                    className={item.done ? 'hub-inline-todo-item done' : 'hub-inline-todo-item'}
                    key={item.id}
                  >
                    <span
                      className="hub-inline-todo-box"
                      role="img"
                      aria-label={item.done ? '已完成' : '未完成'}
                    >
                      {item.done ? '✓' : ''}
                    </span>
                    <span className="hub-inline-todo-text" title={item.title}>{item.title}</span>
                  </li>
                ))}
                {card.more > 0 ? (
                  <li className="hub-inline-todo-more">+{card.more}</li>
                ) : null}
              </ul>
            )}
          </article>
        )
      })}
    </section>
  )
}
