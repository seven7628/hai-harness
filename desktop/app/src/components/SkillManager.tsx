import { useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { FileIcon } from './icons'
import { useT } from '../i18n'

// 技能管理（2026-08-15 拆分）：所有已下载技能（全局 ~/.agents/skills + ~/.go-code/skills +
// 工作区 .agents/skills + .go-code/skills 四层，同名高层优先）。开关 = 真过滤（禁用技能从
// 模型发现清单消失 + load_skill 拒绝）；点卡片查看 SKILL.md 指令预览（skill_get，禁用技能也可看）；
// 远端技能卡片带来源 + 更新/卸载。空态引导去市场。
export default function SkillManager() {
  const t = useT()
  const skills = useAppStore((s) => s.skills)
  const skillDetail = useAppStore((s) => s.skillDetail)
  const toggleSkill = useAppStore((s) => s.toggleSkill)
  const getSkill = useAppStore((s) => s.getSkill)
  const closeSkill = useAppStore((s) => s.closeSkill)
  const updateSkill = useAppStore((s) => s.updateSkill)
  const uninstallSkill = useAppStore((s) => s.uninstallSkill)
  const workspace = useAppStore((s) => s.workspace)
  const [viewing, setViewing] = useState<string | null>(null)

  const openDetail = (name: string) => {
    setViewing(name)
    getSkill(name)
  }

  const doUpdate = (name: string) => updateSkill(name)
  const doUninstall = (name: string) => {
    if (window.confirm(t('skill.uninstallConfirm').replace('{name}', name))) uninstallSkill(name)
  }

  if (skills.length === 0) {
    return (
      <div className="sp-empty">
        <p>{t('skill.empty')}</p>
        <p className="mono" style={{ fontSize: 11.5, color: 'var(--text-3)' }}>
          {t('skill.emptyHint').replace('{market}', t('skill.tab.market'))}{' '}
          <code>{workspace}/.agents/skills/&lt;name&gt;/</code> {t('skill.emptyHintGlobal')} <code>~/.agents/skills/&lt;name&gt;/</code>
        </p>
      </div>
    )
  }

  return (
    <>
      <div className="sp-grid">
        {skills.map((s) => (
          <div className="skill-card" key={s.name} onClick={() => openDetail(s.name)} title={t('skill.viewInstructions')}>
            <div className="sc-h">
              <span className="sc-nm">{s.name}</span>
              {s.source && <span className="sc-remote">remote</span>}
              <span className={`sc-state ${s.enabled ? 'on' : ''}`}>{s.enabled ? t('skill.enabled') : t('skill.disabled')}</span>
              <button
                className={`switch ${s.enabled ? 'on' : ''}`}
                role="switch"
                aria-checked={s.enabled}
                aria-label={t('skill.enableAria').replace('{name}', s.name)}
                onClick={(e) => {
                  e.stopPropagation()
                  toggleSkill(s.name, !s.enabled)
                }}
              />
            </div>
            <div className="sc-desc">{s.description || t('skill.noDesc')}</div>
            {s.source && (
              <div className="sc-src">
                <span className="mono" title={s.source}>{t('skill.remoteSource')}: {s.source}</span>
                <span className="grow" />
                <button
                  className="btn"
                  onClick={(e) => {
                    e.stopPropagation()
                    doUpdate(s.name)
                  }}
                >
                  {t('skill.update')}
                </button>
                <button
                  className="btn"
                  onClick={(e) => {
                    e.stopPropagation()
                    doUninstall(s.name)
                  }}
                >
                  {t('skill.uninstall')}
                </button>
              </div>
            )}
            <div className="sc-meta">
              <span className="mono">{s.name}</span>
              <span className="grow" />
              <FileIcon size={12} />
            </div>
          </div>
        ))}
      </div>

      {viewing && skillDetail && skillDetail.name === viewing && (
        <div className="scrim" onClick={closeSkill}>
          <div className="skill-detail" onClick={(e) => e.stopPropagation()}>
            <div className="sd-h">
              <span className="sc-nm">{skillDetail.name}</span>
              <span className="grow" />
              <button className="btn" onClick={closeSkill}>{t('skill.close')}</button>
            </div>
            <pre className="sd-body">{skillDetail.instructions}</pre>
            {skillDetail.resources.length > 0 && (
              <div className="sd-res">
                <div className="sd-res-t">{t('skill.resources')}</div>
                {skillDetail.resources.map((r) => (
                  <div key={r} className="mono">{r}</div>
                ))}
              </div>
            )}
          </div>
        </div>
      )}
    </>
  )
}
