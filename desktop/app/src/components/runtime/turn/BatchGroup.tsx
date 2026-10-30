import { memo, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import type { MsgBlock } from '../../../store/useAppStore'
import { FoldIcon } from '../FoldIcon'
import { batchRunningLine, batchStatus, failedCallLine } from './batch'
import './batch.css'

type Tool = Extract<MsgBlock, { kind: 'tool' }>

/* 一组调用折成一行 —— 设计 .cy-act（ui.js:2373）：
     「Globbed 模式 · 202 files · 277 ms」+ 右端一个折角
   语法是「动词 · 对象 · 这个动词数的那个量 · 耗时」，量由 batchLine 从真实调用里算，
   这里只负责**它是个可开合的行**：设计给 .cy-act 的是 role=button / tabindex / aria-expanded，
   点开才出现这一组逐条的行（.cy-rows.collapse）。

   规格（2026-10）给这条行加了两件事：
     ① 执行期间说清进度 —— 「3 tool calls · 1 completed · 2 running · 12 s」，
        而不是一句静态的 Read/Globbed（数字全部来自真实 status，见 batch.ts）；
     ② 全部结束自动收缩 —— 但失败必须在收缩态看得见，所以失败调用各占一行摘要。
   默认展开、用户手动点过之后就不再自动开合（尊重用户的选择）。 */
export const BatchGroup = memo(function BatchGroup({ line, calls, children }: { line: string; calls?: Tool[]; children: ReactNode }) {
  const [open, setOpen] = useState(true)
  // 用户手动点过 → 后续状态变化不再自动开合（规格：可手动展开/收缩）
  const touchedRef = useRef(false)
  const running = calls && calls.length ? batchStatus(calls) : null
  const head = running && !running.settled ? batchRunningLine(calls!) : line
  const failed = calls ? calls.filter((c) => c.isError) : []

  // 从「还在跑」变成「全结束」的那一下自动收起；只做一次，且不覆盖用户手动操作
  useEffect(() => {
    if (!running || !running.settled || touchedRef.current) return
    setOpen(false)
  }, [running, running?.settled])

  const toggle = () => {
    touchedRef.current = true
    setOpen((v) => !v)
  }
  return (
    <>
      <button
        type="button"
        className="rt-batch"
        aria-expanded={open}
        data-failed={failed.length ? '1' : undefined}
        onClick={toggle}
        onKeyDown={(e) => {
          if (e.key !== 'Enter' && e.key !== ' ') return
          e.preventDefault()
          toggle()
        }}
      >
        <span className="rt-batch-t">{head}</span>
        {failed.length ? <span className="rt-batch-x" aria-label={`${failed.length} failed`}>✗ {failed.length}</span> : null}
        <FoldIcon open={open} />
      </button>
      <div className={'rt-collapse' + (open ? ' open' : '')}>
        <div>{children}</div>
      </div>
      {/* 收缩态也要看得见失败：每条失败调用一行摘要（结果首行就是错误信息） */}
      {!open && failed.length ? (
        <div className="rt-batch-fails">
          {failed.map((c) => (
            <div className="rt-batch-fail mono" key={c.id}>✗ {failedCallLine(c)}</div>
          ))}
        </div>
      ) : null}
    </>
  )
})
