import { useEffect, useRef, type KeyboardEvent } from 'react'
import AgentSigil from './AgentSigil'
import type { MentionAgent } from './mentions'

// @ 成员选择器（P3b-07，方案 §8.3.3 / §8.3.5）。
//
// 焦点留在 composer 的 textarea 上：选择器只用 role=listbox/option 与
// aria-activedescendant 暴露「当前项」，不抢焦点、不做 focus trap（Esc 关闭后
// Tab 照常离开输入区）。键盘事件由 GroupView 的 textarea 与这里的 onKeyDown 共用
// 同一套语义（↑↓ 移动 / Enter·Tab 确认 / Esc 关闭）。

export interface MentionPickerProps {
  /** listbox id；textarea 的 aria-controls 指向它。 */
  id: string
  candidates: MentionAgent[]
  activeIndex: number
  onActiveIndex: (index: number) => void
  onPick: (agent: MentionAgent) => void
  onClose: () => void
  /** 空查询时的标题（派任务/对话共用一份）。 */
  label?: string
}

export default function MentionPicker({
  id,
  candidates,
  activeIndex,
  onActiveIndex,
  onPick,
  onClose,
  label = '提及成员',
}: MentionPickerProps) {
  const active = candidates[Math.max(0, Math.min(activeIndex, candidates.length - 1))]
  const activeId = active ? optionId(id, candidates.indexOf(active)) : undefined
  const listRef = useRef<HTMLUListElement>(null)

  // 键盘移动时把当前项滚进可视区（长名单/矮弹层，N8）。
  useEffect(() => {
    const el = listRef.current?.querySelector('[aria-selected="true"]')
    if (el && typeof (el as HTMLElement).scrollIntoView === 'function') {
      ;(el as HTMLElement).scrollIntoView({ block: 'nearest' })
    }
  }, [activeId])

  function onKeyDown(e: KeyboardEvent<HTMLUListElement>): void {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      if (candidates.length === 0) return
      const step = e.key === 'ArrowDown' ? 1 : -1
      const next = (Math.max(0, activeIndex) + step + candidates.length) % candidates.length
      onActiveIndex(next)
      return
    }
    if (e.key === 'Enter' || e.key === 'Tab') {
      if (!active) return
      e.preventDefault()
      onPick(active)
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
    }
  }

  return (
    <div className="hub-mention-picker" role="presentation">
      <div className="hub-mention-picker-head">
        <span>{label}</span>
        <span className="hub-mention-picker-hint">↑↓ 选择 · Enter/Tab 确认 · Esc 关闭</span>
      </div>
      {candidates.length === 0 ? (
        <div className="hub-mention-empty" role="status">没有匹配的成员</div>
      ) : (
        <ul
          ref={listRef}
          id={id}
          className="hub-mention-list"
          role="listbox"
          aria-label={label}
          aria-activedescendant={activeId}
          tabIndex={-1}
          onKeyDown={onKeyDown}
        >
          {candidates.map((agent, i) => {
            const groupRole = agent.groupRole || ''
            // @all 的合成候选只要一行「所有人」，别再拼 groupRole/职责。
            const roleText = groupRole === 'all'
              ? ''
              : [groupRole === 'owner' ? 'owner' : groupRole === 'member' ? 'member' : groupRole, agent.role]
                .filter(Boolean)
                .join(' · ')
            return (
              <li
                key={agent.id}
                id={optionId(id, i)}
                role="option"
                aria-selected={i === activeIndex}
                className={`hub-mention-option ${i === activeIndex ? 'on' : ''}`}
                onMouseEnter={() => onActiveIndex(i)}
                // mousedown + preventDefault：选择时 textarea 不失焦（否则 caret 丢失）。
                onMouseDown={(e) => {
                  e.preventDefault()
                  onPick(agent)
                }}
              >
                <AgentSigil agentId={agent.id} name={agent.name} responsibility={agent.role} status={false} size={24} />
                <span className="hub-mention-option-name">{agent.name}</span>
                {roleText ? (
                  <span className="hub-mention-option-role">{roleText}</span>
                ) : groupRole === 'all' ? null : (
                  <span className="hub-mention-option-role">成员</span>
                )}
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}

function optionId(listboxId: string, index: number): string {
  return `${listboxId}-opt-${index}`
}
