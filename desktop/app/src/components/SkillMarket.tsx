import { useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import { LayersIcon } from './icons'
import EmptyState from './EmptyState'

// 技能市场：只记录市场信息（已保存市场 URL）；技能列表每次实时拉取、不存储。
// 刷新/浏览时与本地已装对比标出三态：未安装 → [安装]；已安装·最新 → 「已是最新」；
// 可更新（同源仓库 HEAD 落后本地记录）→ [更新]。安装/更新后自动重浏览当前市场（替代乐观标记）。
//
// 2026-11 收敛（只改组织与样式，安装逻辑与 handler 原样保留）：
// 页头之下分三块 ——「从链接安装」卡（字段化）→「已保存的市场」（含空态）→「浏览结果」。
export default function SkillMarket() {
  const t = useT()
  const browseMarket = useAppStore((s) => s.browseMarket)
  const refreshMarket = useAppStore((s) => s.refreshMarket)
  const marketSkills = useAppStore((s) => s.marketSkills)
  const installSkill = useAppStore((s) => s.installSkill)
  const updateSkill = useAppStore((s) => s.updateSkill)
  const markets = useAppStore((s) => s.markets)
  const addMarket = useAppStore((s) => s.addMarket)
  const removeMarket = useAppStore((s) => s.removeMarket)
  const updateMarketSkills = useAppStore((s) => s.updateMarketSkills)
  // 远程安装/浏览表单
  const [url, setUrl] = useState('')
  const [scope, setScope] = useState<'global' | 'workspace'>('global')
  const [err, setErr] = useState<string | null>(null)
  const [browsing, setBrowsing] = useState(false)
  const [installing, setInstalling] = useState(false)
  // 本次浏览用的链接（行内安装/更新/刷新据此）；行内操作忙态
  const [marketUrl, setMarketUrl] = useState('')
  const [acting, setActing] = useState<string | null>(null)
  const [updatingMarket, setUpdatingMarket] = useState<string | null>(null)

  // 浏览/刷新：实时拉取该 URL 的技能清单（只读）+ 与本地对比
  const doBrowse = (u: string) => {
    if (!u.trim()) {
      setErr(t('skill.remoteErr'))
      return
    }
    setBrowsing(true)
    setErr(null)
    setMarketUrl(u.trim())
    browseMarket(u.trim())
    window.setTimeout(() => setBrowsing(false), 1200)
  }

  const doInstall = () => {
    if (!url.trim()) {
      setErr(t('skill.remoteErr'))
      return
    }
    setInstalling(true)
    setErr(null)
    installSkill(url.trim(), scope)
    window.setTimeout(() => {
      setInstalling(false)
      setUrl('')
    }, 1500)
  }

  const doSaveMarket = () => {
    if (!url.trim()) {
      setErr(t('skill.remoteErr'))
      return
    }
    addMarket(url.trim())
  }

  // 市场行内安装：精确装单个 + 重浏览（bridge 按序处理：skill_install 完成后 skill_browse 见新状态）
  const doMarketInstall = (name: string) => {
    setActing(name)
    installSkill(marketUrl, scope, name)
    refreshMarket(marketUrl)
    window.setTimeout(() => setActing(null), 1500)
  }
  // 市场行内更新：更新已装技能（按清单记录的 Scope 原地更新）+ 重浏览
  const doMarketUpdate = (name: string) => {
    setActing(name)
    updateSkill(name)
    refreshMarket(marketUrl)
    window.setTimeout(() => setActing(null), 1500)
  }
  // 按市场更新该市场全部已装技能
  const doUpdateMarket = (mkUrl: string) => {
    setUpdatingMarket(mkUrl)
    updateMarketSkills(mkUrl)
    window.setTimeout(() => setUpdatingMarket(null), 1500)
  }

  return (
    <div className="sp-market-view">
      {/* 从链接安装 / 存为市场：字段化（链接 → 安装到 → 动作） */}
      <section className="mk-card">
        <div className="mk-card-hd">
          {/* i18n: 建议 key skill.installFromLink */}
          <span className="mk-card-title">从链接安装</span>
          <span className="mk-card-sub">{scope === 'global' ? '全局' : '当前工作区'}</span>
        </div>
        <p className="mk-card-desc">
          {t('skill.remoteHint')}
        </p>
        {err && <p className="mk-status error" role="alert">{err}</p>}
        <div className="mk-field">
          {/* i18n: 建议 key skill.linkLabel */}
          <label className="mk-field-label" htmlFor="skill-remote-url">链接</label>
          <input
            id="skill-remote-url"
            className="sp-remote-url"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder={t('skill.remoteUrlPh')}
            aria-label={t('skill.remoteUrl')}
            spellCheck={false}
            onKeyDown={(e) => {
              // 输入法组合态（中文等用 Enter 选字/上屏）不得触发安装
              if (e.nativeEvent.isComposing || e.keyCode === 229) return
              if (e.key === 'Enter' && !installing && !browsing) doInstall()
            }}
          />
        </div>
        <div className="mk-field">
          {/* i18n: 建议 key skill.installTo */}
          <span className="mk-field-label">安装到</span>
          <div className="seg sp-remote-seg" role="radiogroup" aria-label={t('skill.remoteScope')}>
            {(['global', 'workspace'] as const).map((sc) => (
              <button key={sc} className={scope === sc ? 'cur' : ''} aria-pressed={scope === sc} onClick={() => setScope(sc)}>
                {sc === 'global' ? t('skill.remoteScopeGlobal') : t('skill.remoteScopeWorkspace')}
              </button>
            ))}
          </div>
        </div>
        <div className="mk-actions">
          <button className="btn primary" onClick={doInstall} disabled={installing}>
            {installing ? t('skill.remoteInstalling') : t('skill.remoteInstall')}
          </button>
          <button className="btn" onClick={() => doBrowse(url)} disabled={browsing}>
            {browsing ? t('skill.browsing') : t('skill.browseBtn')}
          </button>
          <button className="btn ghost" onClick={doSaveMarket} disabled={installing}>
            {t('skill.saveMarket')}
          </button>
        </div>
      </section>

      {/* 已保存的市场（免手填 + 按市场更新已装技能） */}
      <section className="mk-card">
        <div className="mk-sec-hd">
          {/* i18n: 建议 key skill.savedMarkets */}
          <span className="mk-sec-title">已保存的市场</span>
          <span className="mk-sec-count">{markets.length}</span>
        </div>
        {markets.length === 0 ? (
          <EmptyState
            compact
            icon={<LayersIcon size={16} />}
            title="还没有保存的市场"
            // i18n: 建议 key skill.savedMarketsEmptyHint
            hint="把常逛的市场链接保存下来，下次可以一键浏览、批量更新。"
          />
        ) : (
          <ul className="sp-market-list">
            {markets.map((mk) => (
              <li className="sp-market-row mk" key={mk.url}>
                <span className="sp-market-nm" title={mk.name}>{mk.name}</span>
                <span className="sp-market-desc" title={mk.url}>{mk.url}</span>
                <span className="grow" />
                <button className="btn" onClick={() => doBrowse(mk.url)}>{t('skill.browseBtn')}</button>
                <button className="btn" onClick={() => doUpdateMarket(mk.url)} disabled={updatingMarket === mk.url}>
                  {updatingMarket === mk.url ? '…' : t('skill.updateMarket')}
                </button>
                <button className="btn ghost" onClick={() => removeMarket(mk.url)}>{t('skill.unbindMarket')}</button>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* 浏览结果（skill_browse 实时拉取，与本地对比：安装/最新/更新） */}
      {marketSkills !== undefined && (
        <section className="mk-card">
          <div className="mk-sec-hd">
            {/* i18n: 建议 key skill.browseResults */}
            <span className="mk-sec-title">浏览结果</span>
            <span className="mk-sec-count">{marketSkills.length}</span>
            <span className="grow" />
            <button className="btn ghost" onClick={() => doBrowse(marketUrl)} disabled={browsing}>
              {t('skill.refresh')}
            </button>
          </div>
          {marketSkills.length === 0 ? (
            <EmptyState compact icon={<LayersIcon size={16} />} title={t('skill.marketEmpty')} hint="换个链接再浏览一次。" />
          ) : (
            <ul className="sp-market-list">
              {marketSkills.map((s) => {
                const busy = acting === s.name
                return (
                  <li className="sp-market-row" key={s.name}>
                    <span className="sp-market-nm" title={s.name}>{s.name}</span>
                    <span className="sp-market-desc" title={s.description}>{s.description || t('skill.noDesc')}</span>
                    <span className="grow" />
                    {!s.installed ? (
                      <button className="btn primary" disabled={busy} onClick={() => doMarketInstall(s.name)}>
                        {busy ? '…' : t('skill.remoteInstall')}
                      </button>
                    ) : s.update_available ? (
                      <button className="btn" disabled={busy} onClick={() => doMarketUpdate(s.name)}>
                        {busy ? '…' : t('skill.updateAvailable')}
                      </button>
                    ) : (
                      <span className="sp-market-installed">{t('skill.upToDate')}</span>
                    )}
                  </li>
                )
              })}
            </ul>
          )}
        </section>
      )}
    </div>
  )
}
