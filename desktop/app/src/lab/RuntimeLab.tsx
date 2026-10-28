import { Face } from '../components/runtime/Face'
import { AgentMark } from '../components/runtime/AgentMark'
import { AGENT_KINDS } from '../components/runtime/agent-marks'
import { Orbit } from '../components/runtime/Orbit'
import { WorkFold } from '../components/runtime/turn/WorkFold'
import { selectTurns, turnStats } from '../components/runtime/turn/selectTurns'
import { Ledger } from '../components/runtime/turn/Ledger'
import { Thinking } from '../components/NarrativeBlocks'
import { Compact } from '../components/runtime/blocks/Compact'
import type { MsgBlock } from '../store/useAppStore'
import { useState } from 'react'

const SYSTEMS = ['pip', 'cat', 'ghost'] as const
const STATES = ['run', 'work', 'done', 'engaged'] as const

const box: React.CSSProperties = { display: 'flex', gap: 14, alignItems: 'center', flexWrap: 'wrap' }
const row: React.CSSProperties = { display: 'grid', gap: 8, padding: '14px 18px', borderBottom: '1px solid var(--ink-line)' }
const lab: React.CSSProperties = { font: '400 10px/1 var(--font-mono)', color: 'var(--ink-3)', letterSpacing: '.05em', textTransform: 'uppercase' }

/* a handful of blocks shaped like the store's, just enough to show a turn's
   working line: three tools, one subagent, one edited file, one compression */
const SAMPLE: MsgBlock[] = [
  { kind: 'user', id: 1, text: '切工作区的时候 skills 没有重新加载' },
  { kind: 'assistant', id: 2, text: '', streaming: false, thinking: 'x', durMs: 4820 },
  { kind: 'tool', id: 3, toolId: 'a', name: 'grep', args: 'os.UserHomeDir', status: 'done', durMs: 431 },
  { kind: 'tool', id: 4, toolId: 'b', name: 'read_file', args: 'session/restore.go', status: 'done', durMs: 122, diff: { path: 'session/restore.go', added: 14, removed: 2 } },
  { kind: 'tool', id: 5, toolId: 'c', name: 'bash', args: 'go test ./session', status: 'done', durMs: 9180 },
  { kind: 'agent', id: 6, runId: 'r1', label: 'scan callers', status: 'completed', spawnedAt: Date.now(), usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0 }, items: [] },
  { kind: 'assistant', id: 7, text: '已定位并修复该问题。', streaming: false, thinking: '', durMs: 3120 },
]

const C = (over: Partial<MsgBlock>) => ({ kind: 'compression', id: 99, before: 18, after: 3, ...over }) as MsgBlock

const COMPACTS: { name: string; b: Extract<MsgBlock, { kind: 'compression' }> }[] = [
  { name: 'running', b: C({ active: true, startedAt: Date.now() }) as never },
  {
    name: 'done',
    b: C({
      ctxTokens: 6800,
      durMs: 1800,
      reason: 'automatic',
      model: 'deepseek-v4-flash',
      usage: { input: 42100, output: 900, cacheRead: 39000, cacheWrite: 0, cacheWrite1h: 0, reasoning: 0, costUsd: 0.0021 },
      summary:
        '用户报告 TestSessionRestore 连跑 5 次挂 2 次。根因不在被测代码：夹具把 HOME 指向仓库内的 fixture 目录，并行跑时两个进程互相清空对方的状态。已把夹具改成 t.TempDir()，restore.go 的路径解析收敛为单一入口。未决：mock.ts 里那份硬编码路径是否一并删除。',
      analysis:
        '18 条里只有 4 条改变了结论：两次失败、一次 diff、一次 5 次跑。\n工具结果头要留（复现结论要用），grep 原始输出 4 条没有被再次引用过，可以丢。\n压成一段 summary 而不是 summary + 片段：下一轮要的是结论和未决项，不是旧原文。',
    }) as never,
  },
  { name: 'failed', b: C({ error: 'llm_error: context length exceeded (400)' }) as never },
  { name: 'aborted', b: C({ aborted: true }) as never },
]

export function RuntimeLab() {
  const [open, setOpen] = useState(false)
  const turn = selectTurns(SAMPLE)[0]
  const stats = turnStats(turn)
  return (
    <div style={{ padding: 18 }}>
      <div style={{ ...row, borderBottom: 'none' }}>
        <span style={lab}>the mark · 3 systems × 4 states · 14 / 22 / 40px</span>
        {SYSTEMS.map((s) => (
          <div key={s} style={box}>
            {STATES.map((st) => (
              <span key={st} style={box}>
                <Face system={s} state={st} />
                <Face system={s} state={st} size={22} />
                <Face system={s} state={st} size={40} />
              </span>
            ))}
          </div>
        ))}
      </div>

      <div style={row}>
        <span style={lab}>compact · running / done / failed / aborted</span>
        {COMPACTS.map((c) => (
          <div key={c.name} style={{ maxWidth: 640 }}>
            <Compact b={c.b} />
          </div>
        ))}
      </div>

      <div style={row}>
        <span style={lab}>agent marks · 10 kinds × 3 states</span>
        {AGENT_KINDS.map((k) => (
          <span key={k.id} style={{ ...box, minWidth: 150 }}>
            <AgentMark kind={k.id} state="run" />
            <AgentMark kind={k.id} state="stop" />
            <AgentMark kind={k.id} state="done" />
            <span style={{ font: '400 10px/1 var(--font-mono)', color: 'var(--ink-3)' }}>{k.name}</span>
          </span>
        ))}
      </div>

      <div style={row}>
        <span style={lab}>work fold · concise (folded) / detailed (open)</span>
        <WorkFold stats={stats} running={false} open={false} onToggle={() => {}} />
        <WorkFold stats={stats} running={false} open={true} onToggle={() => {}} />
        <WorkFold stats={stats} running={true} open={false} onToggle={() => {}} />
        <span style={{ font: '400 10px/1.4 var(--font-mono)', color: 'var(--ink-3)' }}>
          stats: {JSON.stringify(stats)}
        </span>
      </div>

      <div style={row}>
        <span style={lab}>ledger · rest / hover(tap) · 4 facts + 3 timings</span>
        <Ledger calls={10} tokens={4746407} cacheRead={4731008} input={4736569}
          wallMs={249000} ttftMs={3306} llmMs={118400} outputTokens={9838} />
        <Ledger calls={1} tokens={833719} cacheRead={827136} input={830616} wallMs={31000} />
        <span style={{ font: '400 10px/1.4 var(--font-mono)', color: 'var(--ink-3)' }}>
          第二行没有计时字段 → 不渲染展开（宁缺勿造）
        </span>
      </div>

      <div style={row}>
        <span style={lab}>thinking · live / settled (face + duration)</span>
        <Thinking text="先确认这条链路上有没有别的读取点：grep os.UserHomeDir 在会话恢复里只出现两次，其中一次在测试夹具。" live={false} thinkMs={4820} />
      </div>

      <div style={row}>
        <span style={lab}>orbit · 26 (start) / 16 (tool row)</span>
        <span style={box}><Orbit size={26} /><Orbit size={16} /></span>
      </div>
    </div>
  )
}
