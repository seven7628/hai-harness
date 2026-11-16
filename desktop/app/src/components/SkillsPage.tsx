import { useEffect, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import { Sparkles } from './icons'
import SkillMarket from './SkillMarket'
import SkillManager from './SkillManager'
import PageHead from './PageHead'

// Skills 管理（2026-08-15 拆分）：容器 = 页头 + 两个 Tab（技能市场 / 技能管理）。
// 技能市场：只记录市场信息，技能列表每次实时拉取、不存储；刷新时与本地已装对比标「安装/更新」。
// 技能管理：所有已下载技能（启用开关 / 查看指令 / 更新 / 卸载），空态引导去市场安装。
// 2026-11 收敛：页头与定时任务共用 PageHead（56px），计数改成弱色 pill。
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
      <PageHead
        className="sp-head"
        backLabel={t('common.backToSession')}
        onBack={closeSkills}
        icon={<Sparkles size={15} />}
        title={t('skill.pageTitle')}
        right={
          <span
            className="page-head-count"
            title={remoteCount > 0 ? t('skill.remoteCount').replace('{n}', String(remoteCount)) : undefined}
          >
            {t('skill.enabledCount').replace('{n}', String(enabled)).replace('{m}', String(skills.length))}
          </span>
        }
      />
      <div className="sp-body">
        <nav className="sp-side" role="tablist" aria-label={t('skill.pageTitle')}>
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
          {tab === 'market' ? <SkillMarket /> : <SkillManager onBrowseMarket={() => setTab('market')} />}
        </div>
      </div>
    </div>
  )
}
