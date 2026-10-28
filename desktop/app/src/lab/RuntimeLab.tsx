import { Face } from '../components/runtime/Face'
import { AgentMark } from '../components/runtime/AgentMark'
import { AGENT_KINDS } from '../components/runtime/agent-marks'
import { Orbit } from '../components/runtime/Orbit'

const SYSTEMS = ['pip', 'cat', 'ghost'] as const
const STATES = ['run', 'work', 'done', 'engaged'] as const

const box: React.CSSProperties = { display: 'flex', gap: 14, alignItems: 'center', flexWrap: 'wrap' }
const row: React.CSSProperties = { display: 'grid', gap: 8, padding: '14px 18px', borderBottom: '1px solid var(--ink-line)' }
const lab: React.CSSProperties = { font: '400 10px/1 var(--font-mono)', color: 'var(--ink-3)', letterSpacing: '.05em', textTransform: 'uppercase' }

export function RuntimeLab() {
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
        <span style={lab}>orbit · 26 (start) / 16 (tool row)</span>
        <span style={box}><Orbit size={26} /><Orbit size={16} /></span>
      </div>
    </div>
  )
}
