import { useEffect, useMemo, useState } from 'react'
import { useHubStore } from './useHubStore'
import { formatHubDeepLink } from './deeplink'
import {
  contextMessageOptions, messageContextView, pickContextMessage, promptLayerViews,
  promptPreviewBytes, promptRuntimeNote, resolveTriggerJump,
} from './contextView'
import type { HubMessage } from './types'

// Context（只读，P4-02）：Inspector 的第 4 个页签。
//
// 两段信息，都不写：
//   ① 消息级触发上下文：context.trigger_seqs / senders / reason / round_id；
//      点触发 seq 只调 store.requestJump —— 跳转与高亮仍由 GroupView/HubSurface 执行
//      （同一套 seqJumpPlan + scrollToSeq，不在面板里复制滚动逻辑）。
//   ② 智能体系统提示词分层：hub_agent_prompt_preview（L1–L4，只读）；
//      L5 工具 schema 与技能清单在请求期由内核装配，不在预览里（面板恒显示这句）。
//
// 面板里没有输入框/保存按钮：记忆、人格、工具的编辑仍只在资料页（AgentEditor）。

export default function ContextPanel() {
  const activeId = useHubStore((s) => s.activeId)
  const activeGroupId = useHubStore((s) => s.activeGroupId)
  const groups = useHubStore((s) => s.groups)
  const agents = useHubStore((s) => s.agents)
  const messages = useHubStore((s) => s.messages)
  const groupMessages = useHubStore((s) => s.groupMessages)
  const prompt = useHubStore((s) => s.prompt)
  const loadPrompt = useHubStore((s) => s.loadPromptPreview)
  const requestJump = useHubStore((s) => s.requestJump)
  const copyDeepLink = useHubStore((s) => s.copyDeepLink)

  const group = activeGroupId ? (groups.find((g) => g.id === activeGroupId) ?? null) : null
  const targetMessages = group ? groupMessages : messages
  const names = useMemo(() => {
    const map: Record<string, string> = {}
    for (const a of agents) if (a.id) map[a.id] = a.name || a.id
    return map
  }, [agents])

  // 切换会话/群后旧的 seq 选择不再适用：重置成「该会话最新一条」。
  const [selectedSeq, setSelectedSeq] = useState(0)
  useEffect(() => {
    setSelectedSeq(0)
  }, [activeId, activeGroupId])

  const options = useMemo(() => contextMessageOptions(targetMessages, 20), [targetMessages])
  const selected = pickContextMessage(targetMessages, selectedSeq, 20)
  const view = messageContextView(selected, names)
  const loadedSeqs = useMemo(() => new Set(targetMessages.map((m) => Number(m.seq))), [targetMessages])

  // 群：默认第一个成员，可切换（只读，看谁的提示词）；DM：就是当前智能体。
  const members = group?.members ?? []
  const [memberPick, setMemberPick] = useState('')
  const memberIds = members.map((m) => m.agent_id).filter(Boolean)
  const promptAgentId = group
    ? (memberIds.includes(memberPick) ? memberPick : (memberIds[0] ?? ''))
    : (activeId ?? '')
  useEffect(() => {
    if (promptAgentId) void loadPrompt(promptAgentId)
  }, [promptAgentId, loadPrompt])
  const promptAgent = agents.find((a) => a.id === promptAgentId)
  const promptForTarget = prompt && prompt.agentId === promptAgentId ? prompt : null
  const layers = useMemo(() => promptLayerViews(promptForTarget?.layers), [promptForTarget?.layers])
  const totalBytes = promptPreviewBytes(promptForTarget?.totalBytes, layers)
  const note = promptRuntimeNote(promptForTarget?.note)

  const scopeId = group ? group.id : (activeId ?? '')
  const kind = group ? 'group' : 'dm'
  const latestSeq = useMemo(
    () => targetMessages.reduce((max, m) => (Number(m.seq) > max ? Number(m.seq) : max), 0),
    [targetMessages],
  )
  const link = scopeId ? formatHubDeepLink(kind, scopeId, latestSeq) : ''

  if (!scopeId) {
    return (
      <section className="hub-panel" aria-label="上下文">
        <header className="hub-panel-head"><strong>Context</strong><span className="hub-muted">只读</span></header>
        <div className="hub-empty hub-pad">
          <p>先选择一个会话</p>
          <span className="hub-muted">DM 或群的时间线会在这里给出触发上下文与提示词分层</span>
        </div>
      </section>
    )
  }

  return (
    <section className="hub-panel hub-context-panel" aria-label="上下文">
      <header className="hub-panel-head">
        <strong>Context</strong>
        <span className="hub-muted">{group ? group.name : (promptAgent?.name ?? activeId)}</span>
        <span className="hub-chip">只读</span>
      </header>

      <div className="hub-deeplink">
        <span className="hub-muted">深链</span>
        <div className="hub-deeplink-row">
          <code className="hub-deeplink-url" title={link}>{link || '—'}</code>
          <button
            className="hub-btn"
            onClick={() => void copyDeepLink(kind, scopeId, latestSeq)}
            title="复制到剪贴板（含最新消息位置）"
          >
            复制链接
          </button>
        </div>
      </div>

      <div className="hub-context-section">
        <div className="hub-section-head">
          <strong>消息上下文</strong>
          <span className="hub-muted">{options.length} 条可查看</span>
        </div>
        {options.length === 0 ? (
          <div className="hub-empty hub-pad">
            <p>还没有带触发上下文的消息</p>
            <span className="hub-muted">自动对话/群回复会记录 trigger_seqs、senders 与轮次</span>
          </div>
        ) : (
          <>
            <div className="hub-context-picker">
              <label htmlFor="hub-context-msg">消息</label>
              <select
                id="hub-context-msg"
                value={selected ? String(selected.seq) : ''}
                onChange={(e) => setSelectedSeq(Number(e.target.value))}
              >
                {options.map((m) => (
                  <option key={`${m.seq}-${m.id ?? ''}`} value={String(m.seq)}>
                    #{m.seq} · {senderName(m, names)} · {firstLine(m.text) || '（无正文）'}
                  </option>
                ))}
              </select>
            </div>
            <dl className="hub-context-meta">
              <dt>消息</dt>
              <dd>#{view.seq || '—'} · {selected ? senderName(selected, names) : '—'}</dd>
              <dt>轮次</dt>
              <dd>{view.roundId || '—'}</dd>
              <dt>原因</dt>
              <dd>{view.reason || '—'}</dd>
            </dl>
            <div className="hub-context-row">
              <span className="hub-context-key">触发消息</span>
              {view.triggerSeqs.length === 0 ? <span className="hub-muted">—</span> : null}
              {view.triggerSeqs.map((seq) => (
                <button
                  key={seq}
                  className={`hub-btn hub-context-seq${loadedSeqs.has(seq) ? '' : ' missing'}`}
                  onClick={() => requestJump(seq)}
                  title={loadedSeqs.has(seq) ? `跳到 #${seq} 并高亮` : `#${seq} 不在已加载历史里，点击会提示`}
                >
                  #{seq}
                </button>
              ))}
              {view.triggerSeqs.length > 1 ? (
                <button
                  className="hub-link"
                  onClick={() => {
                    const seq = resolveTriggerJump(view.triggerSeqs, targetMessages)
                    if (seq > 0) requestJump(seq)
                  }}
                  title="跳到第一条已加载的触发消息"
                >
                  定位
                </button>
              ) : null}
            </div>
            <div className="hub-context-row">
              <span className="hub-context-key">发送者</span>
              {view.senders.length === 0 ? <span className="hub-muted">—</span> : null}
              {view.senders.map((s) => (
                <span key={s.id || s.name} className="hub-chip">{s.name}</span>
              ))}
            </div>
          </>
        )}
      </div>

      <div className="hub-context-section">
        <div className="hub-section-head">
          <strong>提示词分层</strong>
          {group && memberIds.length > 0 ? (
            <select
              aria-label="选择成员"
              value={promptAgentId}
              onChange={(e) => setMemberPick(e.target.value)}
            >
              {members.map((m) => (
                <option key={m.agent_id} value={m.agent_id}>
                  {(names[m.agent_id] ?? m.agent_id)}{m.role ? `（${m.role}）` : ''}
                </option>
              ))}
            </select>
          ) : (
            <span className="hub-muted">{promptAgent?.name ?? '—'}</span>
          )}
          <button
            className="hub-link"
            onClick={() => void loadPrompt(promptAgentId, true)}
            disabled={!promptAgentId}
            title="重新读取（只读）"
          >
            ↻
          </button>
        </div>

        {!promptAgentId ? (
          <div className="hub-empty hub-pad">
            <p>这个群还没有成员</p>
            <span className="hub-muted">加入成员后才能预览提示词</span>
          </div>
        ) : null}
        {promptAgentId && promptForTarget?.loading ? <div className="hub-muted hub-pad">读取提示词…</div> : null}
        {promptAgentId && promptForTarget?.error ? <p className="hub-error-text hub-pad">{promptForTarget.error}</p> : null}
        {promptAgentId && promptForTarget?.loaded && layers.length === 0 ? (
          <div className="hub-empty hub-pad">
            <p>预览为空</p>
            <span className="hub-muted">这个智能体还没有可展示的提示词层</span>
          </div>
        ) : null}
        {layers.length > 0 ? (
          <div className="hub-prompt-layers">
            {layers.map((layer, index) => (
              <details key={`${layer.key}-${index}`} className="hub-prompt-layer" open={index === 0}>
                <summary>
                  <span className={`hub-chip ${layer.label ? `layer-${layer.label.toLowerCase()}` : ''}`}>{layer.label || 'L?'}</span>
                  <span className="hub-prompt-title">{layer.title}</span>
                  <span className="hub-muted">{layer.chars} 字</span>
                </summary>
                <pre className="hub-prompt-text">{layer.text || '（空）'}</pre>
              </details>
            ))}
          </div>
        ) : null}
        <p className="hub-muted hub-pad hub-prompt-note">
          {layers.length > 0 ? `共 ${totalBytes} 字节 · ` : ''}{note}
        </p>
      </div>
    </section>
  )
}

function senderName(msg: HubMessage, names: Record<string, string>): string {
  const kind = msg.sender?.kind ?? ''
  if (kind === 'human') return '你'
  if (kind === 'system') return '系统'
  const id = String(msg.sender?.agent_id ?? '')
  return (id && names[id]) || id || '成员'
}

function firstLine(text?: string): string {
  const line = String(text ?? '').split('\n').map((v) => v.trim()).find(Boolean) ?? ''
  return line.length > 40 ? `${line.slice(0, 39)}…` : line
}
