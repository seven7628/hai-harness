import { useT } from '../i18n'
import type { HubStreamDraft } from './groupStream'
import { AgentMessage } from './ConversationMessage'
import type { MentionAgent } from './mentions'

// 群内流式草稿（P3a-09）：与最终消息同一版式（52px Luna + 作者头 + 无气泡正文），
// 只多一个「正在输入」标签和光标。旧版另起一套带头气泡，最终消息到达时会跳一下。
export function StreamingMessage({ draft, name, agent }: { draft: HubStreamDraft; name: string; agent?: MentionAgent | null }) {
  const t = useT()
  return (
    <AgentMessage
      agent={agent}
      name={name}
      role={[agent?.role, agent?.groupRole === 'owner' ? '群主' : ''].filter(Boolean).join(' · ') || undefined}
      tags={<span className="agent-tag reply">{t('hub.streaming')}</span>}
      className="hub-group-msg streaming"
    >
      <div className="hub-stream-text" aria-live="polite">
        {draft.text}
        <span className="hub-stream-caret" aria-hidden="true" />
      </div>
    </AgentMessage>
  )
}
