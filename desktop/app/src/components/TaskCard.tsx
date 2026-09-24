// 任务卡（异步任务的外壳）：**一份 DOM，两类宿主**——
//   · 主对话：子 agent 卡（并发后台 agent 的实时过程卡）
//   · 右栏「任务」tab：子 agent 卡 / 后台任务占位卡 / 工具任务卡
//
// 为什么单独成文件（2026-09-23 用户报障「右栏子 agent 卡片的样式和主会话不一样」的第二步）：
// 转录内容先统一到 NarrativeBlocks 之后，两处的**卡壳**仍是两套 —— 主对话是
// `.agent-card`/`.ac-*`/`.ag-*`，右栏是 `.agent-view`/`.av-*`，两份六态映射
// （taskStatusMeta）、两套字号、两套 hover、两套停止按钮文案；更麻烦的是皮肤层
// （narrative-skin.css）只覆盖了前者，换皮肤后右栏任务卡原地不动。
// 现在的约定：**任务卡只有这一份实现**。宿主差异只允许出现在「传什么内容进来」
// （子 agent → 转录；后台任务 → 结果；工具任务 → 参数/结果），不再出现在样式上。
//
// 六态语义、状态色、行首图标（V3）、停止按钮、计时器全部在这里定一次。

import { memo, useState } from 'react'
import type { ReactNode } from 'react'
import { useAppStore } from '../store/useAppStore'
import type { MsgBlock, AsyncTaskStatus, UsageAgg } from '../store/useAppStore'
import { Bot } from './icons'
import { fmtDuration, fmtUsd, UsageChip } from './ui'
import { ElapsedTimer } from './ElapsedTimer'
import { AgentTranscript } from './AgentTranscript'
import { TaskResultInline, ToolIcon } from './NarrativeBlocks'
import { ArtifactCard } from './ArtifactCard'
import JsonView from './JsonView'
import { ToolImages } from './ToolImages'
import { useT } from '../i18n'

/** 异步任务六态 → 图标/文案/卡片类名/是否活动（全仓唯一一份六态映射） */
function taskStatusMeta(status: AsyncTaskStatus, t: (k: string) => string) {
  switch (status) {
    case 'running': return { icon: '▶', label: t('task.status.running'), cls: 'running', active: true }
    case 'interrupting': return { icon: '◼', label: t('task.status.interrupting'), cls: 'interrupting', active: true }
    case 'completed': return { icon: '✓', label: t('task.status.completed'), cls: 'done', active: false }
    case 'interrupted': return { icon: '⏸', label: t('task.status.interrupted'), cls: 'interrupted', active: false }
    case 'failed': return { icon: '✗', label: t('task.status.failed'), cls: 'error', active: false }
    case 'abandoned': return { icon: '○', label: t('task.status.abandoned'), cls: 'abandoned', active: false }
  }
}

export interface TaskCardProps {
  /** 六态（决定状态色、状态图标与胶囊文案） */
  status: AsyncTaskStatus
  /** 标签：子 agent 名 / 工具名（可内嵌徽章，如「后台运行」⬆） */
  label: ReactNode
  /** 标签 title（长名截断时看全名） */
  labelTitle?: string
  /** V3 皮肤的行首图标（默认皮肤不渲染）：子 agent=Bot / 工具任务=按工具名取语义 */
  icon?: ReactNode
  /** 已交付主任务 → 状态胶囊加「 · 已交付」后缀 */
  delivered?: boolean
  usage?: UsageAgg
  /** 思考耗时（有值才显示） */
  thinkMs?: number
  /** 计时起点（子 agent=spawnedAt；工具任务=startedAt） */
  startAt?: number
  /** 终态总耗时（活动态由 ElapsedTimer 自己走） */
  durMs?: number
  /** 覆盖「是否活动」（缺省取六态：running/interrupting 为活动） */
  active?: boolean
  /** 传了才渲染停止按钮（= 这个任务此刻可精确中断） */
  onStop?: () => void
  interrupting?: boolean
  /** 停止按钮文案（缺省「停止该后台子 agent」；工具任务传任务口径的文案） */
  stopTitle?: string
  stopAria?: string
  compressing?: boolean
  /** 重试徽章（文案在调用点算好，这里只渲染） */
  retry?: { message: string; text: string } | null
  /** 展开体；没有内容则整卡不可展开（不渲染 chevron、不响应点击） */
  children?: ReactNode
}

export const TaskCard = memo(function TaskCard({
  status, label, labelTitle, icon, delivered, usage, thinkMs, startAt, durMs,
  active, onStop, interrupting, stopTitle, stopAria, compressing, retry, children,
}: TaskCardProps) {
  const t = useT()
  const [open, setOpen] = useState(false)
  const meta = taskStatusMeta(status, t)
  const isActive = active ?? meta.active
  const canExpand = Boolean(children)
  const toggle = () => { if (canExpand) setOpen((v) => !v) }
  return (
    <div className={`agent-card ${meta.cls}`}>
      <div
        className="ac-main"
        role={canExpand ? 'button' : undefined}
        aria-expanded={canExpand ? open : undefined}
        tabIndex={canExpand ? 0 : undefined}
        onClick={toggle}
        onKeyDown={(e) => {
          if (canExpand && (e.key === 'Enter' || e.key === ' ')) {
            e.preventDefault()
            toggle()
          }
        }}
      >
        {icon && <span className="ic" aria-hidden="true">{icon}</span>}
        <span className={`ag-st ${isActive ? 'busy' : status === 'completed' ? 'ok' : ''}`}>{meta.icon}</span>
        <span className="ag-label" title={labelTitle}>{label}</span>
        <span className="ag-sec mono">{meta.label}{delivered ? t('task.delivered') : ''}</span>
        {compressing && <span className="ag-compressing mono">{t('compression.inProgress')}</span>}
        {retry && <span className="ag-retry mono" title={retry.message}>{retry.text}</span>}
        <UsageChip className="ag-t mono" usage={usage} title={t('task.usage.tip').replace('{tok}', String((usage?.input ?? 0) + (usage?.output ?? 0))).replace('{cost}', fmtUsd(usage?.costUsd ?? 0))} />
        {thinkMs != null && (
          <span className="ag-t mono" title={t('thinking.dur').replace('{dur}', fmtDuration(thinkMs))}>{t('thinking.dur').replace('{dur}', fmtDuration(thinkMs))}</span>
        )}
        {(isActive || durMs != null) && <ElapsedTimer className="ag-t mono" startAt={startAt} running={isActive} durMs={isActive ? undefined : durMs} />}
        {onStop && (
          <button
            className="ag-stop"
            disabled={interrupting}
            title={interrupting ? t('task.stopping') : (stopTitle ?? t('task.stopAgent'))}
            aria-label={stopAria ?? t('task.stopAria')}
            onClick={(e) => {
              e.stopPropagation() // 避免触发展开/收起
              onStop()
            }}
          >
            {interrupting ? '…' : '⏹'}
          </button>
        )}
        {canExpand && <span className="ag-chev">{open ? '▾' : '▸'}</span>}
      </div>
      {canExpand && open && <div className="ac-body">{children}</div>}
    </div>
  )
})

// 子 agent 任务卡：外壳 + 完整活动记录（思考/输出/工具调用）+ 本轮产出 + 任务结果。
// 主对话（Narrative）与右栏「任务」tab（RightPanel）**同一份实现** —— 两处的差别只有
// 「卡片出现在哪里」。
export const AgentTaskCard = memo(function AgentTaskCard({ b }: { b: Extract<MsgBlock, { kind: 'agent' }> }) {
  const t = useT()
  const interruptTask = useAppStore((s) => s.interruptTask)
  const running = b.status === 'running'
  const interrupting = b.status === 'interrupting'
  const canStop = Boolean(b.taskId) && (running || interrupting)
  return (
    <TaskCard
      status={b.status}
      icon={<Bot size={16} />}
      label={b.label}
      delivered={b.deliveredToMain}
      usage={b.usage}
      thinkMs={b.thinkMs}
      startAt={b.spawnedAt}
      durMs={b.durMs}
      compressing={b.compressing}
      retry={b.retrying && b.retryMessage
        ? {
            message: b.retryMessage,
            text: b.retryMax
              ? t('thinking.retryProgress').replace('{n}', String(Math.max(b.retryCount ?? 1, 1))).replace('{m}', String(b.retryMax))
              : t('thinking.retry').replace('{n}', String(Math.max(b.retryCount ?? 1, 1))),
          }
        : null}
      interrupting={interrupting}
      onStop={canStop ? () => interruptTask(b.taskId!) : undefined}
    >
      <AgentTranscript items={b.items} running={running} />
      {b.artifacts && b.artifacts.length > 0 && (
        <div className="ac-artifacts">
          <ArtifactCard items={b.artifacts} {...(b.artifactsTruncated ? { truncated: true } : {})} />
        </div>
      )}
      <TaskResultInline delivered={b.deliveredToMain} result={b.taskResult} error={b.taskError} />
    </TaskCard>
  )
})

// 后台任务卡（右栏「任务」tab 专用）：只有 task_started 占位、还没有 agent 运行数据时
// 的形态 —— 外壳同一份，展开体只有结果（没有转录可看）。
export const AsyncTaskCard = memo(function AsyncTaskCard({ b }: { b: Extract<MsgBlock, { kind: 'async_task' }> }) {
  const t = useT()
  const interruptTask = useAppStore((s) => s.interruptTask)
  const running = b.status === 'running'
  const interrupting = b.status === 'interrupting'
  const hasResult = Boolean(b.taskResult || b.taskError)
  return (
    <TaskCard
      status={b.status}
      icon={b.toolName ? <ToolIcon name={b.toolName} /> : <Bot size={16} />}
      label={b.label}
      delivered={b.deliveredToMain}
      usage={b.usage}
      interrupting={interrupting}
      stopTitle={t('rp.task.stopTask')}
      stopAria={t('rp.task.stopTaskAria')}
      onStop={(running || interrupting) && b.taskId ? () => interruptTask(b.taskId) : undefined}
    >
      {hasResult ? <TaskResultInline delivered={b.deliveredToMain} result={b.taskResult} error={b.taskError} /> : null}
    </TaskCard>
  )
})

// 工具任务卡（右栏「任务」tab 专用）：被摘到后台的工具调用 —— 外壳同一份，
// 展开体 = 主对话工具行的同一套（.tr-sec / .tr-code / 结果图），同一次调用在
// 「对话里的工具行」与这里长得一样。
export const ToolTaskCard = memo(function ToolTaskCard({ b }: { b: Extract<MsgBlock, { kind: 'tool' }> }) {
  const t = useT()
  const interruptTask = useAppStore((s) => s.interruptTask)
  const meta = taskStatusMeta(b.taskStatus ?? (b.status === 'running' ? 'running' : 'completed'), t)
  const active = meta.active
  const canStop = Boolean(b.promoted && b.taskId) && (b.status === 'running' || b.interrupting)
  return (
    <TaskCard
      status={b.taskStatus ?? (b.status === 'running' ? 'running' : 'completed')}
      icon={<ToolIcon name={b.name} />}
      label={<>{b.name}{b.promoted ? <span className="task-promoted" title={t('rp.task.promoted')}>⬆</span> : null}</>}
      labelTitle={b.name}
      usage={b.usage}
      startAt={b.startedAt}
      durMs={b.durMs}
      active={active}
      interrupting={b.interrupting}
      stopTitle={t('rp.task.stopTask')}
      stopAria={t('rp.task.stopTaskAria')}
      onStop={canStop ? () => interruptTask(b.taskId!) : undefined}
    >
      {b.args && (
        <div className="tr-sec">
          <div className="tr-sec-h">{t('tool.args')}</div>
          <JsonView text={b.args} className="json-box" />
        </div>
      )}
      {b.result ? (
        <div className="tr-sec">
          <div className="tr-sec-h">{t('tool.result')}</div>
          <ToolImages images={b.images ?? []} label={t('tool.resultImages')} />
          <pre className="tr-code">{b.result}</pre>
        </div>
      ) : b.status === 'running' ? (
        <div className="tr-sec"><div className="tr-sec-h">{t('tool.executing')}</div></div>
      ) : null}
    </TaskCard>
  )
})
