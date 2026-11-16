import { useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { FileIcon, Sparkles } from './icons'
import { useT } from '../i18n'
import EmptyState from './EmptyState'
import PageModal from './PageModal'

// 技能管理（2026-08-15 拆分）：所有已下载技能（全局 ~/.agents/skills + ~/.go-code/skills +
// 工作区 .agents/skills + .go-code/skills 四层，同名高层优先）。开关 = 真过滤（禁用技能从
// 模型发现清单消失 + load_skill 拒绝）；点卡片查看 SKILL.md 指令预览（skill_get，禁用技能也可看）；
// 远端技能卡片带来源 + 更新/卸载。空态引导去市场。
//
// 2026-11 收敛：卡片等高（描述 3 行截断、footer 贴底）、不可用原因中文兜底、
// 详情弹窗改成与设置弹窗同一套（PageModal：scrim + header/body/footer + Esc）。
// i18n: 建议 key skill.sourceLocal —— 卡片 footer 的来源文案（本地技能）
function unavailableText(reason?: string): string {
  if (!reason) return ''
  const r = reason.toLowerCase()
  // 后端英文原因常见两种：能力不满足（requires model input: image）与会话过滤。
  // 能映射的给中文人话；映射不了的原样显示（完整原文始终在 title 里）。
  if (r.includes('requires model input: image') || r.includes('input: image')) {
    return '当前模型不能读图；换用支持图片输入的模型后即可使用。'
  }
  if (r.includes('unavailable for this session')) {
    return '当前会话不可用该技能；换模型或换会话后再试。'
  }
  if (r.includes('requires tool')) {
    return '当前会话缺少该技能需要的工具。'
  }
  return reason
}

export default function SkillManager({ onBrowseMarket }: { onBrowseMarket?: () => void }) {
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
      <EmptyState
        icon={<Sparkles size={18} />}
        title={t('skill.empty')}
        hint={
          <>
            {t('skill.emptyHint').replace('{market}', t('skill.tab.market'))}{' '}
            <code>{workspace}/.agents/skills/&lt;name&gt;/</code> {t('skill.emptyHintGlobal')} <code>~/.agents/skills/&lt;name&gt;/</code>
          </>
        }
        actions={
          onBrowseMarket ? (
            <button className="btn" onClick={onBrowseMarket}>{t('skill.tab.market')}</button>
          ) : null
        }
      />
    )
  }

  const detail = skillDetail && skillDetail.name === viewing ? skillDetail : null

  return (
    <>
      <div className="sp-grid">
        {skills.map((s) => {
          const reasonText = unavailableText(s.unavailableReason)
          return (
            <div className="skill-card" key={s.name} onClick={() => openDetail(s.name)} title={t('skill.viewInstructions')}>
              <div className="sc-h">
                <span className="sc-nm" title={s.name}>{s.name}</span>
                {s.source && <span className="sc-remote">远程</span>}
                {/* 三态：可用 / 用户禁用 / 当前模型不支持。
                    后两者都是 enabled=false，但只有**用户禁用**该给开关 —— 给"模型不支持"
                    的卡片一个开关，用户会以为拨一下就生效，而真正要做的是换模型。 */}
                {s.unavailableReason ? (
                  <span className="sc-state cap" title={s.unavailableReason}>{t('skill.modelUnsupported')}</span>
                ) : (
                  <>
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
                  </>
                )}
              </div>
              <div className="sc-desc" title={s.description || undefined}>{s.description || t('skill.noDesc')}</div>
              {s.unavailableReason && (
                <div className="sc-cap-reason" title={s.unavailableReason}>{reasonText}</div>
              )}
              {/* 发丝线之上：来源/本地 + 远端动作（贴底，卡片等高时对齐） */}
              <div className="sc-src">
                <FileIcon size={11} />
                <span className="mono" title={s.source || '本地技能'}>{s.source || '本地技能'}</span>
                {s.source && (
                  <>
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
                  </>
                )}
              </div>
            </div>
          )
        })}
      </div>

      {detail && (
        <PageModal
          title={detail.name}
          // i18n: 建议 key skill.detailSub —— 详情弹窗副标题
          subtitle="技能指令（模型按需读取）"
          icon={<FileIcon size={14} />}
          onClose={() => {
            setViewing(null)
            closeSkill()
          }}
          width={680}
          cardClassName="skill-detail"
          closeLabel={t('skill.close')}
          footer={<button className="btn" onClick={() => { setViewing(null); closeSkill() }}>{t('skill.close')}</button>}
        >
          <pre className="sd-body">{detail.instructions}</pre>
          {(detail.resources ?? []).length > 0 ? (
            <div className="sd-res">
              <div className="sd-res-t">{t('skill.resources')}</div>
              {(detail.resources ?? []).map((r) => (
                <div key={r} className="mono" title={r}>{r}</div>
              ))}
            </div>
          ) : (
            // i18n: 建议 key skill.noResources —— 资源缺失时说明一句，不留白
            <div className="sd-res-empty">该技能没有附带资源文件。</div>
          )}
        </PageModal>
      )}
    </>
  )
}
