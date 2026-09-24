import { useEffect, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { Back } from './icons'
import { useT } from '../i18n'
import SkillMarket from './SkillMarket'
import SkillManager from './SkillManager'

// Skills 管理（2026-08-15 拆分）：容器 = 头部 + 两个 Tab（技能市场 / 技能管理）。
// 技能市场：只记录市场信息，技能列表每次实时拉取、不存储；刷新时与本地已装对比标「安装/更新」。
// 技能管理：所有已下载技能（启用开关 / 查看指令 / 更新 / 卸载），空态引导去市场安装。
export default function SkillsPage() {
  const t = useT()
  const closeSkills = useAppStore((s) => s.closeSkills)
  const skills = useAppStore((s) => s.skills)
  const fetchSkills = useAppStore((s) => s.fetchSkills)
  const fetchMarkets = useAppStore((s) => s.fetchMarkets)
  const [tab, setTab] = useState<'market' | 'manager'>('manager')

  useEffect(() => {
    fetchSkills()
    fetchMarkets()
  }, [fetchSkills, fetchMarkets])

  const enabled = skills.filter((s) => s.enabled).length
  const remoteCount = skills.filter((s) => s.source).length

  return (
    <div className="skills-page">
      <header className="sp-head">
        <button className="btn" onClick={closeSkills}><Back /> {t('common.backToSession')}</button>
        <h1>{t('skill.pageTitle')}</h1>
        <span className="grow" />
        <span className="mono" style={{ fontSize: 11.5, color: 'var(--text-3)' }}>
          {t('skill.enabledCount').replace('{n}', String(enabled)).replace('{m}', String(skills.length))}{remoteCount > 0 ? t('skill.remoteCount').replace('{n}', String(remoteCount)) : ''}
        </span>
      </header>
      <div className="sp-body">
        <nav className="sp-side" role="tablist" aria-label="Skills">
          {(['market', 'manager'] as const).map((tb) => (
            <button
              key={tb}
              role="tab"
              aria-selected={tab === tb}
              className={tab === tb ? 'cur' : ''}
              onClick={() => setTab(tb)}
            >
              {tb === 'market' ? t('skill.tab.market') : t('skill.tab.manager')}
            </button>
          ))}
        </nav>
        <div className="sp-content">
          {tab === 'market' ? <SkillMarket /> : <SkillManager />}
        </div>
      </div>
    </div>
  )
}
