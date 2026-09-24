import { useEffect, useState } from 'react'
import { useAppStore } from '../store/useAppStore'
import { useT } from '../i18n'
import { codeToHtml } from './highlight'
import { useResolvedTheme } from '../lib/useTheme'
import {
  BG_PRESETS,
  DARK_CODE_THEMES,
  DEFAULT_APPEARANCE,
  LIGHT_CODE_THEMES,
  SCALE_OPTIONS,
  setAppearance,
  useAppearance,
} from '../lib/appearance'

// 「外观」设置页：主题 / 代码主题（亮·暗各一组）/ 字体大小 / 背景颜色。
//
// 为什么单独一页而不是塞进「通用设置」：这四项都是**改完立刻看得见**的纯外观项，
// 与通用页的「语言/沙箱/技能导入」混排会让人找不到刚动过什么（也就不好回滚）。
//
// 代码主题预览卡走**与对话代码块同一条渲染管线**（codeToHtml 的 themeOverride + 同一个
// shiki 单例）：卡上看到的就是选中后的效果，不会出现「预览好看、实况不对」的漂移。
// 卡里的示例故意用用户给的那段 themePreview —— 主题差异在「关键字/属性/字符串」上最明显。

const SAMPLE = `const theme: ThemeConfig = {
  accent: "#2563eb",
  contrast: 42,
};`

function CodeThemeCard({
  id,
  label,
  selected,
  dark = false,
  onPick,
}: {
  id: string
  label: string
  selected: boolean
  dark?: boolean
  onPick: () => void
}) {
  const resolved = useResolvedTheme()
  const [html, setHtml] = useState('')
  useEffect(() => {
    let alive = true
    codeToHtml(SAMPLE, 'ts', resolved, false, id)
      .then((h) => { if (alive) setHtml(h) })
      .catch(() => { /* 主题载不动：卡片留空，不炸设置页 */ })
    return () => { alive = false }
  }, [id, resolved])
  return (
    <button
      type="button"
      className={`ct-card ${selected ? 'cur' : ''} ${dark ? 'dark' : ''}`}
      aria-pressed={selected}
      onClick={onPick}
    >
      <span className="ct-card-hd">
        <span className="ct-card-name">{label}</span>
        {selected && <span className="ct-card-cur" aria-hidden>✓</span>}
      </span>
      <span className="ct-card-code" dangerouslySetInnerHTML={{ __html: html }} />
    </button>
  )
}

export default function AppearanceTab() {
  const t = useT()
  const settings = useAppStore((s) => s.settings)
  const setTheme = useAppStore((s) => s.setTheme)
  const setNarrativeSkin = useAppStore((s) => s.setNarrativeSkin)
  const ap = useAppearance()

  const themeOptions: { value: 'dark' | 'light' | 'system'; label: string }[] = [
    { value: 'dark', label: t('settings.dark') },
    { value: 'light', label: t('settings.light') },
    { value: 'system', label: t('settings.system') },
  ]
  const skinOptions: { value: 'current' | 'v3'; label: string }[] = [
    { value: 'current', label: t('settings.narrativeSkin.current') },
    { value: 'v3', label: t('settings.narrativeSkin.v3') },
  ]

  return (
    <>
      <div className="settings-sec-title">{t('settings.appearance')}</div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.theme')}</div>
          <div className="set-desc">{t('settings.appearance.theme.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.theme')}>
          {themeOptions.map((o) => (
            <button
              key={o.value}
              className={settings.theme === o.value ? 'cur' : ''}
              aria-pressed={settings.theme === o.value}
              onClick={() => setTheme(o.value)}
            >
              {o.label}
            </button>
          ))}
        </div>
      </div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.narrativeSkin')}</div>
          <div className="set-desc">{t('settings.narrativeSkin.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.narrativeSkin')}>
          {skinOptions.map((o) => (
            <button
              key={o.value}
              className={(settings.narrative_skin ?? 'current') === o.value ? 'cur' : ''}
              aria-pressed={(settings.narrative_skin ?? 'current') === o.value}
              onClick={() => setNarrativeSkin(o.value)}
            >
              {o.label}
            </button>
          ))}
        </div>
      </div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.appearance.fontSize')}</div>
          <div className="set-desc">{t('settings.appearance.fontSize.desc')}</div>
        </div>
        <div className="seg" role="radiogroup" aria-label={t('settings.appearance.fontSize')}>
          {SCALE_OPTIONS.map((o) => (
            <button
              key={o.value}
              className={ap.scale === o.value ? 'cur' : ''}
              aria-pressed={ap.scale === o.value}
              onClick={() => setAppearance({ scale: o.value })}
            >
              {t(o.labelKey)}
            </button>
          ))}
        </div>
      </div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.appearance.bg')}</div>
          <div className="set-desc">{t('settings.appearance.bg.desc')}</div>
        </div>
        <div className="bg-picker">
          {BG_PRESETS.map((c) => (
            <button
              key={c}
              type="button"
              className={`bg-sw ${ap.lightBg === c ? 'cur' : ''}`}
              style={{ background: c }}
              title={c}
              aria-label={c}
              aria-pressed={ap.lightBg === c}
              onClick={() => setAppearance({ lightBg: c })}
            />
          ))}
          <label className="bg-custom" title={t('settings.appearance.custom')}>
            <input
              type="color"
              value={ap.lightBg}
              aria-label={t('settings.appearance.custom')}
              onChange={(e) => setAppearance({ lightBg: e.target.value })}
            />
            <span className="mono">{ap.lightBg}</span>
          </label>
          <button className="btn" onClick={() => setAppearance({ lightBg: DEFAULT_APPEARANCE.lightBg })}>
            {t('settings.appearance.bg.reset')}
          </button>
        </div>
      </div>

      <div className="ct-block">
        <div className="ct-block-hd">{t('settings.appearance.codeLight')}</div>
        <div className="set-desc">{t('settings.appearance.codeLight.desc')}</div>
        <div className="ct-grid">
          {LIGHT_CODE_THEMES.map((x) => (
            <CodeThemeCard
              key={x.id}
              id={x.id}
              label={x.label}
              selected={ap.codeThemeLight === x.id}
              onPick={() => setAppearance({ codeThemeLight: x.id })}
            />
          ))}
        </div>
      </div>

      <div className="set-row">
        <div>
          <div className="set-label">{t('settings.appearance.reset')}</div>
          <div className="set-desc">{t('settings.appearance.reset.desc')}</div>
        </div>
        <button className="btn" onClick={() => setAppearance({ ...DEFAULT_APPEARANCE })}>
          {t('settings.appearance.reset.btn')}
        </button>
      </div>

      <div className="ct-block">
        <div className="ct-block-hd">{t('settings.appearance.codeDark')}</div>
        <div className="set-desc">{t('settings.appearance.codeDark.desc')}</div>
        <div className="ct-grid">
          {DARK_CODE_THEMES.map((x) => (
            <CodeThemeCard
              key={x.id}
              id={x.id}
              label={x.label}
              selected={ap.codeThemeDark === x.id}
              dark
              onPick={() => setAppearance({ codeThemeDark: x.id })}
            />
          ))}
        </div>
      </div>
    </>
  )
}
