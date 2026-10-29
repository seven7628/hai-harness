import { memo, useState } from 'react'
import type { ReactNode } from 'react'
import { FoldIcon } from '../FoldIcon'
import './batch.css'

/* 一组调用折成一行 —— 设计 .cy-act（ui.js:2373）：
     「Globbed 模式 · 202 files · 277 ms」+ 右端一个折角
   语法是「动词 · 对象 · 这个动词数的那个量 · 耗时」，量由 batchLine 从真实调用里算，
   这里只负责**它是个可开合的行**：设计给 .cy-act 的是 role=button / tabindex / aria-expanded，
   点开才出现这一组逐条的行（.cy-rows.collapse）。

   默认展开：设计里那几行就在这行下面摊着（截图里 Bash / Glob / Read 都看得见），
   折角表示「还能收起来」。把默认做成收起等于把「这轮读了什么」藏进一次点击。 */
export const BatchGroup = memo(function BatchGroup({ line, children }: { line: string; children: ReactNode }) {
  const [open, setOpen] = useState(true)
  const toggle = () => setOpen((v) => !v)
  return (
    <>
      <button
        type="button"
        className="rt-batch"
        aria-expanded={open}
        onClick={toggle}
        onKeyDown={(e) => {
          if (e.key !== 'Enter' && e.key !== ' ') return
          e.preventDefault()
          toggle()
        }}
      >
        <span className="rt-batch-t">{line}</span>
        <FoldIcon open={open} />
      </button>
      <div className={'rt-collapse' + (open ? ' open' : '')}>
        <div>{children}</div>
      </div>
    </>
  )
})
