import { memo, useEffect, useMemo, useRef, useState } from 'react'
import { fmtDuration, fmtRun } from './ui'
import { copyText } from '../lib/clipboard'
import { useT } from '../i18n'
import { useAppStore } from '../store/useAppStore'
import { useClock } from '../lib/useNow'
import {
  DEFAULT_SCRIPT_TIMEOUT_MS,
  NESTED_ARGS_PREVIEW_CHARS,
  NESTED_COLLAPSED_LIMIT,
  NESTED_ERROR_PREVIEW_CHARS,
  OUTPUT_PREVIEW_LINES,
  flowBeads,
  formatWall,
  oneLine,
  parseFoundTools,
  parseScriptEnvelope,
  parseScriptOptions,
  pickNestedCalls,
  previewChars,
  previewVisualLines,
  resultDigest,
  resultMetricKey,
  scriptCallSites,
  scriptSummary,
  type CodemodeTier,
  type NestedCallView,
} from '../lib/codemodeNested'

// codemode（编排型工具）工具行内的两块附加渲染 —— 设计文档 §17「交互与渲染（宿主侧）」：
//   1. ToolNestedCalls   ：脚本内发生的嵌套调用清单（折叠态最后 8 条 + "…N earlier calls"）；
//   2. ToolOutputPreview ：脚本文本输出的折叠预览（按**视觉行**限 5 行）+ 单独一行点名 spill 路径。
//
// 为什么放在工具行里而不是各自占一行：脚本内的子调用**不是**模型发起的工具调用
//（pi renderer.ts:6-7 的原话：nested calls never reach the model as tool calls），
// 所以它们只以「清单条目」的形式挂在**外层那一行**下面；store 侧同样吞掉了带
// parent_call_id 的事件（见 useAppStore 的 tool_start 契约注释）。
//
// 两个组件都被 NarrativeBlocks.ToolRow 使用 —— 主对话与子 agent 转录因此天然同款
//（该文件的头注释：数据来源可以有两处，渲染只能有一套）。

/** 折叠态的嵌套调用清单（数据来自外层 ToolResponse.NestedCalls）。
 *
 *  三块信息按「从一眼到细节」排列：
 *    · 头部：条数 / 失败数 / **脚本内检索次数**（discover 只在这里出现 —— 保留操作不是工具
 *      调用，所以它永远不会变成一条清单行；不在这说一句，纯检索脚本在这里就是一片空白）；
 *    · 珠链 `.cm-flow`：一条脚本**顺序**走过哪些工具（取头部，最多 6 颗 + `+N`）——
 *      这是 codemode 与单个 bash 行最直观的分野，只在 ≥2 条时出现（1 条时明细本身就是那一眼）；
 *    · 明细 `.tn-body`：折叠态最后 8 条 + 「…N earlier calls」（取尾部：错误与收尾最要紧）。
 *  珠链取头部、明细取尾部是**刻意相反**的，理由见 lib/codemodeNested.ts 的 flowBeads。
 */
export const ToolNestedCalls = memo(function ToolNestedCalls({ calls, discover = 0 }: { calls: NestedCallView[]; discover?: number }) {
  const t = useT()
  const [open, setOpen] = useState(false)
  // 折叠态 = 最后 8 条 + 一行「…前面还有 N 条」（N 由纯函数给出，见 pickNestedCalls）
  const { visible, hidden } = pickNestedCalls(calls, !open)
  const failed = calls.filter((c) => c.isError).length
  const foldable = calls.length > NESTED_COLLAPSED_LIMIT
  const beads = flowBeads(calls)
  return (
    <div className="tool-nested">
      <div
        className="tn-hd"
        role="button"
        aria-expanded={open}
        tabIndex={0}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            setOpen((v) => !v)
          }
        }}
      >
        <span className="chev">{open ? '▾' : '▸'}</span>
        <span className="tn-label">{t('tool.nested.title')}</span>
        <span className="tn-count mono">{calls.length}</span>
        {failed > 0 && <span className="tn-failed">{t('tool.nested.failed').replace('{n}', String(failed))}</span>}
        {discover > 0 && (
          <span className="tn-discover mono" title={t('tool.codemode.discoverHint')}>
            {t('tool.codemode.discover').replace('{n}', String(discover))}
          </span>
        )}
        <span className="grow" />
        {foldable && (
          <button
            className="tn-more"
            onClick={(e) => {
              e.stopPropagation() // 避免连点两次（按钮 + 头部的点击各自切换一次）
              setOpen((v) => !v)
            }}
          >
            {open ? t('tool.nested.collapseAll') : t('tool.nested.expandAll').replace('{n}', String(calls.length))}
          </button>
        )}
      </div>
      <div className="tn-body">
        {/* 珠链：一眼看完「这段脚本按顺序碰了什么」（≥2 条才有意义） */}
        {calls.length > 1 && (
          <div className="cm-flow">
            {beads.visible.map((c, i) => (
              <span key={c.id || `${c.name}-flow-${i}`} className="cm-flow-item">
                {i > 0 && <span className="cm-flow-arrow" aria-hidden="true">→</span>}
                <span
                  className={`cm-bead ${c.isError ? 'err' : 'ok'}`}
                  title={`${c.name}${c.durMs != null ? ` · ${fmtDuration(c.durMs)}` : ''}${c.isError ? ` · ${t('tool.nested.failed').replace('{n}', '1')}` : ''}`}
                >
                  <span className="mono">{c.name}</span>
                </span>
              </span>
            ))}
            {beads.more > 0 && <span className="cm-flow-more mono">+{beads.more}</span>}
          </div>
        )}
        {/* 「…N earlier calls」在被折叠的那一段的**上方** —— 它描述的正是上面省掉的内容 */}
        {hidden > 0 && <div className="tn-earlier">{t('tool.nested.earlier').replace('{n}', String(hidden))}</div>}
        {visible.map((c, i) => (
          <NestedCallLine key={c.id || `${c.name}-${i}`} call={c} />
        ))}
      </div>
    </div>
  )
})

// 单条嵌套调用：状态图标 + 工具名 + args 预览 + 耗时（对齐 pi renderer.ts:51-63 的行格式）。
// args 预览 200 字符、错误 500 字符（超限带省略号，见 lib/codemodeNested.ts 的常量与理由）。
const NestedCallLine = memo(function NestedCallLine({ call }: { call: NestedCallView }) {
  const args = previewChars(oneLine(call.args), NESTED_ARGS_PREVIEW_CHARS)
  // 出错时把错误文本单独一行显示（错误是这条调用唯一需要人看的东西）；
  // 错误文本为空（工具没给）回退结果文本 —— 两者都是引擎已截断过的原文。
  const detail = call.isError ? previewChars(oneLine(call.error || call.result), NESTED_ERROR_PREVIEW_CHARS) : undefined
  return (
    <div className={`tn-call${call.isError ? ' err' : ''}`}>
      <div className="tn-call-line">
        <span className={`st ${call.isError ? 'err' : 'ok'}`}>{call.isError ? '✗' : '✓'}</span>
        <span className="t mono">{call.name}</span>
        {args.text && <span className="args mono" title={call.args}>{args.text}</span>}
        <span className="grow" />
        {call.durMs != null && <span className="ms mono">{fmtDuration(call.durMs)}</span>}
      </div>
      {detail?.text && <div className="tn-err mono" title={call.error || call.result}>{detail.text}</div>}
    </div>
  )
})

// —— 执行中：脚本控制台条（`.tool-run`）——————————————————————————————————
//
// 为什么不复用通用行的「…」转圈：codemode 的墙钟预算是脚本**自己声明**的（首行
// `// @options: {"timeout_ms": …}`，缺省 30s），几十秒里「还有多久被杀」是这一行最该回答
// 的问题；而 `.cm-flow`/明细要等结果回来才有数据。于是执行中显示三件事：
//   ① 已用 / 预算（真实比值 → 进度条，超时那一刻正好走满，是可信的信号，不是动画）；
//   ② 脚本里的**调用点**（静态分析，标注为「脚本内的调用点」，不做假实时）；
//   ③ 呼吸的插入符（这一段唯一「在动」的东西就是它 —— 动效只表达「还活着」）。
//
// 时钟纪律：只有这个组件订阅 useClock（1s tick），与 ElapsedTimer 同一个约定 ——
// 每秒重渲的范围必须止步于此，不能把正文/工具行拖下水。
export const ToolScriptRun = memo(function ToolScriptRun({ script, startedAt }: { script?: string; startedAt?: number }) {
  const t = useT()
  const opts = useMemo(() => parseScriptOptions(script), [script])
  const sites = useMemo(() => scriptCallSites(script), [script])
  const budgetMs = opts?.timeoutMs ?? DEFAULT_SCRIPT_TIMEOUT_MS
  const elapsed = useClock(startedAt, startedAt != null)
  const used = Math.max(0, Math.min(elapsed ?? 0, budgetMs))
  const pct = budgetMs > 0 ? (used / budgetMs) * 100 : 0
  return (
    <div className="tool-run">
      <div className="run-hd">
        <span className="run-caret" aria-hidden="true" />
        <span className="run-label">{t('tool.codemode.running')}</span>
        <span className="grow" />
        {elapsed != null && (
          <span className="run-clock mono" title={t('tool.codemode.budgetHint')}>
            {/* 两个数用同一把尺（fmtRun：秒级、≥1min 出分）——混用 fmtDuration 会得到
                「1ms / 1m」这种让人怀疑计时器坏了的读数（0ms 会被兜底成 1ms）。 */}
            {`${fmtRun(elapsed)} / ${fmtRun(budgetMs)}`}
          </span>
        )}
      </div>
      <div
        className="run-bar"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={budgetMs}
        aria-valuenow={used}
        aria-label={t('tool.codemode.budgetHint')}
      >
        <i style={{ width: `${pct}%` }} />
      </div>
      {sites.length > 0 && (
        <div className="run-sites" title={t('tool.codemode.sitesHint')}>
          {sites.map((s) => (
            <span key={`${s.kind}:${s.name}`} className={`run-site ${s.kind}`}>
              <span className="run-site-mark" aria-hidden="true">{s.kind === 'discover' ? '⌕' : '▸'}</span>
              <span className="mono">{s.name}</span>
            </span>
          ))}
        </div>
      )}
    </div>
  )
})

// 折叠态默认的视觉行宽（字符单元格）：首帧还没量到真实宽度时的兜底。
// 88 ≈ 主对话列（约 620px）在 11.5px 等宽字体下的容量，量到真实值前也不会炸屏。
const FALLBACK_CELLS = 88
// 探针长度：一个足够长的 '0' 串，除以后得到单字符像素宽（越短误差越大）。
const PROBE_LEN = 32

/** 脚本文本输出的折叠预览：**结果优先** —— 表头收成一行 chip，5 行预算全给返回值，
 *  下方点名 spill 路径与「查看脚本」。 */
export const ToolOutputPreview = memo(function ToolOutputPreview({ text, spillPath, script, nestedCount = 0, tier = 'execute' }: { text: string; spillPath?: string; script?: string; nestedCount?: number; tier?: CodemodeTier }) {
  const t = useT()
  const showToast = useAppStore((s) => s.showToast)
  const preRef = useRef<HTMLPreElement>(null)
  const probeRef = useRef<HTMLSpanElement>(null)
  const [cells, setCells] = useState(FALLBACK_CELLS)
  const [scriptOpen, setScriptOpen] = useState(false)

  // 表头剥离与摘要都只在 text/度量变化时重算：resultDigest 内部有一次 JSON.parse（虽有
  // 尺寸闸门），而 cells 会在拖窗时连续变 —— 挂在 render 上会让每帧都重解析一遍。
  const envelope = useMemo(() => parseScriptEnvelope(text), [text])
  const metric = useMemo(() => resultDigest(envelope, nestedCount), [envelope, nestedCount])
  // 检索档的**产出**是工具清单：把 `{"count":28,"names":[…]}` 渲染成 chip 比一坨 JSON 有用得多。
  // 只在检索档尝试（执行档的返回值是任意数据，没有「工具清单」这层语义）。
  const found = useMemo(() => (tier === 'search' ? parseFoundTools(envelope.body) : undefined), [tier, envelope])

  // —— 视觉行宽 = 容器放得下的**字符单元格**数 ——
  // 为什么必须量：限行要按「渲染后折了几行」算，而折行位置由真实宽度决定（窗口/侧栏拖动
  // 都会变）。量法：同字号探针量单个等宽字符的像素宽，再除以 <pre> 的内容宽度。
  // 量不到（未挂载 / 无布局 / 探针缺失）→ 保留兜底值：预览仍受 CSS max-height 限行
  //（见 index.css 的 .tn-pre），即「单行 100KB 输出不炸屏」这条不变量不依赖测量成功。
  useEffect(() => {
    const measure = () => {
      const pre = preRef.current
      const probe = probeRef.current
      if (!pre || !probe) return
      const cellW = probe.getBoundingClientRect().width / PROBE_LEN
      if (!(cellW > 0)) return
      const cs = getComputedStyle(pre)
      const avail = pre.clientWidth - parseFloat(cs.paddingLeft || '0') - parseFloat(cs.paddingRight || '0')
      if (!(avail > 0)) return
      setCells(Math.max(16, Math.floor(avail / cellW)))
    }
    measure()
    const el = preRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const preview = previewVisualLines(envelope.body, { lines: OUTPUT_PREVIEW_LINES, width: cells })
  const copySpill = async () => {
    if (spillPath && (await copyText(spillPath))) showToast(t('art.pathCopied').replace('{path}', spillPath))
    else showToast(t('art.copyFail'), 'error')
  }
  // 检索档的 chip 点一下 = 复制工具名（模型下一步就是把这个名字写进脚本，人也常在
  // 报告/脚本里贴它）—— 失败走既有 toast 兜底，不静默。
  const copyToolName = async (name: string) => {
    if (await copyText(name)) showToast(t('tool.codemode.toolCopied').replace('{name}', name))
    else showToast(t('art.copyFail'), 'error')
  }
  const scriptHint = scriptSummary(script)
  return (
    <div className="tool-out">
      {/* 量宽探针：同字号（.tn-pre 同族字体）、不可见、不参与布局 */}
      <span className="tn-probe mono" ref={probeRef} aria-hidden="true">{'0'.repeat(PROBE_LEN)}</span>
      {/* 一行「重点」：结局 · 墙钟 · 数字摘要。表头三行 → 一行 chip，省下的行预算归返回值。 */}
      {(envelope.recognized || metric) && (
        <div className="tn-summary">
          {envelope.recognized && (
            <span className={`tn-outcome ${envelope.succeeded ? 'ok' : 'err'}`}>
              {envelope.succeeded ? t('tool.codemode.outcome.ok') : t('tool.codemode.outcome.failed')}
            </span>
          )}
          {envelope.wallSec != null && <span className="tn-wall mono">{formatWall(envelope.wallSec)}</span>}
          {metric && <span className="tn-metric mono">{t(resultMetricKey(metric)).replace('{n}', String(metric.n))}</span>}
        </div>
      )}
      {/* 折叠态预览：行数由 previewVisualLines 按视觉行算好，CSS 再兜一道 max-height 硬限。
          max-height 的**权威值**从这里注入（单一来源 = OUTPUT_PREVIEW_LINES）：写死在 CSS 里
          的两处数字会各自漂移，而这里一旦比 CSS 小，多出来的行会被静默裁掉（看不见也无人知）。
          CSS 里那条只是首帧/无 JS 时的兜底。 */}
      {found ? (
        // 检索档：产出就是这份工具清单 —— 渲染成 chip（名字是模型下一步要写进脚本的东西，
        // 值得一眼扫到、值得点一下复制）。summary 挂在 title 上：不占高度，需要时有。
        <div className="cm-found">
          {found.map((ft) => (
            <button
              key={ft.name}
              type="button"
              className="cm-tool mono"
              title={ft.summary ? `${ft.name}\n${ft.summary}` : ft.name}
              onClick={() => void copyToolName(ft.name)}
            >
              {ft.name}
            </button>
          ))}
        </div>
      ) : (
        <pre className="tn-pre mono" ref={preRef} style={{ maxHeight: `calc(${OUTPUT_PREVIEW_LINES} * 1.55em + 12px)` }}>{preview.text}</pre>
      )}
      {/* 藏了多少行 / 完整输出去哪了 / 脚本源码在哪：**各自单独一行**（pi 的教训 ——
          折叠预览会把结果尾部的截断通知一起藏掉，不另起一行点名，用户就再也找不到全文）。
          路径缺失时对应那一段不显示（不编造路径）。 */}
      {(preview.truncated || spillPath || script) && (
        <div className="tn-foot">
          {preview.truncated && (
            <span className="tn-hidden">{t('tool.output.hiddenLines').replace('{n}', String(preview.hiddenVisualLines))}</span>
          )}
          {spillPath && (
            <button className="tn-spill mono" onClick={() => void copySpill()} title={t('tool.output.spillHint')}>
              {t('tool.output.spill').replace('{path}', spillPath)}
            </button>
          )}
          {script && (
            <button
              className="tn-script"
              onClick={() => setScriptOpen((v) => !v)}
              title={scriptHint || t('tool.codemode.scriptHint')}
            >
              {scriptOpen ? t('tool.codemode.scriptHide') : t('tool.codemode.script')}
            </button>
          )}
        </div>
      )}
      {/* 脚本源码：**默认收起**（用户的取舍是「结果优先」）—— 想看代码的人点「查看脚本」。
          这里用 <pre> 纯文本（不是 shiki 高亮）：折叠态是「快速确认」的场景，不值得为它
          拖入高亮器的流式开销；完整高亮版本在展开行后的「参数」区（JsonView）。 */}
      {scriptOpen && script && (
        <pre className="tn-script-code mono">{script}</pre>
      )}
    </div>
  )
})
