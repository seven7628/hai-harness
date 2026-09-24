// SpanSheet —— 链路 tab 的 Span 详情抽屉（docs/TRACE_CHAIN_REDESIGN.md P4）。
//
// 两条原则：
//   ① **正文永远单份**：trace 只存元数据（时间/用量/name），正文按 span 的回查锚点
//      （tool: tool_id / llm: request_id）从会话 blocks 里取 —— 不抄进 trace，也不额外存储。
//   ② **内容量收敛**（4 层）：段落头折叠 → 结构感知渲染 → 头尾折叠进定高滚动箱 → 逃生舱。
//      必须区分「UI 折叠了」（展开即有）与「引擎本来就截断了」
//      （工具结果 20 KB 截断 tools/engine.go:17、diff 8 KB tools/builtin/diff.go:13、grep 200 条），
//      后者展开也没有，只能提示去源头看。

import { useMemo, useState } from 'react'
import type { ReactElement } from 'react'
import { fmtDuration, fmtTokens, fmtUsd, UnifiedDiff } from './ui'
import { useT } from '../i18n'
import type { TraceSpan } from '../store/trace'
import type { Diff, MsgBlock } from '../store/useAppStore'

// 引擎截断阈值（与 Go 侧一致；用于区分「UI 折叠」与「引擎截断」）
const ENGINE_TOOL_TRUNC = 20000 // tools/engine.go maxResponseSize
const ENGINE_DIFF_TRUNC = 8192 // tools/builtin/diff.go
const FOLD_HEAD_LINES = 14
const FOLD_TAIL_LINES = 5
const ESCAPE_LINES = 1500 // 超过则不在抽屉展开（改为动作卡）
const ESCAPE_BYTES = 48 * 1024

/** span 对应的会话正文（从 blocks 回查，不是从 trace 里读）。 */
export interface SpanPayload {
  args?: string
  result?: string
  diff?: unknown
  errorInfo?: unknown
  text?: string // llm: 输出正文
  thinking?: string // llm: 思考
}

/**
 * 正文回查：从会话 blocks 找该 span 的正文（不在 trace 里存正文，见设计文档 §7.2）。
 *
 * 锚点：
 *   - tool span：`span.id` = 调用 id → `blocks[].toolId`（tool 块直接存了 toolId）
 *   - llm span：`span.id` = request_id → 视图的 `reqBlock[request_id]` = assistant 块 id
 *     （assistant 块本身不存 request_id，映射在 reqBlock 里，故需调用方传入）
 *   - run span（子 agent）：`span.run_id` → agent 块的 runId
 */
export function lookupPayload(
  blocks: MsgBlock[],
  span: TraceSpan | null,
  reqBlock?: Record<string, number>,
): SpanPayload {
  if (!span) return {}
  if (span.kind === 'tool' && span.id) {
    for (const b of blocks) {
      if (b.kind === 'tool' && b.toolId === span.id) {
        return { args: b.args, result: b.result, diff: b.diff, errorInfo: b.errorInfo }
      }
    }
  }
  if (span.kind === 'llm' && span.id) {
    const bid = reqBlock?.[span.id]
    for (const b of blocks) {
      if (b.kind !== 'assistant') continue
      if ((bid != null && b.id === bid) || (bid == null && (b as { runId?: string }).runId === span.run_id)) {
        const ab = b as { text?: string; thinking?: string }
        return { text: ab.text, thinking: ab.thinking }
      }
    }
  }
  if (span.kind === 'run' && span.run_id) {
    // 子 agent 卡片（blocks 里的 agent 块）承载其最终结果
    for (const b of blocks) {
      if (b.kind === 'agent' && (b as { runId?: string }).runId === span.run_id) {
        const ab = b as { taskResult?: string; taskError?: string }
        return { text: ab.taskResult, result: ab.taskError }
      }
    }
  }
  return {}
}

/** 体积徽章：让「有多少」先可见，再决定看不看。 */
function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

const bytesOf = (s?: string): number => (s ? new TextEncoder().encode(s).length : 0)

interface FoldProps {
  text: string
  /** 引擎是否本来就截断了这段（展开也没有更多） */
  engineTrunc?: number
  title: string
  mono?: boolean
  defaultOpen?: boolean
}

/**
 * Fold —— 头尾折叠的可折叠正文段。
 * 折叠掉的内容**不进 DOM**（不是 line-clamp 视觉裁剪）：94 KB 思考实测只落 ~1 KB 文本。
 */
function Fold({ text, engineTrunc, title, mono, defaultOpen }: FoldProps): ReactElement {
  const t = useT()
  const [open, setOpen] = useState(!!defaultOpen)
  const lines = useMemo(() => text.split('\n'), [text])
  const bytes = bytesOf(text)
  const escaped = lines.length > ESCAPE_LINES || bytes > ESCAPE_BYTES
  const truncatedByEngine = engineTrunc != null && bytes >= engineTrunc * 0.98
  const head = lines.slice(0, FOLD_HEAD_LINES)
  const tail = lines.slice(Math.max(FOLD_HEAD_LINES, lines.length - FOLD_TAIL_LINES))
  const hidden = Math.max(0, lines.length - head.length - tail.length)
  const needFold = lines.length > FOLD_HEAD_LINES + FOLD_TAIL_LINES

  return (
    <div className="sheet-sec">
      <button className="sec-hd" onClick={() => setOpen((o) => !o)} aria-expanded={open}>
        <span className={`sec-chev${open ? '' : ' closed'}`}>
          <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
        </span>
        <span className="sec-title">{title}</span>
        <span className="sec-meta mono">{lines.length} 行 · {fmtBytes(bytes)}</span>
        {truncatedByEngine && <span className="sec-tag" title={t('rp.trace.engineTruncHint')}>{t('rp.trace.engineTrunc')}</span>}
      </button>
      {open && (
        <>
          {escaped && !truncatedByEngine ? (
            // 逃生舱：超长内容不在抽屉里直接展开（DOM 会炸），改为动作卡
            <div className="sec-escape">
              {t('rp.trace.tooLarge').replace('{size}', fmtBytes(bytes))}
            </div>
          ) : null}
          <div className={`sec-body${mono ? ' mono' : ''}${open && needFold && !escaped ? ' folded' : ''}`}>
            <pre>{head.join('\n')}</pre>
            {needFold && !escaped && (
              <>
                <div className="fold-gap">{t('rp.trace.foldedLines').replace('{n}', String(hidden)).replace('{size}', fmtBytes(bytes))}</div>
                <pre>{tail.join('\n')}</pre>
              </>
            )}
          </div>
        </>
      )}
    </div>
  )
}

/** 工具结果的结构感知渲染：diff 走真彩行，其余走可折叠正文。 */
function ResultView({ payload }: { payload: SpanPayload }): ReactElement | null {
  const t = useT()
  // diff.unified 是**字符串**（Go core.FileDiff.Unified；见 desktop/bridge/view_reducer.go:222）。
  // 曾按 {t,s}[] 行对象渲染 → diff.unified.map is not a function（edit_file 这类带 diff 的工具必崩）。
  const diff = payload.diff as Diff | undefined
  const unified = diff?.unified
  if (unified) {
    const lines = unified.split('\n')
    const bytes = bytesOf(unified)
    // 截断判定：引擎显式标记优先（diff.go 超 8 KB 截断并附 [file diff truncated]），
    // 其次按体积兜底（旧事件无 truncated 字段）。
    const engineCut = diff?.truncated === true || unified.includes('[file diff truncated') || bytes >= ENGINE_DIFF_TRUNC * 0.98
    return (
      <div className="sheet-sec">
        <div className="sec-hd static">
          <span className="sec-title">{t('rp.trace.diff')}</span>
          <span className="sec-meta mono">{diff?.path ? `${diff.path} · ` : ''}{lines.length} 行 · {fmtBytes(bytes)}</span>
          {engineCut && <span className="sec-tag" title={t('rp.trace.engineTruncHint')}>{t('rp.trace.engineTrunc')}</span>}
        </div>
        <div className="sec-body diff-body">
          <UnifiedDiff unified={unified} />
        </div>
      </div>
    )
  }
  if (!payload.result) return null
  return <Fold text={payload.result} engineTrunc={ENGINE_TOOL_TRUNC} title={t('rp.trace.result')} mono defaultOpen />
}

interface Props {
  span: TraceSpan | null
  payload: SpanPayload
  onClose: () => void
}

/** 抽屉：按 kind 分支渲染（run / llm / tool / compress / wait）。 */
export default function SpanSheet({ span, payload, onClose }: Props): ReactElement | null {
  const t = useT()
  if (!span) return null
  const kindLabel: Record<string, string> = {
    run: t('rp.trace.kind.run'),
    llm: t('rp.trace.kind.llm'),
    tool: t('rp.trace.kind.tool'),
    compress: t('rp.trace.kind.compress'),
    wait: t('rp.trace.kind.wait'),
  }
  return (
    <>
      <div className="sheet-scrim" onClick={onClose} />
      <section className="span-sheet open" role="dialog" aria-modal="true" aria-label={t('rp.trace.detail')}>
        <div className="sheet-hd">
          <span className={`kindchip k-${span.kind}`}>{kindLabel[span.kind] ?? span.kind}</span>
          <span className="sheet-title">{span.label ?? span.name ?? span.id ?? ''}</span>
          <span className={`glyph ${span.status === 'error' ? 'bad' : span.status === 'running' ? 'busy' : span.status === 'interrupted' ? 'warn' : 'ok'}`}>
            {span.status === 'error' ? '✗' : span.status === 'running' ? '▶' : span.status === 'interrupted' ? '⏸' : '✓'}
          </span>
          <button className="sheet-x" onClick={onClose} aria-label={t('rp.trace.close')}>×</button>
        </div>

        {/* 通用：时间 / 时长 / 归属 */}
        <div className="kv">
          <div className="kv-i"><span>{t('rp.trace.offset')}</span><b className="mono">+{fmtDuration(span.start_at)}</b></div>
          <div className="kv-i"><span>{t('rp.trace.duration')}</span><b className="mono">{fmtDuration(span.dur_ms)}</b></div>
          {span.run_id && <div className="kv-i"><span>run</span><b className="mono">{span.run_id}</b></div>}
          {span.depth ? <div className="kv-i"><span>depth</span><b className="mono">{span.depth}</b></div> : null}
          {span.task_id && <div className="kv-i"><span>task</span><b className="mono">{span.task_id}</b></div>}
          {span.retries ? <div className="kv-i"><span>{t('rp.trace.retriesLabel')}</span><b className="mono">{span.retries}</b></div> : null}
        </div>

        {/* kind 分支 */}
        {span.kind === 'llm' && (
          <div className="kv">
            {span.model && <div className="kv-i"><span>{t('rp.trace.model')}</span><b className="mono">{span.model}</b></div>}
            <div className="kv-i"><span>{t('rp.trace.inTok')}</span><b className="mono">{fmtTokens(span.in_tok ?? 0)}</b></div>
            <div className="kv-i"><span>{t('rp.trace.outTok')}</span><b className="mono">{fmtTokens(span.out_tok ?? 0)}</b></div>
            {span.cache_tok ? <div className="kv-i"><span>{t('rp.trace.cacheTok')}</span><b className="mono">{fmtTokens(span.cache_tok)}</b></div> : null}
            {span.cache_write_tok ? <div className="kv-i"><span>{t('rp.trace.cacheWriteTok')}</span><b className="mono">{fmtTokens(span.cache_write_tok)}</b></div> : null}
            {span.reason_tok ? <div className="kv-i"><span>{t('rp.trace.reasonTok')}</span><b className="mono">{fmtTokens(span.reason_tok)}</b></div> : null}
            {span.cost_usd ? <div className="kv-i"><span>{t('rp.trace.cost')}</span><b className="mono">{fmtUsd(span.cost_usd)}</b></div> : null}
          </div>
        )}
        {span.kind === 'compress' && (
          <div className="kv">
            <div className="kv-i"><span>{t('rp.trace.before')}</span><b className="mono">{span.before ?? '—'}</b></div>
            <div className="kv-i"><span>{t('rp.trace.after')}</span><b className="mono">{span.after ?? '—'}</b></div>
            {span.ctx_tokens ? <div className="kv-i"><span>{t('rp.trace.ctxTokens')}</span><b className="mono">{fmtTokens(span.ctx_tokens)}</b></div> : null}
            {span.reason ? <div className="kv-i"><span>{t('rp.trace.reason')}</span><b className="mono">{span.reason}</b></div> : null}
          </div>
        )}
        {span.kind === 'wait' && (
          <div className="kv">
            <div className="kv-i"><span>{t('rp.trace.waitType')}</span><b className="mono">{span.wait_type ?? 'ask_user'}</b></div>
            {span.questions ? <div className="kv-i"><span>{t('rp.trace.questions')}</span><b className="mono">{span.questions}</b></div> : null}
          </div>
        )}

        {/* tool：参数 + 结果 */}
        {span.kind === 'tool' && (
          <>
            {payload.args && <Fold text={prettyJson(payload.args)} title={t('rp.trace.args')} mono />}
            {payload.result ? (
              <ResultView payload={payload} />
            ) : (
              <div className="sheet-sec"><div className="sec-empty">{t('rp.trace.noPayload')}</div></div>
            )}
            {payload.errorInfo ? (
              <div className="sheet-sec">
                <div className="sec-hd static"><span className="sec-title">{t('rp.trace.errorInfo')}</span></div>
                <div className="sec-body mono"><pre>{prettyJson(JSON.stringify(payload.errorInfo))}</pre></div>
              </div>
            ) : null}
          </>
        )}

        {/* llm：思考 + 输出 */}
        {span.kind === 'llm' && (
          <>
            {payload.thinking && <Fold text={payload.thinking} title={t('rp.trace.thinking')} />}
            {payload.text && <Fold text={payload.text} title={t('rp.trace.output')} defaultOpen />}
            {!payload.thinking && !payload.text && <div className="sheet-sec"><div className="sec-empty">{t('rp.trace.noPayload')}</div></div>}
          </>
        )}

        {/* run：目标 / 子 span 概览 */}
        {span.kind === 'run' && (
          <>
            {payload.text && <Fold text={payload.text} title={t('rp.trace.goal')} />}
            {payload.result && <Fold text={payload.result} title={t('rp.trace.errorInfo')} />}
            {!payload.text && !payload.result && <div className="sheet-sec"><div className="sec-empty">{t('rp.trace.noPayload')}</div></div>}
          </>
        )}
      </section>
    </>
  )
}

/** 参数美化：JSON 可解析则缩进（便于阅读长参数），否则原样。 */
function prettyJson(s: string): string {
  try {
    const v = JSON.parse(s)
    return JSON.stringify(v, null, 2)
  } catch {
    return s
  }
}
