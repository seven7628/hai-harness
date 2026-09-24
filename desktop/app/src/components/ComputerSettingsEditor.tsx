import { useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'

// Computer Use 插件设置详情（插件卡内联展开）：
//   - 只读状态：权限（辅助功能/屏幕录制）+ helper 路径 + 已注册工具数；
//   - 「已授权 App」编辑（统一授权门禁：未授权 App 的 computer_open_app/activate
//     会被工具拒绝并提示来这里添加；添加即永久记录，热生效无需重启）。
// 数据源 = plugin_list 附带字段 computer（bridge StatusSnapshot）+ settings.permissions。
export function ComputerSettingsEditor({ onClose }: { onClose: () => void }) {
  const t = useT()
  const plugins = useAppStore((s) => s.plugins)
  const settings = useAppStore((s) => s.settings)
  const computerApprovedSave = useAppStore((s) => s.computerApprovedSave)
  const pickAppFromSystem = useAppStore((s) => s.pickAppFromSystem)
  const cu = plugins.find((p) => p.id === 'computer')?.computer
  const apps = settings.permissions?.computer_approved_apps ?? []
  const [draft, setDraft] = useState('')

  const okColor = 'var(--success)'
  const badColor = 'var(--error)'

  const add = () => {
    const name = draft.trim()
    if (!name || apps.includes(name)) return
    computerApprovedSave([...apps, name])
    setDraft('')
  }
  const remove = (name: string) => computerApprovedSave(apps.filter((a) => a !== name))
  const pickFromSystem = async () => {
    const name = await pickAppFromSystem()
    if (!name || apps.includes(name)) return
    computerApprovedSave([...apps, name])
  }

  return (
    <div className="mcp-form">
      {/* —— 状态：权限 —— */}
      <div className="set-row">
        <div className="set-label">{t('plugin.computer.perm.access')}</div>
        <span style={{ color: cu?.accessibility ? okColor : badColor }}>
          {cu?.accessibility ? t('plugin.computer.perm.ok') : t('plugin.computer.perm.no')}
        </span>
      </div>
      <div className="set-row">
        <div className="set-label">{t('plugin.computer.perm.capture')}</div>
        <span style={{ color: cu?.screenCapture ? okColor : badColor }}>
          {cu?.screenCapture ? t('plugin.computer.perm.ok') : t('plugin.computer.perm.no')}
        </span>
      </div>
      {cu?.trustedApp && (
        <p className="set-desc">
          {t('plugin.computer.trustedApp')}: <span className="mono">{cu.trustedApp}</span>
        </p>
      )}
      {cu?.permError && <p className="mcp-s-err mono">{cu.permError}</p>}
      {!cu?.accessibility && <p className="mcp-s-err">{t('plugin.computer.perm.hint')}</p>}

      <div className="set-row">
        <div className="set-label">{t('plugin.computer.tools')}</div>
        <span className="mono">{cu?.toolCount ?? 0}</span>
      </div>
      {cu?.helperPath && (
        <p className="set-desc">
          {t('plugin.computer.helperPath')}: <span className="mono">{cu.helperPath}</span>
        </p>
      )}

      {/* —— 已授权 App（统一授权门禁） —— */}
      <div className="set-group" style={{ marginTop: 12 }}>
        <div className="set-label">{t('plugin.computer.approved.title')}</div>
        <p className="set-desc">{t('plugin.computer.approved.hint')}</p>
        <div className="perm-add">
          <input
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              // 输入法组合态（中文等用 Enter 选字/上屏）不得触发提交
              if (e.nativeEvent.isComposing || e.keyCode === 229) return
              if (e.key === 'Enter') add()
            }}
            placeholder={t('plugin.computer.approved.placeholder')}
            spellCheck={false}
            aria-label={t('plugin.computer.approved.placeholder')}
          />
          <button className="btn primary" onClick={add} disabled={!draft.trim()}>
            {t('permissions.add')}
          </button>
          <button className="btn" onClick={pickFromSystem} title={t('permissions.pick.title')}>
            {t('permissions.pick')}
          </button>
        </div>
        {apps.length === 0 ? (
          <p className="set-desc">{t('plugin.computer.approved.empty')}</p>
        ) : (
          <div className="perm-list">
            {apps.map((a) => (
              <div className="perm-item" key={a}>
                <span className="perm-name" title={a}>{a}</span>
                <button className="btn" onClick={() => remove(a)}>{t('permissions.remove')}</button>
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="mcp-form-btns">
        <button className="btn" onClick={onClose}>
          {t('plugin.browser.cancel')}
        </button>
      </div>
    </div>
  )
}
