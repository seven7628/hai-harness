import { memo } from 'react'
import type { ReactElement } from 'react'
import type { AgentItem } from '../store/useAppStore'
import { Markdown } from './markdown'
import { CompressionBlock, Thinking, ToolGroup } from './NarrativeBlocks'

// 子 agent 活动记录渲染（思考 / 流式输出 / 工具调用按时间序）。
// 主对话卡片（Narrative.AgentCard）与右栏「任务」tab（RightPanel.AgentView）共用。
//
// **与主 agent 同构 = 同一套渲染原语**（不是「长得像」）：
//   · 正文 → <Markdown> + .md-text（列表/行内码/表格与主对话一致，不再是裸文本）
//   · 工具项 → NarrativeBlocks.ToolRow（族色 / 行首图标 / 参数文件名着色 / 行内 diff /
//     参数与结果展开体全一致），连续工具项按主对话同一规则归组进 .tool-group
//   · 思考项 → NarrativeBlocks.Thinking（V3 皮肤的行首图标、行内码、折叠过渡一并继承）
//   · 压缩项 → NarrativeBlocks.CompressionBlock（子 agent 自己的上下文压缩）
// 历史：这里曾自带一份精简渲染（.at-text / .at-tool）—— 同一份数据在两处字号、字体、
// 配色、列表渲染都对不上，且皮肤层只覆盖主对话那套（2026-09-23 用户报障）。
// 新增块类型时改 NarrativeBlocks，两处同时生效。
//
// memo：items 引用稳定时（块级 memo 保证），逐秒时钟 tick 不会重渲整个转录。
// host='transcript'：工具行不渲染主 run 专属动作（后台 / 停止，见 ToolRow 的说明）。
export const AgentTranscript = memo(function AgentTranscript({ items, running }: { items: AgentItem[]; running: boolean }) {
  // 连续工具项归组（与 Narrative 的分组规则一致：并行工具上下排，共用一张分组框）
  const out: ReactElement[] = []
  for (let i = 0; i < items.length; i++) {
    const it = items[i]
    if (it.kind === 'tool') {
      const group: Extract<AgentItem, { kind: 'tool' }>[] = [it]
      while (i + 1 < items.length && items[i + 1].kind === 'tool') {
        group.push(items[i + 1] as Extract<AgentItem, { kind: 'tool' }>)
        i++
      }
      out.push(<ToolGroup key={`t${i}`} tools={group} host="transcript" />)
      continue
    }
    if (it.kind === 'thinking') {
      // live = 子 agent 仍在跑且这是最后一项：完整可见 + 计时器走 + 自动贴底
      const isLast = i === items.length - 1
      out.push(<Thinking key={`k${i}`} text={it.text} live={running && isLast} thinkStart={it.thinkStart} />)
      continue
    }
    if (it.kind === 'text') {
      const isLast = i === items.length - 1
      out.push(
        // 失败/超限提示（llm_error 挂卡片，不污染主叙述）：卡内一条红字提示，
        // 与主对话的 .msg.error 整块刻意不同（消息级 vs 卡内一行）。
        <div className={`md-text${it.error ? ' at-err' : ''}`} key={`x${i}`}>
          {it.error ? it.text : <Markdown text={it.text} />}
          {running && isLast && <span className="mono" style={{ color: 'var(--running)' }}>▍</span>}
        </div>,
      )
      continue
    }
    out.push(<CompressionBlock key={`c${i}`} b={{ ...it, kind: 'compression', id: i }} />)
  }
  return <div className="agent-tr">{out}</div>
})
