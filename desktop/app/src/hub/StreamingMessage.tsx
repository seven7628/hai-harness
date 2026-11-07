// 群内流式草稿（P3a-09）：轻量纯文本 + 换行，不做 Markdown 重排；
// final hub_message 到达后整条替换为完整 Markdown 气泡。
import type { HubStreamDraft } from './groupStream'

export function StreamingMessage({ draft, name }: { draft: HubStreamDraft; name: string }) {
  return (
    <div className="hub-msg agent streaming" aria-live="polite">
      <div className="hub-group-author">
        {name}
        <span className="hub-chip streaming-chip">正在输入</span>
      </div>
      <div className="hub-body">
        <div className="hub-stream-text">{draft.text}<span className="hub-stream-caret" aria-hidden="true" /></div>
      </div>
    </div>
  )
}
