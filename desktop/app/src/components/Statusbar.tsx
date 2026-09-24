import { useAppStore, modelWindowOf } from '../store/useAppStore'
import { fmtTokens, fmtUsd } from './ui'
import { useT } from '../i18n'

export default function Statusbar() {
  const t = useT()
  const usage = useAppStore((s) => s.usage)
  const turns = useAppStore((s) => s.turns)
  const ctxTokens = useAppStore((s) => s.ctxTokens)
  const costUsd = useAppStore((s) => s.costUsd)
  const streaming = useAppStore((s) => s.streaming)
  const bridgeStatus = useAppStore((s) => s.bridgeStatus)
  const mode = useAppStore((s) => s.mode)
  const persona = useAppStore((s) => s.persona)
  const model = useAppStore((s) => s.model)
  const settings = useAppStore((s) => s.settings)
  const contextWindow = useAppStore((s) => s.contextWindow)
  const git = useAppStore((s) => (s.git.is_repo ? s.git : s.wsGit ?? s.git))
  const setRightTab = useAppStore((s) => s.setRightTab)
  const setRightExpanded = useAppStore((s) => s.setRightExpanded)
  const window = modelWindowOf(settings, model, contextWindow) // 分母优先 harness 上报（问题六）
  const lastTurn = turns.length > 0 ? turns[turns.length - 1] : undefined
  const totalTokens = usage.input + usage.output
  const cachePct = usage.input > 0 ? ((usage.cacheRead / usage.input) * 100).toFixed(2) : '0.00'
  // 悬停明细：命中/输入/写入三件套（写入只有 Anthropic 协议会上报，其余协议恒 0）。
  // 命中率是比例，写入量才是「前缀是否被整段重写」的直接证据 —— 排查缓存问题时按它看。
  // 1h 档写入量（2026-09-23 补齐维度）：Anthropic 长缓存独有 —— 5m/1h 两种缓存规模的
  // 成本差近一倍（1h 写入价 2× 输入价），tooltip 里能区分才看得懂「重写量」的含义。
  const cache1h = usage.cacheWrite1h > 0 ? t('status.cache1h').replace('{write1h}', fmtTokens(usage.cacheWrite1h)) : ''
  const cacheTitle = t('status.cacheTitle')
    .replace('{hit}', `${fmtTokens(usage.cacheRead)}`)
    .replace('{input}', `${fmtTokens(usage.input)}`)
    .replace('{write}', `${fmtTokens(usage.cacheWrite)}`) + cache1h
  const personaLabel = persona === 'work' ? 'Work' : 'Code'
  return (
    <footer className="statusbar">
      <span>{streaming ? 'running' : 'idle'} · run0</span>
      <span>{t('status.turns')} <span className="mono">{turns.length}</span></span>
      <span>in/out <span className="mono" style={{ color: 'var(--text-2)' }}>{fmtTokens(usage.input)}/{fmtTokens(usage.output)}</span></span>
      <span>total <span className="mono">{fmtTokens(totalTokens)}</span></span>
      <span title={cacheTitle}>{t('status.cache')} <span style={{ color: 'var(--success)' }}>{cachePct}%</span></span>
      <span>ctx <span style={{ color: 'var(--running)' }}>{fmtTokens(ctxTokens ?? 0)}/{fmtTokens(window)}</span></span>
      <span>cost <span style={{ color: 'var(--warning)' }}>{fmtUsd(costUsd)}</span></span>
      <span
        className={`status-git ${git.is_repo && git.clean ? 'clean' : ''}`}
        title={git.is_repo ? `${git.branch}${t('status.clickViewChanges')}` : t('status.notRepo')}
        role="button"
        onClick={() => { setRightTab('git'); setRightExpanded(true) }}
      >⑂ <span className="mono">{git.is_repo ? (git.branch || 'detached') : 'no git'}</span>{git.is_repo && !git.clean && <span className="git-modified"> M {git.modified + git.untracked}</span>}</span>
      <span className="grow" />
      <span style={{ color: 'var(--text-3)' }}>{bridgeStatus === 'running' ? `${personaLabel} · ${model} · ${t('status.mode.' + mode) ?? mode}` : bridgeStatus}</span>
    </footer>
  )
}
