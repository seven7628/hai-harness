import { useEffect, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import type { Approval } from '../store/useAppStore'
import { UnifiedDiff } from './ui'
import { prettyArgs } from '../lib/toolSummary'
import JsonView from './JsonView'

// 审批（HITL）等待窗口的兜底值（秒）：**只在事件没带 timeout_ms 时**用于倒计时
// （老事件/其他宿主）。桌面端桥接层会注入 timeout_ms（= 后端审批等待窗口，2026-09-20 起 30 分钟）。
export const APPROVAL_TIMEOUT = 60

// 共享审批卡片：主对话停靠面板与右栏任务 Tab 都渲染它（同一队首 approvals[0]）。
// 决策一律走 useAppStore.getState().approve —— 乐观移除队列项，两处同时消失，
// 配合 approve 的幂等守卫不会重复决策。
// ownerLabel：运行归属标签（右栏传「主 Agent / 子 Agent 名」，停靠面板可省略）。
// folded/onToggleFold：停靠面板可收起（只留标题条，把版面让给叙述区）；右栏不传 → 行为不变。
export default function ApprovalCard({ a, ownerLabel, folded, onToggleFold }: {
  a: Approval
  ownerLabel?: string
  folded?: boolean
  onToggleFold?: () => void
}) {
  const t = useT()
  const [args, setArgs] = useState('')
  const [showArgs, setShowArgs] = useState(false)
  const approve = useAppStore((s) => s.approve)
  // 倒计时窗口：事件带的 timeout_ms 优先（与后端同源），缺省回退兜底值
  const windowMs = (a.timeoutMs && a.timeoutMs > 0 ? a.timeoutMs : APPROVAL_TIMEOUT * 1000)
  const startedAt = a.arrivedAt ?? Date.now()
  const [elapsedMs, setElapsedMs] = useState(() => Date.now() - startedAt)
  const left = Math.max(0, Math.ceil((windowMs - elapsedMs) / 1000))

  useEffect(() => {
    // 默认值 = 美化排版后的参数（可读；编辑后按实际内容批准）
    setArgs(prettyArgs(a.arguments ?? ''))
    setShowArgs(false)
  }, [a.id])

  // 倒计时：按「窗口 - 已过」算（每秒走）。用 startedAt 而非自增计数 —— 切会话/右栏来回
  // 重挂组件时不会把头 60 秒重新开始（重挂即重置会让自动拒绝永远追不上真实时间）。
  useEffect(() => {
    setElapsedMs(Date.now() - startedAt)
    const timer = setInterval(() => setElapsedMs(Date.now() - startedAt), 1000)
    return () => clearInterval(timer)
  }, [a.id, startedAt])

  // 超时 = 自动拒绝（镜像后端；后端也拒绝，前端只是提示与兜底裁决入口）
  useEffect(() => {
    if (left <= 0) approve(a.id, false)
  }, [left, a.id])

  const baseArgs = prettyArgs(a.arguments ?? '')
  const edited = args !== baseArgs
  const diff = a.diff
  const hasDiff = Boolean(diff && diff.unified)
  let path = ''
  try {
    path = JSON.parse(a.arguments)?.path ?? ''
  } catch {
    /* 非 JSON 参数：无路径展示 */
  }
  return (
    <div className={`card ${hasDiff ? 'wide' : ''}`}>
      <h3
        className={onToggleFold ? 'appr-hd' : undefined}
        role={onToggleFold ? 'button' : undefined}
        tabIndex={onToggleFold ? 0 : undefined}
        aria-expanded={onToggleFold ? !folded : undefined}
        title={onToggleFold ? (folded ? t('approval.expand') : t('approval.fold')) : undefined}
        onClick={onToggleFold}
        onKeyDown={onToggleFold ? (e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            onToggleFold()
          }
        } : undefined}
      >
        {onToggleFold && <span className="chev" aria-hidden="true">{folded ? '▸' : '▾'}</span>}
        <span style={{ color: 'var(--warning)' }}>✦</span> {t('approval.pending').replace('{name}', a.name)}
        {ownerLabel && <span className="appr-owner mono">　{ownerLabel}</span>}
        {path && <span className="mono" style={{ color: 'var(--text-3)', fontWeight: 400, fontSize: 12 }}>　{path}</span>}
        {folded && (
          <span className="appr-fold-hint">
            {t('approval.folded').replace('{left}', fmtLeft(left))}
          </span>
        )}
      </h3>
      {folded ? null : hasDiff ? (
        <>
          <div className="appr-diff">
            <div className="appr-diff-hd">
              <span>{t('approval.diffPreview')}</span>
              <span className="mono">
                <span className="add">+{diff!.added}</span> <span className="del">-{diff!.removed}</span>
                {diff!.unified && diff!.unified.includes('[file diff truncated') && <span className="trunc">{t('approval.diffTruncated')}</span>}
              </span>
            </div>
            <UnifiedDiff unified={diff!.unified!} className="appr-scroll" />
          </div>
          <button className="appr-args-toggle" onClick={() => setShowArgs((v) => !v)} aria-expanded={showArgs}>
            {showArgs ? t('approval.toggleArgs') : t('approval.viewArgs')}
          </button>
          {showArgs && <ArgsEditor args={args} onChange={setArgs} />}
        </>
      ) : (
        <ArgsEditor args={args} onChange={setArgs} />
      )}
      <div className="acts">
        <span className="timeout">{t('approval.autoRejectIn').replace('{left}', fmtLeft(left))}</span>
        <button className="btn" onClick={() => approve(a.id, false)}>{t('approval.reject')}</button>
        <button className="btn primary" onClick={() => approve(a.id, true, edited ? args : undefined)}>
          {edited ? t('approval.approveEdited') : t('approval.approve')}
        </button>
      </div>
    </div>
  )
}

// fmtLeft 倒计时的人类可读写法：>= 1 分钟用 M:SS（30 分钟窗口下 "29:58" 比 "1798s" 直观），
// 不足 1 分钟仍按秒（"37s"）——与原 60 秒窗口的观感保持一致。
function fmtLeft(sec: number): string {
  if (sec < 60) return `${sec}s`
  const m = Math.floor(sec / 60)
  const s = sec % 60
  return `${m}:${String(s).padStart(2, '0')}`
}

// ArgsEditor 参数展示/编辑：默认语法高亮预览（可折叠看全），可切「编辑」改参数。
function ArgsEditor({ args, onChange }: { args: string; onChange: (v: string) => void }) {
  const t = useT()
  const [mode, setMode] = useState<'view' | 'edit'>('view')
  return (
    <div className="appr-args">
      <div className="appr-args-hd">
        <span className="param">arguments</span>
        <button className="appr-mode" onClick={() => setMode(mode === 'view' ? 'edit' : 'view')} title={mode === 'view' ? t('approval.switchEdit') : t('approval.switchView')}>
          {mode === 'view' ? t('approval.edit') : t('approval.preview')}
        </button>
      </div>
      {mode === 'view' ? (
        <JsonView text={args} className="json-box" />
      ) : (
        <textarea
          name="arguments"
          value={args}
          onChange={(e) => onChange(e.target.value)}
          aria-label={t('approval.argsAria')}
          spellCheck={false}
        />
      )}
    </div>
  )
}
