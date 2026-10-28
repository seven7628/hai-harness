import { Face } from '../components/runtime/Face'
import { AgentMark } from '../components/runtime/AgentMark'
import { AGENT_KINDS } from '../components/runtime/agent-marks'
import { Orbit } from '../components/runtime/Orbit'
import { WorkFold } from '../components/runtime/turn/WorkFold'
import { selectTurns, turnStats } from '../components/runtime/turn/selectTurns'
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
        <span style={lab}>orbit · 26 (start) / 16 (tool row)</span>
        <span style={box}><Orbit size={26} /><Orbit size={16} /></span>
      </div>
    </div>
  )
}
