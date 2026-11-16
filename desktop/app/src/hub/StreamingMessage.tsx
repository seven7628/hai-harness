import { useT } from '../i18n'
import type { HubStreamDraft } from './groupStream'
import { AgentMessage } from './ConversationMessage'
import type { MentionAgent } from './mentions'

// 群内流式草稿（P3a-09）：与最终消息同一版式（52px Luna + 作者头 + 无气泡正文），
// 只多一个「正在输入」标签和光标。旧版另起一套带头气泡，最终消息到达时会跳一下。
//
// typing：这条草稿此刻还在收 delta —— 只有它配「正在输入」和光标。
// 一轮 AgentEnd 才把各段发成 final，所以工作过程中同一个 agent 会同时挂着好几条
// 草稿（每个 LLM 轮次一条），前面几条其实已经收口：都画光标 = 一次回复里多根光标
// 一起闪。谁算「活的」由 groupStream.liveDraftIds 判定，调用方传进来。
export function StreamingMessage({ draft, name, agent, typing = false }: {
  draft: HubStreamDraft
  name: string
  agent?: MentionAgent | null
  typing?: boolean
}) {
  const t = useT()
  return (
    <AgentMessage
      agent={agent}
      name={name}
      role={[agent?.role, agent?.groupRole === 'owner' ? '群主' : ''].filter(Boolean).join(' · ') || undefined}
      tags={typing ? <span className="agent-tag reply">{t('hub.streaming')}</span> : undefined}
      className="hub-group-msg streaming"
    >
      {/* aria-live 只挂在正在流的那条：几条草稿同时 polite 播报，读屏会被同一个人念好几遍 */}
      <div className="hub-stream-text" aria-live={typing ? 'polite' : undefined}>
        {draft.text}
        {typing ? <span className="hub-stream-caret" aria-hidden="true" /> : null}
      </div>
    </AgentMessage>
  )
}
