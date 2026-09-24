import { useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import Sel from './Sel'

// IM 绑定确认弹窗：陌生 IM 聊天首次发消息时弹出（bridge 发 im_bind_confirm_requested）。
// 用户选择目标 workspace 后确认（写路由表 Chat→workspace）或拒绝。
// 安全模型（docs/IM_INTEGRATION.md §6）：陌生 Chat 未经确认不路由——防止任意 IM 用户驱动本地 agent。
export default function IMBindConfirmModal() {
  const t = useT()
  const pending = useAppStore((s) => s.imBindPending)
  const imBindConfirm = useAppStore((s) => s.imBindConfirm)
  const workspace = useAppStore((s) => s.workspace)
  const projects = useAppStore((s) => s.projects)
  const [ws, setWs] = useState('')

  if (!pending) return null

  // 候选 workspace：当前工作区优先，其次已打开的项目
  const candidates = Array.from(new Set([
    ...(workspace ? [workspace] : []),
    ...projects.map((p) => p.path),
  ].filter(Boolean)))

  return (
    <div className="modal-hint open" role="dialog" aria-modal="true" aria-label={t('settings.im.bindTitle')}>
      <div className="card confirm-card settings-pop" style={{ maxWidth: 460 }}>
        <div className="settings-title-row">
          <div className="settings-title">{t('settings.im.bindTitle')}</div>
        </div>
        <div style={{ padding: '4px 0 10px', fontSize: 12.5, lineHeight: 1.6 }}>
          <p style={{ color: 'var(--text-2)', marginBottom: 8 }}>
            {t('settings.im.bindDesc')}
          </p>
          <div style={{ background: 'var(--bg)', border: '1px solid var(--border-strong)', borderRadius: 8, padding: '8px 10px', marginBottom: 10, fontFamily: 'var(--font-mono)', fontSize: 12 }}>
            <div>{pending.chat.gateway}:{pending.chat.chat_id}</div>
            <div style={{ color: 'var(--text-3)' }}>{pending.userId}</div>
            {pending.text && (
              <div style={{ color: 'var(--text-2)', marginTop: 4, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                「{pending.text}」
              </div>
            )}
          </div>
          <label style={{ display: 'block', fontSize: 11.5, color: 'var(--text-2)', marginBottom: 4 }}>
            {t('settings.im.bindWorkspace')}
          </label>
          <Sel
            value={ws}
            options={[
              { value: '', label: t('settings.im.bindSelectWorkspace') },
              ...candidates.map((p) => ({ value: p, label: p })),
            ]}
            onChange={setWs}
            style={{ marginBottom: 4 }}
          />
          <div style={{ fontSize: 11, color: 'var(--text-3)' }}>{t('settings.im.bindHint')}</div>
        </div>
        <div className="btnrow">
          <button className="btn gho" onClick={() => imBindConfirm(pending.chat, false, '')}>
            {t('settings.im.bindReject')}
          </button>
          <button
            className="btn pri"
            disabled={!ws}
            onClick={() => imBindConfirm(pending.chat, true, ws)}
          >
            {t('settings.im.bindConfirm')}
          </button>
        </div>
      </div>
    </div>
  )
}
