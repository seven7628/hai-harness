import type { MsgBlock } from '../../../store/useAppStore'
import { toolArgsSummary, toolVerb } from '../../../lib/toolSummary'
import type { CompactHead } from './Compact'

/* 压缩运行时要显示「正在读哪几条」——这不是新数据：压缩的对象就是转录里**已经存在的**
   那些块。这里把它们折成一行描述（动词 + 对象，或正文首行），并给出相对时刻。
   拿不到时刻就只有描述，绝不编一个时间。 */

export function headsOf(list: MsgBlock[], limit = 8): CompactHead[] {
  const picked = list.slice(-limit)
  const t0 = picked.reduce<number | undefined>((acc, b) => {
    const ts = 'ts' in b ? b.ts : undefined
    return acc == null && ts != null ? ts : acc
  }, undefined)
  return picked.map((b) => {
    const ts = 'ts' in b ? b.ts : undefined
    const t = t0 != null && ts != null ? '+' + ((ts - t0) / 1000).toFixed(1).padStart(4, '0') : ''
    let h = ''
    if (b.kind === 'tool') {
      const o = toolArgsSummary(b.name, b.args || '')
      h = o ? `${toolVerb(b.name)} ${o}` : toolVerb(b.name)
    } else if (b.kind === 'assistant') {
      h = (b.text || '').split('\n')[0] || (b.thinking ? 'thinking' : '')
    } else if (b.kind === 'user') {
      h = '你：' + (b.text || '').split('\n')[0]
    } else if (b.kind === 'agent') {
      h = 'subagent · ' + (b.label || '')
    } else if (b.kind === 'compression') {
      h = 'compressing'
    }
    return { t, h: h.slice(0, 120) }
  })
}
