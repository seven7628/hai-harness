import { memo, useEffect, useRef, useState } from 'react'
import { fmtDuration } from './ui'
import { copyText } from '../lib/clipboard'
import { useT } from '../i18n'
import { useAppStore } from '../store/useAppStore'
import {
  NESTED_ARGS_PREVIEW_CHARS,
  NESTED_COLLAPSED_LIMIT,
  NESTED_ERROR_PREVIEW_CHARS,
  OUTPUT_PREVIEW_LINES,
  oneLine,
  pickNestedCalls,
  previewChars,
  previewVisualLines,
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

/** 折叠态的嵌套调用清单（数据来自外层 ToolResponse.NestedCalls）。 */
export const ToolNestedCalls = memo(function ToolNestedCalls({ calls }: { calls: NestedCallView[] }) {
  const t = useT()
  const [open, setOpen] = useState(false)
  // 折叠态 = 最后 8 条 + 一行「…前面还有 N 条」（N 由纯函数给出，见 pickNestedCalls）
  const { visible, hidden } = pickNestedCalls(calls, !open)
  const failed = calls.filter((c) => c.isError).length
  const foldable = calls.length > NESTED_COLLAPSED_LIMIT
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

// 折叠态默认的视觉行宽（字符单元格）：首帧还没量到真实宽度时的兜底。
// 88 ≈ 主对话列（约 620px）在 11.5px 等宽字体下的容量，量到真实值前也不会炸屏。
const FALLBACK_CELLS = 88
// 探针长度：一个足够长的 '0' 串，除以后得到单字符像素宽（越短误差越大）。
const PROBE_LEN = 32

/** 脚本文本输出的折叠预览：视觉行限 OUTPUT_PREVIEW_LINES 行 + 下方单独一行点名 spill 路径。 */
export const ToolOutputPreview = memo(function ToolOutputPreview({ text, spillPath }: { text: string; spillPath?: string }) {
  const t = useT()
  const showToast = useAppStore((s) => s.showToast)
  const preRef = useRef<HTMLPreElement>(null)
  const probeRef = useRef<HTMLSpanElement>(null)
  const [cells, setCells] = useState(FALLBACK_CELLS)

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

  const preview = previewVisualLines(text, { lines: OUTPUT_PREVIEW_LINES, width: cells })
  const copySpill = async () => {
    if (spillPath && (await copyText(spillPath))) showToast(t('art.pathCopied').replace('{path}', spillPath))
    else showToast(t('art.copyFail'), 'error')
  }
  return (
    <div className="tool-out">
      {/* 量宽探针：同字号（.tn-pre 同族字体）、不可见、不参与布局 */}
      <span className="tn-probe mono" ref={probeRef} aria-hidden="true">{'0'.repeat(PROBE_LEN)}</span>
      {/* 折叠态预览：行数由 previewVisualLines 按视觉行算好，CSS 再兜一道 max-height 硬限。
          max-height 的**权威值**从这里注入（单一来源 = OUTPUT_PREVIEW_LINES）：写死在 CSS 里
          的两处数字会各自漂移，而这里一旦比 CSS 小，多出来的行会被静默裁掉（看不见也无人知）。
          CSS 里那条只是首帧/无 JS 时的兜底。 */}
      <pre className="tn-pre mono" ref={preRef} style={{ maxHeight: `calc(${OUTPUT_PREVIEW_LINES} * 1.55em + 12px)` }}>{preview.text}</pre>
      {/* 藏了多少行 / 完整输出去哪了：**单独一行**点名路径（pi 的教训 —— 折叠预览会把
          结果尾部的截断通知一起藏掉，不另起一行点名，用户就再也找不到全文）。
          路径缺失时两行都不显示（不编造路径）。 */}
      {(preview.truncated || spillPath) && (
        <div className="tn-foot">
          {preview.truncated && (
            <span className="tn-hidden">{t('tool.output.hiddenLines').replace('{n}', String(preview.hiddenVisualLines))}</span>
          )}
          {spillPath && (
            <button className="tn-spill mono" onClick={() => void copySpill()} title={t('tool.output.spillHint')}>
              {t('tool.output.spill').replace('{path}', spillPath)}
            </button>
          )}
        </div>
      )}
    </div>
  )
})
